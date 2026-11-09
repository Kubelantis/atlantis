// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package models

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup makes cmd lead a new process group, so its children are
// stopped with it.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func interruptGroup(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGINT) }

func killGroup(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGKILL) }

// signalGroup signals cmd's process group while its leader hasn't been
// reaped: once cmd.Wait has returned, the group's ID may belong to another
// process.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
