// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package planstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/runatlantis/atlantis/server/core/objstore/objstoretest"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tally "github.com/uber-go/tally/v4"
)

// Keys of the plan of hostProjectContext in both layouts.
const (
	hostKey   = "pfx/@v2/github.com/acme/infra/42/default/modules/vpc/default.tfplan"
	legacyKey = "pfx/acme/infra/42/default/modules/vpc/default.tfplan"
)

func hostRepo(host string) models.Repo {
	return models.Repo{FullName: "acme/infra", Owner: "acme", Name: "infra", VCSHost: models.VCSHost{Hostname: host}}
}

func hostProjectContext() command.ProjectContext {
	ctx := testProjectContext()
	ctx.BaseRepo = hostRepo("github.com")
	ctx.Pull.HeadCommit = "abc123"
	return ctx
}

func hostStore(t *testing.T) (*planstore.S3PlanStore, *objstoretest.Fake) {
	fake := objstoretest.New()
	return planstore.NewS3PlanStoreWithClient(fake, "bucket", "pfx", logging.NewNoopLogger(t)), fake
}

func putPlan(fake *objstoretest.Fake, key, body, host string) {
	metadata := map[string]string{"head-commit": "abc123"}
	if host != "" {
		metadata["vcs-host"] = host
	}
	fake.Objects[key] = objstoretest.Object{Body: []byte(body), Metadata: metadata}
}

func TestHostKey(t *testing.T) {
	store, _ := hostStore(t)
	assert.Equal(t, hostKey, store.TestS3Key(hostProjectContext(), "/tmp/default.tfplan"))
}

// For one release the plan is written under both keys, so replicas still on
// the earlier version find it, and both carry the VCS host.
func TestSave_WritesBothLayoutsWithHost(t *testing.T) {
	store, fake := hostStore(t)
	planPath := filepath.Join(t.TempDir(), "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("plan"), 0o600))

	require.NoError(t, store.Save(hostProjectContext(), planPath))

	assert.ElementsMatch(t, []string{hostKey, legacyKey}, fake.Keys())
	for _, key := range []string{hostKey, legacyKey} {
		assert.Equal(t, []byte("plan"), fake.Objects[key].Body, key)
		assert.Equal(t, "github.com", fake.Objects[key].Metadata["vcs-host"], key)
		assert.Equal(t, "abc123", fake.Objects[key].Metadata["head-commit"], key)
	}
}

func TestLoad_PrefersHostKey(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")
	planPath := filepath.Join(t.TempDir(), "default.tfplan")

	require.NoError(t, store.Load(hostProjectContext(), planPath))
	got, err := os.ReadFile(planPath)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestLoad_FallsBackToLegacyOfSameHost(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, legacyKey, "legacy", "github.com")
	planPath := filepath.Join(t.TempDir(), "default.tfplan")

	require.NoError(t, store.Load(hostProjectContext(), planPath))
	got, err := os.ReadFile(planPath)
	require.NoError(t, err)
	assert.Equal(t, "legacy", string(got))
}

// Mirrored repos share commits, so the head commit can't tell whose legacy
// plan it is: only the VCS host can, and a plan without one needs a re-plan.
func TestLoad_RefusesLegacyOfOtherOrUnknownHost(t *testing.T) {
	for name, host := range map[string]string{"other host": "gitlab.com", "no host": ""} {
		t.Run(name, func(t *testing.T) {
			store, fake := hostStore(t)
			putPlan(fake, legacyKey, "legacy", host)
			planPath := filepath.Join(t.TempDir(), "default.tfplan")

			err := store.Load(hostProjectContext(), planPath)
			assert.ErrorContains(t, err, "run plan again")
			_, statErr := os.Stat(planPath)
			assert.True(t, os.IsNotExist(statErr), "plan file must not be written")
		})
	}
}

func TestLoad_MissingInBothLayouts(t *testing.T) {
	store, _ := hostStore(t)
	err := store.Load(hostProjectContext(), filepath.Join(t.TempDir(), "default.tfplan"))
	assert.ErrorContains(t, err, legacyKey)
}

// Legacy plans are shared by same-named repos on two hosts, so a pull deletes
// only those made for its own host.
func TestRemove_LeavesOtherHostsLegacyPlan(t *testing.T) {
	for name, host := range map[string]string{"other host": "gitlab.com", "no host": ""} {
		t.Run(name, func(t *testing.T) {
			store, fake := hostStore(t)
			putPlan(fake, hostKey, "new", "github.com")
			putPlan(fake, legacyKey, "legacy", host)

			require.NoError(t, store.Remove(hostProjectContext(), filepath.Join(t.TempDir(), "default.tfplan")))
			assert.Equal(t, []string{legacyKey}, fake.Keys())
		})
	}
}

func TestRemove_DeletesOwnLegacyPlan(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")

	require.NoError(t, store.Remove(hostProjectContext(), filepath.Join(t.TempDir(), "default.tfplan")))
	assert.Empty(t, fake.Keys())
}

func TestDeletePlanForProject_HostChecked(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "gitlab.com")

	require.NoError(t, store.DeletePlanForProject(hostRepo("github.com"), 42, "default", "modules/vpc", "", nil))
	assert.Equal(t, []string{legacyKey}, fake.Keys())

	require.NoError(t, store.DeletePlanForProject(hostRepo("gitlab.com"), 42, "default", "modules/vpc", "", nil))
	assert.Empty(t, fake.Keys())
}

// Closing a pull never deletes the legacy pull prefix as a whole: it can hold
// the plans of a same-named pull on another host.
func TestDeleteForPull_DeletesOnlyOwnLegacyPlans(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, "pfx/@v2/github.com/acme/infra/42/staging/a.tfplan", "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")
	putPlan(fake, "pfx/acme/infra/42/staging/b.tfplan", "other", "gitlab.com")
	putPlan(fake, "pfx/acme/infra/42/staging/c.tfplan", "unknown", "")
	putPlan(fake, "pfx/@v2/gitlab.com/acme/infra/42/default/d.tfplan", "other new", "gitlab.com")

	require.NoError(t, store.DeleteForPull(hostRepo("github.com"), 42, nil))
	assert.ElementsMatch(t, []string{
		"pfx/acme/infra/42/staging/b.tfplan",
		"pfx/acme/infra/42/staging/c.tfplan",
		"pfx/@v2/gitlab.com/acme/infra/42/default/d.tfplan",
	}, fake.Keys())
}

func TestListWorkspacesAndRestore_BothLayoutsHostChecked(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")
	putPlan(fake, "pfx/acme/infra/42/staging/a.tfplan", "own legacy", "github.com")
	putPlan(fake, "pfx/acme/infra/42/other/b.tfplan", "foreign", "gitlab.com")
	putPlan(fake, "pfx/acme/infra/42/unknown/c.tfplan", "unknown", "")

	workspaces, err := store.ListWorkspaces(hostRepo("github.com"), 42)
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "staging"}, workspaces)

	pullDir := t.TempDir()
	require.NoError(t, store.RestorePlans(pullDir, hostRepo("github.com"), 42))
	got, err := os.ReadFile(filepath.Join(pullDir, "default", "modules", "vpc", "default.tfplan"))
	require.NoError(t, err)
	assert.Equal(t, "new", string(got), "the host-scoped plan wins over the legacy copy")
	got, err = os.ReadFile(filepath.Join(pullDir, "staging", "a.tfplan"))
	require.NoError(t, err)
	assert.Equal(t, "own legacy", string(got))
	for _, ws := range []string{"other", "unknown"} {
		_, err := os.Stat(filepath.Join(pullDir, ws))
		assert.True(t, os.IsNotExist(err), "%s must not be restored", ws)
	}
}

// failingLegacyPut fails uploads to the legacy key.
type failingLegacyPut struct{ *objstoretest.Fake }

func (f failingLegacyPut) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if aws.ToString(in.Key) == legacyKey {
		return nil, errors.New("slow down")
	}
	return f.Fake.PutObject(ctx, in, opts...)
}

// A failed save leaves no copy behind: the host-scoped one would otherwise be
// a plan the command reported as failed.
func TestSave_FailedLegacyUploadLeavesNoCopy(t *testing.T) {
	fake := objstoretest.New()
	store := planstore.NewS3PlanStoreWithClient(failingLegacyPut{fake}, "bucket", "pfx", logging.NewNoopLogger(t))
	planPath := filepath.Join(t.TempDir(), "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("plan"), 0o600))

	assert.ErrorContains(t, store.Save(hostProjectContext(), planPath), "slow down")
	assert.Empty(t, fake.Keys())
}

// Plans are keyed by VCS host, so a repo without one is refused rather than
// stored in, or deleted from, the layout shared across hosts.
func TestRepoWithoutHostIsRefused(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, legacyKey, "legacy", "")
	ctx := hostProjectContext()
	ctx.BaseRepo.VCSHost.Hostname = ""
	planPath := filepath.Join(t.TempDir(), "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("plan"), 0o600))

	assert.ErrorContains(t, store.Save(ctx, planPath), "no VCS host")
	assert.ErrorContains(t, store.Load(ctx, planPath), "no VCS host")
	assert.ErrorContains(t, store.DeleteForPull(ctx.BaseRepo, 42, nil), "no VCS host")
	assert.ErrorContains(t, store.DeletePlanForProject(ctx.BaseRepo, 42, "default", "modules/vpc", "", nil), "no VCS host")
	assert.ErrorContains(t, store.Remove(ctx, planPath), "no VCS host")
	_, err := store.ListWorkspaces(ctx.BaseRepo, 42)
	assert.ErrorContains(t, err, "no VCS host")
	assert.ErrorContains(t, store.RestorePlans(t.TempDir(), ctx.BaseRepo, 42), "no VCS host")
	require.NoError(t, store.RestorePlans("", models.Repo{}, 0), "the capability probe needs no repo")
	assert.Equal(t, []string{legacyKey}, fake.Keys())
}

// replaceBeforeDelete makes the other host's pull replace key between the
// ownership check and the delete.
func replaceBeforeDelete(fake *objstoretest.Fake, key string) {
	fake.BeforeDelete = func(k string) {
		if k == key {
			fake.BeforeDelete = nil
			putPlan(fake, key, "other host's plan", "gitlab.com")
			o := fake.Objects[key]
			o.ETag = `"replaced"`
			fake.Objects[key] = o
		}
	}
}

// The delete of a legacy plan is bound to the version whose host was checked,
// so a replacement written in between, maybe by the other host, is kept.
func TestLegacyDeletesKeepAReplacedPlan(t *testing.T) {
	deletes := map[string]func(*planstore.S3PlanStore) error{
		"Remove": func(s *planstore.S3PlanStore) error {
			return s.Remove(hostProjectContext(), filepath.Join(t.TempDir(), "default.tfplan"))
		},
		"DeletePlanForProject": func(s *planstore.S3PlanStore) error {
			return s.DeletePlanForProject(hostRepo("github.com"), 42, "default", "modules/vpc", "", nil)
		},
		"DeleteForPull": func(s *planstore.S3PlanStore) error {
			return s.DeleteForPull(hostRepo("github.com"), 42, nil)
		},
	}
	for name, del := range deletes {
		t.Run(name, func(t *testing.T) {
			store, fake := hostStore(t)
			putPlan(fake, legacyKey, "legacy", "github.com")
			replaceBeforeDelete(fake, legacyKey)

			require.NoError(t, del(store))
			require.Equal(t, []string{legacyKey}, fake.Keys())
			assert.Equal(t, "gitlab.com", fake.Objects[legacyKey].Metadata["vcs-host"])
		})
	}
}

var errLost = errors.New("lost the pull")

// Once the pull is lost, cleanup stops: the plans are the new holder's.
func TestDeleteForPull_StopsWhenThePullIsLost(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, "pfx/@v2/github.com/acme/infra/42/staging/a.tfplan", "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")
	checks := 0
	fence := func() error {
		checks++
		if checks > 1 {
			return errLost
		}
		return nil
	}

	err := store.DeleteForPull(hostRepo("github.com"), 42, fence)
	assert.ErrorIs(t, err, errLost)
	assert.NotErrorIs(t, err, planstore.ErrNotDeleted, "plans kept for the new holder are not failed deletes")
	assert.Len(t, fake.Keys(), 2, "only the delete before the pull was lost happened")
	assert.Contains(t, fake.Keys(), legacyKey)
}

func TestDeletePlanForProject_StopsWhenThePullIsLost(t *testing.T) {
	store, fake := hostStore(t)
	putPlan(fake, hostKey, "new", "github.com")
	putPlan(fake, legacyKey, "legacy", "github.com")

	err := store.DeletePlanForProject(hostRepo("github.com"), 42, "default", "modules/vpc", "", func() error { return errLost })
	assert.ErrorIs(t, err, errLost)
	assert.ElementsMatch(t, []string{hostKey, legacyKey}, fake.Keys())
}

// failingRollback fails uploads to the legacy key and every delete.
type failingRollback struct{ failingLegacyPut }

func (failingRollback) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return nil, errors.New("kms unavailable")
}

// A host-scoped copy left behind by a failed save is a failed plan deletion:
// counted and returned like any other.
func TestSave_FailedRollbackIsReported(t *testing.T) {
	fake := objstoretest.New()
	store := planstore.NewS3PlanStoreWithClient(failingRollback{failingLegacyPut{fake}}, "bucket", "pfx", logging.NewNoopLogger(t))
	scope := tally.NewTestScope("", nil)
	store.SetScope(scope)
	planPath := filepath.Join(t.TempDir(), "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("plan"), 0o600))

	err := store.Save(hostProjectContext(), planPath)
	assert.ErrorContains(t, err, "slow down")
	assert.ErrorContains(t, err, "kms unavailable")
	assert.ErrorIs(t, err, planstore.ErrNotDeleted)
	assert.Equal(t, []string{hostKey}, fake.Keys())
	counter, ok := scope.Snapshot().Counters()["delete_errors_total+"]
	require.True(t, ok, "delete_errors_total was not reported")
	assert.Equal(t, int64(1), counter.Value())
}
