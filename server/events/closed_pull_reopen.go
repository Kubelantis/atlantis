// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"errors"
	"fmt"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

// ErrPullClosed is returned instead of running a command on a pull request
// that is closed and whose cleanup is pending: the cleanup would delete what
// the command makes.
var ErrPullClosed = errors.New("the pull request is closed and its cleanup is pending")

// CancelClosedCleanup lets a command run on pull. If pull is marked closed
// (see ClosedPullRecorder), it confirms with the VCS host that the pull was
// reopened and then clears the mark, so the pending cleanup doesn't delete
// the plans and locks the command makes. The pull's state in a webhook
// payload may predate the close, so only the live state counts. It returns
// ErrPullClosed if the pull is still closed, keeping the mark, and an error
// if it can't tell; in both cases the command must not run.
func CancelClosedCleanup(logger logging.SimpleLogging, closed ClosedPullRecorder, live LivePullHeadFetcher, pull models.PullRequest) error {
	if closed == nil || pull.Num <= 0 {
		return nil
	}
	marked, err := closed.PullMarkedClosed(pull)
	if err != nil {
		return fmt.Errorf("checking for a pending cleanup of the pull request: %w", err)
	}
	if !marked {
		return nil
	}
	if live == nil {
		return fmt.Errorf("%w: its current state can't be fetched", ErrPullClosed)
	}
	// A close after this is marked later and keeps its mark.
	openAt := time.Now()
	livePull, err := live.GetLivePullIdentity(command.ProjectContext{Log: logger, Pull: pull})
	if err != nil {
		return fmt.Errorf("fetching the state of the pull request, which is marked closed: %w", err)
	}
	if livePull.State != models.OpenPullState {
		return ErrPullClosed
	}
	if err := closed.ClearPullClosed(pull, openAt); err != nil {
		return fmt.Errorf("cancelling the cleanup of the reopened pull request: %w", err)
	}
	logger.Info("pull request %d was reopened; cancelled the cleanup pending since it was closed", pull.Num)
	return nil
}
