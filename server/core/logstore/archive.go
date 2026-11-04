package logstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/logging"
)

// ArchiveLogStore keeps job logs on local disk while a job runs and copies
// each completed log to object storage, so that:
//   - any replica can serve a finished job's output without proxying to the
//     replica that ran it, and
//   - job output survives the loss of that replica or its volume.
//
// Live output still streams from the replica running the job.
//
// Object layout under the bucket prefix:
//
//	pulls/<escaped repo>/<pull>/<jobID>.log   the log
//	jobs/<jobID>                              pointer holding the log's key
//
// The pointer exists because lookups only know the job ID.
type ArchiveLogStore struct {
	local  *FileLogStore
	bucket *objstore.Bucket
	logger logging.SimpleLogging

	uploads sync.WaitGroup
	// retries bounds upload attempts; backoff doubles from backoff.
	retries int
	backoff time.Duration

	mu sync.Mutex
	// inflight has an entry per upload in progress, by job ID, so DeletePull
	// can stop and wait for its pull's uploads.
	inflight map[string]*inflightUpload
}

// inflightUpload is one upload in progress.
type inflightUpload struct {
	pull Pull
	// cancel is closed to stop waiting for a retry.
	cancel     chan struct{}
	cancelOnce sync.Once
	done       chan struct{}
}

// uploadWait bounds how long DeletePull waits for a pull's uploads; an upload
// still running after it removes what it wrote itself.
const uploadWait = objstore.OpTimeout

var _ LogStore = (*ArchiveLogStore)(nil)

const pointerKeyMeta = "log-key"

// NewArchiveLogStore wraps local so completed logs are archived to bucket.
func NewArchiveLogStore(local *FileLogStore, bucket *objstore.Bucket, logger logging.SimpleLogging) *ArchiveLogStore {
	a := &ArchiveLogStore{local: local, bucket: bucket, logger: logger, retries: 5, backoff: time.Second, inflight: map[string]*inflightUpload{}}
	// onComplete runs with the local store's locks held; only register and
	// start the upload. It never runs for a job after its pull was deleted:
	// DeletePull holds the same locks.
	local.onComplete = func(pull Pull, jobID, path string) {
		u := &inflightUpload{pull: pull, cancel: make(chan struct{}), done: make(chan struct{})}
		a.mu.Lock()
		a.inflight[jobID] = u
		a.mu.Unlock()
		a.uploads.Go(func() {
			defer func() {
				a.mu.Lock()
				if a.inflight[jobID] == u {
					delete(a.inflight, jobID)
				}
				a.mu.Unlock()
				close(u.done)
			}()
			a.upload(pull, jobID, path, u.cancel)
		})
	}
	return a
}

func (a *ArchiveLogStore) pullPrefix(pull Pull) string {
	return a.bucket.Key("pulls", url.PathEscape(pull.RepoFullName), strconv.Itoa(pull.Num)) + "/"
}

func (a *ArchiveLogStore) logKey(pull Pull, jobID string) string {
	return a.pullPrefix(pull) + jobID + ".log"
}

func (a *ArchiveLogStore) pointerKey(jobID string) string {
	return a.bucket.Key("jobs", jobID)
}

func (a *ArchiveLogStore) upload(pull Pull, jobID, path string, cancel <-chan struct{}) {
	var err error
	for attempt := range a.retries {
		if attempt > 0 {
			select {
			case <-cancel:
				return
			case <-time.After(a.backoff << (attempt - 1)):
			}
		}
		if a.local.jobDeleted(jobID) {
			// Its pull request was deleted meanwhile. An earlier attempt may
			// have written the log before failing on the pointer.
			if attempt > 0 {
				a.removeArchived(pull, jobID)
			}
			return
		}
		if err = a.uploadOnce(pull, jobID, path); errors.Is(err, os.ErrNotExist) {
			return // its pull request was deleted meanwhile
		}
		if err == nil {
			if a.local.jobDeleted(jobID) {
				// Deleted while writing, after DeletePull stopped
				// waiting: remove what this upload wrote.
				a.removeArchived(pull, jobID)
			}
			return
		}
		a.logger.Warn("job log archive: uploading job %s (attempt %d): %s", jobID, attempt+1, err)
	}
	a.logger.Err("job log archive: giving up on job %s after %d attempts: %s", jobID, a.retries, err)
}

func (a *ArchiveLogStore) uploadOnce(pull Pull, jobID, path string) error {
	f, err := os.Open(path) // #nosec G304 -- path comes from the local store's own index.
	if err != nil {
		return err
	}
	defer f.Close() // nolint: errcheck
	ctx, cancel := context.WithTimeout(context.Background(), objstore.OpTimeout)
	defer cancel()
	key := a.logKey(pull, jobID)
	if err := a.bucket.Put(ctx, key, f, map[string]string{"repo": pull.RepoFullName, "pull": strconv.Itoa(pull.Num)}); err != nil {
		return err
	}
	// Written second, so a visible pointer always refers to a complete log.
	return a.bucket.Put(ctx, a.pointerKey(jobID), strings.NewReader(""), map[string]string{pointerKeyMeta: key})
}

// Write implements LogStore.
func (a *ArchiveLogStore) Write(pull Pull, jobID, line string) { a.local.Write(pull, jobID, line) }

// Complete implements LogStore. The upload starts once the local log is
// fully written.
func (a *ArchiveLogStore) Complete(jobID string) { a.local.Complete(jobID) }

// Exists implements LogStore.
func (a *ArchiveLogStore) Exists(jobID string) (bool, error) {
	if ok, err := a.local.Exists(jobID); ok || err != nil {
		return ok, err
	}
	if !ValidJobID(jobID) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), objstore.OpTimeout)
	defer cancel()
	return a.bucket.Exists(ctx, a.pointerKey(jobID))
}

// Replay implements LogStore, preferring the local copy.
func (a *ArchiveLogStore) Replay(jobID string, fn func(line string) bool) error {
	if ok, err := a.local.Exists(jobID); err != nil || ok {
		if err != nil {
			return err
		}
		return a.local.Replay(jobID, fn)
	}
	if !ValidJobID(jobID) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), objstore.OpTimeout)
	defer cancel()
	ptr, err := a.bucket.Get(ctx, a.pointerKey(jobID))
	if objstore.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = ptr.Body.Close()
	key := ""
	for k, v := range ptr.Metadata {
		if strings.EqualFold(k, pointerKeyMeta) {
			key = v
		}
	}
	if key == "" {
		return fmt.Errorf("job log archive: pointer for job %s has no log key", jobID)
	}
	obj, err := a.bucket.Get(ctx, key)
	if objstore.IsNotFound(err) {
		return nil // deleted with its pull request in the meantime
	}
	if err != nil {
		return err
	}
	defer obj.Body.Close() // nolint: errcheck
	return replayLines(obj.Body, fn)
}

func replayLines(r io.Reader, fn func(string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if !fn(sc.Text()) {
			return nil
		}
	}
	return sc.Err()
}

// removeArchived deletes a job's archived log and pointer.
func (a *ArchiveLogStore) removeArchived(pull Pull, jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), objstore.OpTimeout)
	defer cancel()
	if err := errors.Join(a.bucket.Delete(ctx, a.pointerKey(jobID)), a.bucket.Delete(ctx, a.logKey(pull, jobID))); err != nil {
		a.logger.Warn("job log archive: removing job %s of a deleted pull request: %s", jobID, err)
	}
}

// DeletePull implements LogStore. Local and archived logs are both removed;
// all failures are returned. The local store first records the pull's jobs
// as deleted, so they write and upload nothing more; then the pull's uploads
// in progress are stopped or waited for, so none writes after the listing
// below.
func (a *ArchiveLogStore) DeletePull(pull Pull) error {
	errs := []error{a.local.DeletePull(pull)}
	a.waitForUploads(pull)
	ctx := context.Background()
	prefix := a.pullPrefix(pull)
	for key, err := range a.bucket.List(ctx, prefix) {
		if err != nil {
			errs = append(errs, err)
			break
		}
		jobID := strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".log")
		opCtx, cancel := context.WithTimeout(ctx, objstore.OpTimeout)
		// Pointer first, so a job is never visible without its log.
		errs = append(errs, a.bucket.Delete(opCtx, a.pointerKey(jobID)), a.bucket.Delete(opCtx, key))
		cancel()
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("deleting job logs for %s#%d: %w", pull.RepoFullName, pull.Num, err)
	}
	return nil
}

// waitForUploads stops the pull's uploads that are waiting to retry and waits
// up to uploadWait for those that are writing.
func (a *ArchiveLogStore) waitForUploads(pull Pull) {
	a.mu.Lock()
	var ups []*inflightUpload
	for _, u := range a.inflight {
		if u.pull == pull {
			ups = append(ups, u)
		}
	}
	a.mu.Unlock()
	deadline := time.NewTimer(uploadWait)
	defer deadline.Stop()
	for _, u := range ups {
		u.cancelOnce.Do(func() { close(u.cancel) })
		select {
		case <-u.done:
		case <-deadline.C:
			a.logger.Warn("job log archive: uploads of %s#%d still running after %s; they remove what they write", pull.RepoFullName, pull.Num, uploadWait)
			return
		}
	}
}

// Close implements LogStore: it flushes local logs, then waits up to 30s for
// in-flight uploads.
func (a *ArchiveLogStore) Close() error {
	err := a.local.Close()
	done := make(chan struct{})
	go func() { a.uploads.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		a.logger.Warn("job log archive: shutting down with uploads still in progress")
	}
	return err
}
