// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package models_test

import (
	"context"
	"errors"
	"os/exec"
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
	_, err := models.StartSupervised(ctx, cmd)
	Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
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
	stop, err := models.StartSupervised(ctx, cmd)
	Ok(t, err)
	cancel() // the command ends while the process still runs
	Ok(t, cmd.Wait())
	stop()
	Equals(t, "done\n", buf.String())

	// A failure that isn't a timeout keeps its own error.
	ctx, cancel = command.WithExecDeadline(time.Hour, time.Minute)
	defer cancel()
	_, err = models.CombinedOutputSupervised(ctx, exec.Command("sh", "-c", "exit 3"))
	Assert(t, err != nil && !errors.Is(err, command.ErrTimedOut), "expected a plain exit error, got %v", err)
}
