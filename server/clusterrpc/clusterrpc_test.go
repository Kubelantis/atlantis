package clusterrpc_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	tally "github.com/uber-go/tally/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/runatlantis/atlantis/server/clusterrpc"
	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/command"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
)

const token = "s3cret"

type call struct {
	kind    string
	pullNum int
	cmd     *events.CommentCommand
	traceID trace.TraceID
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	done  chan struct{}
}

func newFakeRunner() *fakeRunner { return &fakeRunner{done: make(chan struct{}, 10)} }

func (f *fakeRunner) record(ctx context.Context, c call) {
	c.traceID = trace.SpanContextFromContext(ctx).TraceID()
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	f.done <- struct{}{}
}
func (f *fakeRunner) RunCommentCommand(models.Repo, *models.Repo, *models.PullRequest, models.User, int, *events.CommentCommand) {
	panic("use context variant")
}
func (f *fakeRunner) RunAutoplanCommand(models.Repo, models.Repo, models.PullRequest, models.User) {
	panic("use context variant")
}
func (f *fakeRunner) RunCommentCommandWithContext(ctx context.Context, _ models.Repo, _ *models.Repo, _ *models.PullRequest, _ models.User, pullNum int, cmd *events.CommentCommand) {
	f.record(ctx, call{kind: "comment", pullNum: pullNum, cmd: cmd})
}
func (f *fakeRunner) RunAutoplanCommandWithContext(ctx context.Context, _ models.Repo, _ models.Repo, pull models.PullRequest, _ models.User) {
	f.record(ctx, call{kind: "autoplan", pullNum: pull.Num})
}
func (f *fakeRunner) CleanUpPull(logging.SimpleLogging, models.Repo, models.PullRequest) error {
	f.record(context.Background(), call{kind: "cleanup"})
	return nil
}
func (f *fakeRunner) wait(t *testing.T) {
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for command")
	}
}
func (f *fakeRunner) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type staticOwners struct {
	self    string
	owner   cluster.Member
	members []cluster.Member
}

func (s staticOwners) Identity() string                    { return s.self }
func (s staticOwners) Owner(string) (cluster.Member, bool) { return s.owner, s.owner.Identity != "" }
func (s staticOwners) Members() []cluster.Member           { return s.members }

type jobs map[string]bool

func (j jobs) IsKeyExists(k string) bool { return j[k] }

type drain struct{ shutting bool }

func (d drain) GetStatus() events.DrainStatus { return events.DrainStatus{ShuttingDown: d.shutting} }

func ownerServer(t *testing.T, runner *fakeRunner, d drain, jobHandler http.Handler) *httptest.Server {
	s := &clusterrpc.Server{Token: token, Local: runner, LocalClean: runner, Jobs: jobs{"job-b": true}, JobsHandler: jobHandler, Logger: logging.NewNoopLogger(t), Drain: d}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func router(t *testing.T, local *fakeRunner, owner cluster.Member) *clusterrpc.Router {
	return &clusterrpc.Router{
		Local: local, LocalClean: local,
		Owners: staticOwners{self: "a", owner: owner},
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: 2 * time.Second,
	}
}

var repo = models.Repo{FullName: "org/repo", VCSHost: models.VCSHost{Hostname: "github.com"}}

func TestForwardsToOwnerWithTrace(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, span := tp.Tracer("t").Start(context.Background(), "webhook")
	defer span.End()

	remote, local := newFakeRunner(), newFakeRunner()
	srv := ownerServer(t, remote, drain{}, http.NotFoundHandler())
	r := router(t, local, cluster.Member{Identity: "b", Address: srv.URL})

	r.RunCommentCommandWithContext(ctx, repo, nil, nil, models.User{Username: "u"}, 5, &events.CommentCommand{Name: command.Plan, Workspace: "ws"})
	remote.wait(t)
	Equals(t, 0, local.count())
	Equals(t, 5, remote.calls[0].pullNum)
	Equals(t, "ws", remote.calls[0].cmd.Workspace)
	Equals(t, span.SpanContext().TraceID(), remote.calls[0].traceID)

	r.RunAutoplanCommandWithContext(ctx, repo, repo, models.PullRequest{Num: 6}, models.User{})
	remote.wait(t)
	Equals(t, "autoplan", remote.calls[1].kind)

	Ok(t, r.CleanUpPull(logging.NewNoopLogger(t), repo, models.PullRequest{Num: 6}))
	remote.wait(t)
	Equals(t, "cleanup", remote.calls[2].kind)
}

func TestRunsLocallyWhenSelfOwnedOrOwnerUnavailable(t *testing.T) {
	remote := newFakeRunner()
	draining := ownerServer(t, remote, drain{shutting: true}, http.NotFoundHandler())
	for name, owner := range map[string]cluster.Member{
		"self":        {Identity: "a", Address: "http://unused"},
		"no owner":    {},
		"unreachable": {Identity: "b", Address: "http://127.0.0.1:1"},
		"draining":    {Identity: "b", Address: draining.URL},
	} {
		t.Run(name, func(t *testing.T) {
			local := newFakeRunner()
			router(t, local, owner).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
			local.wait(t)
			Equals(t, 1, local.count())
		})
	}
	Equals(t, 0, remote.count())
}

func TestInternalServerRequiresToken(t *testing.T) {
	remote := newFakeRunner()
	srv := ownerServer(t, remote, drain{}, http.NotFoundHandler())
	for _, auth := range []string{"", "Bearer wrong", token} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/internal/v1/commands", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		Ok(t, err)
		resp.Body.Close()
		Equals(t, http.StatusUnauthorized, resp.StatusCode)
	}
	resp, err := http.Get(srv.URL + "/jobs/job-b")
	Ok(t, err)
	resp.Body.Close()
	Equals(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestJobProxyServesRemoteJob(t *testing.T) {
	jobPage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Assert(t, r.Header.Get(clusterrpc.ForwardedHeader) != "", "proxied request must be marked")
		_, _ = io.WriteString(w, "output of "+r.URL.Path)
	})
	b := ownerServer(t, newFakeRunner(), drain{}, jobPage)

	owners := staticOwners{self: "a", members: []cluster.Member{{Identity: "a", Address: "http://unused"}, {Identity: "c", Address: "http://127.0.0.1:1"}, {Identity: "b", Address: b.URL}}}
	proxy := &clusterrpc.JobProxy{Jobs: jobs{"job-a": true}, Owners: owners, Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient}, Logger: logging.NewNoopLogger(t)}
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "local") })
	rt := mux.NewRouter()
	rt.Handle("/jobs/{job-id}", proxy.Middleware(local))
	a := httptest.NewServer(rt)
	defer a.Close()

	get := func(path string) string {
		resp, err := http.Get(a.URL + path)
		Ok(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	Equals(t, "local", get("/jobs/job-a"))
	Equals(t, "output of /jobs/job-b", get("/jobs/job-b"))
	Equals(t, "local", get("/jobs/unknown"))
}

// ackTimeout is the sender's per-call timeout in tests that withhold an ack.
// Withheld acks wait for the sender to give up rather than sleeping for a
// fixed time, so ackTimeout only needs to cover an ordinary call on a busy
// machine.
const ackTimeout = 500 * time.Millisecond

// withholdAck blocks until the sender gives up on the request r. The body is
// drained first: the server only notices a closed connection, and cancels the
// request context, once the body has been read.
func withholdAck(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

// slowAck runs the real handler, then withholds the response until the sender
// times out: the owner accepted the command but the sender cannot know.
func slowAck(inner http.Handler, slowCalls int) http.Handler {
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		if int(n.Add(1)) <= slowCalls {
			withholdAck(r)
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func lostAckSetup(t *testing.T, slowCalls int, ownerLive bool) (*fakeRunner, *fakeRunner, *clusterrpc.Router, *[]string) {
	remote, local := newFakeRunner(), newFakeRunner()
	s := &clusterrpc.Server{Token: token, Local: remote, LocalClean: remote, Jobs: jobs{}, JobsHandler: http.NotFoundHandler(), Logger: logging.NewNoopLogger(t), Drain: drain{}}
	srv := httptest.NewServer(slowAck(s.Handler(), slowCalls))
	t.Cleanup(srv.Close)
	owner := cluster.Member{Identity: "b", Address: srv.URL}
	owners := staticOwners{self: "a", owner: owner}
	if ownerLive {
		owners.members = []cluster.Member{{Identity: "a"}, owner}
	}
	var comments []string
	r := &clusterrpc.Router{
		Local: local, LocalClean: local, Owners: owners,
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: ackTimeout,
		Notify: func(_ models.Repo, _ int, msg string) error { comments = append(comments, msg); return nil },
	}
	return remote, local, r, &comments
}

func TestLostAckIsNotRunTwice(t *testing.T) {
	remote, local, r, comments := lostAckSetup(t, 3, true)
	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Apply})
	remote.wait(t)
	time.Sleep(200 * time.Millisecond)
	Equals(t, 1, remote.count()) // retries carried the same request ID
	Equals(t, 0, local.count())  // the live owner may be running it: do not run again
	Equals(t, 1, len(*comments))
	Assert(t, strings.Contains((*comments)[0], "could not confirm that replica `b`"), "unexpected comment %q", (*comments)[0])
}

func TestRetryAfterLostAckIsDeduplicated(t *testing.T) {
	remote, local, r, comments := lostAckSetup(t, 1, true)
	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
	remote.wait(t)
	time.Sleep(200 * time.Millisecond)
	Equals(t, 1, remote.count())
	Equals(t, 0, local.count())
	Equals(t, 0, len(*comments))
}

func TestLostAckFromDeadOwnerRunsLocally(t *testing.T) {
	_, local, r, comments := lostAckSetup(t, 3, false)
	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
	local.wait(t)
	Equals(t, 1, local.count())
	Equals(t, 0, len(*comments))
}

type dynOwners struct {
	mu      sync.Mutex
	self    string
	owner   cluster.Member
	members []cluster.Member
}

func (d *dynOwners) Identity() string { return d.self }
func (d *dynOwners) Owner(string) (cluster.Member, bool) {
	return d.owner, d.owner.Identity != ""
}
func (d *dynOwners) Members() []cluster.Member {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]cluster.Member(nil), d.members...)
}
func (d *dynOwners) set(ms ...cluster.Member) { d.mu.Lock(); d.members = ms; d.mu.Unlock() }

type holders map[int]string

func (h holders) PlanHolder(_ models.Repo, pull int) (string, error) { return h[pull], nil }

func TestApplyRoutesToPlanHolder(t *testing.T) {
	holderRunner, ownerRunner := newFakeRunner(), newFakeRunner()
	holderSrv := ownerServer(t, holderRunner, drain{}, http.NotFoundHandler())
	ownerSrv := ownerServer(t, ownerRunner, drain{}, http.NotFoundHandler())
	holder := cluster.Member{Identity: "atlantis-2", Address: holderSrv.URL}
	owner := cluster.Member{Identity: "atlantis-1", Address: ownerSrv.URL}
	apply := &events.CommentCommand{Name: command.Apply}

	newRouter := func(local *fakeRunner, owners *dynOwners, shared bool, wait time.Duration) *clusterrpc.Router {
		return &clusterrpc.Router{
			Local: local, LocalClean: local, Owners: owners,
			Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
			Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: time.Second,
			Plans: holders{1: "atlantis-2", 2: "atlantis-0"}, SharedPlans: shared,
			PlanHolderWait: wait, PlanHolderPoll: 20 * time.Millisecond,
		}
	}

	t.Run("apply goes to the live plan holder, not the hash owner", func(t *testing.T) {
		owners := &dynOwners{self: "atlantis-0", owner: owner}
		owners.set(cluster.Member{Identity: "atlantis-0"}, owner, holder)
		newRouter(newFakeRunner(), owners, false, time.Minute).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, apply)
		holderRunner.wait(t)
	})

	t.Run("plans are held here: apply runs locally", func(t *testing.T) {
		local := newFakeRunner()
		owners := &dynOwners{self: "atlantis-0", owner: owner}
		owners.set(cluster.Member{Identity: "atlantis-0"}, owner)
		newRouter(local, owners, false, time.Minute).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 2, apply)
		local.wait(t)
	})

	t.Run("apply waits for a restarting plan holder", func(t *testing.T) {
		owners := &dynOwners{self: "atlantis-0", owner: owner}
		owners.set(cluster.Member{Identity: "atlantis-0"}, owner) // holder is restarting
		go func() {
			time.Sleep(150 * time.Millisecond)
			owners.set(cluster.Member{Identity: "atlantis-0"}, owner, holder)
		}()
		start := time.Now()
		newRouter(newFakeRunner(), owners, false, time.Minute).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, apply)
		holderRunner.wait(t)
		Assert(t, time.Since(start) >= 150*time.Millisecond, "must have waited for the holder")
		Equals(t, 0, ownerRunner.count())
	})

	t.Run("a draining plan holder is waited for, then the owner takes over", func(t *testing.T) {
		owners := &dynOwners{self: "atlantis-0", owner: owner}
		drainingHolder := holder
		drainingHolder.Draining = true
		owners.set(cluster.Member{Identity: "atlantis-0"}, owner, drainingHolder)
		newRouter(newFakeRunner(), owners, false, 100*time.Millisecond).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, apply)
		ownerRunner.wait(t)
	})

	t.Run("shared plan store: apply uses normal ownership", func(t *testing.T) {
		owners := &dynOwners{self: "atlantis-0", owner: owner}
		owners.set(cluster.Member{Identity: "atlantis-0"}, owner, holder)
		newRouter(newFakeRunner(), owners, true, time.Minute).RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, apply)
		ownerRunner.wait(t)
	})
}

type recordingReplica struct {
	mu    sync.Mutex
	pulls []int
}

func (r *recordingReplica) CleanUpReplica(_ logging.SimpleLogging, _ models.Repo, pull models.PullRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pulls = append(r.pulls, pull.Num)
	return nil
}

func TestCleanUpPeersReachesEveryOtherReplica(t *testing.T) {
	var peers []*recordingReplica
	var members []cluster.Member
	for _, id := range []string{"atlantis-1", "atlantis-2"} {
		rep := &recordingReplica{}
		s := &clusterrpc.Server{Token: token, Local: newFakeRunner(), LocalClean: newFakeRunner(), Replica: rep, Jobs: jobs{}, JobsHandler: http.NotFoundHandler(), Logger: logging.NewNoopLogger(t)}
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)
		peers = append(peers, rep)
		members = append(members, cluster.Member{Identity: id, Address: srv.URL})
	}
	owners := &dynOwners{self: "atlantis-0"}
	owners.set(append(members, cluster.Member{Identity: "atlantis-0", Address: "http://127.0.0.1:1"})...)
	r := &clusterrpc.Router{Owners: owners, Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient}, Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope}

	r.CleanUpPeers(logging.NewNoopLogger(t), repo, models.PullRequest{Num: 9})
	for _, p := range peers {
		Equals(t, []int{9}, p.pulls)
	}
}

type switchDrain struct{ on atomic.Bool }

func (d *switchDrain) GetStatus() events.DrainStatus {
	return events.DrainStatus{ShuttingDown: d.on.Load()}
}

// The owner accepts the command, the ack is lost, and the owner starts
// draining before the retry arrives. The retry must be acknowledged as a
// duplicate, not refused, or the sender would run the command again.
func TestRetryReachingDrainingOwnerIsNotRunTwice(t *testing.T) {
	remote, local := newFakeRunner(), newFakeRunner()
	d := &switchDrain{}
	s := &clusterrpc.Server{Token: token, Local: remote, LocalClean: remote, Jobs: jobs{}, JobsHandler: http.NotFoundHandler(), Logger: logging.NewNoopLogger(t), Drain: d}
	inner := s.Handler()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		if n.Add(1) == 1 {
			d.on.Store(true)
			withholdAck(r)
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(srv.Close)
	owner := cluster.Member{Identity: "b", Address: srv.URL}
	r := &clusterrpc.Router{
		Local: local, LocalClean: local,
		Owners: staticOwners{self: "a", owner: owner, members: []cluster.Member{{Identity: "a"}, owner}},
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: ackTimeout,
	}

	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
	remote.wait(t)
	time.Sleep(200 * time.Millisecond)
	Equals(t, 1, remote.count())
	Equals(t, 0, local.count())
}

// An attempt that may have reached the owner keeps the outcome ambiguous even
// if a later attempt is refused: the refusal does not prove the first attempt
// was not accepted.
func TestRefusalAfterAmbiguousAttemptStaysAmbiguous(t *testing.T) {
	local := newFakeRunner()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			withholdAck(r)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	owner := cluster.Member{Identity: "b", Address: srv.URL}
	var comments []string
	r := &clusterrpc.Router{
		Local: local, LocalClean: local,
		Owners: staticOwners{self: "a", owner: owner, members: []cluster.Member{{Identity: "a"}, owner}},
		Client: &clusterrpc.Client{Token: token, HTTPClient: http.DefaultClient},
		Logger: logging.NewNoopLogger(t), Scope: tally.NoopScope, CallTimeout: ackTimeout,
		Notify: func(_ models.Repo, _ int, msg string) error { comments = append(comments, msg); return nil },
	}

	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
	Equals(t, 0, local.count())
	Equals(t, 1, len(comments))
}

// Shutdown must wait for a command that is still being forwarded: the
// webhook was already acknowledged, so dropping it would lose the command.
func TestShutdownWaitsForRoutedCommand(t *testing.T) {
	remote := newFakeRunner()
	release := make(chan struct{})
	s := &clusterrpc.Server{Token: token, Local: remote, LocalClean: remote, Jobs: jobs{}, JobsHandler: http.NotFoundHandler(), Logger: logging.NewNoopLogger(t), Drain: drain{}}
	inner := s.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	d := &events.Drainer{}
	r := router(t, newFakeRunner(), cluster.Member{Identity: "b", Address: srv.URL})
	r.Drain = d

	routed := make(chan struct{})
	go func() {
		r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
		close(routed)
	}()
	Assert(t, waitFor(func() bool { return d.GetStatus().InProgressOps == 1 }), "routing was not counted as an operation")

	shutdown := make(chan struct{})
	go func() { d.ShutdownBlocking(); close(shutdown) }()
	select {
	case <-shutdown:
		t.Fatal("shutdown finished while a command was still being forwarded")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-routed
	select {
	case <-shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the forward completed")
	}
	// The owner acknowledges, then runs the command in the background.
	remote.wait(t)
}

// A replica that is shutting down still forwards commands it receives: the
// owner is not shutting down and can run them, as it could before the drain
// hook was added.
func TestDrainingReplicaStillForwards(t *testing.T) {
	remote := newFakeRunner()
	s := &clusterrpc.Server{Token: token, Local: remote, LocalClean: remote, Jobs: jobs{}, JobsHandler: http.NotFoundHandler(), Logger: logging.NewNoopLogger(t), Drain: drain{}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	d := &events.Drainer{}
	d.ShutdownBlocking()
	local := newFakeRunner()
	r := router(t, local, cluster.Member{Identity: "b", Address: srv.URL})
	r.Drain = d

	r.RunCommentCommandWithContext(context.Background(), repo, nil, nil, models.User{}, 1, &events.CommentCommand{Name: command.Plan})
	remote.wait(t)
	r.RunAutoplanCommandWithContext(context.Background(), repo, repo, models.PullRequest{Num: 1, BaseRepo: repo}, models.User{})
	remote.wait(t)
	Equals(t, 2, remote.count())
	Equals(t, 0, local.count())
	Equals(t, 0, d.GetStatus().InProgressOps)
}

func waitFor(cond func() bool) bool {
	for range 100 {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
