// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
	. "github.com/runatlantis/atlantis/testing"
)

func TestWithExecDeadline(t *testing.T) {
	t.Run("no timeout: no deadline", func(t *testing.T) {
		ctx, cancel := command.WithExecDeadline(0, time.Second)
		defer cancel()
		Assert(t, !command.HasExecDeadline(ctx), "expected no deadline")
		_, ok := ctx.Deadline()
		Assert(t, !ok, "expected no deadline")
		Ok(t, command.ExecTimeoutError(ctx))
	})
	t.Run("deadline reached", func(t *testing.T) {
		ctx, cancel := command.WithExecDeadline(10*time.Millisecond, 3*time.Second)
		defer cancel()
		Assert(t, command.HasExecDeadline(ctx), "expected a deadline")
		Ok(t, command.ExecTimeoutError(ctx))
		<-ctx.Done()
		err := command.ExecTimeoutError(ctx)
		Assert(t, errors.Is(err, command.ErrTimedOut), "expected ErrTimedOut, got %v", err)
		Equals(t, "command timed out after 10ms", err.Error())
		Equals(t, 3*time.Second, command.ExecGrace(ctx))
	})
	t.Run("command ended: not a timeout", func(t *testing.T) {
		ctx, cancel := command.WithExecDeadline(time.Hour, 0)
		cancel()
		Ok(t, command.ExecTimeoutError(ctx))
		Equals(t, command.DefaultExecGrace, command.ExecGrace(ctx))
	})
	t.Run("set on a command", func(t *testing.T) {
		var c command.Context
		Ok(t, command.ExecTimeoutError(c.ExecCtx()))
		done := c.SetExecDeadline(time.Hour, time.Minute)
		defer done()
		Assert(t, command.HasExecDeadline(c.ExecCtx()), "expected a deadline")
		Assert(t, command.HasExecDeadline(command.ProjectContext{ExecContext: c.ExecContext}.ExecCtx()), "expected the project to share it")
	})
}
