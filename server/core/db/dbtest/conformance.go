// Package dbtest is a conformance suite that every db.Database backend must
// pass, so backends stay interchangeable.
package dbtest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	. "github.com/runatlantis/atlantis/testing"
)

// Factory returns a fresh, empty database for one subtest.
type Factory func(t *testing.T) db.Database

var (
	project   = models.NewProject("owner/repo", "parent/child", "")
	workspace = "default"
)

func newLock(pullNum int, p models.Project, ws string) models.ProjectLock {
	return models.ProjectLock{
		Pull:      models.PullRequest{Num: pullNum, BaseRepo: models.Repo{FullName: p.RepoFullName}},
		User:      models.User{Username: "lkysow"},
		Workspace: ws,
		Project:   p,
		Time:      time.Now().Truncate(time.Second),
	}
}

func newPull(num int, head string) models.PullRequest {
	return models.PullRequest{
		Num:        num,
		HeadCommit: head,
		BaseBranch: "main",
		BaseRepo: models.Repo{
			FullName: "owner/repo",
			VCSHost:  models.VCSHost{Hostname: "github.com", Type: models.Github},
		},
	}
}

func planResult(dir, ws, name string, noChanges bool) command.ProjectResult {
	out := "Plan: 1 to add, 0 to change, 0 to destroy."
	if noChanges {
		out = "No changes. Your infrastructure matches the configuration."
	}
	return command.ProjectResult{
		Command:              command.Plan,
		RepoRelDir:           dir,
		Workspace:            ws,
		ProjectName:          name,
		ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: out}},
	}
}

// Run executes the suite.
func Run(t *testing.T, newDB Factory) {
	t.Run("command lock", func(t *testing.T) {
		d := newDB(t)
		l, err := d.CheckCommandLock(command.Apply)
		Ok(t, err)
		Assert(t, l == nil, "expected no lock")

		_, err = d.LockCommand(command.Apply, time.Now())
		Ok(t, err)
		l, err = d.CheckCommandLock(command.Apply)
		Ok(t, err)
		Equals(t, true, l.IsLocked())

		_, err = d.LockCommand(command.Apply, time.Now())
		ErrEquals(t, "db transaction failed: lock already exists", err)

		Ok(t, d.UnlockCommand(command.Apply))
		ErrEquals(t, "db transaction failed: no lock exists", d.UnlockCommand(command.Apply))
		l, err = d.CheckCommandLock(command.Apply)
		Ok(t, err)
		Assert(t, l == nil, "expected no lock after unlock")
	})

	t.Run("try lock", func(t *testing.T) {
		d := newDB(t)
		acquired, curr, err := d.TryLock(newLock(1, project, workspace))
		Ok(t, err)
		Assert(t, acquired, "expected first lock to succeed")
		Equals(t, 1, curr.Pull.Num)

		acquired, curr, err = d.TryLock(newLock(2, project, workspace))
		Ok(t, err)
		Assert(t, !acquired, "expected second lock to fail")
		Equals(t, 1, curr.Pull.Num)

		// Different workspace and different path are independent locks.
		acquired, _, err = d.TryLock(newLock(2, project, "staging"))
		Ok(t, err)
		Assert(t, acquired, "expected other workspace to lock")
		acquired, _, err = d.TryLock(newLock(2, models.NewProject("owner/repo", "other", ""), workspace))
		Ok(t, err)
		Assert(t, acquired, "expected other path to lock")
	})

	t.Run("try lock is exclusive under contention", func(t *testing.T) {
		d := newDB(t)
		const n = 16
		var wg sync.WaitGroup
		wins := make(chan int, n)
		for i := range n {
			wg.Go(func() {
				acquired, _, err := d.TryLock(newLock(100+i, project, workspace))
				if err != nil {
					t.Error(err)
				}
				if acquired {
					wins <- i
				}
			})
		}
		wg.Wait()
		close(wins)
		Equals(t, 1, len(wins))
	})

	t.Run("unlock", func(t *testing.T) {
		d := newDB(t)
		l, err := d.Unlock(project, workspace)
		Ok(t, err)
		Assert(t, l == nil, "expected nil when unlocking nothing")

		_, _, err = d.TryLock(newLock(1, project, workspace))
		Ok(t, err)
		l, err = d.Unlock(project, workspace)
		Ok(t, err)
		Equals(t, 1, l.Pull.Num)

		acquired, _, err := d.TryLock(newLock(2, project, workspace))
		Ok(t, err)
		Assert(t, acquired, "expected lock after unlock")
	})

	t.Run("unlock if owned by pull", func(t *testing.T) {
		d := newDB(t)
		_, _, err := d.TryLock(newLock(1, project, workspace))
		Ok(t, err)

		l, err := d.UnlockIfOwnedByPull(project, workspace, newLock(2, project, workspace).Pull)
		Ok(t, err)
		Assert(t, l == nil, "must not unlock another pull's lock")
		l, err = d.GetLock(project, workspace)
		Ok(t, err)
		Assert(t, l != nil, "lock must remain")

		l, err = d.UnlockIfOwnedByPull(project, workspace, newLock(1, project, workspace).Pull)
		Ok(t, err)
		Equals(t, 1, l.Pull.Num)
		l, err = d.GetLock(project, workspace)
		Ok(t, err)
		Assert(t, l == nil, "lock must be gone")
	})

	t.Run("list and get", func(t *testing.T) {
		d := newDB(t)
		locks, err := d.List()
		Ok(t, err)
		Equals(t, 0, len(locks))

		want := newLock(1, project, workspace)
		_, _, err = d.TryLock(want)
		Ok(t, err)
		_, _, err = d.TryLock(newLock(1, project, "staging"))
		Ok(t, err)

		locks, err = d.List()
		Ok(t, err)
		Equals(t, 2, len(locks))

		got, err := d.GetLock(project, workspace)
		Ok(t, err)
		Equals(t, want.User, got.User)
		Equals(t, want.Project, got.Project)
		Assert(t, want.Time.Equal(got.Time), "time %v != %v", want.Time, got.Time)
	})

	t.Run("unlock by pull", func(t *testing.T) {
		d := newDB(t)
		_, _, err := d.TryLock(newLock(1, project, workspace))
		Ok(t, err)
		_, _, err = d.TryLock(newLock(1, models.NewProject("owner/repo", "other", ""), workspace))
		Ok(t, err)
		_, _, err = d.TryLock(newLock(2, project, "staging"))
		Ok(t, err)

		unlocked, err := d.UnlockByPull(models.Repo{FullName: "owner/repo"}, 1)
		Ok(t, err)
		Equals(t, 2, len(unlocked))
		locks, err := d.List()
		Ok(t, err)
		Equals(t, 1, len(locks))
		Equals(t, 2, locks[0].Pull.Num)
	})

	// One server can serve two VCS providers that both have owner/repo, so
	// a pull is identified by host as well as repo name and number.
	t.Run("unlocking ignores the same pull number on another host", func(t *testing.T) {
		d := newDB(t)
		github := models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}
		gitlab := models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "gitlab.com"}}
		lock := newLock(1, project, workspace)
		lock.Pull.BaseRepo = github
		_, _, err := d.TryLock(lock)
		Ok(t, err)

		l, err := d.UnlockIfOwnedByPull(project, workspace, models.PullRequest{Num: 1, BaseRepo: gitlab})
		Ok(t, err)
		Assert(t, l == nil, "unlocked the lock of the same pull number on another host")
		unlocked, err := d.UnlockByPull(gitlab, 1)
		Ok(t, err)
		Equals(t, 0, len(unlocked))
		l, err = d.GetLock(project, workspace)
		Ok(t, err)
		Assert(t, l != nil, "lock must remain")

		unlocked, err = d.UnlockByPull(github, 1)
		Ok(t, err)
		Equals(t, 1, len(unlocked))
	})

	t.Run("pull status lifecycle", func(t *testing.T) {
		d := newDB(t)
		pull := newPull(1, "sha1")
		s, err := d.GetPullStatus(pull)
		Ok(t, err)
		Assert(t, s == nil, "expected no status")

		status, err := d.UpdatePullWithResults(pull, []command.ProjectResult{planResult(".", "default", "", false)})
		Ok(t, err)
		Equals(t, 1, len(status.Projects))
		Equals(t, models.PlannedPlanStatus, status.Projects[0].Status)

		// Same commit: projects are merged.
		status, err = d.UpdatePullWithResults(pull, []command.ProjectResult{planResult("mod", "default", "", true)})
		Ok(t, err)
		Equals(t, 2, len(status.Projects))

		got, err := d.GetPullStatus(pull)
		Ok(t, err)
		Equals(t, status.Projects, got.Projects)
		Equals(t, pull.HeadCommit, got.Pull.HeadCommit)

		Ok(t, d.UpdateProjectStatus(pull, "default", ".", "", models.AppliedPlanStatus))
		got, err = d.GetPullStatus(pull)
		Ok(t, err)
		Equals(t, models.AppliedPlanStatus, got.Projects[0].Status)

		// New commit: status is rebuilt from the new results only.
		status, err = d.UpdatePullWithResults(newPull(1, "sha2"), []command.ProjectResult{planResult(".", "default", "", false)})
		Ok(t, err)
		Equals(t, 1, len(status.Projects))

		Ok(t, d.DeletePullStatus(pull))
		got, err = d.GetPullStatus(pull)
		Ok(t, err)
		Assert(t, got == nil, "expected status deleted")
		Ok(t, d.DeletePullStatus(pull))
	})

	t.Run("pull status preserves policy approvals across commits", func(t *testing.T) {
		d := newDB(t)
		res := planResult(".", "default", "", false)
		res.Command = command.PolicyCheck
		res.PolicyCheckResults = &models.PolicyCheckResults{PolicySetResults: []models.PolicySetResult{{
			PolicySetName: "ps",
			Approvals:     []models.PolicySetApproval{{Approver: "alice", Hashes: []string{"h"}}},
			Hashes:        []string{"h"},
		}}}
		_, err := d.UpdatePullWithResults(newPull(1, "sha1"), []command.ProjectResult{res})
		Ok(t, err)

		status, err := d.UpdatePullWithResults(newPull(1, "sha2"), []command.ProjectResult{planResult(".", "default", "", false)})
		Ok(t, err)
		Equals(t, "alice", status.Projects[0].PolicyStatus[0].Approvals[0].Approver)
	})

	t.Run("update project status matches the exact project", func(t *testing.T) {
		d := newDB(t)
		pull := newPull(4, "sha1")
		_, err := d.UpdatePullWithResults(pull, []command.ProjectResult{
			planResult("mod", "default", "a", false),
			planResult("mod", "default", "b", false),
			planResult("mod", "staging", "b", false),
		})
		Ok(t, err)

		Ok(t, d.UpdateProjectStatus(pull, "default", "./mod", "b", models.DiscardedPlanStatus))
		got, err := d.GetPullStatus(pull)
		Ok(t, err)
		statuses := map[string]models.ProjectPlanStatus{}
		for _, p := range got.Projects {
			statuses[p.Workspace+"/"+p.ProjectName] = p.Status
		}
		Equals(t, map[string]models.ProjectPlanStatus{
			"default/a": models.PlannedPlanStatus,
			"default/b": models.DiscardedPlanStatus,
			"staging/b": models.PlannedPlanStatus,
		}, statuses)

		err = d.UpdateProjectStatus(pull, "default", "mod", "c", models.DiscardedPlanStatus)
		Assert(t, errors.Is(err, db.ErrProjectNotFound), "expected ErrProjectNotFound, got %v", err)
		err = d.UpdateProjectStatus(newPull(5, "sha1"), "default", "mod", "a", models.DiscardedPlanStatus)
		Assert(t, errors.Is(err, db.ErrProjectNotFound), "expected ErrProjectNotFound for a pull with no status, got %v", err)
	})

	t.Run("pull status persists plan risk", func(t *testing.T) {
		d := newDB(t)
		res := planResult(".", "default", "", false)
		res.PlanSuccess.Risk = &models.PlanRisk{
			Tier: models.PlanRiskHigh, Deletes: 2, BlastRadius: 2.25,
			Findings:   []models.PlanRiskFinding{{Address: "aws_db_instance.main", Action: "delete", Reason: "stateful", Probability: 0.875}},
			Model:      "jev-1.13.0",
			AssessedAt: time.Now().Truncate(time.Second).UTC(),
		}
		_, err := d.UpdatePullWithResults(newPull(1, "sha1"), []command.ProjectResult{res})
		Ok(t, err)
		got, err := d.GetPullStatus(newPull(1, "sha1"))
		Ok(t, err)
		gotRisk := got.Projects[0].PlanRisk
		Assert(t, gotRisk != nil, "expected plan risk")
		Assert(t, gotRisk.AssessedAt.Equal(res.PlanSuccess.Risk.AssessedAt), "assessed time changed")
		gotRisk.AssessedAt = res.PlanSuccess.Risk.AssessedAt
		Equals(t, *res.PlanSuccess.Risk, *gotRisk)
	})

	t.Run("concurrent pull updates are not lost", func(t *testing.T) {
		d := newDB(t)
		pull := newPull(7, "sha1")
		const n = 8
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				if _, err := d.UpdatePullWithResults(pull, []command.ProjectResult{planResult(fmt.Sprintf("dir%d", i), "default", "", false)}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		got, err := d.GetPullStatus(pull)
		Ok(t, err)
		Equals(t, n, len(got.Projects))
	})

	t.Run("ping", func(t *testing.T) {
		Ok(t, newDB(t).Ping())
	})

	t.Run("operations fail after close", func(t *testing.T) {
		d := newDB(t)
		Ok(t, d.Close())
		_, err := d.UpdatePullWithResults(newPull(1, "sha1"), []command.ProjectResult{planResult(".", "default", "", false)})
		Assert(t, err != nil, "write after close must fail")
		_, _, err = d.TryLock(newLock(1, project, workspace))
		Assert(t, err != nil, "lock after close must fail")
		Assert(t, d.Ping() != nil, "ping after close must fail")
	})
}
