// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package events

import (
	"fmt"
	"strings"
	"sync"

	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
)

//go:generate go tool pegomock generate --package mocks -o mocks/mock_working_dir_locker.go WorkingDirLocker

// WorkingDirLocker is used to prevent multiple commands from executing
// at the same time for a single repo, pull, and workspace. We need to prevent
// this from happening because a specific repo/pull/workspace has a single workspace
// on disk and we haven't written Atlantis (yet) to handle concurrent execution
// within this workspace.
type WorkingDirLockMetadata struct {
	HeadCommit string
	CommitURL  string
	JobURL     string
}

type workingDirLockError struct {
	message      string
	metadata     WorkingDirLockMetadata
	multipleJobs bool
}

func (e *workingDirLockError) Error() string {
	return e.message
}

type workingDirLock struct {
	Command command.Name
	WorkingDirLockMetadata
}

type WorkingDirLocker interface {
	// TryLockPull tries to acquire a pull-level lock for a command. It is used to
	// coordinate commands that can make pull-wide plan state inconsistent.
	// Locks are keyed by the repo ID, which includes the VCS host, so pulls of
	// same-named repos on two providers do not share them.
	TryLockPull(repo models.Repo, pullNum int, cmdName command.Name, metadata WorkingDirLockMetadata) (func(), error)
	// TryLock tries to acquire a lock for this repo, pull, workspace, and path.
	// It returns a function that should be used to unlock the workspace and
	// an error if the workspace is already locked. The error is expected to
	// be printed to the pull request.
	TryLock(repo models.Repo, pullNum int, workspace string, path string, projectName string, cmdName command.Name, metadata WorkingDirLockMetadata) (func(), error)
	// HasCommandLock reports whether this pull request has an active lock for cmdName.
	HasCommandLock(repo models.Repo, pullNum int, cmdName command.Name) bool
	// UnlockByPull unlocks all workspaces for a specific pull request
	UnlockByPull(repo models.Repo, pullNum int)
}

// DefaultWorkingDirLocker implements WorkingDirLocker.
type DefaultWorkingDirLocker struct {
	// mutex prevents against multiple threads calling functions on this struct
	// concurrently. It's only used for entry/exit to each function.
	mutex sync.Mutex
	// locks is a map of workspaces and their owning command metadata.
	locks map[string]workingDirLock
}

// NewDefaultWorkingDirLocker is a constructor.
func NewDefaultWorkingDirLocker() *DefaultWorkingDirLocker {
	return &DefaultWorkingDirLocker{locks: make(map[string]workingDirLock)}
}

func (d *DefaultWorkingDirLocker) TryLock(repo models.Repo, pullNum int, workspace string, path string, projectName string, cmdName command.Name, metadata WorkingDirLockMetadata) (func(), error) {
	repoKey := repo.ID()
	d.mutex.Lock()
	defer d.mutex.Unlock()

	// Unlock releases the pull's project locks, so no command of the pull
	// starts while it runs; see TryLockPull.
	if currentLock, exists := d.locks[d.pullKey(repoKey, pullNum)]; exists && currentLock.Command == command.Unlock {
		return func() {}, d.pullLockError(repoKey, pullNum, cmdName, currentLock)
	}
	workspaceKey := d.workspaceKey(repoKey, pullNum, workspace, path, projectName)
	if currentLock, exists := d.locks[workspaceKey]; exists {
		return func() {}, &workingDirLockError{
			message: fmt.Sprintf("cannot run %q: the %s workspace at path %s is currently locked for this pull request by %q%s.\n"+
				"Wait until the previous command is complete and try again", cmdName, workspace, path, currentLock.Command, formatCommitSuffix(currentLock)),
			metadata: currentLock.WorkingDirLockMetadata,
		}
	}
	d.locks[workspaceKey] = workingDirLock{Command: cmdName, WorkingDirLockMetadata: metadata}
	return func() {
		d.unlock(repoKey, pullNum, workspace, path, projectName)
	}, nil
}

func (d *DefaultWorkingDirLocker) TryLockPull(repo models.Repo, pullNum int, cmdName command.Name, metadata WorkingDirLockMetadata) (func(), error) {
	repoKey := repo.ID()
	d.mutex.Lock()
	defer d.mutex.Unlock()

	pullKey := d.pullKey(repoKey, pullNum)
	if currentLock, exists := d.locks[pullKey]; exists {
		return func() {}, d.pullLockError(repoKey, pullNum, cmdName, currentLock)
	}
	var currentLock workingDirLock
	var exists bool
	if cmdName == command.Unlock {
		// Unlock releases the pull's project locks, so it waits for every
		// command of the pull: import and state rm hold only workspace locks,
		// and releasing their project lock would let another pull lock the
		// project while they still change its state.
		currentLock, exists = d.findAnyLock(repoKey, pullNum)
	} else {
		currentLock, exists = d.findCommandLock(repoKey, pullNum, command.Plan, metadata.HeadCommit)
		if !exists && metadata.HeadCommit != "" {
			currentLock, exists = d.findCommandLock(repoKey, pullNum, command.Plan, "")
		}
	}
	if exists {
		return func() {}, d.pullLockError(repoKey, pullNum, cmdName, currentLock)
	}
	d.locks[pullKey] = workingDirLock{Command: cmdName, WorkingDirLockMetadata: metadata}
	return func() {
		d.unlockPull(repoKey, pullNum)
	}, nil
}

func (d *DefaultWorkingDirLocker) HasCommandLock(repo models.Repo, pullNum int, cmdName command.Name) bool {
	repoKey := repo.ID()
	d.mutex.Lock()
	defer d.mutex.Unlock()

	_, exists := d.findCommandLock(repoKey, pullNum, cmdName, "")
	return exists
}

func (d *DefaultWorkingDirLocker) findCommandLock(repoKey string, pullNum int, cmdName command.Name, headCommit string) (workingDirLock, bool) {
	prefix := d.pullLockPrefix(repoKey, pullNum)
	var match workingDirLock
	var matchKey string
	for key, currentLock := range d.locks {
		if strings.HasPrefix(key, prefix) && currentLock.Command == cmdName && (headCommit == "" || currentLock.HeadCommit == headCommit) && (matchKey == "" || key < matchKey) {
			match = currentLock
			matchKey = key
		}
	}
	return match, matchKey != ""
}

// findAnyLock returns the pull's workspace lock with the lowest key, if any.
func (d *DefaultWorkingDirLocker) findAnyLock(repoKey string, pullNum int) (workingDirLock, bool) {
	prefix := d.pullLockPrefix(repoKey, pullNum)
	pullKey := d.pullKey(repoKey, pullNum)
	var match workingDirLock
	var matchKey string
	for key, currentLock := range d.locks {
		if key != pullKey && strings.HasPrefix(key, prefix) && (matchKey == "" || key < matchKey) {
			match = currentLock
			matchKey = key
		}
	}
	return match, matchKey != ""
}

func (d *DefaultWorkingDirLocker) pullLockError(repoKey string, pullNum int, cmdName command.Name, currentLock workingDirLock) error {
	metadata := currentLock.WorkingDirLockMetadata
	metadata.JobURL = ""
	jobURLs := make(map[string]struct{})
	prefix := d.pullLockPrefix(repoKey, pullNum)
	pullKey := d.pullKey(repoKey, pullNum)
	for key, lock := range d.locks {
		if key != pullKey && strings.HasPrefix(key, prefix) && lock.Command == currentLock.Command && lock.HeadCommit == currentLock.HeadCommit && lock.JobURL != "" {
			jobURLs[lock.JobURL] = struct{}{}
		}
	}

	multipleJobs := len(jobURLs) > 1
	if len(jobURLs) == 1 {
		for jobURL := range jobURLs {
			metadata.JobURL = jobURL
		}
	}
	message := fmt.Sprintf("cannot run %q: pull request %d is currently locked by %q%s.\n"+
		"Wait until the previous command is complete and try again", cmdName, pullNum, currentLock.Command, formatCommitSuffix(currentLock))
	return &workingDirLockError{message: message, metadata: metadata, multipleJobs: multipleJobs}
}

func formatCommitSuffix(lock workingDirLock) string {
	if lock.HeadCommit == "" {
		return ""
	}
	return " for commit " + shortSHA(lock.HeadCommit)
}

func WorkingDirLockMetadataForPull(pull models.PullRequest) WorkingDirLockMetadata {
	metadata := WorkingDirLockMetadata{HeadCommit: pull.HeadCommit}
	if pull.HeadCommit == "" || pull.URL == "" {
		return metadata
	}

	pullURL := strings.TrimRight(pull.URL, "/")
	switch pull.BaseRepo.VCSHost.Type {
	case models.Github, models.Gitea:
		metadata.CommitURL = pullURL + "/commits/" + pull.HeadCommit
	case models.Gitlab:
		metadata.CommitURL = pullURL + "/diffs?commit_id=" + pull.HeadCommit
	}
	return metadata
}

func WorkingDirLockMetadataForProject(ctx command.ProjectContext, jobURL string) WorkingDirLockMetadata {
	metadata := WorkingDirLockMetadataForPull(ctx.Pull)
	metadata.JobURL = jobURL
	return metadata
}

// UnlockByPull unlocks all workspaces for a specific pull request
func (d *DefaultWorkingDirLocker) UnlockByPull(repo models.Repo, pullNum int) {
	repoKey := repo.ID()
	d.mutex.Lock()
	defer d.mutex.Unlock()

	// Find and remove all locks for this pull request
	prefix := d.pullLockPrefix(repoKey, pullNum)
	for key := range d.locks {
		if strings.HasPrefix(key, prefix) {
			delete(d.locks, key)
		}
	}
}

// Unlock unlocks the workspace for this pull.
func (d *DefaultWorkingDirLocker) unlock(repoKey string, pullNum int, workspace string, path string, projectName string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	workspaceKey := d.workspaceKey(repoKey, pullNum, workspace, path, projectName)
	delete(d.locks, workspaceKey)
}

func (d *DefaultWorkingDirLocker) unlockPull(repoKey string, pullNum int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	delete(d.locks, d.pullKey(repoKey, pullNum))
}

func (d *DefaultWorkingDirLocker) workspaceKey(repo string, pull int, workspace string, path string, projectName string) string {
	return strings.TrimRight(fmt.Sprintf("%s%s/%s/%s", d.pullLockPrefix(repo, pull), workspace, path, projectName), "/")
}

func (d *DefaultWorkingDirLocker) pullKey(repo string, pull int) string {
	return d.pullLockPrefix(repo, pull) + ".pull"
}

func (d *DefaultWorkingDirLocker) pullLockPrefix(repo string, pull int) string {
	return fmt.Sprintf("%d:%s/%d/", len(repo), repo, pull)
}
