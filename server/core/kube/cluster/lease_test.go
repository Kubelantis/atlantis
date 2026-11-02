package cluster_test

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
	. "github.com/runatlantis/atlantis/testing"
)

func leaseFor(c client.Client, ns, incarnation string) *cluster.TimedLease {
	return &cluster.TimedLease{
		Client: c, Namespace: ns, Name: "test-lease",
		Holder: "atlantis-0", Incarnation: incarnation, Duration: 3 * time.Second,
	}
}

func leaseExists(t *testing.T, c client.Client, ns string) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "test-lease"}, &coordinationv1.Lease{})
	if apierrors.IsNotFound(err) {
		return false
	}
	Ok(t, err)
	return true
}

// Two processes with the same replica name (a force-deleted pod still
// running, or one --kubernetes-identity on every replica) must not share a
// lease.
func TestLeaseIsNotSharedByTwoIncarnationsOfOneName(t *testing.T) {
	c, ns := kubetest.Client(t)
	ctx := context.Background()
	first, second := leaseFor(c, ns, "one"), leaseFor(c, ns, "two")

	Ok(t, first.Acquire(ctx))
	err := second.Acquire(ctx)
	Assert(t, errors.Is(err, cluster.ErrHeld), "second incarnation acquired a held lease: %v", err)
	Assert(t, errors.Is(err, cluster.ErrIdentityInUse), "error does not say the identity is in use: %v", err)
	Ok(t, first.Acquire(ctx)) // the holder still renews

	// A release by the other incarnation leaves the lease alone.
	Ok(t, second.Release(ctx))
	Assert(t, leaseExists(t, c, ns), "lease was deleted by an incarnation that does not hold it")
	Ok(t, first.Release(ctx))
	Assert(t, !leaseExists(t, c, ns), "holder's release did not delete the lease")
}

// A restarted process takes over its predecessor's lease once it expires,
// and the predecessor, if still running, then sees it as held.
func TestLeaseTakenOverByNewIncarnationAfterExpiry(t *testing.T) {
	c, ns := kubetest.Client(t)
	ctx := context.Background()
	old, restarted := leaseFor(c, ns, "one"), leaseFor(c, ns, "two")
	Ok(t, old.Acquire(ctx))

	restarted.Now = func() time.Time { return time.Now().Add(10 * time.Second) }
	Ok(t, restarted.Acquire(ctx))
	err := old.Acquire(ctx)
	Assert(t, errors.Is(err, cluster.ErrHeld), "old incarnation renewed a lease taken over by its successor: %v", err)
}

// A lease written by a version without incarnations belongs to an unknown
// process: it is held until it expires.
func TestLeaseWithoutIncarnationIsHeldUntilExpired(t *testing.T) {
	c, ns := kubetest.Client(t)
	ctx := context.Background()
	legacy, current := leaseFor(c, ns, ""), leaseFor(c, ns, "two")
	Ok(t, legacy.Acquire(ctx))

	err := current.Acquire(ctx)
	Assert(t, errors.Is(err, cluster.ErrHeld), "lease without an incarnation was treated as ours: %v", err)
	current.Now = func() time.Time { return time.Now().Add(10 * time.Second) }
	Ok(t, current.Acquire(ctx))
}

// An earlier version compares holderIdentity alone and keeps annotations it
// does not know, so the incarnation must be in holderIdentity: then a live
// older-version process with the same name sees another holder and can
// neither renew nor delete the lease of a newer process.
func TestLeaseOfNewProcessIsSafeFromOlderVersion(t *testing.T) {
	c, ns := kubetest.Client(t)
	ctx := context.Background()
	older := leaseFor(c, ns, "") // behaves as earlier versions do
	newer := leaseFor(c, ns, "two")
	Ok(t, older.Acquire(ctx))

	// The older process stops renewing (partition); the newer one takes over.
	newer.Now = func() time.Time { return time.Now().Add(10 * time.Second) }
	Ok(t, newer.Acquire(ctx))
	var lease coordinationv1.Lease
	Ok(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "test-lease"}, &lease))
	Equals(t, "atlantis-0/two", cluster.Holder(&lease))
	Equals(t, "atlantis-0", cluster.Replica(&lease))

	// The older process comes back: it is refused, and its release leaves the
	// newer process's lease alone.
	err := older.Acquire(ctx)
	Assert(t, errors.Is(err, cluster.ErrHeld), "older version renewed the newer process's lease: %v", err)
	Ok(t, older.Release(ctx))
	Assert(t, leaseExists(t, c, ns), "older version deleted the newer process's lease")
	Ok(t, newer.Acquire(ctx))
}
