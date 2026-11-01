package clusterrpc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tally "github.com/uber-go/tally/v4"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/clusterrpc"
	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/core/kube/kubedb"
	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
	"github.com/runatlantis/atlantis/server/core/locking"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

// These tests run two replicas against one API server: each has its own
// KubeDB, locker, membership, router and internal listener, as in a
// StatefulSet. Only the command runner is a stand-in; it calls the real
// locking, unlock and apply validation code for the commands it receives.

// ran is one command a replica ran.
type ran struct {
	replica string
	name    command.Name
	pullNum int
	err     error
}

// replicaRunner runs commands on one replica.
type replicaRunner struct {
	id         string
	db         *kubedb.KubeDB
	locker     locking.Locker
	unlock     *events.DefaultDeleteLockCommand
	validator  *events.DefaultApplyPlanValidator
	logger     logging.SimpleLogging
	out        chan<- ran
	workingDir *recordingWorkingDir
}

func (r *replicaRunner) RunCommentCommand(models.Repo, *models.Repo, *models.PullRequest, models.User, int, *events.CommentCommand) {
	panic("use context variant")
}

func (r *replicaRunner) RunAutoplanCommand(models.Repo, models.Repo, models.PullRequest, models.User) {
	panic("use context variant")
}

func (r *replicaRunner) RunCommentCommandWithContext(_ context.Context, _ models.Repo, _ *models.Repo, pull *models.PullRequest, user models.User, pullNum int, cmd *events.CommentCommand) {
	var err error
	switch cmd.Name {
	case command.Plan:
		err = r.plan(*pull, user)
	case command.Unlock:
		_, err = r.unlock.DeleteLocksByPull(r.logger, pull.BaseRepo, pullNum)
	case command.Apply:
		err = r.validator.ValidateProjectPlanStatus(command.ProjectContext{Pull: *pull, Workspace: "default", RepoRelDir: "."})
	}
	r.out <- ran{replica: r.id, name: cmd.Name, pullNum: pullNum, err: err}
}

// plan takes the project lock and records a successful plan, as the plan
// command does.
func (r *replicaRunner) plan(pull models.PullRequest, user models.User) error {
	if _, err := r.locker.TryLock(models.NewProject(pull.BaseRepo.FullName, ".", ""), "default", pull, user); err != nil {
		return err
	}
	_, err := r.db.UpdatePullWithResults(pull, []command.ProjectResult{{
		Command: command.Plan, RepoRelDir: ".", Workspace: "default",
		ProjectCommandOutput: command.ProjectCommandOutput{PlanSuccess: &models.PlanSuccess{TerraformOutput: "Plan: 1 to add"}},
	}})
	return err
}

func (r *replicaRunner) RunAutoplanCommandWithContext(_ context.Context, _ models.Repo, _ models.Repo, pull models.PullRequest, _ models.User) {
	r.out <- ran{replica: r.id, name: command.Autoplan, pullNum: pull.Num}
}

func (r *replicaRunner) CleanUpPull(logging.SimpleLogging, models.Repo, models.PullRequest) error {
	return nil
}

// recordingWorkingDir records plan deletions; unlock calls nothing else.
type recordingWorkingDir struct {
	events.WorkingDir
	mu      sync.Mutex
	deleted int
}

func (w *recordingWorkingDir) DeletePlan(logging.SimpleLogging, models.Repo, models.PullRequest, string, string, string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted++
	return nil
}

func (w *recordingWorkingDir) deletions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deleted
}

type replica struct {
	id      string
	addr    string
	c       client.Client
	ns      string
	members *cluster.Membership
	stopRun func()
	router  *clusterrpc.Router
	drainer *events.Drainer
	runner  *replicaRunner
}

func newReplica(t *testing.T, c client.Client, ns, id string, out chan<- ran) *replica {
	t.Helper()
	logger := logging.NewNoopLogger(t)
	d, err := kubedb.New(kubedb.Config{Client: c, Namespace: ns, Identity: id})
	Ok(t, err)
	locker := locking.NewClient(d)
	wd := &recordingWorkingDir{}
	runner := &replicaRunner{
		id: id, db: d, locker: locker, logger: logger, out: out, workingDir: wd,
		unlock:    &events.DefaultDeleteLockCommand{Locker: locker, WorkingDir: wd, Database: d},
		validator: &events.DefaultApplyPlanValidator{PullStatusFetcher: d},
	}
	r := &replica{id: id, c: c, ns: ns, drainer: &events.Drainer{}, runner: runner}
	srv := httptest.NewServer((&clusterrpc.Server{
		Token: token, Local: runner, LocalClean: runner, Jobs: jobs{},
		JobsHandler: http.NotFoundHandler(), Logger: logger, Drain: r.drainer,
	}).Handler())
	t.Cleanup(srv.Close)
	r.addr = srv.URL
	r.router = &clusterrpc.Router{
		Local: runner, LocalClean: runner,
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logger, Scope: tally.NoopScope, CallTimeout: 2 * time.Second,
		Plans: d, Drain: r.drainer,
	}
	r.start(t)
	return r
}

// start runs a fresh membership for the replica, as a restarted pod does:
// same identity and address, new process state.
func (r *replica) start(t *testing.T) {
	t.Helper()
	m := cluster.NewMembership(cluster.MembershipConfig{
		Client: r.c, Namespace: r.ns, Identity: r.id, Address: r.addr,
		LeaseDuration: 3 * time.Second, RenewInterval: 200 * time.Millisecond,
		Logger: logging.NewNoopLogger(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	var once sync.Once
	r.stopRun = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(r.stopRun)
	r.members = m
	r.router.Owners = m
}

// sees reports whether the replica's membership lists exactly ids, with
// draining as given.
func (r *replica) sees(draining map[string]bool, ids ...string) bool {
	members := r.members.Members()
	if len(members) != len(ids) {
		return false
	}
	for i, m := range members {
		if m.Identity != ids[i] || m.Draining != draining[m.Identity] {
			return false
		}
	}
	return true
}

func twoReplicas(t *testing.T) (a, b *replica, out chan ran) {
	t.Helper()
	c, ns := kubetest.Client(t)
	out = make(chan ran, 16)
	a = newReplica(t, c, ns, "atlantis-0", out)
	b = newReplica(t, c, ns, "atlantis-1", out)
	eventually(t, func() bool {
		return a.sees(nil, "atlantis-0", "atlantis-1") && b.sees(nil, "atlantis-0", "atlantis-1")
	}, "replicas did not see each other")
	return a, b, out
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// pullOwnedBy returns a pull number that r owns.
func pullOwnedBy(t *testing.T, r *replica) int {
	t.Helper()
	for n := 1; n < 1000; n++ {
		if o, ok := r.members.Owner(kube.PullKey(repo.VCSHost.Hostname, repo.FullName, n)); ok && o.Identity == r.id {
			return n
		}
	}
	t.Fatalf("no pull owned by %s", r.id)
	return 0
}

func comment(r *replica, pullNum int, name command.Name) {
	pull := models.PullRequest{Num: pullNum, HeadCommit: "sha", BaseRepo: repo}
	r.router.RunCommentCommandWithContext(context.Background(), repo, &repo, &pull, models.User{Username: "u"}, pullNum, &events.CommentCommand{Name: name})
}

func next(t *testing.T, out <-chan ran) ran {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a command to run")
		return ran{}
	}
}

// nothingElse fails if another command runs: every command must run once.
func nothingElse(t *testing.T, out <-chan ran) {
	t.Helper()
	select {
	case r := <-out:
		t.Fatalf("unexpected extra run: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestEachCommandRunsOnceOnItsOwner(t *testing.T) {
	a, b, out := twoReplicas(t)
	for _, owner := range []*replica{a, b} {
		pullNum := pullOwnedBy(t, owner)
		for _, receiver := range []*replica{a, b} {
			comment(receiver, pullNum, command.Plan)
			got := next(t, out)
			Equals(t, owner.id, got.replica)
			Equals(t, pullNum, got.pullNum)
			Ok(t, got.err)
			nothingElse(t, out)
		}
	}
}

// A plan on one replica must not be applied after `atlantis unlock` ran on
// the other: the plan file stays on the replica that planned, so only the
// discarded status in the shared database stops the apply.
func TestUnlockOnOtherReplicaStopsApplyOfItsPlan(t *testing.T) {
	a, b, out := twoReplicas(t)
	pullNum := pullOwnedBy(t, a)

	comment(b, pullNum, command.Plan)
	got := next(t, out)
	Equals(t, a.id, got.replica)
	Ok(t, got.err)

	// The apply goes to the replica holding the plan and passes validation.
	comment(b, pullNum, command.Apply)
	got = next(t, out)
	Equals(t, a.id, got.replica)
	Ok(t, got.err)

	// a restarts. While it is gone b owns the pull, so the unlock runs on b,
	// which cannot reach a's plan file.
	a.stopRun()
	eventually(t, func() bool { return b.sees(nil, "atlantis-1") }, "b still sees a")
	comment(b, pullNum, command.Unlock)
	got = next(t, out)
	Equals(t, b.id, got.replica)
	Ok(t, got.err)
	Equals(t, 0, a.runner.workingDir.deletions())
	locks, err := b.runner.db.List()
	Ok(t, err)
	Equals(t, 0, len(locks))

	a.start(t)
	eventually(t, func() bool { return b.sees(nil, "atlantis-0", "atlantis-1") }, "b did not see a return")

	// The apply goes back to a, which still has the plan file, and is refused.
	comment(b, pullNum, command.Apply)
	got = next(t, out)
	Equals(t, a.id, got.replica)
	Assert(t, got.err != nil && strings.Contains(got.err.Error(), `has status "plan_discarded"`), "apply was not refused as discarded: %v", got.err)
	nothingElse(t, out)
}

// During a rolling update the replica shutting down keeps receiving
// webhooks. Its commands must move to the other replica and run once there.
func TestDrainingReplicaHandsCommandsToTheOther(t *testing.T) {
	a, b, out := twoReplicas(t)
	pullNum := pullOwnedBy(t, a)

	// The shutdown order in server.Start: hand pulls off, then stop new work.
	a.members.SetDraining(context.Background())
	a.drainer.ShutdownBlocking()
	draining := map[string]bool{a.id: true}
	eventually(t, func() bool {
		return a.sees(draining, "atlantis-0", "atlantis-1") && b.sees(draining, "atlantis-0", "atlantis-1")
	}, "drain not observed")

	for _, receiver := range []*replica{a, b} {
		comment(receiver, pullNum, command.Plan)
		got := next(t, out)
		Equals(t, b.id, got.replica)
		Ok(t, got.err)
		nothingElse(t, out)
	}

	// A command forwarded to a while it is shutting down is refused, and the
	// sender runs it itself instead of dropping it.
	stale := &clusterrpc.Router{
		Local: b.runner, LocalClean: b.runner,
		Owners: staticOwners{self: b.id, owner: cluster.Member{Identity: a.id, Address: a.addr}},
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: 2 * time.Second,
	}
	pull := models.PullRequest{Num: pullNum, HeadCommit: "sha", BaseRepo: repo}
	stale.RunCommentCommandWithContext(context.Background(), repo, &repo, &pull, models.User{Username: "u"}, pullNum, &events.CommentCommand{Name: command.Plan})
	got := next(t, out)
	Equals(t, b.id, got.replica)
	nothingElse(t, out)
}
