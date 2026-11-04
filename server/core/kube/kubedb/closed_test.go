package kubedb_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/core/kube/kubedb/kubedbtest"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	. "github.com/runatlantis/atlantis/testing"
)

// A pull marked closed is listed until its status is deleted, which happens
// when its cleanup finishes. Marking keeps the recorded projects.
func TestMarkPullClosed(t *testing.T) {
	db := kubedbtest.New(t)
	t.Cleanup(func() { db.Close() })
	repo := models.Repo{FullName: "org/repo", Owner: "org", Name: "repo", VCSHost: models.VCSHost{Hostname: "github.com", Type: models.Github}}
	pull := models.PullRequest{Num: 7, BaseRepo: repo}
	other := models.PullRequest{Num: 8, BaseRepo: repo}

	_, err := db.UpdatePullWithResults(pull, []command.ProjectResult{{Command: command.Plan, RepoRelDir: "app", Workspace: "default", ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{}}}})
	Ok(t, err)
	_, err = db.UpdatePullWithResults(other, []command.ProjectResult{{Command: command.Plan, RepoRelDir: "app", Workspace: "default", ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{}}}})
	Ok(t, err)

	Ok(t, db.MarkPullClosed(pull))
	Ok(t, db.MarkPullClosed(pull)) // idempotent
	closed, err := db.ClosedPulls()
	Ok(t, err)
	Equals(t, 1, len(closed))
	Equals(t, pull.Num, closed[0].Num)
	Equals(t, repo.VCSHost.Hostname, closed[0].BaseRepo.VCSHost.Hostname)
	status, err := db.GetPullStatus(pull)
	Ok(t, err)
	Equals(t, 1, len(status.Projects))

	// A pull with no status yet gets one, so its close isn't lost.
	fresh := models.PullRequest{Num: 9, BaseRepo: repo}
	Ok(t, db.MarkPullClosed(fresh))
	closed, err = db.ClosedPulls()
	Ok(t, err)
	Equals(t, 2, len(closed))

	Ok(t, db.DeletePullStatus(pull))
	Ok(t, db.DeletePullStatus(fresh))
	closed, err = db.ClosedPulls()
	Ok(t, err)
	Equals(t, 0, len(closed))
}
