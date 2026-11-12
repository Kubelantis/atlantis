// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package models_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/core/runtime/models"
	"github.com/runatlantis/atlantis/server/events/command"
	. "github.com/runatlantis/atlantis/testing"
)

// At the deadline the process is interrupted, and the error says why.
func TestCombinedOutputSupervised_InterruptsAtDeadline(t *testing.T) {
	ctx, cancel := command.WithExecDeadline(100*time.Millisecond, time.Minute)
	defer cancel()
	start := time.Now()
	_, err := models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", "sleep 30"))
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Assert(t, time.Since(start) < 10*time.Second, "expected the interrupt to stop it, took %s", time.Since(start))
}

// A process that ignores the interrupt is killed after the grace period,
// with the processes it started: they would otherwise keep its output open.
func TestCombinedOutputSupervised_KillsAfterGrace(t *testing.T) {
	ctx, cancel := command.WithExecDeadline(100*time.Millisecond, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", `trap "echo interrupted" INT; sleep 30 & wait; sleep 30`))
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Assert(t, strings.Contains(string(out), "interrupted"), "expected the process to see the interrupt, got %q", out)
	Assert(t, time.Since(start) < 10*time.Second, "expected the kill to stop it, took %s", time.Since(start))
}

// Past the deadline nothing starts.
func TestStartSupervised_RefusesAfterDeadline(t *testing.T) {
	ctx, cancel := command.WithExecDeadline(time.Millisecond, time.Minute)
	defer cancel()
	<-ctx.Done()
	cmd := exec.Command("sh", "-c", "exit 0")
	_, err := models.StartSupervised(ctx, cmd, nil)
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Equals(t, "command timed out after 1ms", err.Error())
	Assert(t, cmd.Process == nil, "expected the process not to start")
}

// Without a deadline, or once the command ended normally, nothing is stopped.
func TestCombinedOutputSupervised_NoDeadline(t *testing.T) {
	out, err := models.CombinedOutputSupervised(context.Background(), exec.Command("sh", "-c", "echo ok"))
	Ok(t, err)
	Equals(t, "ok\n", string(out))

	ctx, cancel := command.WithExecDeadline(time.Hour, time.Minute)
	cmd := exec.Command("sh", "-c", "sleep 0.3; echo done")
	var buf strings.Builder
	cmd.Stdout = &buf
	s, err := models.StartSupervised(ctx, cmd, nil)
	Ok(t, err)
	cancel() // the command ends while the process still runs
	Ok(t, s.Finish(cmd.Wait()))
	Equals(t, "done\n", buf.String())

	// A failure that isn't a timeout keeps its own error.
	ctx, cancel = command.WithExecDeadline(time.Hour, time.Minute)
	defer cancel()
	_, err = models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", "exit 3"))
	Assert(t, err != nil && !errors.Is(err, command.ErrTimedOut), "expected a plain exit error, got %v", err)
}

// A process that exits successfully after the interrupt was still cut short:
// an apply stopped midway must not be recorded as a success.
func TestCombinedOutputSupervised_InterruptedSuccessIsATimeout(t *testing.T) {
	ctx, cancel := command.WithExecDeadline(100*time.Millisecond, time.Minute)
	defer cancel()
	_, err := models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", `trap "exit 0" INT; while true; do sleep 0.1; done`))
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Assert(t, errors.Is(err, models.ErrInterrupted), "expected ErrInterrupted, got %v", err)
}

// A process that left the group and holds the output open doesn't keep the
// caller waiting: once the group is killed, the output is abandoned.
func TestCombinedOutputSupervised_AbandonsOutputHeldOutsideTheGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	defer func(d time.Duration) { models.AbandonDelay = d }(models.AbandonDelay)
	models.AbandonDelay = 200 * time.Millisecond
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Cleanup(func() {
		if pid, err := os.ReadFile(pidFile); err == nil {
			_ = exec.Command("kill", strings.TrimSpace(string(pid))).Run()
		}
	})
	ctx, cancel := command.WithExecDeadline(100*time.Millisecond, 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", "setsid sleep 60 & echo $! > "+pidFile+"; sleep 60"))

	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
	Assert(t, time.Since(start) < 10*time.Second, "expected the caller to stop waiting, took %s", time.Since(start))
}
