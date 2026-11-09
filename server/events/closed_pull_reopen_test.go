// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"errors"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// fakeClosedMarks is a ClosedPullRecorder holding at most one mark.
type fakeClosedMarks struct {
	marked   bool
	readErr  error
	clearErr error
	cleared  []time.Time
}

func (f *fakeClosedMarks) MarkPullClosed(models.PullRequest) error { f.marked = true; return nil }

func (f *fakeClosedMarks) PullMarkedClosed(models.PullRequest) (bool, error) {
	return f.marked, f.readErr
}

func (f *fakeClosedMarks) ClearPullClosed(_ models.PullRequest, openAt time.Time) error {
	f.cleared = append(f.cleared, openAt)
	if f.clearErr != nil {
		return f.clearErr
	}
	f.marked = false
	return nil
}

// fakeLivePull returns the pull with a fixed live state.
type fakeLivePull struct {
	state models.PullRequestState
	err   error
	calls int
}

func (f *fakeLivePull) GetLivePullIdentity(ctx command.ProjectContext) (models.PullRequest, error) {
	f.calls++
	pull := ctx.Pull
	pull.State = f.state
	return pull, f.err
}

func TestCancelClosedCleanup(t *testing.T) {
	pull := models.PullRequest{Num: 3, BaseRepo: models.Repo{FullName: "org/repo"}, State: models.OpenPullState}
	logger := logging.NewNoopLogger(t)

	t.Run("no mark: no live fetch", func(t *testing.T) {
		marks, live := &fakeClosedMarks{}, &fakeLivePull{state: models.ClosedPullState}
		Ok(t, events.CancelClosedCleanup(logger, marks, live, pull))
		Equals(t, 0, live.calls)
	})
	t.Run("reopened: mark cleared", func(t *testing.T) {
		marks, live := &fakeClosedMarks{marked: true}, &fakeLivePull{state: models.OpenPullState}
		before := time.Now()
		Ok(t, events.CancelClosedCleanup(logger, marks, live, pull))
		Assert(t, !marks.marked, "expected the mark cleared")
		Equals(t, 1, len(marks.cleared))
		Assert(t, !marks.cleared[0].Before(before), "expected openAt taken before the live fetch, not earlier")
	})
	t.Run("still closed: refused, mark kept", func(t *testing.T) {
		// The payload says open, but it predates the close.
		marks, live := &fakeClosedMarks{marked: true}, &fakeLivePull{state: models.ClosedPullState}
		err := events.CancelClosedCleanup(logger, marks, live, pull)
		Assert(t, errors.Is(err, events.ErrPullClosed), "expected ErrPullClosed, got %v", err)
		Assert(t, marks.marked, "expected the mark kept")
	})
	t.Run("live state unknown: refused, mark kept", func(t *testing.T) {
		marks, live := &fakeClosedMarks{marked: true}, &fakeLivePull{err: errors.New("vcs down")}
		ErrContains(t, "vcs down", events.CancelClosedCleanup(logger, marks, live, pull))
		Assert(t, marks.marked, "expected the mark kept")
		marks = &fakeClosedMarks{marked: true}
		err := events.CancelClosedCleanup(logger, marks, nil, pull)
		Assert(t, errors.Is(err, events.ErrPullClosed), "expected ErrPullClosed without a fetcher, got %v", err)
	})
	t.Run("errors refuse", func(t *testing.T) {
		ErrContains(t, "apiserver down", events.CancelClosedCleanup(logger, &fakeClosedMarks{readErr: errors.New("apiserver down")}, &fakeLivePull{}, pull))
		ErrContains(t, "conflict", events.CancelClosedCleanup(logger, &fakeClosedMarks{marked: true, clearErr: errors.New("conflict")}, &fakeLivePull{state: models.OpenPullState}, pull))
	})
	t.Run("no recorder", func(t *testing.T) {
		Ok(t, events.CancelClosedCleanup(logger, nil, nil, pull))
	})
}
