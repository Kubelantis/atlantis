// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package logstore

import "time"

// Flush runs one flush pass synchronously, so tests need not wait for the
// flush interval.
func (s *FileLogStore) Flush() { s.flushAll() }

// SetBackoff sets the delay before the first upload retry.
func (a *ArchiveLogStore) SetBackoff(d time.Duration) { a.backoff = d }

// SetClock replaces the clock used for deletion markers.
func (s *FileLogStore) SetClock(now func() time.Time) { s.now = now }

// SetAfterDropJobs sets a hook DeletePull calls after it removes the pull's
// pending jobs.
func (s *FileLogStore) SetAfterDropJobs(fn func()) { s.afterDropJobs = fn }

// DeletedMarkers returns how many deleted-job markers are kept.
func (s *FileLogStore) DeletedMarkers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deleted)
}
