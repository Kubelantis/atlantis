// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package models

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func interruptGroup(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(os.Interrupt)
}

func killGroup(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
