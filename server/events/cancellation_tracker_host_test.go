// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/models"
	. "github.com/runatlantis/atlantis/testing"
)

// Cancelling a pull leaves the pull with the same number of a same-named repo
// on another VCS host alone.
func TestCancellationIsPerVCSHost(t *testing.T) {
	tracker := events.NewCancellationTracker()
	github := models.PullRequest{Num: 1, BaseRepo: models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}}
	gitlab := models.PullRequest{Num: 1, BaseRepo: models.Repo{FullName: "owner/repo", VCSHost: models.VCSHost{Hostname: "gitlab.com"}}}

	tracker.Cancel(github)
	Assert(t, tracker.IsCancelled(github), "the cancelled pull is not cancelled")
	Assert(t, !tracker.IsCancelled(gitlab), "the other host's pull was cancelled")
}
