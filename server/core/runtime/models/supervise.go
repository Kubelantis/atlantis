// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/runatlantis/atlantis/server/events/command"
)

// StartSupervised starts cmd under execCtx, a context from
// command.WithExecDeadline. Without a deadline it is cmd.Start. With one, cmd
// runs in its own process group; at the deadline the group is interrupted,
// so terraform can stop cleanly and release its state lock, and it is killed
// if it is still running after the grace period. Once the deadline has
// passed, cmd isn't started. Call stop after cmd.Wait returns, and pass its
// error through TimeoutError.
func StartSupervised(execCtx context.Context, cmd *exec.Cmd) (stop func(), err error) {
	if !command.HasExecDeadline(execCtx) {
		return func() {}, cmd.Start()
	}
	if err := command.ExecTimeoutError(execCtx); err != nil {
		return func() {}, err
	}
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return func() {}, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-execCtx.Done():
		}
		// Done also fires when the command ends; only the deadline stops
		// processes.
		if command.ExecTimeoutError(execCtx) == nil {
			return
		}
		interruptGroup(cmd)
		select {
		case <-done:
		case <-time.After(command.ExecGrace(execCtx)):
			killGroup(cmd)
		}
	}()
	return func() { close(done) }, nil
}

// TimeoutError returns err, wrapped with command.ErrTimedOut if execCtx has
// reached its deadline, so the step reports why it was stopped.
func TimeoutError(execCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if timeout := command.ExecTimeoutError(execCtx); timeout != nil {
		return fmt.Errorf("%w: %w", timeout, err)
	}
	return err
}

// CombinedOutputSupervised is cmd.CombinedOutput under execCtx; see
// StartSupervised.
func CombinedOutputSupervised(execCtx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	stop, err := StartSupervised(execCtx, cmd)
	if err != nil {
		return nil, err
	}
	err = cmd.Wait()
	stop()
	return out.Bytes(), TimeoutError(execCtx, err)
}
