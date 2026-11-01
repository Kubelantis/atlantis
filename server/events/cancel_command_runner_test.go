// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"testing"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	vcsmocks "github.com/runatlantis/atlantis/server/events/vcs/mocks"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// Cancel stops queued work but must not release the locks of commands that
// are still running: a new plan or apply of the pull could otherwise start
// beside them, or beside an `atlantis unlock` that is recording discards.
func TestCancelKeepsLocksOfRunningCommands(t *testing.T) {
	RegisterMockTestingT(t)
	vcsClient := vcsmocks.NewMockClient()
	locker := events.NewDefaultWorkingDirLocker()
	tracker := events.NewCancellationTracker()
	pull := models.PullRequest{Num: 1, BaseRepo: models.Repo{FullName: "owner/repo"}}

	// A running command of the pull holds its locks.
	releasePull, err := locker.TryLockPull("owner/repo", 1, command.Unlock, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer releasePull()
	releaseWorkspace, err := locker.TryLock("owner/repo", 1, "default", ".", "", command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer releaseWorkspace()

	runner := events.NewCancelCommandRunner(vcsClient, &events.DefaultProjectCommandRunner{CancellationTracker: tracker, WorkingDirLocker: locker}, nil, false)
	runner.Run(&command.Context{Log: logging.NewNoopLogger(t), Pull: pull}, &events.CommentCommand{Name: command.Cancel})

	Assert(t, tracker.IsCancelled(pull), "queued work was not cancelled")
	_, err = locker.TryLockPull("owner/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "a new command could lock the pull while another was running")
	_, err = locker.TryLock("owner/repo", 1, "default", ".", "", command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "a new command could lock the workspace while another was running")
	vcsClient.VerifyWasCalledOnce().CreateComment(Any[logging.SimpleLogging](), Eq(pull.BaseRepo), Eq(1), Any[string](), Eq(""))
}
