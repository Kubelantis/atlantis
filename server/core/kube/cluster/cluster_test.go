package cluster_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

func start(t *testing.T, c client.Client, ns, id string) (*cluster.Membership, context.CancelFunc) {
	m := cluster.NewMembership(cluster.MembershipConfig{
		Client: c, Namespace: ns, Identity: id, Address: "http://" + id + ":4142",
		LeaseDuration: 3 * time.Second, RenewInterval: 200 * time.Millisecond,
		Logger: logging.NewNoopLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return m, stop
}

func TestMembershipOwnershipAndDrain(t *testing.T) {
	c, ns := kubetest.Client(t)
	a, _ := start(t, c, ns, "atlantis-0")
	b, stopB := start(t, c, ns, "atlantis-1")
	eventually(t, 5*time.Second, func() bool { return len(a.Members()) == 2 && len(b.Members()) == 2 }, "members did not converge")

	// Both replicas agree on every owner, and both get some pulls.
	owned := map[string]int{}
	for i := range 200 {
		key := fmt.Sprintf("github.com::org/repo::%d", i)
		oa, ok := a.Owner(key)
		Assert(t, ok, "expected owner")
		ob, _ := b.Owner(key)
		Equals(t, oa.Identity, ob.Identity)
		owned[oa.Identity]++
	}
	Assert(t, owned["atlantis-0"] > 50 && owned["atlantis-1"] > 50, "unbalanced ownership: %v", owned)

	// A draining member stays listed but owns nothing.
	b.SetDraining(context.Background())
	eventually(t, 5*time.Second, func() bool {
		for _, m := range a.Members() {
			if m.Identity == "atlantis-1" && m.Draining {
				return true
			}
		}
		return false
	}, "drain not observed")
	for i := range 50 {
		o, _ := a.Owner(fmt.Sprintf("k%d", i))
		Equals(t, "atlantis-0", o.Identity)
	}

	// Stopping releases the lease immediately rather than waiting for expiry.
	stopB()
	eventually(t, 2*time.Second, func() bool { return len(a.Members()) == 1 }, "stopped member still listed")
}

// Replicas on earlier versions read a member's name from holderIdentity
// alone. During a rolling upgrade they must see an upgraded member under its
// replica name, the name PullStatus.PlannedBy and rendezvous ownership use.
func TestMemberLeaseIsReadableByEarlierVersions(t *testing.T) {
	c, ns := kubetest.Client(t)
	m, _ := start(t, c, ns, "atlantis-0")
	eventually(t, 5*time.Second, func() bool { return len(m.Members()) == 1 }, "replica did not join")

	var leases coordinationv1.LeaseList
	Ok(t, c.List(context.Background(), &leases, client.InNamespace(ns), client.MatchingLabels{kube.LabelType: kube.TypeMember}))
	Equals(t, 1, len(leases.Items))
	Equals(t, "atlantis-0", cluster.Holder(&leases.Items[0]))
}

// A second process with a name already in use is not a member and reports
// the conflict; it becomes the member once the first one leaves.
func TestMembershipDetectsDuplicateIdentity(t *testing.T) {
	c, ns := kubetest.Client(t)
	first, stopFirst := start(t, c, ns, "atlantis-0")
	eventually(t, 5*time.Second, func() bool { return len(first.Members()) == 1 }, "first replica did not join")

	second, _ := start(t, c, ns, "atlantis-0")
	eventually(t, 5*time.Second, second.IdentityConflict, "second process did not report the duplicate identity")
	Assert(t, !first.IdentityConflict(), "the holder must not report a conflict")
	Equals(t, 1, len(first.Members()))

	stopFirst() // releases the member lease
	eventually(t, 5*time.Second, func() bool { return !second.IdentityConflict() }, "second process did not take over after the first left")
	Equals(t, "http://atlantis-0:4142", second.Members()[0].Address)
}

func TestRendezvousOnlyMovesDepartedMembersKeys(t *testing.T) {
	c, ns := kubetest.Client(t)
	ms := []*cluster.Membership{}
	stops := []context.CancelFunc{}
	for i := range 3 {
		m, stop := start(t, c, ns, fmt.Sprintf("atlantis-%d", i))
		ms, stops = append(ms, m), append(stops, stop)
	}
	eventually(t, 5*time.Second, func() bool { return len(ms[0].Members()) == 3 }, "members did not converge")
	before := map[string]string{}
	for i := range 300 {
		k := fmt.Sprint(i)
		o, _ := ms[0].Owner(k)
		before[k] = o.Identity
	}
	stops[2]()
	eventually(t, 5*time.Second, func() bool { return len(ms[0].Members()) == 2 }, "member did not leave")
	for k, prev := range before {
		o, _ := ms[0].Owner(k)
		if prev != "atlantis-2" {
			Equals(t, prev, o.Identity)
		}
	}
}

func TestLeaderElectionFailover(t *testing.T) {
	_, ns := kubetest.Client(t)
	var leaders [2]*cluster.Leader
	var cancels [2]context.CancelFunc
	var runs atomic.Int32
	for i := range 2 {
		leaders[i] = cluster.NewLeader(cluster.LeaderConfig{
			RestConfig: kubetest.RestConfig(), Namespace: ns, Identity: fmt.Sprintf("atlantis-%d", i),
			LeaseDuration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: 200 * time.Millisecond,
			Logger: logging.NewNoopLogger(t),
			Run:    func(ctx context.Context) { runs.Add(1); <-ctx.Done() },
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		t.Cleanup(cancel)
		go func() { _ = leaders[i].Run(ctx) }()
	}
	leaderIdx := func() int {
		n, idx := 0, -1
		for i, l := range leaders {
			if l.IsLeader() {
				n++
				idx = i
			}
		}
		if n > 1 {
			t.Fatal("two leaders at once")
		}
		return idx
	}
	eventually(t, 10*time.Second, func() bool { return leaderIdx() >= 0 }, "no leader elected")
	first := leaderIdx()
	cancels[first]()
	eventually(t, 10*time.Second, func() bool { return leaderIdx() == 1-first }, "no failover")
	Assert(t, runs.Load() >= 2, "leader task did not run on both")
}

func TestHousekeeperDeletesOnlyStaleLeases(t *testing.T) {
	c, ns := kubetest.Client(t)
	ctx := context.Background()
	mk := func(name string, renewed time.Time) {
		l := &cluster.TimedLease{Client: c, Namespace: ns, Name: name, Holder: "x", Duration: time.Second,
			Labels: kube.Labels(kube.TypePullLock, nil), Now: func() time.Time { return renewed }}
		Ok(t, l.Acquire(ctx))
	}
	mk("stale", time.Now().Add(-time.Hour))
	mk("fresh", time.Now())
	h := &cluster.Housekeeper{Client: c, Namespace: ns, Logger: logging.NewNoopLogger(t), Interval: time.Hour, Grace: time.Minute}
	hctx, cancel := context.WithCancel(ctx)
	go h.Run(hctx)
	eventually(t, 5*time.Second, func() bool {
		var list coordinationv1.LeaseList
		Ok(t, c.List(ctx, &list, client.InNamespace(ns)))
		return len(list.Items) == 1 && list.Items[0].Name == "fresh"
	}, "stale lease not collected")
	cancel()
}
