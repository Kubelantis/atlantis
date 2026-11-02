// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package logging_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/logging"
	"github.com/stretchr/testify/assert"
)

func TestStructuredLoggerSavesHistory(t *testing.T) {
	logger := logging.NewNoopLogger(t)

	historyLogger := logger.WithHistory()

	expectedStr := "[DBUG] Hello World\n[INFO] foo bar\n"

	historyLogger.Debug("Hello World")
	historyLogger.Info("foo bar")

	assert.Equal(t, expectedStr, historyLogger.GetHistory())
}

// Messages often carry command output and errors, and the history is posted
// to pull requests, so credentials in URLs are redacted.
func TestStructuredLoggerRedactsURLCredentials(t *testing.T) {
	historyLogger := logging.NewNoopLogger(t).WithHistory()

	historyLogger.Err("terraform init: %s", "Error: git::https://user:s3cret@github.com/org/mod.git")

	assert.Equal(t, "[EROR] terraform init: Error: git::https://<redacted>@github.com/org/mod.git\n", historyLogger.GetHistory())
}
