// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package logstore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runatlantis/atlantis/server/logging"
)

const (
	// DirName is the directory under the data dir that FileLogStore writes to
	// by default.
	DirName = "job-logs"

	// DefaultFlushInterval bounds how far a job's file lags its output: on a
	// crash, or for a viewer on another replica sharing the directory.
	DefaultFlushInterval = 2 * time.Second

	// flushBytes triggers a flush ahead of the interval once a job has this
	// much output buffered, so a noisy job neither waits a full interval nor
	// holds much in memory.
	flushBytes = 256 << 10

	// maxPendingBytes caps what is buffered for one job while writes to its
	// file keep failing. Past it, lines are counted instead of kept, and a
	// marker recording how many were dropped is written once writes succeed.
	maxPendingBytes = 16 << 20

	// maxCompleteFlushAttempts bounds retries for a finished job, so a
	// permanently failing volume cannot hold its output in memory forever.
	maxCompleteFlushAttempts = 10

	// deletedJobTTL is how long DeletePull remembers the jobs it deleted, so
	// output still arriving from them is dropped. Job IDs are not reused.
	deletedJobTTL = time.Hour
)

// FileLogStore persists job output as plain-text files on local disk, laid
// out as <root>/<escaped repo full name>/<pull num>/<job id>.log. Grouping by
// pull request lets DeletePull drop a closed pull request's logs in one step,
// even after a restart has emptied the output handler's memory.
//
// Write and Complete never touch the filesystem. Output is buffered per job
// and appended to its file by a background flusher every flush interval, so
// a slow volume (such as a network mount) never stalls the goroutine that
// fans output out to live viewers. At most one interval of output is lost if
// the process dies.
type FileLogStore struct {
	root   string
	logger logging.SimpleLogging

	mu   sync.Mutex
	jobs map[string]*pendingLog
	// deleted holds the jobs of pull requests deleted by DeletePull, with
	// when; their later output is dropped (see deletedJobTTL).
	deleted map[string]time.Time
	closed  bool

	// flushMu serializes file I/O: flushes, DeletePull and Close. It is always
	// taken before mu, never while holding it.
	flushMu sync.Mutex

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	// index maps a job ID to its log file. It is loaded from disk once, on
	// first use, and kept current by create and DeletePull, so lookups do not
	// walk the directory tree.
	indexOnce sync.Once
	indexMu   sync.RWMutex
	index     map[string]string

	// onComplete, if set, is called after a completed job's log file has been
	// fully written, synced and closed.
	onComplete func(pull Pull, jobID, path string)
}

// pendingLog is a job's output not yet appended to its file.
type pendingLog struct {
	pull     Pull
	buf      bytes.Buffer
	dropped  int
	complete bool
	failures int

	// f is only used by the flusher, under flushMu.
	f *os.File
}

// NewFileLogStore returns a FileLogStore rooted at root, creating it if
// needed, that flushes buffered output every flushInterval until Close.
func NewFileLogStore(root string, flushInterval time.Duration, logger logging.SimpleLogging) (*FileLogStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating job log dir %q: %w", root, err)
	}
	s := &FileLogStore{
		root:    root,
		logger:  logger,
		jobs:    map[string]*pendingLog{},
		deleted: map[string]time.Time{},
		kick:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go s.run(flushInterval)
	return s, nil
}

func (s *FileLogStore) Write(pull Pull, jobID, line string) {
	if !ValidJobID(jobID) {
		return
	}
	s.mu.Lock()
	if _, gone := s.deleted[jobID]; gone {
		// Still running when its pull request was deleted.
		s.mu.Unlock()
		return
	}
	p, ok := s.jobs[jobID]
	if !ok {
		p = &pendingLog{pull: pull}
		s.jobs[jobID] = p
	}
	if p.buf.Len()+len(line)+1 > maxPendingBytes {
		if p.dropped == 0 {
			s.logger.Warn("job log store: buffer for job %s is full; dropping output until its log file accepts writes again", jobID)
		}
		p.dropped++
		s.mu.Unlock()
		return
	}
	p.buf.WriteString(line)
	p.buf.WriteByte('\n')
	full := p.buf.Len() >= flushBytes
	s.mu.Unlock()

	if full {
		s.signal()
	}
}

func (s *FileLogStore) Complete(jobID string) {
	s.mu.Lock()
	p, ok := s.jobs[jobID]
	if ok {
		p.complete = true
	}
	closed := s.closed
	s.mu.Unlock()
	if !ok {
		return
	}
	// After Close the background flusher is gone, so flush inline instead.
	if closed {
		s.flushMu.Lock()
		s.flushJob(jobID)
		s.flushMu.Unlock()
		return
	}
	s.signal()
}

func (s *FileLogStore) Exists(jobID string) (bool, error) {
	path, err := s.find(jobID)
	return path != "", err
}

func (s *FileLogStore) Replay(jobID string, fn func(line string) bool) error {
	path, err := s.find(jobID)
	if err != nil || path == "" {
		return err
	}
	// #nosec G304 -- path was found by listing root for a validated job ID.
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // deleted with its pull request in the meantime
		}
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" && !fn(strings.TrimSuffix(line, "\n")) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *FileLogStore) DeletePull(pull Pull) error {
	dir, err := s.pullDir(pull)
	if err != nil {
		return err
	}

	// Holding flushMu keeps an in-flight flush from recreating dir after it
	// is removed.
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	var dropped []*pendingLog
	var deletedIDs []string
	for jobID, p := range s.jobs {
		if p.pull == pull {
			delete(s.jobs, jobID)
			dropped = append(dropped, p)
			deletedIDs = append(deletedIDs, jobID)
		}
	}
	s.mu.Unlock()
	for _, p := range dropped {
		if p.f != nil {
			_ = p.f.Close()
		}
	}

	deletedIDs = append(deletedIDs, s.indexDeleteUnder(dir)...)
	s.markDeleted(deletedIDs)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("deleting job logs for %s#%d: %w", pull.RepoFullName, pull.Num, err)
	}
	// Drop the repo directory too once its last pull request is gone.
	_ = os.Remove(filepath.Dir(dir))
	return nil
}

// Close flushes everything buffered, closes all log files and stops the
// background flusher. It reports an error if some output could not be
// written.
func (s *FileLogStore) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stop)
		<-s.done
		s.flushAll()

		s.flushMu.Lock()
		s.mu.Lock()
		for _, p := range s.jobs {
			if p.f != nil {
				s.closeFile(p)
			}
		}
		s.mu.Unlock()
		s.flushMu.Unlock()
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	unflushed := 0
	for _, p := range s.jobs {
		if p.buf.Len() > 0 || p.dropped > 0 {
			unflushed++
		}
	}
	if unflushed > 0 {
		return fmt.Errorf("job log store: output for %d job(s) could not be written", unflushed)
	}
	return nil
}

func (s *FileLogStore) signal() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *FileLogStore) run(flushInterval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		case <-s.kick:
		}
		s.flushAll()
	}
}

func (s *FileLogStore) flushAll() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	jobIDs := make([]string, 0, len(s.jobs))
	for jobID, p := range s.jobs {
		if p.buf.Len() > 0 || p.dropped > 0 || p.complete {
			jobIDs = append(jobIDs, jobID)
		}
	}
	s.mu.Unlock()

	for _, jobID := range jobIDs {
		s.flushJob(jobID)
	}
}

// flushJob appends jobID's buffered output to its file, and syncs and closes
// the file once the job is complete and fully written. The caller must hold
// flushMu. Only the bytes actually written are removed from the buffer, so a
// failed or short write is retried, together with anything buffered since,
// on the next flush.
func (s *FileLogStore) flushJob(jobID string) {
	s.mu.Lock()
	p, ok := s.jobs[jobID]
	if !ok {
		s.mu.Unlock()
		return
	}
	data := bytes.Clone(p.buf.Bytes())
	bufLen := len(data)
	dropped := p.dropped
	complete := p.complete
	s.mu.Unlock()

	if dropped > 0 {
		// The dropped lines came after everything buffered, so the marker
		// goes last. It is only consumed once it is written.
		data = fmt.Appendf(data, "[atlantis: %d line(s) of output dropped while the job log could not be written]\n", dropped)
	}

	var err error
	written := 0
	if len(data) > 0 {
		written, err = s.append(jobID, p, data)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p.buf.Next(min(written, bufLen))
	if written == len(data) {
		p.dropped -= dropped
	}

	if err != nil {
		p.failures++
		s.logger.Warn("job log store: writing output for job %s (attempt %d): %s", jobID, p.failures, err)
		if complete && p.failures >= maxCompleteFlushAttempts {
			s.logger.Err("job log store: giving up on %d byte(s) of output for job %s after %d attempts", p.buf.Len(), jobID, p.failures)
			s.closeFile(p)
			delete(s.jobs, jobID)
		}
		return
	}
	p.failures = 0

	// Write may have added more output while the lock was released; the job
	// is only done once nothing is left.
	if p.complete && p.buf.Len() == 0 && p.dropped == 0 {
		var path string
		if p.f != nil {
			path = p.f.Name()
		}
		s.closeFile(p)
		delete(s.jobs, jobID)
		if path != "" && s.onComplete != nil {
			s.onComplete(p.pull, jobID, path)
		}
	}
}

// append writes data to jobID's log file, creating it on first use. The
// caller must hold flushMu.
func (s *FileLogStore) append(jobID string, p *pendingLog, data []byte) (int, error) {
	if p.f == nil {
		f, err := s.create(p.pull, jobID)
		if err != nil {
			return 0, err
		}
		p.f = f
	}
	return p.f.Write(data)
}

// closeFile syncs and closes p's file. The caller must hold flushMu.
func (s *FileLogStore) closeFile(p *pendingLog) {
	if p.f == nil {
		return
	}
	if err := errors.Join(p.f.Sync(), p.f.Close()); err != nil {
		s.logger.Warn("job log store: closing log file %s: %s", p.f.Name(), err)
	}
	p.f = nil
}

func (s *FileLogStore) create(pull Pull, jobID string) (*os.File, error) {
	dir, err := s.pullDir(pull)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, jobID+".log")
	// #nosec G304 -- dir is built from an escaped repo name and a pull number, and jobID is validated by ValidJobID.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s.indexPut(jobID, path)
	return f, nil
}

// pullDir is where pull's job logs live. The repo full name is path-escaped
// into a single segment, so "owner/repo" and GitLab subgroups alike map to one
// directory and can never climb out of root.
func (s *FileLogStore) pullDir(pull Pull) (string, error) {
	repo := url.PathEscape(pull.RepoFullName)
	if repo == "" || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid repo name %q", pull.RepoFullName)
	}
	return filepath.Join(s.root, repo, strconv.Itoa(pull.Num)), nil
}

// find returns the path of jobID's log, or "" if there is none. Job IDs are
// unique across pull requests, so the first match is the only one.
func (s *FileLogStore) find(jobID string) (string, error) {
	if !ValidJobID(jobID) {
		return "", nil
	}
	if err := s.loadIndex(); err != nil {
		return "", err
	}
	s.indexMu.RLock()
	path := s.index[jobID]
	s.indexMu.RUnlock()
	return path, nil
}

// loadIndex scans root once for existing log files (from before a restart).
func (s *FileLogStore) loadIndex() error {
	var err error
	s.indexOnce.Do(func() {
		index := map[string]string{}
		err = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil // removed by a concurrent DeletePull
				}
				return walkErr
			}
			if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".log") {
				if id := strings.TrimSuffix(d.Name(), ".log"); ValidJobID(id) {
					index[id] = path
				}
			}
			return nil
		})
		s.indexMu.Lock()
		for id, path := range s.index { // entries created while scanning
			index[id] = path
		}
		s.index = index
		s.indexMu.Unlock()
	})
	return err
}

func (s *FileLogStore) indexPut(jobID, path string) {
	s.indexMu.Lock()
	if s.index == nil {
		s.index = map[string]string{}
	}
	s.index[jobID] = path
	s.indexMu.Unlock()
}

// indexDeleteUnder drops every entry whose file is under dir and returns
// their job IDs.
func (s *FileLogStore) indexDeleteUnder(dir string) []string {
	prefix := dir + string(filepath.Separator)
	var ids []string
	s.indexMu.Lock()
	for id, path := range s.index {
		if strings.HasPrefix(path, prefix) {
			delete(s.index, id)
			ids = append(ids, id)
		}
	}
	s.indexMu.Unlock()
	return ids
}

// markDeleted records jobs of a deleted pull request and forgets those
// recorded longer than deletedJobTTL ago.
func (s *FileLogStore) markDeleted(ids []string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, at := range s.deleted {
		if now.Sub(at) > deletedJobTTL {
			delete(s.deleted, id)
		}
	}
	for _, id := range ids {
		s.deleted[id] = now
	}
}

// jobDeleted reports whether jobID belongs to a pull request deleted by
// DeletePull.
func (s *FileLogStore) jobDeleted(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, gone := s.deleted[jobID]
	return gone
}
