// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
)

// AbandonDelay is how long a killed process's output is waited for before
// it is abandoned; see StartSupervised. A variable for tests.
var AbandonDelay = 5 * time.Second

// ErrInterrupted is wrapped by the error of a step whose process exited
// successfully after being interrupted at its command's deadline: it was cut
// short, so it must not count as a success.
var ErrInterrupted = errors.New("the process was interrupted")

// Supervision watches a process started by StartSupervised.
type Supervision struct {
	execCtx     context.Context
	done        chan struct{}
	interrupted atomic.Bool
}

// StartSupervised starts cmd under execCtx, a context from
// command.WithExecDeadline. Without a deadline it is cmd.Start. With one, cmd
// runs in its own process group. At the deadline the group is interrupted,
// so terraform can stop cleanly and release its state lock, and it is killed
// if it is still running after the grace period. If the caller still hasn't
// finished AbandonDelay after the kill, because a process that left the
// group holds its output open, abandon is called: it must close the caller's
// output readers so they see EOF. Once the deadline has passed, cmd isn't
// started. After cmd.Wait returns, pass its error through Finish.
func StartSupervised(execCtx context.Context, cmd *exec.Cmd, abandon func()) (*Supervision, error) {
	s := &Supervision{execCtx: execCtx, done: make(chan struct{})}
	if !command.HasExecDeadline(execCtx) {
		close(s.done)
		return s, cmd.Start()
	}
	if err := command.ExecTimeoutError(execCtx); err != nil {
		close(s.done)
		return s, err
	}
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		close(s.done)
		return s, err
	}
	go s.watch(cmd, abandon)
	return s, nil
}

func (s *Supervision) watch(cmd *exec.Cmd, abandon func()) {
	select {
	case <-s.done:
		return
	case <-s.execCtx.Done():
	}
	// Done also fires when the command ends; only the deadline stops
	// processes.
	if command.ExecTimeoutError(s.execCtx) == nil {
		return
	}
	if !interruptGroup(cmd) {
		return
	}
	s.interrupted.Store(true)
	select {
	case <-s.done:
		return
	case <-time.After(command.ExecGrace(s.execCtx)):
	}
	killGroup(cmd)
	select {
	case <-s.done:
		return
	case <-time.After(AbandonDelay):
	}
	if abandon != nil {
		abandon()
	}
}

// Finish stops watching the process and returns the step's error: waitErr,
// or, if the process was interrupted at the deadline, an error wrapping
// command.ErrTimedOut even when it exited successfully.
func (s *Supervision) Finish(waitErr error) error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	if !s.interrupted.Load() {
		return TimeoutError(s.execCtx, waitErr)
	}
	timeout := command.ExecTimeoutError(s.execCtx)
	if waitErr == nil {
		return fmt.Errorf("%w: %w", timeout, ErrInterrupted)
	}
	return fmt.Errorf("%w: %w", timeout, waitErr)
}

// TimeoutError returns err, wrapped with command.ErrTimedOut if execCtx has
// reached its deadline, so the step reports why it was stopped.
func TimeoutError(execCtx context.Context, err error) error {
	if err == nil || errors.Is(err, command.ErrTimedOut) {
		return err
	}
	if timeout := command.ExecTimeoutError(execCtx); timeout != nil {
		return fmt.Errorf("%w: %w", timeout, err)
	}
	return err
}

// CombinedOutputSupervised is cmd.CombinedOutput under execCtx; see
// StartSupervised. Without a deadline it is cmd.CombinedOutput. With one,
// the output goes through a pipe it owns, so a process that left the group
// and holds the pipe open can't keep it waiting past the deadline.
func CombinedOutputSupervised(execCtx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if !command.HasExecDeadline(execCtx) {
		return cmd.CombinedOutput()
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close() // nolint: errcheck
	cmd.Stdout = w
	cmd.Stderr = w
	s, err := StartSupervised(execCtx, cmd, func() { _ = r.Close() })
	// The child has its own copy; EOF comes once every writer is gone.
	_ = w.Close()
	if err != nil {
		return nil, s.Finish(err)
	}
	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	return out.Bytes(), s.Finish(cmd.Wait())
}
