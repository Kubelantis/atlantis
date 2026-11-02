// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package vcs_test

import (
	"testing"

	. "github.com/petergtz/pegomock/v4"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs"
	"github.com/runatlantis/atlantis/server/events/vcs/mocks"
	"github.com/runatlantis/atlantis/server/logging"
)

// Comments and commit statuses carry command output and errors; credentials
// in URLs are redacted before they reach the VCS.
func TestClientProxyRedactsURLCredentials(t *testing.T) {
	RegisterMockTestingT(t)
	github := mocks.NewMockClient()
	proxy := vcs.NewClientProxy(github, nil, nil, nil, nil, nil)
	repo := models.Repo{FullName: "org/repo", VCSHost: models.VCSHost{Type: models.Github}}
	logger := logging.NewNoopLogger(t)

	_ = proxy.CreateComment(logger, repo, 1, "Error: git::https://user:s3cret@github.com/org/mod.git", "plan")
	_ = proxy.UpdateStatus(logger, repo, models.PullRequest{Num: 1}, models.FailedCommitStatus, "atlantis/plan", "clone https://ghp_tok@github.com failed", "")

	github.VerifyWasCalledOnce().CreateComment(Any[logging.SimpleLogging](), Eq(repo), Eq(1), Eq("Error: git::https://<redacted>@github.com/org/mod.git"), Eq("plan"))
	github.VerifyWasCalledOnce().UpdateStatus(Any[logging.SimpleLogging](), Eq(repo), Any[models.PullRequest](), Eq(models.FailedCommitStatus), Eq("atlantis/plan"), Eq("clone https://<redacted>@github.com failed"), Eq(""))
}
