// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultExecGrace is how long a process interrupted at a command's deadline
// has to exit before it is killed, when no grace period is configured.
const DefaultExecGrace = 5 * time.Minute

// ErrTimedOut is wrapped by the error of a step stopped, or not started,
// because its command reached its deadline (--command-timeout-seconds).
var ErrTimedOut = errors.New("command timed out")

type execDeadlineKey struct{}

type execDeadline struct {
	timeout time.Duration
	grace   time.Duration
}

// WithExecDeadline returns the context that bounds the processes a command
// runs: it expires timeout from now, and processes still running then are
// interrupted, then killed after grace. It doesn't derive from the trace
// context, so the end of a forwarded request can't stop them. With timeout
// <= 0 there is no deadline. Call cancel when the command ends to free its
// timer; that doesn't stop anything (see ExecTimeoutError).
func WithExecDeadline(timeout, grace time.Duration) (ctx context.Context, cancel context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}
	if grace <= 0 {
		grace = DefaultExecGrace
	}
	ctx = context.WithValue(context.Background(), execDeadlineKey{}, execDeadline{timeout: timeout, grace: grace})
	return context.WithTimeout(ctx, timeout)
}

// ExecTimeoutError returns an error wrapping ErrTimedOut once ctx, made by
// WithExecDeadline, has reached its deadline, and nil otherwise; a context
// cancelled because its command ended doesn't count.
func ExecTimeoutError(ctx context.Context) error {
	if ctx == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	d, _ := ctx.Value(execDeadlineKey{}).(execDeadline)
	return fmt.Errorf("%w after %s", ErrTimedOut, d.timeout)
}

// ExecGrace returns how long a process interrupted at ctx's deadline has to
// exit before it is killed.
func ExecGrace(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(execDeadlineKey{}).(execDeadline); ok && d.grace > 0 {
		return d.grace
	}
	return DefaultExecGrace
}

// HasExecDeadline reports whether ctx was made by WithExecDeadline with a
// deadline.
func HasExecDeadline(ctx context.Context) bool {
	_, ok := ctx.Value(execDeadlineKey{}).(execDeadline)
	return ok
}

// SetExecDeadline gives c a deadline for the processes its steps run; see
// WithExecDeadline. Call the returned func when the command ends.
func (c *Context) SetExecDeadline(timeout, grace time.Duration) func() {
	ctx, cancel := WithExecDeadline(timeout, grace)
	c.ExecContext = ctx
	return cancel
}

// ExecCtx returns the context bounding the command's processes, or
// context.Background() if it has none.
func (c *Context) ExecCtx() context.Context {
	if c == nil || c.ExecContext == nil {
		return context.Background()
	}
	return c.ExecContext
}

// ExecCtx returns the context bounding the processes of the project's
// command, or context.Background() if it has none.
func (p ProjectContext) ExecCtx() context.Context {
	if p.ExecContext == nil {
		return context.Background()
	}
	return p.ExecContext
}
