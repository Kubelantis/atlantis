// Package cluster coordinates Atlantis replicas on Kubernetes:
//
//   - Membership: every replica holds a member Lease that it renews. The set
//     of live members is refreshed on the same interval.
//   - Ownership: each pull request is owned by exactly one live, non-draining
//     member, chosen by rendezvous hashing. Commands for a pull are routed to
//     its owner so clones, live job output, and cancellation stay on one pod.
//     When membership changes only the pulls of the affected member move.
//   - Leader election: one replica runs cluster-wide housekeeping.
package cluster

import (
	"context"
	"errors"
	"hash/fnv"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/logging"
)

const (
	memberPrefix       = "member"
	annotationDraining = "atlantis.runatlantis.io/draining"
)

// Member is a live Atlantis replica.
type Member struct {
	Identity string
	// Address is the base URL of the member's internal cluster listener.
	Address  string
	Draining bool
}

// MembershipConfig configures Membership.
type MembershipConfig struct {
	Client    client.Client
	Namespace string
	Identity  string
	// Incarnation identifies this process; see kube.NewIncarnation. A
	// random one is used when empty.
	Incarnation string
	// Address is this replica's internal base URL, e.g. http://10.0.0.5:4142.
	Address string
	// LeaseDuration is how long a member stays live without renewing.
	LeaseDuration time.Duration
	// RenewInterval is how often the lease is renewed and members refreshed.
	RenewInterval time.Duration
	Logger        logging.SimpleLogging
	// OnChange is called with the new member list whenever it changes.
	OnChange func([]Member)
	// OnSync is called after every renewal with its error, or nil. It lets
	// callers track API server health without probing it separately.
	OnSync func(error)
}

// Membership maintains this replica's member Lease and the live member set.
type Membership struct {
	cfg      MembershipConfig
	lease    *TimedLease
	draining atomic.Bool
	// tickMu serializes renewals from Run and SetDraining.
	tickMu sync.Mutex

	// conflictSince is when renewals started failing because another process
	// with this identity holds the member lease (UnixNano); 0 when there is
	// no conflict.
	conflictSince atomic.Int64

	mu      sync.RWMutex
	members []Member
	// synced is true once the member list has been loaded at least once.
	synced bool
}

// NewMembership returns a Membership. Call Run to start it.
func NewMembership(cfg MembershipConfig) *Membership {
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewInterval == 0 {
		cfg.RenewInterval = cfg.LeaseDuration / 3
	}
	if cfg.Incarnation == "" {
		cfg.Incarnation = kube.NewIncarnation()
	}
	m := &Membership{cfg: cfg}
	m.lease = &TimedLease{
		Client:      cfg.Client,
		Namespace:   cfg.Namespace,
		Name:        kube.Name(memberPrefix, cfg.Identity),
		Holder:      cfg.Identity,
		Incarnation: cfg.Incarnation,
		// Earlier versions read a member's name from holderIdentity.
		HolderIsReplica: true,
		Duration:        cfg.LeaseDuration,
		Labels:          kube.Labels(kube.TypeMember, nil),
	}
	return m
}

// Identity returns this replica's identity.
func (m *Membership) Identity() string { return m.cfg.Identity }

// Run renews the member lease and refreshes members until ctx is done, then
// releases the lease so other replicas take over this pod's pulls at once.
func (m *Membership) Run(ctx context.Context) {
	m.tick(ctx)
	t := time.NewTicker(m.cfg.RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.lease.Release(rctx); err != nil {
				m.cfg.Logger.Warn("releasing member lease: %s", err)
			}
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Membership) tick(ctx context.Context) {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.cfg.RenewInterval)
	defer cancel()
	m.lease.Annotations = map[string]string{
		kube.AnnotationAddress: m.cfg.Address,
		annotationDraining:     strconv.FormatBool(m.draining.Load()),
	}
	renewErr := m.lease.Acquire(ctx)
	m.trackConflict(renewErr)
	if renewErr != nil && !errors.Is(renewErr, ErrHeld) {
		m.cfg.Logger.Warn("renewing member lease: %s", renewErr)
	}
	refreshErr := m.refresh(ctx)
	if refreshErr != nil {
		m.cfg.Logger.Warn("listing cluster members: %s", refreshErr)
	}
	if m.cfg.OnSync != nil {
		// A lease held by another process means the API answered; see
		// IdentityConflict for that.
		if errors.Is(renewErr, ErrHeld) {
			renewErr = nil
		}
		m.cfg.OnSync(errors.Join(renewErr, refreshErr))
	}
}

func (m *Membership) refresh(ctx context.Context) error {
	var list coordinationv1.LeaseList
	if err := m.cfg.Client.List(ctx, &list, client.InNamespace(m.cfg.Namespace), client.MatchingLabels(kube.Labels(kube.TypeMember, nil))); err != nil {
		return err
	}
	now := time.Now()
	var members []Member
	for i := range list.Items {
		l := &list.Items[i]
		if Expired(l, now) {
			continue
		}
		draining, _ := strconv.ParseBool(l.Annotations[annotationDraining])
		members = append(members, Member{Identity: Replica(l), Address: l.Annotations[kube.AnnotationAddress], Draining: draining})
	}
	slices.SortFunc(members, func(a, b Member) int {
		if a.Identity < b.Identity {
			return -1
		}
		if a.Identity > b.Identity {
			return 1
		}
		return 0
	})

	m.mu.Lock()
	changed := !m.synced || !slices.Equal(m.members, members)
	m.members, m.synced = members, true
	m.mu.Unlock()
	if changed && m.cfg.OnChange != nil {
		m.cfg.OnChange(members)
	}
	return nil
}

// trackConflict records whether the member lease is held by another process
// with this identity. Right after a crash that is this replica's previous
// run, whose lease has not expired yet; once it has lasted longer than a
// lease, the other process is alive and renewing it.
//
// The member lease is named by this identity, so any other holder, including
// the winner of a race to create it, is a process with the same name. Once
// detected, the conflict lasts until this process holds the lease: an API
// error says nothing about who holds it, and clearing the conflict on one
// would let a duplicate become ready and run another process's commands.
func (m *Membership) trackConflict(renewErr error) {
	switch {
	case renewErr == nil:
		m.conflictSince.Store(0)
		return
	case !errors.Is(renewErr, ErrHeld):
		return
	}
	now := time.Now()
	since := m.conflictSince.Load()
	if since == 0 {
		m.conflictSince.Store(now.UnixNano())
		since = now.UnixNano()
	}
	if now.Sub(time.Unix(0, since)) > m.cfg.LeaseDuration {
		m.cfg.Logger.Err("another Atlantis process is running as replica %q and holds its member lease; this process stays not ready. Give every replica a unique --kubernetes-identity, and make sure a force-deleted pod is no longer running", m.cfg.Identity)
		return
	}
	m.cfg.Logger.Warn("waiting for the member lease of replica %q held by a previous process to expire", m.cfg.Identity)
}

// IdentityConflict reports whether another process with this replica's
// identity holds its member lease. Such a replica is not a member and must
// not receive traffic.
func (m *Membership) IdentityConflict() bool { return m.conflictSince.Load() != 0 }

// SetDraining marks this replica as draining: it stays a member (its running
// jobs remain reachable) but stops owning pulls. It is published immediately.
func (m *Membership) SetDraining(ctx context.Context) {
	m.draining.Store(true)
	m.tick(ctx)
}

// Members returns the live members, sorted by identity.
func (m *Membership) Members() []Member {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.members)
}

// Owner returns the member that owns key. ok is false when no eligible member
// is known (e.g. the API server is unreachable at startup); callers should
// then handle the work locally.
func (m *Membership) Owner(key string) (owner Member, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return rendezvous(m.members, key)
}

// rendezvous implements highest-random-weight hashing over non-draining members.
func rendezvous(members []Member, key string) (Member, bool) {
	var best Member
	var bestScore uint64
	found := false
	for _, mem := range members {
		if mem.Draining {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(mem.Identity))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(key))
		if s := h.Sum64(); !found || s > bestScore {
			best, bestScore, found = mem, s, true
		}
	}
	return best, found
}
