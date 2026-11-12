// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/kube/kubedb/kubedbtest"
	lockmocks "github.com/runatlantis/atlantis/server/core/locking/mocks"
	"github.com/runatlantis/atlantis/server/core/planstore"
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
	l.EXPECT().GetLock("id").Return(lock, nil).Times(2) // read again once the pull is locked
	l.EXPECT().UnlockExact(*lock).Return(lock, nil)
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
	l.EXPECT().UnlockExact(*lock).DoAndReturn(func(models.ProjectLock) (*models.ProjectLock, error) {
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
	l.EXPECT().UnlockExact(*lock).Return(lock, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: db}

	got, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Equals(t, lock, got)
}

// expectPullLocks makes l report locks as the pull's locks and expect each to
// be released with UnlockExact.
func expectPullLocks(l *lockmocks.MockLocker, locks ...models.ProjectLock) {
	all := map[string]models.ProjectLock{}
	for _, lock := range locks {
		all[models.GenerateLockKey(lock.Project, lock.Workspace)] = lock
		l.EXPECT().UnlockExact(lock).Return(&lock, nil)
	}
	l.EXPECT().List().Return(all, nil)
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
	l.EXPECT().List().Return(nil, errors.New("err"))
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
	expectPullLocks(l)
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
	expectPullLocks(l, []models.ProjectLock{
		{
			Pull:      pull,
			Workspace: workspace,
			Project: models.Project{
				Path:         path,
				RepoFullName: pull.BaseRepo.FullName,
				ProjectName:  projectName,
			},
		},
	}...)
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
	expectPullLocks(l, []models.ProjectLock{
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
	}...)
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

	expectPullLocks(l, []models.ProjectLock{{
		Pull:      pull,
		Workspace: "default",
		Project:   models.Project{Path: "dir1", RepoFullName: "owner/repo"},
	}}...)
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

	expectPullLocks(l, []models.ProjectLock{
		{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo", ProjectName: "b"}},
		// No status was recorded for this one: it is skipped, not an error.
		{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir2", RepoFullName: "owner/repo"}},
	}...)
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

func unlockTestPull() models.PullRequest {
	return models.PullRequest{
		BaseRepo:   models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo", VCSHost: models.VCSHost{Hostname: "github.com"}},
		Num:        2,
		HeadCommit: "abc",
	}
}

func recordPlan(t *testing.T, d db.Database, pull models.PullRequest, dir string) models.ProjectLock {
	t.Helper()
	_, err := d.UpdatePullWithResults(pull, []command.ProjectResult{{
		Command: command.Plan, RepoRelDir: dir, Workspace: "default",
		ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "Plan: 1 to add"}},
	}})
	Ok(t, err)
	return models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: dir, RepoFullName: pull.BaseRepo.FullName}}
}

// Every discard is recorded before the first lock is released, so a plan
// left on another replica or in a plan store is never applicable once its
// lock is free.
func TestDeleteLocksByPull_RecordsDiscardsBeforeUnlocking(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	d := kubedbtest.New(t)
	t.Cleanup(func() { d.Close() })
	pull := unlockTestPull()
	lock1, lock2 := recordPlan(t, d, pull, "dir1"), recordPlan(t, d, pull, "dir2")

	l.EXPECT().List().Return(map[string]models.ProjectLock{"a": lock1, "b": lock2}, nil)
	l.EXPECT().UnlockExact(gomock.Any()).Times(2).DoAndReturn(func(lock models.ProjectLock) (*models.ProjectLock, error) {
		status, err := d.GetPullStatus(pull)
		Ok(t, err)
		for _, proj := range status.Projects {
			Equals(t, models.DiscardedPlanStatus, proj.Status)
		}
		return &lock, nil
	})
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: events.NewMockWorkingDir(), Database: d}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	Ok(t, err)
	Equals(t, 2, n)
}

// If a discard cannot be recorded no lock is released and no plan deleted:
// releasing them would leave plans applicable that the user asked to discard.
func TestDeleteLocksByPull_KeepsLocksWhenDiscardFails(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	d := kubedbtest.New(t)
	d.Close() // every write fails
	pull := unlockTestPull()
	lock := models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}

	l.EXPECT().List().Return(map[string]models.ProjectLock{"a": lock}, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, Database: d}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	ErrContains(t, "no locks were released", err)
	Equals(t, 0, n)
	workingDir.VerifyWasCalled(Never()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](), Any[string](), Any[string](), Any[string]())
}

// Only this pull's locks are released: not another pull's, not the same
// number on another VCS host, and not a lock the pull no longer holds.
func TestDeleteLocksByPull_ReleasesOnlyThisPullsLocks(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	pull := unlockTestPull()
	otherHost := pull
	otherHost.BaseRepo.VCSHost.Hostname = "gitlab.com"
	otherPull := pull
	otherPull.Num = 3
	mine := models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}
	gone := models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir2", RepoFullName: "owner/repo"}}

	l.EXPECT().List().Return(map[string]models.ProjectLock{
		"mine":  mine,
		"gone":  gone,
		"host":  {Pull: otherHost, Workspace: "default", Project: models.Project{Path: "dir3", RepoFullName: "owner/repo"}},
		"other": {Pull: otherPull, Workspace: "default", Project: models.Project{Path: "dir4", RepoFullName: "owner/repo"}},
	}, nil)
	l.EXPECT().UnlockExact(mine).Return(&mine, nil)
	// Released, or taken again, since it was listed.
	l.EXPECT().UnlockExact(gone).Return(nil, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	Ok(t, err)
	Equals(t, 1, n)
	workingDir.VerifyWasCalled(Once()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](), Any[string](), Any[string](), Any[string]())
}

// A plan or apply of the pull could write a new plan, or take a lock again,
// while the unlock runs; holding the pull's working-dir lock keeps them out.
func TestDeleteLocksByPull_RefusesWhileACommandRuns(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl) // no calls expected
	workingDirLocker := events.NewDefaultWorkingDirLocker()
	pull := unlockTestPull()
	release, err := workingDirLocker.TryLock(pull.BaseRepo, pull.Num, "default", "dir1", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer release()
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: events.NewMockWorkingDir(), WorkingDirLocker: workingDirLocker}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	ErrContains(t, "currently locked", err)
	Equals(t, 0, n)
}

// recordingPlanStore records plan store deletions.
type recordingPlanStore struct {
	planstore.PlanStore
	projects []string
	pulls    int
}

func (r *recordingPlanStore) DeletePlanForProject(repo models.Repo, pullNum int, workspace, repoRelDir, projectName string, _ planstore.Fence) error {
	r.projects = append(r.projects, fmt.Sprintf("%s|%s|%d|%s|%s|%s", repo.Owner, repo.Name, pullNum, workspace, repoRelDir, projectName))
	return nil
}

func (r *recordingPlanStore) DeleteForPull(models.Repo, int, planstore.Fence) error {
	r.pulls++
	return nil
}

// The unlock deletes only the stored plans of the locks it released, by the
// pull's repo (whose owner and name, for a GitLab subgroup, differ from
// splitting the full name at the first /).
func TestDeleteLocksByPull_DeletesOnlyReleasedPlansFromStore(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	store := &recordingPlanStore{}
	pull := models.PullRequest{
		Num:      2,
		BaseRepo: models.Repo{FullName: "group/sub/repo", Owner: "group/sub", Name: "repo", VCSHost: models.VCSHost{Hostname: "gitlab.com"}},
	}
	lock := models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "group/sub/repo"}}
	expectPullLocks(l, lock)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: events.NewMockWorkingDir(), PlanStore: store}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	Ok(t, err)
	Equals(t, 1, n)
	Equals(t, []string{"group/sub|repo|2|default|dir1|"}, store.projects)
	Equals(t, 0, store.pulls)
}

// Discarding one plan from the UI takes the pull's working-dir lock, so no
// plan or apply of that pull runs meanwhile.
func TestDeleteLock_RefusesWhileACommandRuns(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDirLocker := events.NewDefaultWorkingDirLocker()
	pull := unlockTestPull()
	lock := &models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}
	release, err := workingDirLocker.TryLock(pull.BaseRepo, pull.Num, "default", "dir1", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer release()
	l.EXPECT().GetLock("id").Return(lock, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: events.NewMockWorkingDir(), WorkingDirLocker: workingDirLocker}

	_, err = dlc.DeleteLock(logger, "id")
	ErrContains(t, "currently locked", err)
}

// If the lock was released and taken again before the pull's working-dir lock
// was held, the plan read first is not the one to discard: nothing is changed.
func TestDeleteLock_LockChangedBeforePullWasLocked(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	pull := unlockTestPull()
	first := &models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}, Time: time.Unix(100, 0)}
	again := *first
	again.Time = time.Unix(200, 0)
	gomock.InOrder(
		l.EXPECT().GetLock("id").Return(first, nil),
		l.EXPECT().GetLock("id").Return(&again, nil),
	)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir, WorkingDirLocker: events.NewDefaultWorkingDirLocker()}

	_, err := dlc.DeleteLock(logger, "id")
	ErrContains(t, "the lock changed", err)
	workingDir.VerifyWasCalled(Never()).DeletePlan(Any[logging.SimpleLogging](), Any[models.Repo](), Any[models.PullRequest](), Any[string](), Any[string](), Any[string]())
}

// If the exact lock read is gone by the time it is released (the pull was
// closed meanwhile), the discard stands and the pull's own plan files are
// still removed: they are keyed by the pull, not by the lock.
func TestDeleteLock_LockAlreadyReleased(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	workingDir := events.NewMockWorkingDir()
	lock := &models.ProjectLock{Pull: unlockTestPull(), Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}}
	l.EXPECT().GetLock("id").Return(lock, nil)
	l.EXPECT().UnlockExact(*lock).Return(nil, nil)
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: workingDir}

	got, err := dlc.DeleteLock(logger, "id")
	Ok(t, err)
	Equals(t, lock, got)
	workingDir.VerifyWasCalledOnce().DeletePlan(Any[logging.SimpleLogging](), Eq(lock.Pull.BaseRepo), Eq(lock.Pull), Eq("default"), Eq("dir1"), Eq(""))
}

// failingPlanStore fails every stored plan deletion.
type failingPlanStore struct{ planstore.PlanStore }

func (failingPlanStore) DeletePlanForProject(models.Repo, int, string, string, string, planstore.Fence) error {
	return fmt.Errorf("deleting plan from S3: %w: forbidden", planstore.ErrNotDeleted)
}

// A stored plan that can't be deleted doesn't undo the unlock: the locks are
// released and the plans discarded, and the caller is told what was left.
func TestDeleteLocksByPull_ReportsPlansLeftInStore(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	pull := models.PullRequest{Num: 2, BaseRepo: models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo"}}
	expectPullLocks(l, models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "dir1", RepoFullName: "owner/repo"}})
	dlc := events.DefaultDeleteLockCommand{Locker: l, WorkingDir: events.NewMockWorkingDir(), PlanStore: failingPlanStore{}}

	n, err := dlc.DeleteLocksByPull(logger, pull.BaseRepo, pull.Num)
	Equals(t, 1, n)
	var cleanup *events.PlanStoreCleanupError
	Assert(t, errors.As(err, &cleanup), "expected a PlanStoreCleanupError, got %v", err)
	Assert(t, errors.Is(err, planstore.ErrNotDeleted), "expected ErrNotDeleted, got %v", err)
}

func TestDeleteLock_ReportsPlanLeftInStore(t *testing.T) {
	logger := logging.NewNoopLogger(t)
	RegisterMockTestingT(t)
	pull := models.PullRequest{BaseRepo: models.Repo{FullName: "owner/repo", Owner: "owner", Name: "repo"}}
	ctrl := gomock.NewController(t)
	l := lockmocks.NewMockLocker(ctrl)
	lock := &models.ProjectLock{Pull: pull, Workspace: "default", Project: models.Project{Path: "path", RepoFullName: "owner/repo"}}
	l.EXPECT().GetLock("id").Return(lock, nil).Times(2)
	l.EXPECT().UnlockExact(*lock).Return(lock, nil)
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })
	dlc := events.DefaultDeleteLockCommand{
		Locker: l, Database: db, WorkingDirLocker: events.NewDefaultWorkingDirLocker(),
		WorkingDir: events.NewMockWorkingDir(), PlanStore: failingPlanStore{},
	}

	got, err := dlc.DeleteLock(logger, "id")
	Assert(t, got != nil, "the released lock must be returned")
	var cleanup *events.PlanStoreCleanupError
	Assert(t, errors.As(err, &cleanup), "expected a PlanStoreCleanupError, got %v", err)
}
