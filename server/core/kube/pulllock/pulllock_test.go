package pulllock_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
	"github.com/runatlantis/atlantis/server/core/kube/pulllock"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

func replicas(t *testing.T, n int) []*pulllock.Locker {
	c, ns := kubetest.Client(t)
	var out []*pulllock.Locker
	for i := range n {
		out = append(out, pulllock.New(pulllock.Config{
			Local:     events.NewDefaultWorkingDirLocker(),
			Client:    c,
			Namespace: ns,
			Identity:  string(rune('a' + i)),
			Duration:  3 * time.Second,
			Logger:    logging.NewNoopLogger(t),
		}))
	}
	return out
}

func TestPullLockIsExclusiveAcrossReplicas(t *testing.T) {
	r := replicas(t, 2)
	unlock, err := r[0].TryLockPull("org/repo", 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)

	_, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "second replica must not lock the same pull")
	ErrContains(t, "another Atlantis replica", err)

	// A different pull is independent.
	unlock2, err := r[1].TryLockPull("org/repo", 2, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlock2()

	unlock()
	unlock, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlock()
}

func TestPullLockIsReentrantWithinReplica(t *testing.T) {
	r := replicas(t, 2)
	var unlocks []func()
	for _, dir := range []string{"a", "b", "c"} {
		u, err := r[0].TryLock("org/repo", 1, "default", dir, "", command.Plan, events.WorkingDirLockMetadata{})
		Ok(t, err)
		unlocks = append(unlocks, u)
	}
	unlocks[0]()
	unlocks[1]()
	_, err := r[1].TryLock("org/repo", 1, "default", "a", "", command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "lease must be held while any project lock is held")
	unlocks[2]()
	u, err := r[1].TryLock("org/repo", 1, "default", "a", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	u()
}

func TestPullLockSurvivesPastDurationWhileRenewed(t *testing.T) {
	r := replicas(t, 2)
	unlock, err := r[0].TryLockPull("org/repo", 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	time.Sleep(5 * time.Second)
	_, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "renewed lease must not expire")
	unlock()
}

func TestStaleReleaseDoesNotDropNewAcquisition(t *testing.T) {
	r := replicas(t, 2)
	stale, err := r[0].TryLock("org/repo", 1, "default", "a", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	r[0].UnlockByPull("org/repo", 1)
	fresh, err := r[0].TryLock("org/repo", 1, "default", "b", "", command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	stale()
	_, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "fresh acquisition must still hold the lease")
	fresh()
}

func TestConcurrentReplicasOneWinner(t *testing.T) {
	r := replicas(t, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for _, l := range r {
		wg.Go(func() {
			if _, err := l.TryLockPull("org/repo", 9, command.Apply, events.WorkingDirLockMetadata{}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	Equals(t, 1, wins)
}

func TestLosingLeaseToAnotherReplicaFencesAndNotifies(t *testing.T) {
	c, ns := kubetest.Client(t)
	lost := make(chan string, 1)
	a := pulllock.New(pulllock.Config{
		Local: events.NewDefaultWorkingDirLocker(), Client: c, Namespace: ns, Identity: "a",
		Duration: 3 * time.Second, Logger: logging.NewNoopLogger(t),
		OnLost: func(repo string, pull int) { lost <- fmt.Sprintf("%s#%d", repo, pull) },
	})
	unlock, err := a.TryLockPull("org/repo", 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()
	Assert(t, !a.LostPullLock("org/repo", 1), "fresh lease must be trusted")

	// Another replica takes the lease over (as it would after A stopped
	// renewing); A must notice on its next renewal.
	var leases coordinationv1.LeaseList
	Ok(t, c.List(context.Background(), &leases, client.InNamespace(ns)))
	Equals(t, 1, len(leases.Items))
	other := "b"
	now := metav1.NewMicroTime(time.Now())
	leases.Items[0].Spec.HolderIdentity, leases.Items[0].Spec.RenewTime = &other, &now
	Ok(t, c.Update(context.Background(), &leases.Items[0]))

	select {
	case got := <-lost:
		Equals(t, "org/repo#1", got)
	case <-time.After(5 * time.Second):
		t.Fatal("OnLost was not called")
	}
	Assert(t, a.LostPullLock("org/repo", 1), "lost lease must fail the fencing check")

	// A cannot start a new command on the pull while the command that lost
	// the lease is still running, and that command stays fenced.
	_, err = a.TryLock("org/repo", 1, "default", "other", "", command.Plan, events.WorkingDirLockMetadata{})
	ErrContains(t, "lost the lock for pull request 1", err)
	Assert(t, a.LostPullLock("org/repo", 1), "the running command must stay fenced after a new command tried to start")
}

func TestUnlockByPullKeepsLeaseWhileCommandsRun(t *testing.T) {
	r := replicas(t, 2)
	running, err := r[0].TryLock("org/repo", 1, "default", "a", "", command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)

	// `atlantis cancel` clears local locks; the running apply keeps going, so
	// another replica must not get the pull.
	r[0].UnlockByPull("org/repo", 1)
	_, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	ErrContains(t, "another Atlantis replica", err)
	Assert(t, r[0].Holds("org/repo", 1), "replica running the apply must still hold the lease")

	running()
	unlock, err := r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlock()
}

// Two processes with the same replica name must not both hold a pull: a
// pull lock re-entrant by name would let both run terraform on it.
func TestPullLockIsExclusiveBetweenProcessesWithOneName(t *testing.T) {
	c, ns := kubetest.Client(t)
	locker := func() *pulllock.Locker {
		return pulllock.New(pulllock.Config{
			Local: events.NewDefaultWorkingDirLocker(), Client: c, Namespace: ns, Identity: "atlantis-0",
			Duration: 3 * time.Second, Logger: logging.NewNoopLogger(t),
		})
	}
	old, restarted := locker(), locker()

	unlock, err := old.TryLockPull("org/repo", 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	_, err = restarted.TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Assert(t, err != nil, "a second process with the same name locked a held pull")

	unlock()
	unlock, err = restarted.TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
	Ok(t, err)
	unlock()
}

// grantingLocker is a local WorkingDirLocker that always grants, so a test
// can exercise the lease alone.
type grantingLocker struct{}

func (grantingLocker) TryLockPull(string, int, command.Name, events.WorkingDirLockMetadata) (func(), error) {
	return func() {}, nil
}

func (grantingLocker) TryLock(string, int, string, string, string, command.Name, events.WorkingDirLockMetadata) (func(), error) {
	return func() {}, nil
}

func (grantingLocker) HasCommandLock(string, int, command.Name) bool { return false }

func (grantingLocker) UnlockByPull(string, int) {}

// A release that deletes the lease must not delete a lease the same replica
// takes again meanwhile. The second acquisition may run before, during or
// after the first one's release; in every order the lease must stay held.
// The local locker always grants: with the real one, a second acquisition
// that runs before the release is refused locally, which is correct but not
// what this test is about.
func TestReleaseThenReacquireKeepsLease(t *testing.T) {
	c, ns := kubetest.Client(t)
	var r []*pulllock.Locker
	for _, id := range []string{"a", "b"} {
		r = append(r, pulllock.New(pulllock.Config{
			Local: grantingLocker{}, Client: c, Namespace: ns, Identity: id,
			Duration: 3 * time.Second, Logger: logging.NewNoopLogger(t),
		}))
	}
	for i := range 20 {
		first, err := r[0].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
		Ok(t, err)
		done := make(chan func())
		go func() {
			second, err := r[0].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
			if err != nil {
				t.Errorf("iteration %d: reacquire: %s", i, err)
				second = func() {}
			}
			done <- second
		}()
		first()
		second := <-done
		// The second acquisition holds the pull; the other replica must not.
		_, err = r[1].TryLockPull("org/repo", 1, command.Plan, events.WorkingDirLockMetadata{})
		Assert(t, err != nil, "iteration %d: lease was deleted under a live acquisition", i)
		second()
	}
}

func TestUnrenewedLeaseStopsBeingTrusted(t *testing.T) {
	c, ns := kubetest.Client(t)
	var offset atomic.Int64
	a := pulllock.New(pulllock.Config{
		Local: events.NewDefaultWorkingDirLocker(), Client: c, Namespace: ns, Identity: "a",
		Duration: 30 * time.Second, Logger: logging.NewNoopLogger(t),
		Now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) },
	})
	unlock, err := a.TryLockPull("org/repo", 1, command.Apply, events.WorkingDirLockMetadata{})
	Ok(t, err)
	defer unlock()
	Assert(t, !a.LostPullLock("org/repo", 1), "fresh lease must be trusted")

	// Half the lease duration without a renewal: other replicas may soon take
	// it over, so this replica must stop starting steps before they can.
	offset.Store(int64(16 * time.Second))
	Assert(t, a.LostPullLock("org/repo", 1), "stale lease must fail the fencing check")
	Assert(t, !a.LostPullLock("org/repo", 2), "pulls not locked here are not fenced")
}
