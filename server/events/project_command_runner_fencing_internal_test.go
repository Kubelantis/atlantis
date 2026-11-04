package events

import (
	"errors"
	"testing"

	"github.com/runatlantis/atlantis/server/core/config/valid"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	. "github.com/runatlantis/atlantis/testing"
)

type fencingLocker struct {
	WorkingDirLocker
	lost bool
}

func (f *fencingLocker) LostPullLock(models.Repo, int) bool { return f.lost }

type countingStep struct{ calls int }

func (c *countingStep) Run(command.ProjectContext, []string, string, map[string]string) (string, error) {
	c.calls++
	return "ok", nil
}

func TestRunStepRefusesAfterPullLockLost(t *testing.T) {
	apply := &countingStep{}
	locker := &fencingLocker{WorkingDirLocker: NewDefaultWorkingDirLocker()}
	p := &DefaultProjectCommandRunner{PlanStepRunner: apply, WorkingDirLocker: locker}
	ctx := command.ProjectContext{Pull: models.PullRequest{Num: 1, BaseRepo: models.Repo{FullName: "org/repo"}}}

	out, err := p.runStep(valid.Step{StepName: "plan"}, ctx, t.TempDir(), map[string]string{})
	Ok(t, err)
	Equals(t, "ok", out)

	locker.lost = true
	_, err = p.runStep(valid.Step{StepName: "plan"}, ctx, t.TempDir(), map[string]string{})
	Assert(t, errors.Is(err, ErrPullLockLost), "expected ErrPullLockLost, got %v", err)
	Equals(t, 1, apply.calls)
}
