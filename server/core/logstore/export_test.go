// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package logstore

import "time"

// Flush runs one flush pass synchronously, so tests need not wait for the
// flush interval.
func (s *FileLogStore) Flush() { s.flushAll() }

// SetBackoff sets the delay before the first upload retry.
func (a *ArchiveLogStore) SetBackoff(d time.Duration) { a.backoff = d }
