// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package events

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/runatlantis/atlantis/server/logging"

	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/locking"
	"github.com/runatlantis/atlantis/server/core/runtime"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs"
	"github.com/runatlantis/atlantis/server/jobs"
)

//go:generate go tool pegomock generate github.com/runatlantis/atlantis/server/events --package mocks -o mocks/mock_resource_cleaner.go ResourceCleaner

type ResourceCleaner interface {
	CleanUp(pullInfo jobs.PullInfo)
}

//go:generate go tool pegomock generate github.com/runatlantis/atlantis/server/events --package mocks -o mocks/mock_pull_cleaner.go PullCleaner

// PullCleaner cleans up pull requests after they're closed/merged.
type PullCleaner interface {
	// CleanUpPull deletes the workspaces used by the pull request on disk
	// and deletes any locks associated with this pull request for all workspaces.
	CleanUpPull(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) error
}

// PullClosedExecutor executes the tasks required to clean up a closed pull
// request.
type PullClosedExecutor struct {
	Locker                   locking.Locker
	VCSClient                vcs.Client
	WorkingDir               WorkingDir
	Database                 db.Database
	PullClosedTemplate       PullCleanupTemplate
	LogStreamResourceCleaner ResourceCleaner
	CancellationTracker      CancellationTracker
	PlanStore                runtime.PlanStore
	// Peers, if set, removes the pull's replica-local resources on every
	// other replica. With several replicas a pull's clones and job output can
	// be spread across them (e.g. plans made on one, applies on another).
	Peers PeerCleaner
	// WorkingDirLocker, if set, keeps cleanup from running under a command of
	// the pull: cleanup holds the pull's lock, and a pull closed while a
	// command runs is cleaned up when it finishes (see PullIdle).
	WorkingDirLocker WorkingDirLocker
	// Closed, if set, holds the durable closed mark the webhook sets before
	// cleanup (see ClosedPullRecorder). Cleanup runs only while the mark is
	// there: a pull reopened since it was closed keeps its plans and locks.
	Closed ClosedPullRecorder

	pendingMu sync.Mutex
	// pending holds pulls closed while a command ran, by working-dir key.
	pending map[string]pendingCleanup
}

// ClosedPullRecorder durably marks a pull closed whose cleanup is due. The
// close webhook sets the mark before cleanup, so a cleanup deferred while a
// command runs survives a failed webhook response, a restart, and commands
// running on another replica: the leader's sweep finds it. The mark goes
// with the pull's status at the end of its cleanup, or when a command shows
// the pull was reopened. It is implemented by kubedb.KubeDB.
type ClosedPullRecorder interface {
	MarkPullClosed(pull models.PullRequest) error
	PullMarkedClosed(pull models.PullRequest) (bool, error)
	ClearPullClosed(pull models.PullRequest, openAt time.Time) error
}

type pendingCleanup struct {
	logger logging.SimpleLogging
	repo   models.Repo
	pull   models.PullRequest
}

func pendingKey(repoKey string, pullNum int) string {
	return fmt.Sprintf("%s#%d", repoKey, pullNum)
}

// deferCleanup notes that pull was closed while a command ran, so the idle
// hook retries its cleanup when the pull's last local command finishes. The
// durable mark set by the webhook covers everything else.
func (p *PullClosedExecutor) deferCleanup(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest, busy error) {
	logger.Info("pull request closed while a command is running; cleaning up when it finishes: %s", busy)
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.pending == nil {
		p.pending = map[string]pendingCleanup{}
	}
	p.pending[pendingKey(workingDirKey(repo), pull.Num)] = pendingCleanup{logger: logger, repo: repo, pull: pull}
}

// PullIdle is the working-dir locker's idle hook: it finishes the cleanup of
// a pull closed while one of its commands ran.
func (p *PullClosedExecutor) PullIdle(repoKey string, pullNum int) {
	key := pendingKey(repoKey, pullNum)
	p.pendingMu.Lock()
	c, ok := p.pending[key]
	delete(p.pending, key)
	p.pendingMu.Unlock()
	if !ok {
		return
	}
	if err := p.CleanUpPull(c.logger, c.repo, c.pull); err != nil {
		c.logger.Err("cleaning up closed pull request: %s", err)
	}
}

// PullJobsCleaner drops all job output of a pull request, whatever project
// it belongs to. It is implemented by jobs.AsyncProjectCommandOutputHandler.
type PullJobsCleaner interface {
	CleanUpPullJobs(repo models.Repo, pullNum int)
}

// PeerCleaner asks other replicas to clean up a closed pull's local
// resources. It is best effort: replicas that miss it are cleaned by their
// janitor.
type PeerCleaner interface {
	CleanUpPeers(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest)
}

// ReplicaCleaner removes the resources a replica holds locally for a closed
// pull request: its clones and its job output. Shared state (locks, pull
// status, external plans) is left to PullClosedExecutor.
type ReplicaCleaner struct {
	WorkingDir          WorkingDir
	Jobs                PullJobsCleaner
	CancellationTracker CancellationTracker
	// WorkingDirLocker, if set, keeps a command of the pull running on this
	// replica from having its clones deleted; they are left to the janitor.
	WorkingDirLocker WorkingDirLocker
}

// CleanUpReplica implements the replica-local part of closing a pull.
func (c *ReplicaCleaner) CleanUpReplica(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) error {
	if c.Jobs != nil {
		c.Jobs.CleanUpPullJobs(pull.BaseRepo, pull.Num)
	}
	if c.CancellationTracker != nil {
		c.CancellationTracker.Clear(pull)
	}
	if c.WorkingDirLocker != nil {
		unlock, err := c.WorkingDirLocker.TryLockPull(repo, pull.Num, command.Unlock, WorkingDirLockMetadataForPull(pull))
		if err != nil {
			logger.Info("not deleting clones of closed pull request %d while a command uses them; the janitor removes them later: %s", pull.Num, err)
			return nil
		}
		defer unlock()
	}
	if err := c.WorkingDir.Delete(logger, repo, pull); err != nil {
		return fmt.Errorf("cleaning workspace: %w", err)
	}
	return nil
}

type templatedProject struct {
	RepoRelDir string
	Workspaces string
}

var pullClosedTemplate = template.Must(template.New("").Parse(
	"Locks and plans deleted for the projects and workspaces modified in this pull request:\n" +
		"{{ range . }}\n" +
		"- dir: `{{ .RepoRelDir }}` {{ .Workspaces }}{{ end }}"))

type PullCleanupTemplate interface {
	Execute(wr io.Writer, data any) error
}

type PullClosedEventTemplate struct{}

func (t *PullClosedEventTemplate) Execute(wr io.Writer, data any) error {
	return pullClosedTemplate.Execute(wr, data)
}

// CleanUpPull cleans up after a closed pull request.
func (p *PullClosedExecutor) CleanUpPull(logger logging.SimpleLogging, repo models.Repo, pull models.PullRequest) error {
	// Hold the pull's lock so no command of the pull runs while its files and
	// locks are removed. Taken as for unlock, it waits for every running
	// command, including import and state rm.
	if p.WorkingDirLocker != nil {
		unlock, err := p.WorkingDirLocker.TryLockPull(repo, pull.Num, command.Unlock, WorkingDirLockMetadataForPull(pull))
		if err != nil {
			p.deferCleanup(logger, repo, pull, err)
			return nil
		}
		defer unlock()
	}
	// The idle hook and the leader's sweep run a cleanup deferred earlier;
	// under the pull's lock, check it still applies.
	if p.Closed != nil {
		closed, err := p.Closed.PullMarkedClosed(pull)
		if err != nil {
			return fmt.Errorf("checking the pull request is still closed: %w", err)
		}
		if !closed {
			logger.Info("pull request %d is no longer marked closed; it was reopened or already cleaned up", pull.Num)
			return nil
		}
	}
	pullStatus, err := p.Database.GetPullStatus(pull)
	if err != nil {
		// Log and continue to clean up other resources.
		logger.Err("retrieving pull status: %s", err)
	}

	if pj, ok := p.LogStreamResourceCleaner.(PullJobsCleaner); ok {
		// One pass over every job of the pull, persisted logs included.
		pj.CleanUpPullJobs(pull.BaseRepo, pull.Num)
	} else if pullStatus != nil {
		for _, project := range pullStatus.Projects {
			jobContext := jobs.PullInfo{
				PullNum:      pull.Num,
				Repo:         pull.BaseRepo.Name,
				RepoFullName: pull.BaseRepo.FullName,
				VCSHostname:  pull.BaseRepo.VCSHost.Hostname,
				ProjectName:  project.ProjectName,
				Path:         project.RepoRelDir,
				Workspace:    project.Workspace,
			}
			p.LogStreamResourceCleaner.CleanUp(jobContext)
		}
	}

	// Workflow hook jobs are keyed by the pull request alone, and its
	// persisted job logs must go even when no pull status was recorded.
	if _, ok := p.LogStreamResourceCleaner.(PullJobsCleaner); !ok && p.LogStreamResourceCleaner != nil {
		p.LogStreamResourceCleaner.CleanUp(jobs.PullInfo{
			PullNum:      pull.Num,
			Repo:         pull.BaseRepo.Name,
			RepoFullName: pull.BaseRepo.FullName,
			VCSHostname:  pull.BaseRepo.VCSHost.Hostname,
		})
	}

	var workspaceErr error
	if err := p.WorkingDir.Delete(logger, repo, pull); err != nil {
		workspaceErr = fmt.Errorf("cleaning workspace: %w", err)
	}
	if p.Peers != nil {
		p.Peers.CleanUpPeers(logger, repo, pull)
	}

	// Always attempt external plan cleanup even if workspace deletion failed,
	// so S3 objects are not orphaned when local delete errors.
	if p.PlanStore != nil {
		if err := p.PlanStore.DeleteForPull(repo.Owner, repo.Name, pull.Num); err != nil {
			logger.Warn("failed to delete plans from external store: %s", err)
		}
	}

	if workspaceErr != nil {
		return workspaceErr
	}

	// Finally, delete locks. We do this last because when someone
	// unlocks a project, right now we don't actually delete the plan
	// so we might have plans laying around but no locks.
	locks, err := p.Locker.UnlockByPull(repo, pull.Num)
	if err != nil {
		return fmt.Errorf("cleaning up locks: %w", err)
	}

	// Delete pull from DB.
	if err := p.Database.DeletePullStatus(pull); err != nil {
		logger.Err("deleting pull from db: %s", err)
	}

	// Clear any operations to avoid unbounded growth.
	if p.CancellationTracker != nil {
		p.CancellationTracker.Clear(pull)
	}

	// If there are no locks then there's no need to comment.
	if len(locks) == 0 {
		return nil
	}

	templateData := p.buildTemplateData(locks)
	var buf bytes.Buffer
	if err = pullClosedTemplate.Execute(&buf, templateData); err != nil {
		return fmt.Errorf("rendering template for comment: %w", err)
	}
	return p.VCSClient.CreateComment(logger, repo, pull.Num, buf.String(), "")
}

// buildTemplateData formats the lock data into a slice that can easily be
// templated for the VCS comment. We organize all the workspaces by their
// respective project paths so the comment can look like:
// dir: {dir}, workspaces: {all-workspaces}
func (p *PullClosedExecutor) buildTemplateData(locks []models.ProjectLock) []templatedProject {
	workspacesByPath := make(map[string][]string)
	for _, l := range locks {
		path := l.Project.Path
		// Check if workspace already exists to avoid duplicates
		if !slices.Contains(workspacesByPath[path], l.Workspace) {
			workspacesByPath[path] = append(workspacesByPath[path], l.Workspace)
		}
	}

	// sort keys so we can write deterministic tests
	var sortedPaths []string
	for p := range workspacesByPath {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	var projects []templatedProject
	for _, p := range sortedPaths {
		workspace := workspacesByPath[p]
		sort.Strings(workspace)
		workspacesStr := fmt.Sprintf("`%s`", strings.Join(workspace, "`, `"))
		if len(workspace) == 1 {
			projects = append(projects, templatedProject{
				RepoRelDir: p,
				Workspaces: "workspace: " + workspacesStr,
			})
		} else {
			projects = append(projects, templatedProject{
				RepoRelDir: p,
				Workspaces: "workspaces: " + workspacesStr,
			})

		}
	}
	return projects
}
