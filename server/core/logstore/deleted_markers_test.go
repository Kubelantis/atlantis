package logstore_test

import (
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/core/logstore"
	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

func archiveWithLocal(t *testing.T, g *gatedS3) (*logstore.ArchiveLogStore, *logstore.FileLogStore) {
	local, err := logstore.NewFileLogStore(t.TempDir(), time.Hour, logging.NewNoopLogger(t))
	Ok(t, err)
	return logstore.NewArchiveLogStore(local, objstore.NewBucket(g, objstore.Config{Bucket: "b", Prefix: "logs"}), logging.NewNoopLogger(t)), local
}

func assertLocalGone(t *testing.T, local *logstore.FileLogStore) {
	t.Helper()
	ok, err := local.Exists("job-1")
	Ok(t, err)
	Assert(t, !ok, "the local log was recreated")
}

// Output written while DeletePull runs, after it dropped the pull's pending
// jobs, must not recreate the log or reach the archive.
func TestWriteDuringDeletePullIsDropped(t *testing.T) {
	g := newGatedS3(-1)
	a, local := archiveWithLocal(t, g)
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "line before close")
	local.Flush()

	local.SetAfterDropJobs(func() { a.Write(pull, "job-1", "line during close") })
	Ok(t, a.DeletePull(pull))
	local.SetAfterDropJobs(nil)
	a.Complete("job-1")
	local.Flush()

	assertLocalGone(t, local)
	assertNothingArchived(t, g, a)
}

// A job still running more than deletedJobTTL after its pull was deleted
// stays deleted, even when a later cleanup expires old markers. Its marker
// expires only once the job has completed.
func TestRunningJobStaysDeletedPastMarkerTTL(t *testing.T) {
	g := newGatedS3(-1)
	a, local := archiveWithLocal(t, g)
	now := time.Now()
	local.SetClock(func() time.Time { return now })
	pull := logstore.Pull{RepoFullName: "org/repo", Num: 7}
	a.Write(pull, "job-1", "line before close")
	local.Flush()
	Ok(t, a.DeletePull(pull))

	now = now.Add(2 * time.Hour)
	Ok(t, a.DeletePull(logstore.Pull{RepoFullName: "org/other", Num: 1})) // expires old markers
	a.Write(pull, "job-1", "line two hours after close")
	local.Flush()
	assertLocalGone(t, local)

	// Once the job completes, its marker expires after the TTL.
	a.Complete("job-1")
	now = now.Add(2 * time.Hour)
	Ok(t, a.DeletePull(logstore.Pull{RepoFullName: "org/other", Num: 2}))
	Equals(t, 0, local.DeletedMarkers())
	assertNothingArchived(t, g, a)
}
