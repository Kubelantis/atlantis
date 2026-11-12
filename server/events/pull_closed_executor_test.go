// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package events_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/core/kube/kubedb/kubedbtest"
	"github.com/runatlantis/atlantis/server/core/logstore"
	"github.com/runatlantis/atlantis/server/jobs"
	"github.com/runatlantis/atlantis/server/logging"
	"github.com/stretchr/testify/assert"

	. "github.com/petergtz/pegomock/v4"
	lockmocks "github.com/runatlantis/atlantis/server/core/locking/mocks"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/mocks"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/models/testdata"
	vcsmocks "github.com/runatlantis/atlantis/server/events/vcs/mocks"
	loggermocks "github.com/runatlantis/atlantis/server/logging/mocks"
	. "github.com/runatlantis/atlantis/testing"
	"go.uber.org/mock/gomock"
)

func TestCleanUpPullWorkspaceErr(t *testing.T) {
	t.Log("when workspace.Delete returns an error, we return it")
	RegisterMockTestingT(t)
	logger := logging.NewNoopLogger(t)
	w := mocks.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})
	pce := events.PullClosedExecutor{
		WorkingDir:         w,
		PullClosedTemplate: &events.PullClosedEventTemplate{},
		Database:           db,
	}
	err := errors.New("err")
	When(w.Delete(logger, testdata.GithubRepo, testdata.Pull)).ThenReturn(err)
	actualErr := pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
	Equals(t, "cleaning workspace: err", actualErr.Error())
}

func TestCleanUpPullWorkspaceErrStillDeletesExternalPlans(t *testing.T) {
	t.Log("when workspace.Delete fails, external plans are still cleaned up")
	RegisterMockTestingT(t)
	logger := logging.NewNoopLogger(t)
	w := mocks.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})
	store := &countingPlanStore{}
	pce := events.PullClosedExecutor{
		WorkingDir:         w,
		PullClosedTemplate: &events.PullClosedEventTemplate{},
		Database:           db,
		PlanStore:          store,
	}
	When(w.Delete(logger, testdata.GithubRepo, testdata.Pull)).ThenReturn(errors.New("disk full"))
	actualErr := pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
	Equals(t, "cleaning workspace: disk full", actualErr.Error())
	Equals(t, 1, store.deleteForPullCalls)
}

func TestCleanUpPullUnlockErr(t *testing.T) {
	t.Log("when locker.UnlockByPull returns an error, we return it")
	RegisterMockTestingT(t)
	logger := logging.NewNoopLogger(t)
	w := mocks.NewMockWorkingDir()
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})
	pce := events.PullClosedExecutor{
		Locker:             l,
		WorkingDir:         w,
		Database:           db,
		PullClosedTemplate: &events.PullClosedEventTemplate{},
	}
	err := errors.New("err")
	l.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).Return(nil, err)
	actualErr := pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
	Equals(t, "cleaning up locks: err", actualErr.Error())
}

func TestCleanUpPullNoLocks(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	t.Log("when there are no locks to clean up, we don't comment")
	RegisterMockTestingT(t)
	w := mocks.NewMockWorkingDir()
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	cp := vcsmocks.NewMockClient()
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})
	pce := events.PullClosedExecutor{
		Locker:     l,
		VCSClient:  cp,
		WorkingDir: w,
		Database:   db,
	}
	l.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).Return(nil, nil)
	err := pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
	Ok(t, err)
	cp.VerifyWasCalled(Never()).CreateComment(Any[logging.SimpleLogging](), Any[models.Repo](), Any[int](), Any[string](), Any[string]())
}

func TestCleanUpPullComments(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	t.Log("should comment correctly")
	RegisterMockTestingT(t)
	cases := []struct {
		Description string
		Locks       []models.ProjectLock
		Exp         string
	}{
		{
			"single lock, empty path",
			[]models.ProjectLock{
				{
					Project:   models.NewProject("owner/repo", "", ""),
					Workspace: "default",
				},
			},
			"- dir: `.` workspace: `default`",
		},
		{
			"single lock, named project",
			[]models.ProjectLock{
				{
					Project:   models.NewProject("owner/repo", "", "projectname"),
					Workspace: "default",
				},
			},
			// TODO: Should project name be included in output?
			"- dir: `.` workspace: `default`",
		},
		{
			"single lock, non-empty path",
			[]models.ProjectLock{
				{
					Project:   models.NewProject("owner/repo", "path", ""),
					Workspace: "default",
				},
			},
			"- dir: `path` workspace: `default`",
		},
		{
			"single path, multiple workspaces",
			[]models.ProjectLock{
				{
					Project:   models.NewProject("owner/repo", "path", ""),
					Workspace: "workspace1",
				},
				{
					Project:   models.NewProject("owner/repo", "path", ""),
					Workspace: "workspace2",
				},
			},
			"- dir: `path` workspaces: `workspace1`, `workspace2`",
		},
		{
			"multiple paths, multiple workspaces",
			[]models.ProjectLock{
				{
					Project:   models.NewProject("owner/repo", "path", ""),
					Workspace: "workspace1",
				},
				{
					Project:   models.NewProject("owner/repo", "path", ""),
					Workspace: "workspace2",
				},
				{
					Project:   models.NewProject("owner/repo", "path2", ""),
					Workspace: "workspace1",
				},
				{
					Project:   models.NewProject("owner/repo", "path2", ""),
					Workspace: "workspace2",
				},
			},
			"- dir: `path` workspaces: `workspace1`, `workspace2`\n- dir: `path2` workspaces: `workspace1`, `workspace2`",
		},
	}
	for _, c := range cases {
		func() {
			w := mocks.NewMockWorkingDir()
			cp := vcsmocks.NewMockClient()
			ctrl := gomock.NewController(t)
			l := lockmocks.NewMockLocker(ctrl)
			db := kubedbtest.New(t)
			t.Cleanup(func() {
				db.Close()
			})
			pce := events.PullClosedExecutor{
				Locker:     l,
				VCSClient:  cp,
				WorkingDir: w,
				Database:   db,
			}
			t.Log("testing: " + c.Description)
			l.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).Return(c.Locks, nil)
			err := pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
			Ok(t, err)
			_, _, _, comment, _ := cp.VerifyWasCalledOnce().CreateComment(
				Any[logging.SimpleLogging](), Any[models.Repo](), Any[int](), Any[string](), Any[string]()).GetCapturedArguments()

			expected := "Locks and plans deleted for the projects and workspaces modified in this pull request:\n\n" + c.Exp
			Equals(t, expected, comment)
		}()
	}
}

func TestCleanUpLogStreaming(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)

	t.Run("Should Clean Up Log Streaming Resources When PR is closed", func(t *testing.T) {

		// Create Log streaming resources
		prjCmdOutput := make(chan *jobs.ProjectCmdOutputLine)
		prjCmdOutHandler := jobs.NewAsyncProjectCommandOutputHandler(prjCmdOutput, logger, logstore.NoopLogStore{})
		ctx := command.ProjectContext{
			BaseRepo:    testdata.GithubRepo,
			Pull:        testdata.Pull,
			ProjectName: *testdata.Project.Name,
			Workspace:   "default",
		}

		go prjCmdOutHandler.Handle()
		prjCmdOutHandler.Send(ctx, "Test Message", false)

		// Create the database and add a pull request.

		database := kubedbtest.New(t)
		result := []command.ProjectResult{
			{
				RepoRelDir:  testdata.GithubRepo.FullName,
				Workspace:   "default",
				ProjectName: *testdata.Project.Name,
			},
		}

		// Create a new record for pull
		_, err := database.UpdatePullWithResults(testdata.Pull, result)
		Ok(t, err)

		workingDir := mocks.NewMockWorkingDir()
		gmockCtrl := gomock.NewController(t)
		locker := lockmocks.NewMockLocker(gmockCtrl)
		client := vcsmocks.NewMockClient()
		logger := loggermocks.NewMockSimpleLogging()

		pullClosedExecutor := events.PullClosedExecutor{
			Locker:                   locker,
			WorkingDir:               workingDir,
			Database:                 database,
			VCSClient:                client,
			PullClosedTemplate:       &events.PullClosedEventTemplate{},
			LogStreamResourceCleaner: prjCmdOutHandler,
		}

		locks := []models.ProjectLock{
			{
				Project:   models.NewProject(testdata.GithubRepo.FullName, "", ""),
				Workspace: "default",
			},
		}
		locker.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).Return(locks, nil)

		// Clean up.
		err = pullClosedExecutor.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
		Ok(t, err)

		close(prjCmdOutput)
		_, _, _, comment, _ := client.VerifyWasCalledOnce().CreateComment(
			Any[logging.SimpleLogging](), Any[models.Repo](), Any[int](), Any[string](), Any[string]()).GetCapturedArguments()
		expectedComment := "Locks and plans deleted for the projects and workspaces modified in this pull request:\n\n" + "- dir: `.` workspace: `default`"
		Equals(t, expectedComment, comment)

		// Assert log streaming resources are cleaned up.
		dfPrjCmdOutputHandler := prjCmdOutHandler.(*jobs.AsyncProjectCommandOutputHandler)
		assert.Empty(t, dfPrjCmdOutputHandler.GetProjectOutputBuffer(ctx.PullInfo()))
		assert.Empty(t, dfPrjCmdOutputHandler.GetReceiverBufferForPull(ctx.PullInfo()))
	})
}

func TestCleanUpPullWithCorrectJobContext(t *testing.T) {
	t.Log("CleanUpPull should call LogStreamResourceCleaner.CleanUp with complete PullInfo including RepoFullName and Path")
	RegisterMockTestingT(t)
	logger := logging.NewNoopLogger(t)

	// Create mocks
	workingDir := mocks.NewMockWorkingDir()
	gmockCtrl := gomock.NewController(t)
	locker := lockmocks.NewMockLocker(gmockCtrl)
	client := vcsmocks.NewMockClient()
	resourceCleaner := mocks.NewMockResourceCleaner()

	// Create temporary database
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})

	// Create test data with multiple projects to verify all fields are populated correctly
	testProjects := []command.ProjectResult{
		{
			RepoRelDir:  "path/to/project1",
			Workspace:   "default",
			ProjectName: "project1",
		},
		{
			RepoRelDir:  "path/to/project2",
			Workspace:   "staging",
			ProjectName: "project2",
		},
	}

	// Add pull status to database
	_, err := db.UpdatePullWithResults(testdata.Pull, testProjects)
	Ok(t, err)

	// Create executor
	pce := events.PullClosedExecutor{
		Locker:                   locker,
		VCSClient:                client,
		WorkingDir:               workingDir,
		Database:                 db,
		PullClosedTemplate:       &events.PullClosedEventTemplate{},
		LogStreamResourceCleaner: resourceCleaner,
	}

	// Setup mock expectations
	locker.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).Return(nil, nil)

	// Execute CleanUpPull
	err = pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull)
	Ok(t, err)

	// Verify ResourceCleaner.CleanUp was called once for each project, then
	// once for the pull request as a whole.
	resourceCleaner.VerifyWasCalled(Times(3)).CleanUp(Any[jobs.PullInfo]())

	// Get the captured arguments to verify they contain all required fields
	capturedArgs := resourceCleaner.VerifyWasCalled(Times(3)).CleanUp(Any[jobs.PullInfo]()).GetAllCapturedArguments()

	// Verify first project's PullInfo
	expectedPullInfo1 := jobs.PullInfo{
		PullNum:      testdata.Pull.Num,
		Repo:         testdata.Pull.BaseRepo.Name,
		RepoFullName: testdata.Pull.BaseRepo.FullName,
		VCSHostname:  testdata.Pull.BaseRepo.VCSHost.Hostname,
		ProjectName:  "project1",
		Path:         "path/to/project1",
		Workspace:    "default",
	}
	Equals(t, expectedPullInfo1, capturedArgs[0])

	// Verify second project's PullInfo
	expectedPullInfo2 := jobs.PullInfo{
		PullNum:      testdata.Pull.Num,
		Repo:         testdata.Pull.BaseRepo.Name,
		RepoFullName: testdata.Pull.BaseRepo.FullName,
		VCSHostname:  testdata.Pull.BaseRepo.VCSHost.Hostname,
		ProjectName:  "project2",
		Path:         "path/to/project2",
		Workspace:    "staging",
	}
	Equals(t, expectedPullInfo2, capturedArgs[1])

	// And the pull-level call, which covers workflow hook jobs.
	Equals(t, jobs.PullInfo{
		PullNum:      testdata.Pull.Num,
		Repo:         testdata.Pull.BaseRepo.Name,
		RepoFullName: testdata.Pull.BaseRepo.FullName,
		VCSHostname:  testdata.Pull.BaseRepo.VCSHost.Hostname,
	}, capturedArgs[2])
}

type countingPlanStore struct {
	deleteForPullCalls int
}

func (s *countingPlanStore) Save(command.ProjectContext, string) error   { return nil }
func (s *countingPlanStore) Load(command.ProjectContext, string) error   { return nil }
func (s *countingPlanStore) Remove(command.ProjectContext, string) error { return nil }
func (s *countingPlanStore) ListWorkspaces(models.Repo, int) ([]string, error) {
	return nil, nil
}
func (s *countingPlanStore) RestorePlans(string, models.Repo, int) error { return nil }
func (s *countingPlanStore) DeleteForPull(models.Repo, int, planstore.Fence) error {
	s.deleteForPullCalls++
	return nil
}
func (s *countingPlanStore) DeletePlanForProject(models.Repo, int, string, string, string, planstore.Fence) error {
	return nil
}

// Closing a pull while one of its commands runs doesn't delete its files or
// release its locks under it. The close is recorded, and the cleanup runs
// when the command finishes.
func TestCleanUpPullWaitsForRunningCommand(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	w := mocks.NewMockWorkingDir()
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })
	locker := events.NewDefaultWorkingDirLocker()
	pce := &events.PullClosedExecutor{
		Locker:           l,
		VCSClient:        vcsmocks.NewMockClient(),
		WorkingDir:       w,
		Database:         db,
		WorkingDirLocker: locker,
		Closed:           db,
	}
	locker.SetIdleHook(pce.PullIdle)

	// An apply of the pull is running.
	unlockPull, err := locker.TryLockPull(testdata.GithubRepo, testdata.Pull.Num, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlockWs, err := locker.TryLock(testdata.GithubRepo, testdata.Pull.Num, "default", ".", "", command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)

	cleaned := make(chan struct{})
	l.EXPECT().UnlockByPull(testdata.GithubRepo, testdata.Pull.Num).DoAndReturn(func(models.Repo, int) ([]models.ProjectLock, error) {
		close(cleaned)
		return nil, nil
	})

	// The close webhook marks the pull before cleaning it up.
	Ok(t, db.MarkPullClosed(testdata.Pull))
	Ok(t, pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull))
	w.VerifyWasCalled(Never()).Delete(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest]())
	closed, err := db.ClosedPulls()
	Ok(t, err)
	Equals(t, 1, len(closed))
	Equals(t, testdata.Pull.Num, closed[0].Num)

	// The apply finishes: the cleanup runs.
	unlockWs()
	unlockPull()
	select {
	case <-cleaned:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not run once the command finished")
	}
	w.VerifyWasCalledEventually(Once(), 5*time.Second).Delete(Any[logging.SimpleLogging](), Eq(testdata.GithubRepo), Eq(testdata.Pull))
}

// A peer doesn't delete clones that a command of the pull is using on it;
// its janitor removes them later.
func TestCleanUpReplicaSkipsClonesInUse(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	w := mocks.NewMockWorkingDir()
	locker := events.NewDefaultWorkingDirLocker()
	unlock, err := locker.TryLock(testdata.GithubRepo, testdata.Pull.Num, "default", ".", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()
	c := &events.ReplicaCleaner{WorkingDir: w, WorkingDirLocker: locker}

	Ok(t, c.CleanUpReplica(logger, testdata.GithubRepo, testdata.Pull))
	w.VerifyWasCalled(Never()).Delete(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest]())
}

// A pull reopened while a command it had when it was closed still runs keeps
// its plans and locks: commands clear the closed mark, and the deferred
// cleanup checks the mark before deleting anything.
func TestCleanUpPullSkipsReopenedPull(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	w := mocks.NewMockWorkingDir()
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl) // no calls expected: nothing is unlocked
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })
	locker := events.NewDefaultWorkingDirLocker()
	pce := &events.PullClosedExecutor{
		Locker:           l,
		VCSClient:        vcsmocks.NewMockClient(),
		WorkingDir:       w,
		Database:         db,
		WorkingDirLocker: locker,
		Closed:           db,
	}
	idle := make(chan struct{})
	var once sync.Once
	locker.SetIdleHook(func(repoKey string, pullNum int) {
		pce.PullIdle(repoKey, pullNum)
		once.Do(func() { close(idle) })
	})

	unlockWs, err := locker.TryLock(testdata.GithubRepo, testdata.Pull.Num, "default", ".", "", command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	Ok(t, db.MarkPullClosed(testdata.Pull))
	Ok(t, pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull))

	// The pull is reopened and a command runs on it.
	Ok(t, db.ClearPullClosed(testdata.Pull, time.Now()))

	unlockWs()
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("idle hook did not run")
	}
	w.VerifyWasCalled(Never()).Delete(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest]())
	// The leader's sweep has nothing left to do either.
	closed, err := db.ClosedPulls()
	Ok(t, err)
	Equals(t, 0, len(closed))
	Ok(t, pce.CleanUpPull(logger, testdata.GithubRepo, testdata.Pull))
	w.VerifyWasCalled(Never()).Delete(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest]())
}
