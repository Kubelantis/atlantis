// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/locking"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
)

//go:generate go tool pegomock generate github.com/runatlantis/atlantis/server/events --package mocks -o mocks/mock_delete_lock_command.go DeleteLockCommand

// PlanStoreCleanupError reports stored plans that couldn't be deleted from
// the external plan store. It is returned only when that is the only problem:
// the locks were released and the plans recorded as discarded, so they can't
// be applied, but the plan files are still in the bucket.
type PlanStoreCleanupError struct{ Err error }

func (e *PlanStoreCleanupError) Error() string { return e.Err.Error() }
func (e *PlanStoreCleanupError) Unwrap() error { return e.Err }

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

// errLockChanged is returned when a lock changes while it is being deleted.
var errLockChanged = errors.New("the lock changed while it was being deleted; reload the page and try again")

// DeleteLock discards the plan of the lock at id and releases the lock.
// The discard is recorded in the shared database first: the plan file may be
// on another replica or in a plan store, where deleting files here cannot
// reach it, and only the discarded status stops a later apply. If it cannot
// be recorded nothing is changed and an error is returned, so the caller is
// never told the plan was discarded when it is still applicable, and can
// retry.
//
// As in DeleteLocksByPull, it holds the pull's working-dir lock throughout,
// so no plan or apply of the pull runs meanwhile, and releases only the
// exact lock acquisition it read. The pull is known only once the lock has
// been read, so the lock is read again after the pull is locked; if it
// changed in between, nothing is changed.
func (l *DefaultDeleteLockCommand) DeleteLock(logger logging.SimpleLogging, id string) (*models.ProjectLock, error) {
	lock, err := l.Locker.GetLock(id)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, nil
	}
	if l.WorkingDirLocker != nil && lock.Pull.BaseRepo != (models.Repo{}) {
		unlockPull, err := l.WorkingDirLocker.TryLockPull(lock.Pull.BaseRepo, lock.Pull.Num, command.Unlock, WorkingDirLockMetadata{})
		if err != nil {
			return nil, err
		}
		defer unlockPull()
		current, err := l.Locker.GetLock(id)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, nil
		}
		if !current.Pull.SameAs(lock.Pull) || !current.Time.Equal(lock.Time) {
			return nil, errLockChanged
		}
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

	released, err := l.Locker.UnlockExact(*lock)
	if err != nil {
		return nil, err
	}
	if released == nil {
		// Released meanwhile by a path that does not take the pull's
		// working-dir lock, such as the pull being closed. The plan is
		// discarded and its files below belong to this pull (plan paths
		// and stored plan keys include the pull number), so removing them
		// is still right; a lock another pull may hold now is untouched.
		logger.Debug("lock for dir %q workspace %q was already released", lock.Project.Path, lock.Workspace)
	}

	removeErr := l.WorkingDir.DeletePlan(logger, lock.Pull.BaseRepo, lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName)
	if removeErr != nil {
		logger.Warn("Failed to delete plan: %s", removeErr)
		return nil, removeErr
	}

	if l.PlanStore != nil {
		if err := l.PlanStore.DeletePlanForProject(
			lock.Pull.BaseRepo,
			lock.Pull.Num,
			lock.Workspace,
			lock.Project.Path,
			lock.Project.ProjectName,
			pullFence(l.WorkingDirLocker, lock.Pull.BaseRepo, lock.Pull.Num),
		); err != nil {
			logger.Err("plan discarded, but not deleted from the plan store: %s", err)
			return lock, &PlanStoreCleanupError{Err: err}
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
// harmless.
//
// It holds the pull's working-dir lock throughout, so no plan or apply of
// the pull runs meanwhile, and releases only the exact lock acquisitions it
// listed, so a lock taken again since is kept with its plan.
func (l *DefaultDeleteLockCommand) DeleteLocksByPull(logger logging.SimpleLogging, repo models.Repo, pullNum int) (int, error) {
	repoFullName := repo.FullName
	pull := models.PullRequest{Num: pullNum, BaseRepo: repo}
	if l.WorkingDirLocker != nil {
		unlockPull, err := l.WorkingDirLocker.TryLockPull(repo, pullNum, command.Unlock, WorkingDirLockMetadata{})
		if err != nil {
			return 0, err
		}
		defer unlockPull()
	}
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
	// storeErrs are stored plans that couldn't be deleted; see PlanStoreCleanupError.
	var storeErrs []error
	numLocks := 0
	for _, lock := range locks {
		released, err := l.Locker.UnlockExact(lock)
		if err != nil {
			logger.Warn("Failed to release lock for dir %q workspace %q: %s", lock.Project.Path, lock.Workspace, err)
			errs = append(errs, err)
			continue
		}
		if released == nil {
			// Released, or taken again, since it was listed.
			continue
		}
		numLocks++
		if err := l.WorkingDir.DeletePlan(logger, lock.Pull.BaseRepo, lock.Pull, lock.Workspace, lock.Project.Path, lock.Project.ProjectName); err != nil {
			logger.Warn("Failed to delete plan: %s", err)
			errs = append(errs, err)
		}
		// Only this lock's stored plan, not the pull's: the lock is what
		// was discarded.
		if l.PlanStore != nil {
			if err := l.PlanStore.DeletePlanForProject(lock.Pull.BaseRepo, pullNum, lock.Workspace, lock.Project.Path, lock.Project.ProjectName, pullFence(l.WorkingDirLocker, repo, pullNum)); err != nil {
				logger.Err("plan discarded, but not deleted from the plan store: %s", err)
				storeErrs = append(storeErrs, err)
			}
		}
	}

	if numLocks == 0 {
		logger.Debug("No locks found for repo '%v', pull request: %v", repoFullName, pullNum)
	}

	if len(errs) > 0 {
		return numLocks, errors.Join(append(errs, storeErrs...)...)
	}
	if len(storeErrs) > 0 {
		return numLocks, &PlanStoreCleanupError{Err: errors.Join(storeErrs...)}
	}
	return numLocks, nil
}
