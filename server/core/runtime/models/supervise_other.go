// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package models

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

// interruptGroup interrupts cmd and reports whether it did.
func interruptGroup(cmd *exec.Cmd) bool {
	return cmd.Process.Signal(os.Interrupt) == nil
}

func killGroup(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
