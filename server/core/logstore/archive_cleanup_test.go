package logstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/runatlantis/atlantis/server/core/logstore"
	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/core/objstore/objstoretest"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// gatedS3 blocks its n-th PutObject until release is closed.
type gatedS3 struct {
	*objstoretest.Fake
	n       int
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func newGatedS3(n int) *gatedS3 {
	return &gatedS3{Fake: objstoretest.New(), n: n, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedS3) PutObject(ctx context.Context, in *s3.PutObjectInput, o ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	g.mu.Lock()
	g.calls++
	gate := g.calls == g.n
	g.mu.Unlock()
	if gate {
		close(g.entered)
		<-g.release
	}
	return g.Fake.PutObject(ctx, in, o...)
}

func gatedReplica(t *testing.T, g *gatedS3) *logstore.ArchiveLogStore {
	local, err := logstore.NewFileLogStore(t.TempDir(), 10*time.Millisecond, logging.NewNoopLogger(t))
	Ok(t, err)
	a := logstore.NewArchiveLogStore(local, objstore.NewBucket(g, objstore.Config{Bucket: "b", Prefix: "logs"}), logging.NewNoopLogger(t))
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// deleteWhileBlocked runs DeletePull while the gated Put is in progress and
// releases the Put shortly after, as a slow upload would finish.
func deleteWhileBlocked(t *testing.T, g *gatedS3, a *logstore.ArchiveLogStore, pull logstore.Pull) {
	t.Helper()
	<-g.entered
	done := make(chan error, 1)
	go func() { done <- a.DeletePull(pull) }()
	time.Sleep(100 * time.Millisecond)
	close(g.release)
	select {
	case err := <-done:
		Ok(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("DeletePull did not return")
	}
}

func assertNothingArchived(t *testing.T, g *gatedS3, a *logstore.ArchiveLogStore) {
	t.Helper()
	Ok(t, a.Close())
	Equals(t, 0, len(g.Keys()))
	ok, err := a.Exists("job-1")
	Ok(t, err)
	Assert(t, !ok, "deleted job is still visible")
}

// An upload already writing the log when the pull is cleaned up must not
// leave the log or its pointer behind.
func TestDeletePullRemovesLogUploadedDuringCleanup(t *testing.T) {
	g := newGatedS3(1)
	a := gatedReplica(t, g)
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "plan output")
	a.Complete("job-1")

	deleteWhileBlocked(t, g, a, pull)
	assertNothingArchived(t, g, a)
}

// The same when the cleanup lands between the log and its pointer.
func TestDeletePullRemovesPointerWrittenDuringCleanup(t *testing.T) {
	g := newGatedS3(2)
	a := gatedReplica(t, g)
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "plan output")
	a.Complete("job-1")

	deleteWhileBlocked(t, g, a, pull)
	assertNothingArchived(t, g, a)
}

// A job still running when the pull is cleaned up must not recreate its log,
// locally or in the archive.
func TestJobRunningAtCleanupIsNotArchived(t *testing.T) {
	g := newGatedS3(-1)
	local, err := logstore.NewFileLogStore(t.TempDir(), time.Hour, logging.NewNoopLogger(t))
	Ok(t, err)
	a := logstore.NewArchiveLogStore(local, objstore.NewBucket(g, objstore.Config{Bucket: "b", Prefix: "logs"}), logging.NewNoopLogger(t))
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}

	a.Write(pull, "job-1", "line before close")
	local.Flush()
	Ok(t, a.DeletePull(pull))
	a.Write(pull, "job-1", "line after close")
	a.Complete("job-1")
	local.Flush()

	assertNothingArchived(t, g, a)
	ok, err := local.Exists("job-1")
	Ok(t, err)
	Assert(t, !ok, "the local log was recreated")
}

// A reopened pull's new jobs are archived as usual.
func TestNewJobAfterCleanupIsArchived(t *testing.T) {
	g := newGatedS3(-1)
	a := gatedReplica(t, g)
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "before close")
	a.Complete("job-1")
	waitFor(t, func() bool { return len(g.Keys()) == 2 })
	Ok(t, a.DeletePull(pull))

	a.Write(pull, "job-2", "after reopen")
	a.Complete("job-2")
	waitFor(t, func() bool { ok, _ := a.Exists("job-2"); return ok && len(g.Keys()) == 2 })
	Equals(t, []string{"logs/jobs/job-2", "logs/pulls/org%2Frepo/7/job-2.log"}, g.Keys())
}

// failingS3 fails its n-th PutObject.
type failingS3 struct {
	*objstoretest.Fake
	n     int
	mu    sync.Mutex
	calls int
}

func (f *failingS3) PutObject(ctx context.Context, in *s3.PutObjectInput, o ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls == f.n
	f.mu.Unlock()
	if fail {
		return nil, context.DeadlineExceeded
	}
	return f.Fake.PutObject(ctx, in, o...)
}

// An upload waiting to retry after writing only the log is stopped by
// DeletePull, which then removes the log: nothing is left, and shutdown does
// not wait for the retry.
func TestDeletePullStopsUploadWaitingToRetry(t *testing.T) {
	f := &failingS3{Fake: objstoretest.New(), n: 2} // the pointer write fails
	local, err := logstore.NewFileLogStore(t.TempDir(), 10*time.Millisecond, logging.NewNoopLogger(t))
	Ok(t, err)
	a := logstore.NewArchiveLogStore(local, objstore.NewBucket(f, objstore.Config{Bucket: "b", Prefix: "logs"}), logging.NewNoopLogger(t))
	a.SetBackoff(time.Hour)
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "plan output")
	a.Complete("job-1")
	waitFor(t, func() bool { return len(f.Keys()) == 1 }) // the log, without its pointer

	Ok(t, a.DeletePull(pull))
	Equals(t, 0, len(f.Keys()))
	start := time.Now()
	Ok(t, a.Close())
	Assert(t, time.Since(start) < 5*time.Second, "Close waited for the retry: %s", time.Since(start))
}
