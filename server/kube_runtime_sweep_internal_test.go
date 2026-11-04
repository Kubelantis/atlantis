package server

import (
	"testing"

	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

type fakeClosedPulls []models.PullRequest

func (f fakeClosedPulls) ClosedPulls() ([]models.PullRequest, error) { return f, nil }

type recordingPullCleaner struct{ cleaned []int }

func (r *recordingPullCleaner) CleanUpPull(_ logging.SimpleLogging, _ models.Repo, pull models.PullRequest) error {
	r.cleaned = append(r.cleaned, pull.Num)
	return nil
}

// The leader's sweep runs the cleanup of every pull still marked closed.
func TestSweepClosedPullsCleansMarkedPulls(t *testing.T) {
	repo := models.Repo{FullName: "org/repo"}
	cleaner := &recordingPullCleaner{}
	k := &kubeRuntime{
		logger:      logging.NewNoopLogger(t),
		closedPulls: fakeClosedPulls{{Num: 3, BaseRepo: repo}, {Num: 5, BaseRepo: repo}},
		pullCleaner: cleaner,
	}
	k.sweepClosedPullsOnce()
	Equals(t, []int{3, 5}, cleaner.cleaned)
}
