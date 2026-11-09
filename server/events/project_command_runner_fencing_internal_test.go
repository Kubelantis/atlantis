package events

import (
	"errors"
	"testing"
	"time"

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

// Deleting a pull's stored plans stops once its lock is lost; a locker that
// can't lose it gives no fence.
func TestPullFence(t *testing.T) {
	Assert(t, pullFence(NewDefaultWorkingDirLocker(), models.Repo{}, 1) == nil, "a local locker needs no fence")
	Assert(t, pullFence(nil, models.Repo{}, 1) == nil, "no locker needs no fence")

	locker := &fencingLocker{WorkingDirLocker: NewDefaultWorkingDirLocker()}
	fence := pullFence(locker, models.Repo{FullName: "org/repo"}, 1)
	Ok(t, fence())
	locker.lost = true
	Assert(t, errors.Is(fence(), ErrPullLockLostCleanup), "expected ErrPullLockLostCleanup, got %v", fence())
}

// Past its command's deadline no step starts: the remaining steps and
// queued projects end at once with the timeout instead of starting and being
// interrupted.
func TestRunStepRefusesAfterCommandDeadline(t *testing.T) {
	plan := &countingStep{}
	p := &DefaultProjectCommandRunner{PlanStepRunner: plan, WorkingDirLocker: NewDefaultWorkingDirLocker()}
	execCtx, cancel := command.WithExecDeadline(20*time.Millisecond, time.Minute)
	defer cancel()
	ctx := command.ProjectContext{Pull: models.PullRequest{Num: 1, BaseRepo: models.Repo{FullName: "org/repo"}}, ExecContext: execCtx}

	_, err := p.runStep(valid.Step{StepName: "plan"}, ctx, t.TempDir(), map[string]string{})
	Ok(t, err)

	<-execCtx.Done()
	_, err = p.runStep(valid.Step{StepName: "plan"}, ctx, t.TempDir(), map[string]string{})
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Equals(t, 1, plan.calls)
}
