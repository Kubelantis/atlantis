// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package events_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	. "github.com/runatlantis/atlantis/testing"
)

var repo = models.Repo{FullName: "repo/owner"}
var workspace = "default"
var path = "."
var projectName = "testProjectName"
var cmd = command.Plan

func TestTryLock(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	// The first lock should succeed.
	unlockFn, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	// Now another lock for the same repo, workspace, projectName and pull should fail
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, command.Apply, events.WorkingDirLockMetadata{})
	ErrEquals(t, "cannot run \"apply\": the default workspace at path . is currently locked for this pull request by \"plan\".\n"+
		"Wait until the previous command is complete and try again", err)

	// Unlock should work.
	unlockFn()
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestTryLockIncludesCommitMetadata(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const commitURL = "https://github.com/owner/repo/commit/0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name     string
		metadata events.WorkingDirLockMetadata
		want     string
	}{
		{name: "sha", metadata: events.WorkingDirLockMetadata{HeadCommit: sha}, want: "by \"plan\" for commit 0123456."},
		{name: "url remains structured", metadata: events.WorkingDirLockMetadata{HeadCommit: sha, CommitURL: commitURL}, want: "by \"plan\" for commit 0123456."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locker := events.NewDefaultWorkingDirLocker()
			_, err := locker.TryLock(repo, 1, workspace, path, projectName, command.Plan, tt.metadata)
			Ok(t, err)
			_, err = locker.TryLock(repo, 1, workspace, path, projectName, command.Apply, events.WorkingDirLockMetadata{})
			ErrContains(t, tt.want, err)
		})
	}
}

func TestTryLockPullIncludesCommitMetadata(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const commitURL = "https://github.com/owner/repo/commit/0123456789abcdef0123456789abcdef01234567"
	locker := events.NewDefaultWorkingDirLocker()
	_, err := locker.TryLockPull(repo, 1, command.Plan, events.WorkingDirLockMetadata{HeadCommit: sha, CommitURL: commitURL})
	Ok(t, err)
	_, err = locker.TryLockPull(repo, 1, command.Apply, events.WorkingDirLockMetadata{})
	ErrContains(t, "by \"plan\" for commit 0123456.", err)
	Assert(t, !strings.Contains(err.Error(), commitURL), "expected commit URL to remain out of plain error: %s", err)
}

func TestTryLockSameCommand(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	// The first lock should succeed.
	unlockFn, err := locker.TryLock(repo, 1, workspace, path, projectName, command.Import, events.WorkingDirLockMetadata{})
	Ok(t, err)

	// Now another lock for the same repo, workspace, projectName and pull should fail
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, command.Import, events.WorkingDirLockMetadata{})
	ErrEquals(t, "cannot run \"import\": the default workspace at path . is currently locked for this pull request by \"import\".\n"+
		"Wait until the previous command is complete and try again", err)

	// Unlock should work.
	unlockFn()
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestTryLockDifferentWorkspaces(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("a lock for the same repo and pull but different workspace should succeed")
	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = locker.TryLock(repo, 1, "new-workspace", path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	t.Log("and both should now be locked")
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "exp err")
	_, err = locker.TryLock(repo, 1, "new-workspace", path, projectName, cmd, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "exp err")
}

func TestTryLockDifferentRepo(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("a lock for a different repo but the same workspace and pull should succeed")
	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	newRepo := models.Repo{FullName: "owner/newrepo"}
	_, err = locker.TryLock(newRepo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	t.Log("and both should now be locked")
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(newRepo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
}

func TestTryLockDifferentPulls(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("a lock for a different pull but the same repo, workspace, projectName should succeed")
	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	newPull := 2
	_, err = locker.TryLock(repo, newPull, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	t.Log("and both should now be locked")
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(repo, newPull, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
}

func TestTryLockDifferentPaths(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("a lock for a different path but the same repo, pull, projectName and workspace should succeed")
	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	newPath := "new-path"
	_, err = locker.TryLock(repo, 1, workspace, newPath, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	t.Log("and both should now be locked")
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(repo, 1, workspace, newPath, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
}

func TestTryLockDifferentProjectNames(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("a lock for a different projectName but the same repo, pull, path and workspace should succeed")
	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	newProjectName := "new-project"
	_, err = locker.TryLock(repo, 1, workspace, path, newProjectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)

	t.Log("and both should now be locked")
	_, err = locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(repo, 1, workspace, path, newProjectName, cmd, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
}

func TestUnlock(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	t.Log("unlocking should work")
	unlockFn, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockFn()
	_, err = locker.TryLock(repo, 1, workspace, "", projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestUnlockDifferentWorkspaces(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	t.Log("unlocking should work for different workspaces")
	unlockFn1, err1 := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err1)
	unlockFn2, err2 := locker.TryLock(repo, 1, "new-workspace", path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err2)
	unlockFn1()
	unlockFn2()

	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = locker.TryLock(repo, 1, "new-workspace", path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestUnlockDifferentRepos(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	t.Log("unlocking should work for different repos")
	unlockFn1, err1 := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err1)
	newRepo := models.Repo{FullName: "owner/newrepo"}
	unlockFn2, err2 := locker.TryLock(newRepo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err2)
	unlockFn1()
	unlockFn2()

	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = locker.TryLock(newRepo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestUnlockDifferentPulls(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	t.Log("unlocking should work for different pulls")
	unlockFn1, err1 := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err1)
	newPull := 2
	unlockFn2, err2 := locker.TryLock(repo, newPull, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err2)
	unlockFn1()
	unlockFn2()

	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = locker.TryLock(repo, newPull, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestUnlockDifferentProjectNames(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	t.Log("unlocking should work for different projects")
	unlockFn1, err1 := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err1)
	newProjectName := "new-project"
	unlockFn2, err2 := locker.TryLock(repo, 1, workspace, path, newProjectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err2)
	unlockFn1()
	unlockFn2()

	_, err := locker.TryLock(repo, 1, workspace, path, projectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = locker.TryLock(repo, 1, workspace, path, newProjectName, cmd, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

func TestWorkingDirLocker_HasCommandLockDoesNotCrossMatchSlashNestedRepoNames(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	unlock, err := locker.TryLockPull(models.Repo{FullName: "group/repo/1"}, 2, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()

	Assert(t, locker.HasCommandLock(models.Repo{FullName: "group/repo/1"}, 2, command.Plan), "expected exact nested repo lock")
	Assert(t, !locker.HasCommandLock(models.Repo{FullName: "group/repo"}, 1, command.Plan), "did not expect parent repo/pull prefix to match nested repo lock")
}

func TestWorkingDirLocker_TryLockPullDoesNotCrossMatchSlashNestedRepoNames(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	unlock, err := locker.TryLockPull(models.Repo{FullName: "group/repo/1"}, 2, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()

	unlockParent, err := locker.TryLockPull(models.Repo{FullName: "group/repo"}, 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlockParent()
}

func TestWorkingDirLocker_PullLockExactRepoAndPullIsolation(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	unlock, err := locker.TryLockPull(models.Repo{FullName: "group/repo/12"}, 3, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()

	Assert(t, locker.HasCommandLock(models.Repo{FullName: "group/repo/12"}, 3, command.Plan), "expected exact repo/pull lock")
	Assert(t, !locker.HasCommandLock(models.Repo{FullName: "group/repo"}, 12, command.Plan), "did not expect pull prefix to match nested repo path")
	unlockOther, err := locker.TryLockPull(models.Repo{FullName: "group/repo"}, 12, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlockOther()
}

func TestWorkingDirLocker_ProjectLocksStillUseExactWorkspaceDirProjectIdentity(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()

	unlock, err := locker.TryLock(models.Repo{FullName: "group/repo/1"}, 2, "default", "dir", "proj", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()

	Assert(t, locker.HasCommandLock(models.Repo{FullName: "group/repo/1"}, 2, command.Plan), "expected exact project lock to count as command lock")
	Assert(t, !locker.HasCommandLock(models.Repo{FullName: "group/repo"}, 1, command.Plan), "did not expect nested repo project lock to match parent repo/pull")
	_, err = locker.TryLock(models.Repo{FullName: "group/repo/1"}, 2, "default", "dir", "proj", command.Apply, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(models.Repo{FullName: "group/repo/1"}, 2, "default", "dir", "other", command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
}

// Unlock releases the pull's project locks, so it must not run while any
// command of the pull holds a workspace: import and state rm hold no pull
// lock, and releasing their project lock would let another pull lock the
// project while they still change its state.
func TestWorkingDirLocker_UnlockWaitsForAnyRunningCommand(t *testing.T) {
	for _, name := range []command.Name{command.Import, command.State, command.Plan, command.Apply, command.PolicyCheck} {
		t.Run(name.String(), func(t *testing.T) {
			locker := events.NewDefaultWorkingDirLocker()
			unlockWs, err := locker.TryLock(repo, 1, workspace, path, projectName, name, events.WorkingDirLockMetadata{})
			Ok(t, err)

			_, err = locker.TryLockPull(repo, 1, command.Unlock, events.WorkingDirLockMetadata{})
			ErrContains(t, "currently locked", err)

			unlockWs()
			unlockPull, err := locker.TryLockPull(repo, 1, command.Unlock, events.WorkingDirLockMetadata{})
			Ok(t, err)
			unlockPull()
		})
	}
}

// A command started while unlock runs could have its project lock released
// by that unlock, so it waits for the unlock to finish.
func TestWorkingDirLocker_CommandsWaitForUnlock(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	unlockPull, err := locker.TryLockPull(repo, 1, command.Unlock, events.WorkingDirLockMetadata{})
	Ok(t, err)

	for _, name := range []command.Name{command.Import, command.State, command.Plan, command.Apply} {
		_, err = locker.TryLock(repo, 1, workspace, path, projectName, name, events.WorkingDirLockMetadata{})
		ErrContains(t, "currently locked by \"unlock\"", err)
	}
	// Other pulls are unaffected.
	unlockOther, err := locker.TryLock(repo, 2, workspace, path, projectName, command.Import, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockOther()

	unlockPull()
	unlockWs, err := locker.TryLock(repo, 1, workspace, path, projectName, command.Import, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockWs()
}

// Plan and apply lock their projects' workspaces inside their own pull lock.
func TestWorkingDirLocker_PlanAndApplyStillLockWorkspacesInsideTheirPullLock(t *testing.T) {
	for _, name := range []command.Name{command.Plan, command.Apply} {
		locker := events.NewDefaultWorkingDirLocker()
		unlockPull, err := locker.TryLockPull(repo, 1, name, events.WorkingDirLockMetadata{})
		Ok(t, err)
		unlockWs, err := locker.TryLock(repo, 1, workspace, path, projectName, name, events.WorkingDirLockMetadata{})
		Ok(t, err)
		unlockWs()
		unlockPull()
	}
}

// Two VCS providers can each have a repo with this name. Their pulls with the
// same number share one working directory, repos/<owner/repo>/<pull>, until
// directories carry the VCS host (#18), so they wait for each other.
func TestTryLockSameNamedRepoOnOtherHostSharesTheWorkingDir(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	github := models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}
	gitlab := models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "gitlab.com"}}

	unlockPull, err := locker.TryLockPull(github, 1, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlock, err := locker.TryLock(github, 1, "default", ".", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)

	_, err = locker.TryLockPull(gitlab, 1, command.Plan, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)
	_, err = locker.TryLock(gitlab, 1, "default", ".", "", command.Plan, events.WorkingDirLockMetadata{})
	ErrContains(t, "currently locked", err)

	unlock()
	unlockPull()
	unlockOther, err := locker.TryLock(gitlab, 1, "default", ".", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockOther()
}

// The idle hook fires when the last lock of a pull is released, not before.
func TestWorkingDirLocker_IdleHookFiresWhenPullIsIdle(t *testing.T) {
	locker := events.NewDefaultWorkingDirLocker()
	idle := make(chan string, 4)
	locker.SetIdleHook(func(repoKey string, pullNum int) { idle <- fmt.Sprintf("%s#%d", repoKey, pullNum) })

	unlockPull, err := locker.TryLockPull(repo, 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockWs, err := locker.TryLock(repo, 1, workspace, path, projectName, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)

	unlockWs()
	select {
	case got := <-idle:
		t.Fatalf("idle hook fired while the pull lock was held: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	unlockPull()
	select {
	case got := <-idle:
		Equals(t, repo.FullName+"#1", got)
	case <-time.After(5 * time.Second):
		t.Fatal("idle hook did not fire once the pull was idle")
	}
}
