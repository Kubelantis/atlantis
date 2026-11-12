// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package planstore

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/runatlantis/atlantis/server/events/models"
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
	metaVCSHost    = "vcs-host"
)

// keySpace is the first segment of every host-scoped key (ADR 0009). A legacy
// key starts with an owner name, and no VCS provider allows '@' in one, so
// the two layouts never collide.
const keySpace = "@v2"

// S3PlanStore keeps plan files in S3 so any replica can apply them.
//
// Keys are <prefix>/@v2/<host>/<owner>/<repo>/<pull>/<workspace>/<repoRelDir>/<plan file>.
// Every plan carries the head commit it was made at, and Load refuses a plan
// made at a different commit.
//
// Earlier versions used the legacy key <prefix>/<owner>/<repo>/<pull>/..., which
// is shared by same-named repos on two VCS hosts. For one release every plan is
// also written there, so replicas still running an earlier version during a
// rolling update find it. Every plan carries its VCS host, and a legacy object
// is read or deleted only for a pull on that host: neither the plan hash nor
// the head commit tells mirrored repos apart. A legacy object without a host,
// written by an earlier version, is never used and needs a re-plan.
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

// hostOf returns the repo's VCS host. Every plan is keyed by it, so a repo
// without one is an error rather than a plan in a layout shared across hosts.
func hostOf(repo models.Repo) (string, error) {
	if repo.VCSHost.Hostname == "" {
		return "", fmt.Errorf("repo %q has no VCS host, which plans in S3 are keyed by", repo.FullName)
	}
	return repo.VCSHost.Hostname, nil
}

// layouts are the layouts a plan can be in during this release: the
// host-scoped one, then the legacy one (legacy is true).
var layouts = []bool{false, true}

// pullKey joins parts below a pull in the host-scoped layout, or in the
// legacy one when legacy is true. Callers check the repo has a host first.
func (s *S3PlanStore) pullKey(repo models.Repo, pullNum int, legacy bool, parts ...string) string {
	base := []string{repo.Owner, repo.Name, strconv.Itoa(pullNum)}
	if !legacy {
		base = append([]string{keySpace, repo.VCSHost.Hostname}, base...)
	}
	return s.bucket.Key(append(base, parts...)...)
}

// pullPrefix is the key prefix of every plan of a pull, with a trailing slash.
func (s *S3PlanStore) pullPrefix(repo models.Repo, pullNum int, legacy bool) string {
	return s.pullKey(repo, pullNum, legacy) + "/"
}

func (s *S3PlanStore) s3Key(ctx command.ProjectContext, planPath string, legacy bool) string {
	return s.pullKey(ctx.BaseRepo, ctx.Pull.Num, legacy, ctx.Workspace, ctx.RepoRelDir, filepath.Base(planPath))
}

// Save implements PlanStore. The plan is written under the host-scoped key and
// the legacy key; a failure of either is returned, and the copies already
// written are deleted so no layout holds a plan the command reported failed.
// A copy that can't be deleted is counted and returned, marked ErrNotDeleted.
func (s *S3PlanStore) Save(ctx command.ProjectContext, planPath string) error {
	host, err := hostOf(ctx.BaseRepo)
	if err != nil {
		return err
	}
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
	metadata[metaVCSHost] = host
	keys := []string{s.s3Key(ctx, planPath, false), s.s3Key(ctx, planPath, true)}

	for i, key := range keys {
		if err := s.put(f, key, metadata); err != nil {
			errs := []error{err}
			for _, written := range keys[:i] {
				c, cancel := opCtx()
				if delErr := s.bucket.Delete(c, written); delErr != nil {
					errs = append(errs, s.deleteFailed(fmt.Sprintf("deleting plan from S3 after a failed upload (key=%s)", written), delErr))
				}
				cancel()
			}
			return errors.Join(errs...)
		}
	}
	return nil
}

func (s *S3PlanStore) put(f *os.File, key string, metadata map[string]string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding plan file for S3 upload: %w", err)
	}
	c, cancel := opCtx()
	defer cancel()
	if err := s.bucket.Put(c, key, f, metadata); err != nil {
		return fmt.Errorf("uploading plan to S3 (key=%s): %w", key, err)
	}
	s.logger.Info("uploaded plan to s3://%s/%s", s.bucket.Name, key)
	return nil
}

// Load implements PlanStore. It reads the host-scoped key, falling back to
// the legacy key only for a plan made for this pull's VCS host, and refuses
// plans made at another head commit.
func (s *S3PlanStore) Load(ctx command.ProjectContext, planPath string) error {
	host, err := hostOf(ctx.BaseRepo)
	if err != nil {
		return err
	}
	// Download next to the destination first, so a rejected or interrupted
	// download never replaces or truncates the plan at planPath.
	tmp := planPath + ".download"
	defer os.Remove(tmp) // nolint: errcheck

	key := s.s3Key(ctx, planPath, false)
	metadata, err := s.download(key, tmp)
	if objstore.IsNotFound(err) {
		key = s.s3Key(ctx, planPath, true)
		metadata, err = s.download(key, tmp)
		if err == nil && !strings.EqualFold(metadataValue(metadata, metaVCSHost), host) {
			return fmt.Errorf("plan in S3 was not made for VCS host %q (key=%s) — run plan again", host, key)
		}
	}
	if err != nil {
		return fmt.Errorf("downloading plan from S3 (key=%s): %w", key, err)
	}

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

func (s *S3PlanStore) download(key, localPath string) (map[string]string, error) {
	c, cancel := opCtx()
	defer cancel()
	return s.bucket.Download(c, key, localPath)
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

// legacyETag reports whether the legacy object at key was made for host, and
// if so its ETag. A missing object is not owned.
func (s *S3PlanStore) legacyETag(key, host string) (string, bool, error) {
	c, cancel := opCtx()
	defer cancel()
	info, found, err := s.bucket.Head(c, key)
	if err != nil || !found {
		return "", false, err
	}
	return info.ETag, strings.EqualFold(metadataValue(info.Metadata, metaVCSHost), host), nil
}

// fenced returns fence's error, if the caller no longer holds the pull.
func fenced(fence Fence) error {
	if fence == nil {
		return nil
	}
	return fence()
}

// deleteLegacy deletes the legacy object at key, read with etag and found to
// be made for this pull's host. The delete is bound to etag: the legacy layout
// is shared by same-named repos on two hosts, and an object replaced since it
// was read may be the other host's plan, so it is kept.
func (s *S3PlanStore) deleteLegacy(key, etag string, fence Fence) (bool, error) {
	if err := fenced(fence); err != nil {
		return false, err
	}
	c, cancel := opCtx()
	defer cancel()
	deleted, err := s.bucket.DeleteIfMatch(c, key, etag)
	if err != nil {
		return false, s.deleteFailed(fmt.Sprintf("deleting plan from S3 (key=%s)", key), err)
	}
	if !deleted {
		s.logger.Debug("kept plan at s3://%s/%s: it changed or went away since it was read", s.bucket.Name, key)
	}
	return deleted, nil
}

// deleteKey deletes the plan at key, or, for a legacy key, only the version
// made for the repo's host, as deleteLegacy does.
func (s *S3PlanStore) deleteKey(repo models.Repo, key string, legacy bool, fence Fence) error {
	if legacy {
		etag, owned, err := s.legacyETag(key, repo.VCSHost.Hostname)
		if err != nil {
			return s.deleteFailed(fmt.Sprintf("checking plan in S3 (key=%s)", key), err)
		}
		if !owned {
			return nil
		}
		_, err = s.deleteLegacy(key, etag, fence)
		return err
	}
	if err := fenced(fence); err != nil {
		return err
	}
	c, cancel := opCtx()
	defer cancel()
	if err := s.bucket.Delete(c, key); err != nil {
		return s.deleteFailed(fmt.Sprintf("deleting plan from S3 (key=%s)", key), err)
	}
	s.logger.Debug("deleted plan from s3://%s/%s", s.bucket.Name, key)
	return nil
}

// Remove implements PlanStore. The local plan file is removed even when the
// S3 delete fails; that failure is returned, marked ErrNotDeleted.
func (s *S3PlanStore) Remove(ctx command.ProjectContext, planPath string) error {
	if _, err := hostOf(ctx.BaseRepo); err != nil {
		return errors.Join(s.deleteFailed("deleting plan from S3", err), utils.RemoveIgnoreNonExistent(planPath))
	}
	var errs []error
	for _, legacy := range layouts {
		if err := s.deleteKey(ctx.BaseRepo, s.s3Key(ctx, planPath, legacy), legacy, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errors.Join(errs...), utils.RemoveIgnoreNonExistent(planPath))
}

// planKeys yields the key, pull-relative path and ETag of every plan file of a
// pull in one layout. In the legacy layout, only plans made for the repo's
// host are yielded, and etag is the version that was checked; in the
// host-scoped layout etag is empty.
func (s *S3PlanStore) planKeys(repo models.Repo, pullNum int, legacy bool, fn func(key, rel, etag string) error) error {
	prefix := s.pullPrefix(repo, pullNum, legacy)
	host := repo.VCSHost.Hostname
	for key, err := range s.bucket.List(context.Background(), prefix) {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(key, ".tfplan") {
			continue
		}
		etag := ""
		if legacy {
			var owned bool
			var err error
			etag, owned, err = s.legacyETag(key, host)
			if err != nil {
				return fmt.Errorf("checking plan in S3 (key=%s): %w", key, err)
			}
			if !owned {
				continue
			}
		}
		if err := fn(key, strings.TrimPrefix(key, prefix), etag); err != nil {
			return err
		}
	}
	return nil
}

// ListWorkspaces implements PlanStore.
func (s *S3PlanStore) ListWorkspaces(repo models.Repo, pullNum int) ([]string, error) {
	if _, err := hostOf(repo); err != nil {
		return nil, fmt.Errorf("listing workspaces from S3: %w", err)
	}
	seen := map[string]struct{}{}
	for _, legacy := range layouts {
		err := s.planKeys(repo, pullNum, legacy, func(_, rel, _ string) error {
			if workspace, _, ok := strings.Cut(rel, "/"); ok && workspace != "" {
				seen[workspace] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("listing workspaces from S3: %w", err)
		}
	}
	workspaces := make([]string, 0, len(seen))
	for w := range seen {
		workspaces = append(workspaces, w)
	}
	slices.Sort(workspaces)
	return workspaces, nil
}

// RestorePlans implements PlanStore by downloading every plan of the pull
// into pullDir. A plan found in both layouts is taken from the host-scoped
// one. An empty pullDir is a capability probe.
func (s *S3PlanStore) RestorePlans(pullDir string, repo models.Repo, pullNum int) error {
	if pullDir == "" {
		return nil
	}
	if _, err := hostOf(repo); err != nil {
		return fmt.Errorf("restoring plans from S3: %w", err)
	}
	restored := map[string]struct{}{}
	for _, legacy := range layouts {
		err := s.planKeys(repo, pullNum, legacy, func(key, rel, _ string) error {
			if _, ok := restored[rel]; ok {
				return nil
			}
			localPath, err := securejoin.SecureJoin(pullDir, rel)
			if err != nil {
				return fmt.Errorf("resolving safe path for S3 key %s: %w", key, err)
			}
			if _, err := s.download(key, localPath); err != nil {
				return fmt.Errorf("downloading plan from S3 (key=%s): %w", key, err)
			}
			restored[rel] = struct{}{}
			s.logger.Info("restored plan from s3://%s/%s to %s", s.bucket.Name, key, localPath)
			return nil
		})
		if err != nil {
			return fmt.Errorf("restoring plans from S3: %w", err)
		}
	}
	s.logger.Info("restored %d plan(s) from S3 for %s#%d", len(restored), repo.FullName, pullNum)
	return nil
}

// DeleteForPull implements PlanStore. It attempts every deletion and returns
// all failures, so callers can report plans left in the bucket. Legacy plans
// are deleted one by one, and only the versions made for this pull's host.
// Once fence reports the pull lost, it stops and returns that error: the
// plans left are the new holder's.
func (s *S3PlanStore) DeleteForPull(repo models.Repo, pullNum int, fence Fence) error {
	if _, err := hostOf(repo); err != nil {
		return s.deleteFailed("deleting plans from S3", err)
	}
	var errs []error
	deleted := 0
	stop := func(err error) error {
		s.logger.Warn("stopped deleting plans from S3 for %s#%d after deleting %d: %s", repo.FullName, pullNum, deleted, err)
		return errors.Join(append(errs, err)...)
	}
	for key, err := range s.bucket.List(context.Background(), s.pullPrefix(repo, pullNum, false)) {
		if err != nil {
			errs = append(errs, s.deleteFailed(fmt.Sprintf("listing plans in S3 for %s#%d", repo.FullName, pullNum), err))
			break
		}
		if err := fenced(fence); err != nil {
			return stop(err)
		}
		c, cancel := opCtx()
		err := s.bucket.Delete(c, key)
		cancel()
		if err != nil {
			errs = append(errs, s.deleteFailed(fmt.Sprintf("deleting plan from S3 (key=%s)", key), err))
			continue
		}
		deleted++
	}
	var fenceErr error
	err := s.planKeys(repo, pullNum, true, func(key, _, etag string) error {
		ok, err := s.deleteLegacy(key, etag, fence)
		if errors.Is(err, ErrNotDeleted) {
			errs = append(errs, err)
			return nil
		}
		if err != nil {
			fenceErr = err
			return err
		}
		if ok {
			deleted++
		}
		return nil
	})
	if fenceErr != nil {
		return stop(fenceErr)
	}
	if err != nil {
		errs = append(errs, s.deleteFailed(fmt.Sprintf("listing plans in S3 for %s#%d", repo.FullName, pullNum), err))
	}
	if deleted > 0 {
		s.logger.Info("deleted %d plan(s) from S3 for %s#%d", deleted, repo.FullName, pullNum)
	}
	return errors.Join(errs...)
}

// DeletePlanForProject implements PlanStore. A failed delete is returned,
// marked ErrNotDeleted.
func (s *S3PlanStore) DeletePlanForProject(repo models.Repo, pullNum int, workspace, repoRelDir, projectName string, fence Fence) error {
	if _, err := hostOf(repo); err != nil {
		return s.deleteFailed("deleting plan from S3", err)
	}
	var errs []error
	for _, legacy := range layouts {
		key := s.pullKey(repo, pullNum, legacy, workspace, repoRelDir, planFilename(workspace, projectName))
		if err := s.deleteKey(repo, key, legacy, fence); err != nil {
			errs = append(errs, err)
			if !errors.Is(err, ErrNotDeleted) {
				break // the pull was lost: the plan is the new holder's
			}
		}
	}
	return errors.Join(errs...)
}

// planFilename must match runtime.GetPlanFilename (runtime imports this
// package, so it cannot be called here; a test keeps them in sync).
func planFilename(workspace, projectName string) string {
	if projectName == "" {
		return workspace + ".tfplan"
	}
	return strings.ReplaceAll(projectName, "/", "::") + "-" + workspace + ".tfplan"
}
