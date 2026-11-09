// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package web_templates

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/jobs"
	. "github.com/runatlantis/atlantis/testing"
)

func TestIndexTemplate(t *testing.T) {
	err := IndexTemplate.Execute(io.Discard, IndexData{
		Locks: []LockIndexData{
			{
				LockPath:      "lock path",
				RepoFullName:  "repo full name",
				PullNum:       1,
				Path:          "path",
				Workspace:     "workspace",
				Time:          time.Now(),
				TimeFormatted: "2006-01-02 15:04:05",
			},
		},
		ApplyLock: ApplyLockData{
			Locked:        true,
			Time:          time.Now(),
			TimeFormatted: "2006-01-02 15:04:05",
		},
		AtlantisVersion: "v0.0.0",
		CleanedBasePath: "/path",
		PullToJobMapping: []jobs.PullInfoWithJobIDs{
			{
				Pull: jobs.PullInfo{
					PullNum:      1,
					Repo:         "repo",
					RepoFullName: "repo full name",
					ProjectName:  "project name",
					Path:         "path",
					Workspace:    "workspace",
				},
				JobIDInfos: []jobs.JobIDInfo{
					{JobID: "job id", JobIDUrl: "job id url", JobDescription: "job description", Time: time.Now(), TimeFormatted: "02-01-2006 15:04:05", JobStep: "job step"},
				},
			},
		},
	})
	Ok(t, err)
}

func TestLockTemplate(t *testing.T) {
	err := LockTemplate.Execute(io.Discard, LockDetailData{
		LockKeyEncoded:  "lock key encoded",
		LockKey:         "lock key",
		PullRequestLink: "https://example.com",
		LockedBy:        "locked by",
		Workspace:       "workspace",
		AtlantisVersion: "v0.0.0",
		CleanedBasePath: "/path",
		RepoOwner:       "repo owner",
		RepoName:        "repo name",
	})
	Ok(t, err)
}

func TestProjectJobsTemplate(t *testing.T) {
	err := ProjectJobsTemplate.Execute(io.Discard, ProjectJobData{
		AtlantisVersion: "v0.0.0",
		ProjectPath:     "project path",
		CleanedBasePath: "/path",
	})
	Ok(t, err)
}

func TestProjectJobsErrorTemplate(t *testing.T) {
	err := ProjectJobsTemplate.Execute(io.Discard, ProjectJobsError{
		AtlantisVersion: "v0.0.0",
		ProjectPath:     "project path",
		CleanedBasePath: "/path",
	})
	Ok(t, err)
}

func TestGithubAppSetupTemplate(t *testing.T) {
	err := GithubAppSetupTemplate.Execute(io.Discard, GithubSetupData{
		Target:          "target",
		Manifest:        "manifest",
		ID:              1,
		Key:             "key",
		WebhookSecret:   "webhook secret",
		URL:             "https://example.com",
		CleanedBasePath: "/path",
	})
	Ok(t, err)
}

// A discard whose plan file stayed in the plan store is reported as a
// warning, not as the plain success banner: the lock page passes the
// controller's X-Atlantis-Warning header on, and the index page shows a fixed
// warning (nothing from the URL is rendered).
func TestDiscardWarningIsShown(t *testing.T) {
	var lock strings.Builder
	Ok(t, LockTemplate.Execute(&lock, LockDetailData{CleanedBasePath: "/path"}))
	Assert(t, strings.Contains(lock.String(), `xhr.getResponseHeader("X-Atlantis-Warning") === "plan-store-cleanup"`), "lock page doesn't check the warning header")
	// html/template escapes the base path's slash inside the JS string.
	Assert(t, strings.Contains(lock.String(), `"\/path/?discard=true" + warning`), "lock page doesn't pass the warning on")

	var index strings.Builder
	Ok(t, IndexTemplate.Execute(&index, IndexData{CleanedBasePath: "/path"}))
	Assert(t, strings.Contains(index.String(), `<p class="js-discard-warning">`), "index page has no warning banner")
	Assert(t, strings.Contains(index.String(), `indexOf("plan_store=failed")`), "index page doesn't show the warning banner")
}
