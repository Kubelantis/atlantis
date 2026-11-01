// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"errors"
	"testing"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/core/kube/kubedb/kubedbtest"
	lockmocks "github.com/runatlantis/atlantis/server/core/locking/mocks"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
	"go.uber.org/mock/gomock"
)

func TestDeleteLock_LockerErr(t *testing.T) {
	t.Log("If there is an error retrieving the lock, we return the error")
	logger := logging.NewNoopLogger(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	l.EXPECT().GetLock("id").Return(nil, errors.New("err"))
	dlc := events.DefaultDeleteLockCommand{Locker: l}
	_, err := dlc.DeleteLock(logger, "id")
	ErrEquals(t, "err", err)
}

func TestDeleteLock_None(t *testing.T) {
	t.Log("If there is no lock at that ID we return nil")
	logger := logging.NewNoopLogger(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	l.EXPECT().GetLock("id").Return(nil, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l}
	lock, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Assert(t, lock == nil, "lock was not nil")
}

func TestDeleteLock_Success(t *testing.T) {
	t.Log("Delete lock deletes successfully the plan file")
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t) // needed for pegomock WorkingDir mock
	workspace := "workspace"
	path := "path"
	projectName := ""
	pull := models.PullRequest{
		BaseRepo: models.Repo{FullName: "owner/repo"},
	}
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	lock := &models.ProjectLock{
		Pull:      pull,
		Workspace: workspace,
		Project: models.Project{
			Path:         path,
			RepoFullName: pull.BaseRepo.FullName,
		},
	}
	l.EXPECT().GetLock("id").Return(lock, nil)
	l.EXPECT().UnlockIfOwnedByPull(lock.Project, workspace, pull).Return(lock, nil)
	workingDir := events.NewMockWorkingDir()
	workingDirLocker := events.NewDefaultWorkingDirLocker()
	db := kubedbtest.New(t)
	t.Cleanup(func() {
		db.Close()
	})
	dlc := events.DefaultDeleteLockCommand{
		Locker:           l,
		Database:         db,
		WorkingDirLocker: workingDirLocker,
		WorkingDir:       workingDir,
	}
	lock, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Assert(t, lock != nil, "lock was nil")
	workingDir.VerifyWasCalledOnce().DeletePlan(Any[logging.SimpleLogging](), Eq(pull.BaseRepo), Eq(pull), Eq(workspace),
		Eq(path), Eq(projectName))
}

// The discard is recorded where every replica sees it before the lock is
// released, so a plan file left on another replica or in a plan store cannot
// be applied once the lock is free.
func TestDeleteLock_RecordsDiscardBeforeUnlocking(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })

	pull := models.PullRequest{
		BaseRepo:   models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo", VCSHost: models.VCSHost{Hostname: "github.com"}},
		Num:        2,
		HeadCommit: "abc",
	}
	_, err := db.UpdatePullWithResults(pull, []command.ProjectResult{{
		Command:              command.Plan,
		RepoRelDir:           "dir1",
		Workspace:            "default",
		ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "Plan: 1 to add"}},
	}})
	Ok(t, err)
	lock := &models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}

	l.EXPECT().GetLock("id").Return(lock, nil)
	l.EXPECT().UnlockIfOwnedByPull(lock.Project, "default", pull).DoAndReturn(func(models.Project, string, models.PullRequest) (*models.ProjectLock, error) {
		status, err := db.GetPullStatus(pull)
		Ok(t, err)
		Equals(t, models.DiscardedPlanStatus, status.Projects[0].Status)
		return lock, nil
	})
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	got, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Equals(t, lock, got)
	workingDir.VerifyWasCalledOnce().DeletePlan(Any[logging.SimpleLogging](), Eq(pull.BaseRepo), Eq(pull), Eq("default"), Eq("dir1"), Eq(""))
}

// If the discard cannot be recorded, the lock is kept and nothing is deleted:
// releasing it would leave the plan applicable while reporting it discarded.
func TestDeleteLock_KeepsLockWhenDiscardFails(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	db := kubedbtest.New(t)
	db.Close() // every write fails

	pull := models.PullRequest{BaseRepo: models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}, Num: 2}
	l.EXPECT().GetLock("id").Return(&models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	_, err := dlc.DeleteLock(logger, "id")
	ErrContains(t, "the lock was kept", err)
	workingDir.VerifyWasCalled(Never()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](), Any[string](), Any[string](), Any[string]())
}

// A lock with no recorded plan has nothing to discard; it is still released.
func TestDeleteLock_NoPlanStatus(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })

	pull := models.PullRequest{BaseRepo: models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}, Num: 2}
	lock := &models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}
	l.EXPECT().GetLock("id").Return(lock, nil)
	l.EXPECT().UnlockIfOwnedByPull(lock.Project, "default", pull).Return(lock, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	got, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Equals(t, lock, got)
}

func TestDeleteLocksByPull_LockerErr(t *testing.T) {
	t.Log("If there is an error retrieving the lock, returned a failed status")
	logger := logging.NewNoopLogger(t)
	repoName := "reponame"
	pullNum := 2
	RegisterMockTestingT(t) // needed for pegomock WorkingDir mock
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	l.EXPECT().UnlockByPull(models.Repo{FullName: repoName}, pullNum).Return(nil, errors.New("err"))
	dlc := events.DefaultDeleteLockCommand{
		Locker:     l,
		WorkingDir: workingDir,
	}
	_, err := dlc.DeleteLocksByPull(logger, models.Repo{FullName: repoName}, pullNum)
	ErrEquals(t, "err", err)
	workingDir.VerifyWasCalled(Never()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](),
		Any[string](), Any[string](), Any[string]())
}

func TestDeleteLocksByPull_None(t *testing.T) {
	t.Log("If there is no lock at that ID there is no error")
	logger := logging.NewNoopLogger(t)
	repoName := "reponame"
	pullNum := 2
	RegisterMockTestingT(t) // needed for pegomock WorkingDir mock
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	l.EXPECT().UnlockByPull(models.Repo{FullName: repoName}, pullNum).Return([]models.ProjectLock{}, nil)
	dlc := events.DefaultDeleteLockCommand{
		Locker:     l,
		WorkingDir: workingDir,
	}
	_, err := dlc.DeleteLocksByPull(logger, models.Repo{FullName: repoName}, pullNum)
	Ok(t, err)
	workingDir.VerifyWasCalled(Never()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](),
		Any[string](), Any[string](), Any[string]())
}

func TestDeleteLocksByPull_SingleSuccess(t *testing.T) {
	t.Log("If a single lock is successfully deleted")
	logger := logging.NewNoopLogger(t)
	repoName := "reponame"
	pullNum := 2
	path := "."
	workspace := "default"
	projectName := "projectname"

	RegisterMockTestingT(t) // needed for pegomock WorkingDir mock
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	pull := models.PullRequest{
		BaseRepo: models.Repo{FullName: repoName},
		Num:      pullNum,
	}
	l.EXPECT().UnlockByPull(models.Repo{FullName: repoName}, pullNum).Return([]models.ProjectLock{
		{
			Pull:      pull,
			Workspace: workspace,
			Project: models.Project{
				Path:         path,
				RepoFullName: pull.BaseRepo.FullName,
				ProjectName:  projectName,
			},
		},
	}, nil,
	)
	dlc := events.DefaultDeleteLockCommand{
		Locker:     l,
		WorkingDir: workingDir,
	}
	_, err := dlc.DeleteLocksByPull(logger, models.Repo{FullName: repoName}, pullNum)
	Ok(t, err)
	workingDir.VerifyWasCalled(Once()).DeletePlan(Any[logging.SimpleLogging](), Eq(pull.BaseRepo), Eq(pull), Eq(workspace),
		Eq(path), Eq(projectName))
}

func TestDeleteLocksByPull_MultipleSuccess(t *testing.T) {
	t.Log("If multiple locks are successfully deleted")
	logger := logging.NewNoopLogger(t)
	repoName := "reponame"
	pullNum := 2
	path1 := "path1"
	path2 := "path2"
	workspace := "default"
	projectName := ""

	RegisterMockTestingT(t) // needed for pegomock WorkingDir mock
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	pull := models.PullRequest{
		BaseRepo: models.Repo{FullName: repoName},
		Num:      pullNum,
	}
	l.EXPECT().UnlockByPull(models.Repo{FullName: repoName}, pullNum).Return([]models.ProjectLock{
		{
			Pull:      pull,
			Workspace: workspace,
			Project: models.Project{
				Path:         path1,
				RepoFullName: pull.BaseRepo.FullName,
			},
		},
		{
			Pull:      pull,
			Workspace: workspace,
			Project: models.Project{
				Path:         path2,
				RepoFullName: pull.BaseRepo.FullName,
			},
		},
	}, nil,
	)
	dlc := events.DefaultDeleteLockCommand{
		Locker:     l,
		WorkingDir: workingDir,
	}
	_, err := dlc.DeleteLocksByPull(logger, models.Repo{FullName: repoName}, pullNum)
	Ok(t, err)
	workingDir.VerifyWasCalled(Once()).DeletePlan(logger, pull.BaseRepo, pull, workspace, path1, projectName)
	workingDir.VerifyWasCalled(Once()).DeletePlan(logger, pull.BaseRepo, pull, workspace, path2, projectName)
}

// With several replicas the plan file can be on another replica's disk, so
// unlocking must record the discard where every replica sees it.
func TestDeleteLocksByPull_MarksPlansDiscarded(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })

	pull := models.PullRequest{
		BaseRepo:   models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo", VCSHost: models.VCSHost{Hostname: "github.com"}},
		Num:        2,
		HeadCommit: "abc",
	}
	_, err := db.UpdatePullWithResults(pull, []command.ProjectResult{{
		Command:              command.Plan,
		RepoRelDir:           "dir1",
		Workspace:            "default",
		ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "Plan: 1 to add"}},
	}})
	Ok(t, err)

	l.EXPECT().UnlockByPull(pull.BaseRepo, 2).Return([]models.ProjectLock{{
		Pull:      pull,
		Workspace: "default",
		Project:   models.Project{Path: "dir1", RepoFullName: "owner/repo"},
	}}, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, 2)
	Ok(t, err)
	Equals(t, 1, n)
	status, err := db.GetPullStatus(pull)
	Ok(t, err)
	Equals(t, models.DiscardedPlanStatus, status.Projects[0].Status)
}

// Two named projects can share a directory and workspace. Unlocking one must
// discard that project's plan, not the first one in the directory.
func TestDeleteLocksByPull_DiscardsTheNamedProject(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })

	pull := models.PullRequest{
		BaseRepo:   models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo", VCSHost: models.VCSHost{Hostname: "github.com"}},
		Num:        2,
		HeadCommit: "abc",
	}
	plan := func(name string) command.ProjectResult {
		return command.ProjectResult{
			Command:              command.Plan,
			RepoRelDir:           "dir1",
			Workspace:            "default",
			ProjectName:          name,
			ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "Plan: 1 to add"}},
		}
	}
	_, err := db.UpdatePullWithResults(pull, []command.ProjectResult{plan("a"), plan("b")})
	Ok(t, err)

	l.EXPECT().UnlockByPull(pull.BaseRepo, 2).Return([]models.ProjectLock{
		{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo", ProjectName: "b"}},
		// No status was recorded for this one: it is skipped, not an error.
		{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir2", RepoFullName: "owner/repo"}},
	}, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, 2)
	Ok(t, err)
	Equals(t, 2, n)
	status, err := db.GetPullStatus(pull)
	Ok(t, err)
	got := map[string]models.ProjectPlanStatus{}
	for _, p := range status.Projects {
		got[p.ProjectName] = p.Status
	}
	Equals(t, map[string]models.ProjectPlanStatus{"a": models.PlannedPlanStatus, "b": models.DiscardedPlanStatus}, got)
}
