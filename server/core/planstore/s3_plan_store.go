// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package planstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	tally "github.com/uber-go/tally/v4"

	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/logging"
	"github.com/runatlantis/atlantis/server/utils"
)

// S3Client is the subset of the S3 API used by S3PlanStore.
type S3Client = objstore.API

// S3PlanStoreConfig configures an S3PlanStore.
type S3PlanStoreConfig = objstore.Config

// Object metadata keys written with every plan.
const (
	metaHeadCommit = "head-commit"
	metaPlannedBy  = "planned-by"
)

// S3PlanStore keeps plan files in S3 so any replica can apply them.
//
// Keys are <prefix>/<owner>/<repo>/<pull>/<workspace>/<repoRelDir>/<plan file>.
// Every plan carries the head commit it was made at, and Load refuses a plan
// made at a different commit.
type S3PlanStore struct {
	bucket *objstore.Bucket
	logger logging.SimpleLogging
	// deleteErrors counts failed plan deletions; nil until SetScope.
	deleteErrors tally.Counter
}

var _ PlanStore = (*S3PlanStore)(nil)

// ErrNotDeleted marks a plan that couldn't be deleted from the plan store.
// The plan stays in the bucket, where it holds the planned values, until a
// later delete or a lifecycle rule removes it. It never makes a discarded or
// applied plan applicable again: that is decided by the pull's status.
var ErrNotDeleted = errors.New("plan left in the plan store")

// SetScope reports failed plan deletions as the counter delete_errors_total
// in scope.
func (s *S3PlanStore) SetScope(scope tally.Scope) {
	s.deleteErrors = scope.Counter("delete_errors_total")
}

// deleteFailed counts a failed deletion and marks err with ErrNotDeleted.
func (s *S3PlanStore) deleteFailed(what string, err error) error {
	if s.deleteErrors != nil {
		s.deleteErrors.Inc(1)
	}
	return fmt.Errorf("%s: %w: %w", what, ErrNotDeleted, err)
}

// NewS3PlanStore connects to the configured bucket and checks it is reachable.
func NewS3PlanStore(cfg S3PlanStoreConfig, logger logging.SimpleLogging) (*S3PlanStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := objstore.NewS3(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("validating S3 plan store: %w", err)
	}
	return NewS3PlanStoreWithBucket(objstore.NewBucket(client, cfg), logger), nil
}

// NewS3PlanStoreWithClient returns a store on an existing client, without
// encryption settings. It is mainly used by tests.
func NewS3PlanStoreWithClient(client S3Client, bucket, prefix string, logger logging.SimpleLogging) *S3PlanStore {
	return NewS3PlanStoreWithBucket(objstore.NewBucket(client, objstore.Config{Bucket: bucket, Prefix: prefix}), logger)
}

// NewS3PlanStoreWithBucket returns a store on bucket.
func NewS3PlanStoreWithBucket(bucket *objstore.Bucket, logger logging.SimpleLogging) *S3PlanStore {
	return &S3PlanStore{bucket: bucket, logger: logger}
}

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), objstore.OpTimeout)
}

// Save implements PlanStore.
func (s *S3PlanStore) Save(ctx command.ProjectContext, planPath string) error {
	key := s.s3Key(ctx, planPath)
	f, err := os.Open(planPath)
	if err != nil {
		return fmt.Errorf("opening plan file for S3 upload: %w", err)
	}
	defer f.Close() // nolint: errcheck

	metadata := map[string]string{}
	if ctx.Pull.HeadCommit != "" {
		metadata[metaHeadCommit] = ctx.Pull.HeadCommit
	}
	if ctx.User.Username != "" {
		metadata[metaPlannedBy] = ctx.User.Username
	}

	c, cancel := opCtx()
	defer cancel()
	if err := s.bucket.Put(c, key, f, metadata); err != nil {
		return fmt.Errorf("uploading plan to S3 (key=%s): %w", key, err)
	}
	s.logger.Info("uploaded plan to s3://%s/%s", s.bucket.Name, key)
	return nil
}

// Load implements PlanStore. It refuses plans made at another head commit.
func (s *S3PlanStore) Load(ctx command.ProjectContext, planPath string) error {
	key := s.s3Key(ctx, planPath)
	c, cancel := opCtx()
	defer cancel()

	// Download next to the destination first, so a rejected or interrupted
	// download never replaces or truncates the plan at planPath.
	tmp := planPath + ".download"
	metadata, err := s.bucket.Download(c, key, tmp)
	if err != nil {
		return fmt.Errorf("downloading plan from S3 (key=%s): %w", key, err)
	}
	defer os.Remove(tmp) // nolint: errcheck

	planCommit := metadataValue(metadata, metaHeadCommit)
	if planCommit == "" {
		return fmt.Errorf("plan in S3 has no head-commit metadata (key=%s) — run plan again", key)
	}
	if ctx.Pull.HeadCommit != "" && planCommit != ctx.Pull.HeadCommit {
		return fmt.Errorf("plan was created at commit %.8s but PR is now at %.8s — run plan again", planCommit, ctx.Pull.HeadCommit)
	}
	if err := os.Rename(tmp, planPath); err != nil {
		return fmt.Errorf("writing plan file from S3: %w", err)
	}
	s.logger.Debug("downloaded plan from s3://%s/%s", s.bucket.Name, key)
	return nil
}

// metadataValue looks up an S3 metadata key case-insensitively; S3 returns
// user metadata keys in canonical header case.
func metadataValue(metadata map[string]string, key string) string {
	for k, v := range metadata {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// Remove implements PlanStore. The local plan file is removed even when the
// S3 delete fails; that failure is returned, marked ErrNotDeleted.
func (s *S3PlanStore) Remove(ctx command.ProjectContext, planPath string) error {
	key := s.s3Key(ctx, planPath)
	c, cancel := opCtx()
	defer cancel()
	var storeErr error
	if err := s.bucket.Delete(c, key); err != nil {
		storeErr = s.deleteFailed(fmt.Sprintf("deleting plan from S3 (key=%s)", key), err)
	} else {
		s.logger.Debug("deleted plan from s3://%s/%s", s.bucket.Name, key)
	}
	return errors.Join(storeErr, utils.RemoveIgnoreNonExistent(planPath))
}

// pullPrefix is the key prefix of every plan of a pull, with a trailing slash.
func (s *S3PlanStore) pullPrefix(owner, repo string, pullNum int) string {
	return s.bucket.Key(owner, repo, strconv.Itoa(pullNum)) + "/"
}

// planKeys yields the keys of every plan file of a pull.
func (s *S3PlanStore) planKeys(owner, repo string, pullNum int, fn func(key, rel string) error) error {
	prefix := s.pullPrefix(owner, repo, pullNum)
	for key, err := range s.bucket.List(context.Background(), prefix) {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(key, ".tfplan") {
			continue
		}
		if err := fn(key, strings.TrimPrefix(key, prefix)); err != nil {
			return err
		}
	}
	return nil
}

// ListWorkspaces implements PlanStore.
func (s *S3PlanStore) ListWorkspaces(owner, repo string, pullNum int) ([]string, error) {
	seen := map[string]struct{}{}
	err := s.planKeys(owner, repo, pullNum, func(_, rel string) error {
		if workspace, _, ok := strings.Cut(rel, "/"); ok && workspace != "" {
			seen[workspace] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing workspaces from S3: %w", err)
	}
	workspaces := make([]string, 0, len(seen))
	for w := range seen {
		workspaces = append(workspaces, w)
	}
	slices.Sort(workspaces)
	return workspaces, nil
}

// RestorePlans implements PlanStore by downloading every plan of the pull
// into pullDir. An empty pullDir is a capability probe.
func (s *S3PlanStore) RestorePlans(pullDir, owner, repo string, pullNum int) error {
	if pullDir == "" {
		return nil
	}
	restored := 0
	err := s.planKeys(owner, repo, pullNum, func(key, rel string) error {
		localPath, err := securejoin.SecureJoin(pullDir, rel)
		if err != nil {
			return fmt.Errorf("resolving safe path for S3 key %s: %w", key, err)
		}
		c, cancel := opCtx()
		defer cancel()
		if _, err := s.bucket.Download(c, key, localPath); err != nil {
			return fmt.Errorf("downloading plan from S3 (key=%s): %w", key, err)
		}
		restored++
		s.logger.Info("restored plan from s3://%s/%s to %s", s.bucket.Name, key, localPath)
		return nil
	})
	if err != nil {
		return fmt.Errorf("restoring plans from S3: %w", err)
	}
	s.logger.Info("restored %d plan(s) from S3 for %s/%s#%d", restored, owner, repo, pullNum)
	return nil
}

// DeleteForPull implements PlanStore. It attempts every deletion and returns
// all failures, so callers can report plans left in the bucket.
func (s *S3PlanStore) DeleteForPull(owner, repo string, pullNum int) error {
	deleted, err := s.bucket.DeletePrefix(context.Background(), s.pullPrefix(owner, repo, pullNum))
	if deleted > 0 {
		s.logger.Info("deleted %d plan(s) from S3 for %s/%s#%d", deleted, owner, repo, pullNum)
	}
	if err != nil {
		return s.deleteFailed(fmt.Sprintf("deleting plans from S3 for %s/%s#%d", owner, repo, pullNum), err)
	}
	return nil
}

// DeletePlanForProject implements PlanStore. A failed delete is returned,
// marked ErrNotDeleted.
func (s *S3PlanStore) DeletePlanForProject(owner, repo string, pullNum int, workspace, repoRelDir, projectName string) error {
	key := s.bucket.Key(owner, repo, strconv.Itoa(pullNum), workspace, repoRelDir, planFilename(workspace, projectName))
	c, cancel := opCtx()
	defer cancel()
	if err := s.bucket.Delete(c, key); err != nil {
		return s.deleteFailed(fmt.Sprintf("deleting plan from S3 (key=%s)", key), err)
	}
	s.logger.Debug("deleted plan from s3://%s/%s", s.bucket.Name, key)
	return nil
}

func (s *S3PlanStore) s3Key(ctx command.ProjectContext, planPath string) string {
	return s.bucket.Key(ctx.BaseRepo.Owner, ctx.BaseRepo.Name, strconv.Itoa(ctx.Pull.Num), ctx.Workspace, ctx.RepoRelDir, filepath.Base(planPath))
}

// planFilename must match runtime.GetPlanFilename (runtime imports this
// package, so it cannot be called here; a test keeps them in sync).
func planFilename(workspace, projectName string) string {
	if projectName == "" {
		return workspace + ".tfplan"
	}
	return strings.ReplaceAll(projectName, "/", "::") + "-" + workspace + ".tfplan"
}
