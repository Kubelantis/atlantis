package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube"
)

// ErrHeld is returned when a lease is held by another live holder.
var ErrHeld = errors.New("lease is held by another holder")

// ErrIdentityInUse is returned, along with ErrHeld, when the other holder is
// another process with the same holder name.
var ErrIdentityInUse = errors.New("another process with the same identity holds it")

// TimedLease is a small helper around a coordination.k8s.io Lease with
// holder/expiry semantics, used for membership and pull locks. Leader
// election uses client-go's implementation instead.
type TimedLease struct {
	Client    client.Client
	Namespace string
	Name      string
	// Holder is the stable name of the holder (the replica name).
	Holder string
	// Incarnation identifies this run of the holder's process. When set, the
	// lease's holderIdentity is Holder/Incarnation, so another process with
	// the same name, including one running an earlier version that compares
	// holderIdentity alone, sees another holder: it can neither renew nor
	// delete the lease until it expires. Holder is also recorded in the
	// AnnotationReplica annotation; see Replica.
	Incarnation string
	// HolderIsReplica keeps holderIdentity as Holder alone and records the
	// incarnation in the AnnotationIncarnation annotation instead. Membership
	// leases need it: earlier versions read a member's replica name from
	// holderIdentity, and a mixed-version cluster during a rolling upgrade
	// must agree on member names for routing and plan-holder lookup. Same-name
	// processes of this version still never share the lease; a live process
	// of an earlier version with the same name is not fenced off, as before.
	// A later release can drop this once no version that reads holderIdentity
	// as the replica name remains in service.
	HolderIsReplica bool
	Duration        time.Duration
	Labels          map[string]string
	// Annotations are written on every acquire and renew.
	Annotations map[string]string
	Now         func() time.Time
}

func (l *TimedLease) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Expired reports whether a lease's holder has stopped renewing it.
func Expired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	d := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	return now.After(lease.Spec.RenewTime.Add(d))
}

// Holder returns the holder identity of a lease.
func Holder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// Replica returns the stable name of a lease's holder: the replica name for
// routing. Leases written by earlier versions hold it in holderIdentity.
func Replica(lease *coordinationv1.Lease) string {
	if r := lease.Annotations[kube.AnnotationReplica]; r != "" {
		return r
	}
	return Holder(lease)
}

// holderIdentity is the holderIdentity this holder writes.
func (l *TimedLease) holderIdentity() string {
	if l.Incarnation == "" || l.HolderIsReplica {
		return l.Holder
	}
	return l.Holder + "/" + l.Incarnation
}

// ours reports whether lease is held by this holder and incarnation.
func (l *TimedLease) ours(lease *coordinationv1.Lease) bool {
	if Holder(lease) != l.holderIdentity() {
		return false
	}
	if l.HolderIsReplica && l.Incarnation != "" {
		return lease.Annotations[kube.AnnotationIncarnation] == l.Incarnation
	}
	return true
}

// annotations returns the annotations to write: Annotations plus the
// replica name.
func (l *TimedLease) annotations() map[string]string {
	a := make(map[string]string, len(l.Annotations)+1)
	for k, v := range l.Annotations {
		a[k] = v
	}
	if l.Incarnation != "" {
		a[kube.AnnotationReplica] = l.Holder
		if l.HolderIsReplica {
			a[kube.AnnotationIncarnation] = l.Incarnation
		}
	}
	return a
}

// Acquire takes the lease if it is free, expired, or already ours, and
// renews it if ours. It returns ErrHeld (wrapped with the holder) otherwise.
func (l *TimedLease) Acquire(ctx context.Context) error {
	now := metav1.NewMicroTime(l.now())
	secs := int32(l.Duration / time.Second)
	holder := l.holderIdentity()
	annotations := l.annotations()

	var lease coordinationv1.Lease
	err := l.Client.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: l.Name}, &lease)
	if apierrors.IsNotFound(err) {
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: l.Name, Namespace: l.Namespace, Labels: l.Labels, Annotations: annotations},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &secs,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		err = l.Client.Create(ctx, &lease)
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%w: lost acquire race", ErrHeld)
		}
		return err
	}
	if err != nil {
		return err
	}

	ours := l.ours(&lease)
	if !ours && !Expired(&lease, now.Time) {
		if Replica(&lease) == l.Holder {
			return fmt.Errorf("%w: %w (%s)", ErrHeld, ErrIdentityInUse, Holder(&lease))
		}
		return fmt.Errorf("%w: %s", ErrHeld, Holder(&lease))
	}
	if !ours {
		lease.Spec.AcquireTime = &now
		transitions := int32(0)
		if lease.Spec.LeaseTransitions != nil {
			transitions = *lease.Spec.LeaseTransitions
		}
		transitions++
		lease.Spec.LeaseTransitions = &transitions
	}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = &secs
	lease.Spec.RenewTime = &now
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	for k, v := range l.Labels {
		lease.Labels[k] = v
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		lease.Annotations[k] = v
	}
	// Update carries the resourceVersion we read, so two replicas taking over
	// the same expired lease cannot both succeed.
	err = l.Client.Update(ctx, &lease)
	if apierrors.IsConflict(err) {
		return fmt.Errorf("%w: lost takeover race", ErrHeld)
	}
	return err
}

// Release deletes the lease if this holder and incarnation still hold it.
func (l *TimedLease) Release(ctx context.Context) error {
	var lease coordinationv1.Lease
	err := l.Client.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: l.Name}, &lease)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !l.ours(&lease) {
		return nil
	}
	uid, rv := lease.UID, lease.ResourceVersion
	err = l.Client.Delete(ctx, &lease, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}
