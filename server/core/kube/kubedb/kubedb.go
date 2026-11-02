// Package kubedb implements db.Database on the Kubernetes API so any number
// of Atlantis replicas can share locks and pull status without BoltDB or Redis.
//
// Storage layout, all in one namespace:
//   - project locks: coordination.k8s.io Leases named lock-<hash>. Creating the
//     Lease acquires the lock; the API server's create semantics make this
//     atomic across replicas. The lock payload is kept in an annotation.
//   - command locks (global apply lock): Leases named cmdlock-<hash>.
//   - pull status: atlantis.runatlantis.io/v1alpha1 PullStatus objects named
//     pull-<hash>, updated with optimistic concurrency.
//
// Project locks are not time bounded. Unlike leader election they represent a
// user-visible claim that lasts until apply, unlock, or pull close.
package kubedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/core/kube/apis/v1alpha1"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
)

const (
	lockPrefix    = "lock"
	cmdLockPrefix = "cmdlock"
	pullPrefix    = "pull"
)

var _ db.Database = (*KubeDB)(nil)

// conflictBackoff retries optimistic-concurrency conflicts. Writers to one
// pull status are few, but a burst of project results can race; the jitter
// spreads them out.
var conflictBackoff = wait.Backoff{Steps: 12, Duration: 10 * time.Millisecond, Factor: 1.6, Jitter: 0.5, Cap: time.Second}

// Config configures a KubeDB.
type Config struct {
	Client    client.Client
	Namespace string
	// Identity is recorded on the objects this replica creates, for debugging.
	Identity string
	// Timeout bounds every API call. Defaults to 10s.
	Timeout time.Duration
}

// KubeDB is a db.Database backed by the Kubernetes API.
type KubeDB struct {
	c        apiClient
	closed   atomic.Bool
	ns       string
	identity string
	timeout  time.Duration
}

// New returns a KubeDB.
func New(cfg Config) (*KubeDB, error) {
	if cfg.Client == nil {
		return nil, errors.New("kubedb: client is required")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("kubedb: namespace is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &KubeDB{c: cfg.Client, ns: cfg.Namespace, identity: cfg.Identity, timeout: cfg.Timeout}, nil
}

func (k *KubeDB) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), k.timeout)
}

// ---- project locks ----

func (k *KubeDB) lockName(p models.Project, workspace string) (string, string) {
	key := models.GenerateLockKey(p, workspace)
	return kube.Name(lockPrefix, key), key
}

// TryLock implements db.Database.
func (k *KubeDB) TryLock(newLock models.ProjectLock) (bool, models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	name, key := k.lockName(newLock.Project, newLock.Workspace)
	lease, err := k.projectLockLease(name, key, sanitizeLock(newLock))
	if err != nil {
		return false, newLock, err
	}

	// A lock can be released between our failed create and the follow-up get,
	// so retry a few times before reporting an error.
	for range 3 {
		err = k.client().Create(ctx, lease)
		if err == nil {
			return true, newLock, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return false, newLock, fmt.Errorf("creating lock lease: %w", err)
		}
		curr, getErr := k.getLockLease(ctx, name)
		if getErr != nil {
			return false, newLock, getErr
		}
		if curr != nil {
			return false, *curr, nil
		}
	}
	return false, newLock, errors.New("lock was contended: created and released concurrently")
}

func (k *KubeDB) projectLockLease(name, key string, l models.ProjectLock) (*coordinationv1.Lease, error) {
	data, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("serializing lock: %w", err)
	}
	holder := fmt.Sprintf("%s#%d", l.Pull.BaseRepo.FullName, l.Pull.Num)
	acquired := metav1.NewMicroTime(l.Time)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.ns,
			Labels: kube.Labels(kube.TypeProjectLock, map[string]string{
				kube.LabelRepo: kube.Hash(l.Project.RepoFullName),
				kube.LabelPull: strconv.Itoa(l.Pull.Num),
			}),
			Annotations: map[string]string{
				kube.AnnotationKey:  key,
				kube.AnnotationData: string(data),
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &acquired},
	}, nil
}

// getLockLease returns the lock stored in the named lease, or nil.
func (k *KubeDB) getLockLease(ctx context.Context, name string) (*models.ProjectLock, error) {
	var lease coordinationv1.Lease
	err := k.client().Get(ctx, client.ObjectKey{Namespace: k.ns, Name: name}, &lease)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting lock lease: %w", err)
	}
	l, err := decodeLock(&lease)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func decodeLock(lease *coordinationv1.Lease) (models.ProjectLock, error) {
	var l models.ProjectLock
	if err := json.Unmarshal([]byte(lease.Annotations[kube.AnnotationData]), &l); err != nil {
		return l, fmt.Errorf("deserializing lock %s: %w", lease.Name, err)
	}
	return l, nil
}

// deleteExact deletes the lease only if it is still the version we read.
func (k *KubeDB) deleteExact(ctx context.Context, lease *coordinationv1.Lease) error {
	uid, rv := lease.UID, lease.ResourceVersion
	err := k.client().Delete(ctx, lease, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Unlock implements db.Database.
func (k *KubeDB) Unlock(p models.Project, workspace string) (*models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	name, _ := k.lockName(p, workspace)
	var found *models.ProjectLock
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		var lease coordinationv1.Lease
		err := k.client().Get(ctx, client.ObjectKey{Namespace: k.ns, Name: name}, &lease)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		l, err := decodeLock(&lease)
		if err != nil {
			return err
		}
		if err := k.deleteExact(ctx, &lease); err != nil {
			return err
		}
		found = &l
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unlocking: %w", err)
	}
	return found, nil
}

// UnlockIfOwnedByPull implements db.Database.
func (k *KubeDB) UnlockIfOwnedByPull(p models.Project, workspace string, pull models.PullRequest) (*models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	name, _ := k.lockName(p, workspace)
	var found *models.ProjectLock
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		var lease coordinationv1.Lease
		err := k.client().Get(ctx, client.ObjectKey{Namespace: k.ns, Name: name}, &lease)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		l, err := decodeLock(&lease)
		if err != nil {
			return err
		}
		if !l.Pull.SameAs(pull) {
			return nil
		}
		if err := k.deleteExact(ctx, &lease); err != nil {
			return err
		}
		found = &l
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unlocking: %w", err)
	}
	return found, nil
}

func (k *KubeDB) listLocks(ctx context.Context, extra map[string]string) ([]coordinationv1.Lease, error) {
	var list coordinationv1.LeaseList
	sel := client.MatchingLabels(kube.Labels(kube.TypeProjectLock, extra))
	if err := k.client().List(ctx, &list, client.InNamespace(k.ns), sel); err != nil {
		return nil, fmt.Errorf("listing lock leases: %w", err)
	}
	return list.Items, nil
}

// List implements db.Database.
func (k *KubeDB) List() ([]models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	leases, err := k.listLocks(ctx, nil)
	if err != nil {
		return nil, err
	}
	var locks []models.ProjectLock
	for i := range leases {
		l, err := decodeLock(&leases[i])
		if err != nil {
			return locks, err
		}
		locks = append(locks, l)
	}
	return locks, nil
}

// UnlockExact implements db.Database.
func (k *KubeDB) UnlockExact(want models.ProjectLock) (*models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	name, _ := k.lockName(want.Project, want.Workspace)
	var found *models.ProjectLock
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		found = nil
		var lease coordinationv1.Lease
		err := k.client().Get(ctx, client.ObjectKey{Namespace: k.ns, Name: name}, &lease)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		l, err := decodeLock(&lease)
		if err != nil {
			return err
		}
		if !l.Pull.SameAs(want.Pull) || !l.Time.Equal(want.Time) {
			return nil
		}
		if err := k.deleteExact(ctx, &lease); err != nil {
			return err
		}
		found = &l
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unlocking: %w", err)
	}
	return found, nil
}

// GetLock implements db.Database.
func (k *KubeDB) GetLock(p models.Project, workspace string) (*models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	name, _ := k.lockName(p, workspace)
	l, err := k.getLockLease(ctx, name)
	if l != nil {
		l.Time = l.Time.Local()
	}
	return l, err
}

// UnlockByPull implements db.Database. It matches the repo name exactly, so
// "org/repo" does not release "org/repo2", and the VCS host, so a pull of
// the same name and number on another host keeps its locks.
func (k *KubeDB) UnlockByPull(repo models.Repo, pullNum int) ([]models.ProjectLock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	pull := models.PullRequest{Num: pullNum, BaseRepo: repo}
	leases, err := k.listLocks(ctx, map[string]string{
		kube.LabelRepo: kube.Hash(repo.FullName),
		kube.LabelPull: strconv.Itoa(pullNum),
	})
	if err != nil {
		return nil, err
	}
	var locks []models.ProjectLock
	for i := range leases {
		l, err := decodeLock(&leases[i])
		if err != nil {
			return locks, err
		}
		if l.Project.RepoFullName != repo.FullName || !l.Pull.SameAs(pull) {
			continue
		}
		if err := k.deleteExact(ctx, &leases[i]); err != nil {
			if apierrors.IsConflict(err) {
				// Re-acquired since we listed it; it is no longer this pull's lock.
				continue
			}
			return locks, fmt.Errorf("unlocking repo %s, path %s, workspace %s: %w", l.Project.RepoFullName, l.Project.Path, l.Workspace, err)
		}
		locks = append(locks, l)
	}
	return locks, nil
}

// ---- command locks ----

func (k *KubeDB) commandLockName(cmdName command.Name) string {
	return kube.Name(cmdLockPrefix, cmdName.String())
}

// LockCommand implements db.Database.
func (k *KubeDB) LockCommand(cmdName command.Name, lockTime time.Time) (*command.Lock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	lock := command.Lock{CommandName: cmdName, LockMetadata: command.LockMetadata{UnixTime: lockTime.Unix()}}
	data, err := json.Marshal(lock)
	if err != nil {
		return nil, err
	}
	holder := k.identity
	acquired := metav1.NewMicroTime(lockTime)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        k.commandLockName(cmdName),
			Namespace:   k.ns,
			Labels:      kube.Labels(kube.TypeCommandLock, nil),
			Annotations: map[string]string{kube.AnnotationKey: cmdName.String(), kube.AnnotationData: string(data)},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &acquired},
	}
	if err := k.client().Create(ctx, lease); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, errors.New("db transaction failed: lock already exists")
		}
		return nil, fmt.Errorf("db transaction failed: %w", err)
	}
	return &lock, nil
}

// UnlockCommand implements db.Database.
func (k *KubeDB) UnlockCommand(cmdName command.Name) error {
	ctx, cancel := k.ctx()
	defer cancel()
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: k.commandLockName(cmdName), Namespace: k.ns}}
	err := k.client().Delete(ctx, lease)
	if apierrors.IsNotFound(err) {
		return errors.New("db transaction failed: no lock exists")
	}
	if err != nil {
		return fmt.Errorf("db transaction failed: %w", err)
	}
	return nil
}

// CheckCommandLock implements db.Database.
func (k *KubeDB) CheckCommandLock(cmdName command.Name) (*command.Lock, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	var lease coordinationv1.Lease
	err := k.client().Get(ctx, client.ObjectKey{Namespace: k.ns, Name: k.commandLockName(cmdName)}, &lease)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lock command.Lock
	if err := json.Unmarshal([]byte(lease.Annotations[kube.AnnotationData]), &lock); err != nil {
		return nil, fmt.Errorf("failed to deserialize command lock: %w", err)
	}
	return &lock, nil
}

// ---- pull status ----

func (k *KubeDB) pullObjectKey(pull models.PullRequest) client.ObjectKey {
	key := kube.PullKey(pull.BaseRepo.VCSHost.Hostname, pull.BaseRepo.FullName, pull.Num)
	return client.ObjectKey{Namespace: k.ns, Name: kube.Name(pullPrefix, key)}
}

// getPull returns the stored object, or nil if there is none.
func (k *KubeDB) getPull(ctx context.Context, pull models.PullRequest) (*v1alpha1.PullStatus, error) {
	var obj v1alpha1.PullStatus
	err := k.client().Get(ctx, k.pullObjectKey(pull), &obj)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &obj, nil
}

// writePull creates or updates the object; obj is nil when none exists yet.
// A lost create race is reported as a conflict so callers retry.
// plannedBy is the replica to record as the plan holder; empty keeps the
// current value.
func (k *KubeDB) writePull(ctx context.Context, obj *v1alpha1.PullStatus, pull models.PullRequest, status models.PullStatus, plannedBy string) error {
	spec := pullStatusToSpec(status)
	spec.PlannedBy = plannedBy
	if obj != nil {
		if plannedBy == "" {
			spec.PlannedBy = obj.Spec.PlannedBy
		}
		obj.Spec = spec
		return k.client().Update(ctx, obj)
	}
	key := k.pullObjectKey(pull)
	obj = &v1alpha1.PullStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: kube.Labels("pull-status", map[string]string{
				kube.LabelRepo: kube.Hash(pull.BaseRepo.FullName),
				kube.LabelPull: strconv.Itoa(pull.Num),
			}),
			Annotations: map[string]string{
				kube.AnnotationKey: kube.PullKey(pull.BaseRepo.VCSHost.Hostname, pull.BaseRepo.FullName, pull.Num),
			},
		},
		Spec: spec,
	}
	err := k.client().Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("pullstatuses").GroupResource(), key.Name, err)
	}
	return err
}

// UpdatePullWithResults implements db.Database.
func (k *KubeDB) UpdatePullWithResults(pull models.PullRequest, newResults []command.ProjectResult) (models.PullStatus, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	var newStatus models.PullStatus
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		obj, err := k.getPull(ctx, pull)
		if err != nil {
			return err
		}
		var curr *models.PullStatus
		if obj != nil {
			// An unreadable status (e.g. from an older schema) is discarded, not fatal.
			if s, err := specToPullStatus(obj.Spec); err == nil {
				curr = &s
			}
		}
		newStatus = db.MergePullResults(curr, pull, newResults)
		plannedBy := ""
		for _, r := range newResults {
			if r.Command == command.Plan {
				plannedBy = k.identity
			}
		}
		return k.writePull(ctx, obj, pull, newStatus, plannedBy)
	})
	if err != nil {
		return models.PullStatus{}, fmt.Errorf("DB transaction failed: %w", err)
	}
	return newStatus, nil
}

// GetPullStatus implements db.Database.
func (k *KubeDB) GetPullStatus(pull models.PullRequest) (*models.PullStatus, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	obj, err := k.getPull(ctx, pull)
	if err != nil {
		return nil, fmt.Errorf("DB transaction failed: %w", err)
	}
	if obj == nil {
		return nil, nil
	}
	s, err := specToPullStatus(obj.Spec)
	if err != nil {
		return nil, fmt.Errorf("deserializing pull %s: %w", obj.Name, err)
	}
	return &s, nil
}

// DeletePullStatus implements db.Database.
func (k *KubeDB) DeletePullStatus(pull models.PullRequest) error {
	ctx, cancel := k.ctx()
	defer cancel()
	key := k.pullObjectKey(pull)
	obj := &v1alpha1.PullStatus{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if err := k.client().Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("DB transaction failed: %w", err)
	}
	return nil
}

// UpdateProjectStatus implements db.Database.
func (k *KubeDB) UpdateProjectStatus(pull models.PullRequest, workspace string, repoRelDir string, projectName string, newStatus models.ProjectPlanStatus) error {
	ctx, cancel := k.ctx()
	defer cancel()
	cleanDir := filepath.Clean(repoRelDir)
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		obj, err := k.getPull(ctx, pull)
		if err != nil {
			return err
		}
		if obj == nil {
			return db.ErrProjectNotFound
		}
		curr, err := specToPullStatus(obj.Spec)
		if err != nil {
			return err
		}
		// Several named projects can share a directory and workspace, so the
		// name is part of the match: marking the wrong one discarded would
		// leave the right one applicable.
		found := false
		for i := range curr.Projects {
			p := &curr.Projects[i]
			if p.Workspace == workspace && filepath.Clean(p.RepoRelDir) == cleanDir && p.ProjectName == projectName {
				p.Status = newStatus
				found = true
			}
		}
		if !found {
			return db.ErrProjectNotFound
		}
		return k.writePull(ctx, obj, pull, curr, "")
	})
	if err != nil {
		return fmt.Errorf("DB transaction failed: %w", err)
	}
	return nil
}

// PullStatusExists reports whether any VCS host has a PullStatus for the
// repo's pull. Local files only know the repo name, not the VCS host.
func (k *KubeDB) PullStatusExists(repoFullName string, pullNum int) (bool, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	var list v1alpha1.PullStatusList
	err := k.client().List(ctx, &list, client.InNamespace(k.ns), client.MatchingLabels{
		kube.LabelRepo: kube.Hash(repoFullName),
		kube.LabelPull: strconv.Itoa(pullNum),
	})
	if err != nil {
		return false, fmt.Errorf("listing pull statuses: %w", err)
	}
	for _, item := range list.Items {
		if item.Spec.Pull.BaseRepo.FullName == repoFullName && item.Spec.Pull.Num == pullNum {
			return true, nil
		}
	}
	return false, nil
}

// PlanHolder returns the replica that made the pull's latest plan, or "".
func (k *KubeDB) PlanHolder(repo models.Repo, pullNum int) (string, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	obj, err := k.getPull(ctx, models.PullRequest{Num: pullNum, BaseRepo: repo})
	if err != nil || obj == nil {
		return "", err
	}
	return obj.Spec.PlannedBy, nil
}

// ---- lifecycle ----

// Ping implements db.Database. It verifies connectivity and RBAC by listing
// at most one Lease.
func (k *KubeDB) Ping() error {
	ctx, cancel := k.ctx()
	defer cancel()
	var list coordinationv1.LeaseList
	return k.client().List(ctx, &list, client.InNamespace(k.ns), client.Limit(1))
}

// Close implements db.Database. Later operations fail with ErrClosed, as
// they would on a closed database connection.
func (k *KubeDB) Close() error {
	k.closed.Store(true)
	return nil
}

// ErrClosed is returned by operations on a closed KubeDB.
var ErrClosed = errors.New("kubedb: database is closed")

// apiClient is the part of the Kubernetes client KubeDB uses.
type apiClient interface {
	client.Reader
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
	Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
	Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error
}

func (k *KubeDB) client() apiClient {
	if k.closed.Load() {
		return closedClient{}
	}
	return k.c
}

// closedClient fails every call with ErrClosed.
type closedClient struct{}

func (closedClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return ErrClosed
}
func (closedClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return ErrClosed
}
func (closedClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return ErrClosed
}
func (closedClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return ErrClosed
}
func (closedClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return ErrClosed
}
