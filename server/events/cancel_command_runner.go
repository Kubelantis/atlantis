// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/vcs"
)

const cancelComment = "Cancelled all queued operations for this pull request.\n" +
	"Operations that are already running will continue to completion; new operations can start once they finish."

func NewCancelCommandRunner(
	vcsClient vcs.Client,
	projectCmdRunner ProjectCommandRunner,
	pullUpdater *PullUpdater,
	silenceNoProjects bool,
) *CancelCommandRunner {
	return &CancelCommandRunner{
		VCSClient:         vcsClient,
		ProjectCmdRunner:  projectCmdRunner,
		PullUpdater:       pullUpdater,
		SilenceNoProjects: silenceNoProjects,
	}
}

type CancelCommandRunner struct {
	VCSClient         vcs.Client
	ProjectCmdRunner  ProjectCommandRunner
	PullUpdater       *PullUpdater
	SilenceNoProjects bool
}

func (c *CancelCommandRunner) Run(ctx *command.Context, cmd *CommentCommand) {
	if c.ProjectCmdRunner == nil {
		ctx.Log.Err("ProjectCmdRunner is nil")
		return
	}

	// Get the DefaultProjectCommandRunner to access the process tracker
	defaultRunner, ok := c.ProjectCmdRunner.(*DefaultProjectCommandRunner)
	if !ok {
		ctx.Log.Err("ProjectCmdRunner is not a DefaultProjectCommandRunner")
		return
	}

	if defaultRunner.CancellationTracker == nil {
		ctx.Log.Err("CancellationTracker is nil")
		return
	}

	// Cancel the entire pull request to prevent future execution order groups from running
	defaultRunner.CancellationTracker.Cancel(ctx.Pull)

	// Working directory locks are left alone. They belong to commands that
	// are still running, which release them when they end; releasing them
	// here would let a new plan, apply or unlock of the pull start beside a
	// running one.

	ctx.Log.Info("Cancelled all queued operations and future execution groups for pull request; currently running operations will continue to completion")
	if err := c.VCSClient.CreateComment(ctx.Log, ctx.Pull.BaseRepo, ctx.Pull.Num, cancelComment, ""); err != nil {
		ctx.Log.Err("unable to comment: %s", err)
	}
}
