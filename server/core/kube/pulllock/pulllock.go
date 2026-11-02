// Package pulllock provides a WorkingDirLocker that is safe across replicas.
//
// Commands for a pull are normally routed to the pull's owning replica, so the
// in-process locker is enough. During a membership change (scale up/down, a
// rolling update) two replicas may briefly both believe they own a pull; this
// locker closes that window with a per-pull Lease. The Lease is re-entrant for
// the process holding it, so parallel project commands on one replica share
// it. It records the process's incarnation as well as the replica name, so a
// second process with the same name (a force-deleted pod still running, or a
// shared --kubernetes-identity) cannot share it.
//
// Fencing: a replica stops trusting its lease once it has gone Duration/2
// without a successful renewal, or as soon as another replica holds it. Other
// replicas only take the lease over after a full Duration, so the gap absorbs
// clock skew. Losing the lease calls OnLost (the server cancels the pull's
// queued work) and makes LostPullLock report true, which the project runner
// checks before starting every workflow step. A terraform process that is
// already running cannot be stopped this way; the state backend's own
// locking is the last line of defence for that window.
package pulllock

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/logging"
)

var _ events.WorkingDirLocker = (*Locker)(nil)

// Config configures a Locker.
type Config struct {
	Local     events.WorkingDirLocker
	Client    client.Client
	Namespace string
	Identity  string
	// Incarnation identifies this process; see kube.NewIncarnation. A
	// random one is used when empty, so another process with the same
	// Identity never shares a pull's lease.
	Incarnation string
	// Duration is how long the lease survives without renewal. Defaults to 30s.
	Duration time.Duration
	// Timeout bounds each API call. Defaults to 10s.
	Timeout time.Duration
	Logger  logging.SimpleLogging
	// OnLost is called once, in its own goroutine, when a held lease is lost.
	OnLost func(repoFullName string, pullNum int)
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

type held struct {
	refs   int
	lease  *cluster.TimedLease
	cancel context.CancelFunc
	done   chan struct{}

	repoFullName string
	pullNum      int
	// lastRenew is the UnixNano time of the last successful acquire/renew.
	lastRenew atomic.Int64
	lost      atomic.Bool
}

// Locker layers a per-pull Lease over a local WorkingDirLocker.
type Locker struct {
	cfg  Config
	mu   sync.Mutex
	held map[string]*held
	// releasing has an entry, closed when done, for each pull whose lease is
	// being deleted. acquire waits for it so the delete cannot remove a lease
	// that was just taken again.
	releasing map[string]chan struct{}
}

// New returns a Locker.
func New(cfg Config) *Locker {
	if cfg.Duration == 0 {
		cfg.Duration = 30 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Incarnation == "" {
		cfg.Incarnation = kube.NewIncarnation()
	}
	return &Locker{cfg: cfg, held: map[string]*held{}, releasing: map[string]chan struct{}{}}
}

// trustWindow is how long a lease is trusted after its last renewal.
func (l *Locker) trustWindow() time.Duration { return l.cfg.Duration / 2 }

// stale reports whether h can no longer be trusted.
func (l *Locker) stale(h *held) bool {
	return h.lost.Load() || l.cfg.Now().Sub(time.Unix(0, h.lastRenew.Load())) >= l.trustWindow()
}

func (l *Locker) markLost(h *held, reason error) {
	if !h.lost.CompareAndSwap(false, true) {
		return
	}
	l.cfg.Logger.Err("lost pull lock %s for %s#%d: %s; cancelling queued work for the pull", h.lease.Name, h.repoFullName, h.pullNum, reason)
	if l.cfg.OnLost != nil {
		go l.cfg.OnLost(h.repoFullName, h.pullNum)
	}
}

// Holds reports whether this replica currently holds a trusted lease for
// the pull, i.e. a command for it is running here.
func (l *Locker) Holds(repoFullName string, pullNum int) bool {
	l.mu.Lock()
	h, ok := l.held[pullKey(repoFullName, pullNum)]
	l.mu.Unlock()
	return ok && !l.stale(h)
}

// LostPullLock reports whether this replica held the pull's lease and can no
// longer trust it. It is false when the pull is not locked here at all.
func (l *Locker) LostPullLock(repoFullName string, pullNum int) bool {
	l.mu.Lock()
	h, ok := l.held[pullKey(repoFullName, pullNum)]
	l.mu.Unlock()
	return ok && l.stale(h)
}

func pullKey(repoFullName string, pullNum int) string {
	return repoFullName + "#" + strconv.Itoa(pullNum)
}

// acquire takes (or re-enters) the pull lease and returns its release func.
func (l *Locker) acquire(repoFullName string, pullNum int, cmdName command.Name) (func(), error) {
	key := pullKey(repoFullName, pullNum)
	l.mu.Lock()
	for {
		ch, ok := l.releasing[key]
		if !ok {
			break
		}
		l.mu.Unlock()
		<-ch
		l.mu.Lock()
	}
	defer l.mu.Unlock()

	if h, ok := l.held[key]; ok {
		if !l.stale(h) {
			h.refs++
			return l.releaseFunc(key, h), nil
		}
		// The lease was lost while commands still reference it. The entry
		// stays until they release it, so their fencing checks keep failing;
		// replacing it here would make LostPullLock report false for them.
		return func() {}, fmt.Errorf("cannot run %q: this Atlantis replica lost the lock for pull request %d while a previous command was still running.\n"+
			"Wait until that command stops and try again", cmdName, pullNum)
	}

	lease := &cluster.TimedLease{
		Client:      l.cfg.Client,
		Namespace:   l.cfg.Namespace,
		Name:        kube.Name("pulllock", key),
		Holder:      l.cfg.Identity,
		Incarnation: l.cfg.Incarnation,
		Duration:    l.cfg.Duration,
		Labels: kube.Labels(kube.TypePullLock, map[string]string{
			kube.LabelRepo: kube.Hash(repoFullName),
			kube.LabelPull: strconv.Itoa(pullNum),
		}),
		Annotations: map[string]string{kube.AnnotationKey: key, kube.AnnotationData: cmdName.String()},
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.cfg.Timeout)
	err := lease.Acquire(ctx)
	cancel()
	if errors.Is(err, cluster.ErrHeld) {
		return func() {}, fmt.Errorf("cannot run %q: pull request %d is currently running a command on another Atlantis replica (%s).\n"+
			"Wait until the previous command is complete and try again", cmdName, pullNum, err)
	}
	if err != nil {
		return func() {}, fmt.Errorf("acquiring pull lock: %w", err)
	}

	rctx, rcancel := context.WithCancel(context.Background())
	h := &held{refs: 1, lease: lease, cancel: rcancel, done: make(chan struct{}), repoFullName: repoFullName, pullNum: pullNum}
	h.lastRenew.Store(l.cfg.Now().UnixNano())
	l.held[key] = h
	go l.renew(rctx, h)
	return l.releaseFunc(key, h), nil
}

// renew renews the lease every Duration/6, so two consecutive failures still
// fit inside the trust window.
func (l *Locker) renew(ctx context.Context, h *held) {
	defer close(h.done)
	t := time.NewTicker(l.cfg.Duration / 6)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			timeout := min(l.cfg.Timeout, l.trustWindow()/2)
			actx, cancel := context.WithTimeout(ctx, timeout)
			err := h.lease.Acquire(actx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			switch {
			case err == nil:
				h.lastRenew.Store(l.cfg.Now().UnixNano())
			case errors.Is(err, cluster.ErrHeld):
				l.markLost(h, err)
				return
			default:
				l.cfg.Logger.Warn("renewing pull lock %s: %s", h.lease.Name, err)
				if l.stale(h) {
					l.markLost(h, fmt.Errorf("no successful renewal for %s: %w", l.trustWindow(), err))
					return
				}
			}
		}
	}
}

// releaseFunc returns a release func bound to one acquisition, so a release
// that arrives after UnlockByPull cannot drop a later acquisition's reference.
func (l *Locker) releaseFunc(key string, owner *held) func() {
	var once sync.Once
	return func() { once.Do(func() { l.release(key, owner) }) }
}

// release drops a reference to owner and deletes the lease when none remain.
func (l *Locker) release(key string, owner *held) {
	l.mu.Lock()
	h, ok := l.held[key]
	if !ok || h != owner {
		l.mu.Unlock()
		return
	}
	h.refs--
	if h.refs > 0 {
		l.mu.Unlock()
		return
	}
	delete(l.held, key)
	done := make(chan struct{})
	l.releasing[key] = done
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.releasing, key)
		l.mu.Unlock()
		close(done)
	}()

	h.cancel()
	<-h.done
	ctx, cancel := context.WithTimeout(context.Background(), l.cfg.Timeout)
	defer cancel()
	if err := h.lease.Release(ctx); err != nil {
		l.cfg.Logger.Warn("releasing pull lock %s: %s", h.lease.Name, err)
	}
}

// TryLockPull implements events.WorkingDirLocker.
func (l *Locker) TryLockPull(repoFullName string, pullNum int, cmdName command.Name, metadata events.WorkingDirLockMetadata) (func(), error) {
	unlockLocal, err := l.cfg.Local.TryLockPull(repoFullName, pullNum, cmdName, metadata)
	if err != nil {
		return unlockLocal, err
	}
	release, err := l.acquire(repoFullName, pullNum, cmdName)
	if err != nil {
		unlockLocal()
		return func() {}, err
	}
	return func() { unlockLocal(); release() }, nil
}

// TryLock implements events.WorkingDirLocker.
func (l *Locker) TryLock(repoFullName string, pullNum int, workspace string, path string, projectName string, cmdName command.Name, metadata events.WorkingDirLockMetadata) (func(), error) {
	unlockLocal, err := l.cfg.Local.TryLock(repoFullName, pullNum, workspace, path, projectName, cmdName, metadata)
	if err != nil {
		return unlockLocal, err
	}
	release, err := l.acquire(repoFullName, pullNum, cmdName)
	if err != nil {
		unlockLocal()
		return func() {}, err
	}
	return func() { unlockLocal(); release() }, nil
}

// HasCommandLock implements events.WorkingDirLocker. Commands for a pull run
// on its owner, so the local view is authoritative.
func (l *Locker) HasCommandLock(repoFullName string, pullNum int, cmdName command.Name) bool {
	return l.cfg.Local.HasCommandLock(repoFullName, pullNum, cmdName)
}

// UnlockByPull implements events.WorkingDirLocker. It clears the local locks
// but keeps the lease: commands still running here hold references to it and
// release it when they finish. Dropping it now would let another replica start
// on the pull while they run.
func (l *Locker) UnlockByPull(repoFullName string, pullNum int) {
	l.cfg.Local.UnlockByPull(repoFullName, pullNum)
}
