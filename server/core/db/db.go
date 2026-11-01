// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.
//
// Package db defines the database interface for Atlantis.
package db

import (
	"errors"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
)

//go:generate go tool mockgen -package mocks -destination mocks/mock_database.go . Database

// ErrProjectNotFound is returned by UpdateProjectStatus when the pull has no
// status for the project.
var ErrProjectNotFound = errors.New("no status recorded for this project")

// Database is an implementation of the database API we require.
type Database interface {
	TryLock(lock models.ProjectLock) (bool, models.ProjectLock, error)
	Unlock(project models.Project, workspace string) (*models.ProjectLock, error)
	// UnlockIfOwnedByPull unlocks project and workspace only when pull holds
	// the lock (see models.PullRequest.SameAs).
	UnlockIfOwnedByPull(project models.Project, workspace string, pull models.PullRequest) (*models.ProjectLock, error)
	List() ([]models.ProjectLock, error)
	GetLock(project models.Project, workspace string) (*models.ProjectLock, error)
	// UnlockByPull unlocks every lock held by pull number pullNum of repo,
	// matched by VCS host as well as full name.
	UnlockByPull(repo models.Repo, pullNum int) ([]models.ProjectLock, error)
	// UpdateProjectStatus sets the status of the project with this workspace,
	// directory and name. It returns ErrProjectNotFound when the pull has no
	// such project.
	UpdateProjectStatus(pull models.PullRequest, workspace string, repoRelDir string, projectName string, newStatus models.ProjectPlanStatus) error
	GetPullStatus(pull models.PullRequest) (*models.PullStatus, error)
	DeletePullStatus(pull models.PullRequest) error
	UpdatePullWithResults(pull models.PullRequest, newResults []command.ProjectResult) (models.PullStatus, error)

	LockCommand(cmdName command.Name, lockTime time.Time) (*command.Lock, error)
	UnlockCommand(cmdName command.Name) error
	CheckCommandLock(cmdName command.Name) (*command.Lock, error)

	// Ping checks the database connection health.
	Ping() error
	Close() error
}
