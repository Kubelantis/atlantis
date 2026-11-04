package server

import (
	"context"
	"testing"
	"time"

	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// A process whose replica identity is held by another live process must stay
// out of the Service.
func TestKubeRuntimeNotReadyWhileIdentityIsHeldElsewhere(t *testing.T) {
	c, ns := kubetest.Client(t)
	run := func() *kubeRuntime {
		m := cluster.NewMembership(cluster.MembershipConfig{
			Client: c, Namespace: ns, Identity: "atlantis-0", Address: "http://atlantis-0:4142",
			LeaseDuration: 3 * time.Second, RenewInterval: 200 * time.Millisecond,
			Logger: logging.NewNoopLogger(t),
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { m.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		k := &kubeRuntime{membership: m}
		k.started.Store(true)
		return k
	}
	first := run()
	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatal(msg)
	}
	waitFor(func() bool { return len(first.membership.Members()) == 1 }, "first process did not join")

	second := run()
	waitFor(second.membership.IdentityConflict, "second process did not see the conflict")
	ok, reason := second.ready(false)
	Assert(t, !ok, "second process must not be ready")
	Equals(t, "another process holds this replica's identity", reason)

	ok, _ = first.ready(false)
	Assert(t, ok, "the process holding the identity must stay ready")
}
