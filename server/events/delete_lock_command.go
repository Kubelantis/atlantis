// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/locking"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

//go:generate go tool pegomock generate github.com/runatlantis/atlantis/server/events --package mocks -o mocks/mock_delete_lock_command.go DeleteLockCommand

// DeleteLockCommand is the first step after a command request has been parsed.
type DeleteLockCommand interface {
	DeleteLock(logger logging.SimpleLogging, id string) (*models.ProjectLock, error)
	DeleteLocksByPull(logger logging.SimpleLogging, repo models.Repo, pullNum int) (int, error)
}

// DefaultDeleteLockCommand deletes a specific lock after a request from the LocksController.
type DefaultDeleteLockCommand struct {
	Locker           locking.Locker
	WorkingDir       WorkingDir
	WorkingDirLocker WorkingDirLocker
	Database         db.Database
	PlanStore        planstore.PlanStore
}

// DeleteLock discards the plan of the lock at id and releases the lock.
// The discard is recorded in the shared database first: the plan file may be
// on another replica or in a plan store, where deleting files here cannot
// reach it, and only the discarded status stops a later apply. If it cannot
// be recorded nothing is changed and an error is returned, so the caller is
// never told the plan was discarded when it is still applicable, and can
// retry.
func (l *DefaultDeleteLockCommand) DeleteLock(logger logging.SimpleLogging, id string) (*models.ProjectLock, error) {
	lock, err := l.Locker.GetLock(id)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, nil
	}

	// Locks from before BaseRepo was recorded cannot be matched to a status.
	if l.Database != nil && lock.Pull.BaseRepo != (models.Repo{}) {
		err := l.Database.UpdateProjectStatus(lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName, models.DiscardedPlanStatus)
		switch {
		case errors.Is(err, db.ErrProjectNotFound):
			logger.Debug("no plan status to discard for dir %q workspace %q project %q", lock.Project.Path, lock.Workspace, lock.Project.ProjectName)
		case err != nil:
			return nil, fmt.Errorf("recording the plan as discarded: %w; the lock was kept, try again", err)
		}
	}

	// Release the lock only if it still belongs to the same pull: it may have
	// been released and taken by another pull since it was read.
	if _, err := l.Locker.UnlockIfOwnedByPull(lock.Project, lock.Workspace, lock.Pull); err != nil {
		return nil, err
	}

	removeErr := l.WorkingDir.DeletePlan(logger, lock.Pull.BaseRepo, lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName)
	if removeErr != nil {
		logger.Warn("Failed to delete plan: %s", removeErr)
		return nil, removeErr
	}

	if l.PlanStore != nil {
		if err := l.PlanStore.DeletePlanForProject(
			lock.Pull.BaseRepo.Owner,
			lock.Pull.BaseRepo.Name,
			lock.Pull.Num,
			lock.Workspace,
			lock.Project.Path,
			lock.Project.ProjectName,
		); err != nil {
			logger.Warn("Failed to delete plan from external store: %s", err)
		}
	}

	return lock, nil
}

// DeleteLocksByPull discards the plans of every lock the pull holds and
// releases the locks. As in DeleteLock, the discards are recorded in the
// shared database first: plan files may be on another replica or in a plan
// store, where deleting files here cannot reach them. If any discard cannot
// be recorded no lock is released and no plan deleted, so nothing the user
// asked to discard becomes applicable; recording a discard again on retry is
// harmless. Only the locks listed here are released, so a lock the pull takes
// meanwhile (a plan running at the same time) is kept.
func (l *DefaultDeleteLockCommand) DeleteLocksByPull(logger logging.SimpleLogging, repo models.Repo, pullNum int) (int, error) {
	repoFullName := repo.FullName
	pull := models.PullRequest{Num: pullNum, BaseRepo: repo}
	all, err := l.Locker.List()
	if err != nil {
		return 0, err
	}
	keys := slices.Sorted(maps.Keys(all))
	var locks []models.ProjectLock
	for _, key := range keys {
		if all[key].Pull.SameAs(pull) {
			locks = append(locks, all[key])
		}
	}

	for _, lock := range locks {
		// Locks from before BaseRepo was recorded cannot be matched to a status.
		if l.Database == nil || lock.Pull.BaseRepo == (models.Repo{}) {
			continue
		}
		err := l.Database.UpdateProjectStatus(lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName, models.DiscardedPlanStatus)
		switch {
		case errors.Is(err, db.ErrProjectNotFound):
			// Apply needs a recorded plan status, so there is nothing
			// another replica could apply.
			logger.Debug("no plan status to discard for dir %q workspace %q project %q", lock.Project.Path, lock.Workspace, lock.Project.ProjectName)
		case err != nil:
			return 0, fmt.Errorf("recording the plan for dir %q workspace %q as discarded: %w; no locks were released, try again", lock.Project.Path, lock.Workspace, err)
		}
	}

	// Every plan is discarded now, so keep going past a failure: the
	// remaining locks can be released safely.
	var errs []error
	numLocks := 0
	for _, lock := range locks {
		released, err := l.Locker.UnlockIfOwnedByPull(lock.Project, lock.Workspace, lock.Pull)
		if err != nil {
			logger.Warn("Failed to release lock for dir %q workspace %q: %s", lock.Project.Path, lock.Workspace, err)
			errs = append(errs, err)
			continue
		}
		if released == nil {
			// Released, or taken by another pull, since it was listed.
			continue
		}
		numLocks++
		if err := l.WorkingDir.DeletePlan(logger, lock.Pull.BaseRepo, lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName); err != nil {
			logger.Warn("Failed to delete plan: %s", err)
			errs = append(errs, err)
		}
	}

	// Always clean up the external plan store for this pull, even when no
	// locks were found locally. Locks can be cleaned via other paths (manual
	// unlock, partial failure) which would otherwise leave orphaned S3 plans.
	if l.PlanStore != nil {
		owner, name, ok := strings.Cut(repoFullName, "/")
		if !ok {
			logger.Warn("cannot parse owner/repo from %q; skipping external plan store cleanup", repoFullName)
		} else if err := l.PlanStore.DeleteForPull(owner, name, pullNum); err != nil {
			logger.Warn("Failed to delete plans from external store: %s", err)
		}
	}

	if numLocks == 0 {
		logger.Debug("No locks found for repo '%v', pull request: %v", repoFullName, pullNum)
	}

	return numLocks, errors.Join(errs...)
}
