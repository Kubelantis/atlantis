package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	tally "github.com/uber-go/tally/v4"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/runatlantis/atlantis/server/clusterrpc"
	"github.com/runatlantis/atlantis/server/core/db"
	"github.com/runatlantis/atlantis/server/core/kube"
	"github.com/runatlantis/atlantis/server/core/kube/cluster"
	"github.com/runatlantis/atlantis/server/core/kube/janitor"
	"github.com/runatlantis/atlantis/server/core/kube/kubedb"
	"github.com/runatlantis/atlantis/server/core/kube/pulllock"
	"github.com/runatlantis/atlantis/server/core/logstore"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/events"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs"
	"github.com/runatlantis/atlantis/server/jobs"
	"github.com/runatlantis/atlantis/server/logging"
)

// kubeRuntime holds everything Atlantis needs to run as one of several
// replicas on Kubernetes. NewServer always creates it; it is nil only in unit tests that build a Server directly.
type kubeRuntime struct {
	client     client.Client
	restConfig *rest.Config
	namespace  string
	identity   string
	// incarnation identifies this process among any others that run with
	// the same identity; see kube.NewIncarnation.
	incarnation string
	token       string
	port        int

	membership  *cluster.Membership
	leader      *cluster.Leader
	housekeeper *cluster.Housekeeper
	rpcClient   *clusterrpc.Client
	logger      logging.SimpleLogging
	scope       tally.Scope

	cancel context.CancelFunc
	wg     sync.WaitGroup
	srv    *http.Server

	// started is set once the cluster listener, membership and leader
	// election are running.
	started atomic.Bool

	// replicaCleaner handles cleanup broadcasts for closed pulls.
	replicaCleaner *events.ReplicaCleaner
	// janitor removes local leftovers of pulls closed while this replica was down.
	janitor *janitor.Janitor

	// closedPulls and pullCleaner let the leader finish the cleanup of pulls
	// closed while a command ran; see sweepClosedPulls. Set by wire.
	closedPulls closedPullLister
	pullCleaner events.PullCleaner
}

// closedPullLister lists pulls closed while a command ran whose cleanup
// hasn't finished. It is implemented by kubedb.KubeDB.
type closedPullLister interface {
	ClosedPulls() ([]models.PullRequest, error)
}

// closedPullSweepInterval is how often the leader retries the cleanup of
// pulls closed while a command ran.
const closedPullSweepInterval = 5 * time.Minute

// runLeader runs while this replica leads the cluster.
func (k *kubeRuntime) runLeader(ctx context.Context) {
	go k.sweepClosedPulls(ctx)
	k.housekeeper.Run(ctx)
}

// sweepClosedPulls retries the cleanup of pulls closed while a command ran.
// The owner normally finishes it when the command ends; this covers a
// restart in between, and a command that ran on another replica than the
// owner.
func (k *kubeRuntime) sweepClosedPulls(ctx context.Context) {
	if k.closedPulls == nil || k.pullCleaner == nil {
		return
	}
	t := time.NewTicker(closedPullSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.sweepClosedPullsOnce()
		}
	}
}

// sweepClosedPullsOnce runs the cleanup of each closed pull. Cleanup of a
// pull whose command still runs is deferred again, so this is safe to repeat.
func (k *kubeRuntime) sweepClosedPullsOnce() {
	pulls, err := k.closedPulls.ClosedPulls()
	if err != nil {
		k.logger.Warn("listing closed pull requests to clean up: %s", err)
		return
	}
	for _, pull := range pulls {
		if err := k.pullCleaner.CleanUpPull(k.logger, pull.BaseRepo, pull); err != nil {
			k.logger.Warn("cleaning up closed pull request %s#%d: %s", pull.BaseRepo.FullName, pull.Num, err)
		}
	}
}

// newKubeRuntime connects to the Kubernetes API. c, if non-nil, is used
// instead of the in-cluster or KUBECONFIG client (tests pass a fake); leader
// election then stays off because it needs a REST config.
func newKubeRuntime(userConfig UserConfig, c client.Client, logger logging.SimpleLogging, scope tally.Scope) (*kubeRuntime, error) {
	ns, err := kube.ResolveNamespace(userConfig.KubernetesNamespace)
	if err != nil {
		return nil, err
	}
	identity, err := kube.ResolveIdentity(userConfig.KubernetesIdentity)
	if err != nil {
		return nil, fmt.Errorf("resolving replica identity: %w", err)
	}
	var restCfg *rest.Config
	if c == nil {
		if c, restCfg, err = kube.NewClient(50, 100); err != nil {
			return nil, err
		}
	}
	address := userConfig.ClusterAddress
	if address == "" {
		ip := os.Getenv("POD_IP")
		if ip == "" {
			return nil, errors.New("--cluster-address is not set and $POD_IP is empty; set POD_IP from the downward API")
		}
		address = "http://" + net.JoinHostPort(ip, strconv.Itoa(userConfig.ClusterPort))
	}

	incarnation := kube.NewIncarnation()
	logger = logger.With("replica", identity)
	token := userConfig.ClusterToken
	if token == "" {
		// Safe for one replica. With several, forwards between replicas with
		// different tokens are rejected and commands run where they arrive.
		token = randomToken()
		logger.Warn("--cluster-token is not set; generated a random one. Set the same ATLANTIS_CLUSTER_TOKEN on every replica so they can forward work to each other")
	}
	scope = scope.SubScope("cluster")
	k := &kubeRuntime{
		client: c, restConfig: restCfg, namespace: ns, identity: identity, incarnation: incarnation,
		token: token, port: userConfig.ClusterPort,
		logger: logger, scope: scope,
		rpcClient: &clusterrpc.Client{
			Token:      token,
			HTTPClient: &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		},
	}
	membersGauge := scope.Gauge("members")
	apiHealthy := scope.Gauge("api_healthy")
	identityConflict := scope.Gauge("identity_conflict")
	k.membership = cluster.NewMembership(cluster.MembershipConfig{
		Client: c, Namespace: ns, Identity: identity, Incarnation: incarnation, Address: address, Logger: logger,
		OnChange: func(members []cluster.Member) {
			membersGauge.Update(float64(len(members)))
			logger.Info("cluster membership changed: %d live replicas", len(members))
		},
		OnSync: func(err error) {
			if err != nil {
				apiHealthy.Update(0)
			} else {
				apiHealthy.Update(1)
			}
			if k.membership.IdentityConflict() {
				identityConflict.Update(1)
			} else {
				identityConflict.Update(0)
			}
		},
	})
	k.housekeeper = &cluster.Housekeeper{Client: c, Namespace: ns, Logger: logger}
	leaderGauge := scope.Gauge("leader")
	k.leader = cluster.NewLeader(cluster.LeaderConfig{
		// The incarnation keeps two processes with one name from both
		// leading.
		RestConfig: restCfg, Namespace: ns, Identity: identity + "/" + incarnation, Logger: logger,
		Run: k.runLeader,
		OnChange: func(isLeader bool, leader string) {
			if isLeader {
				leaderGauge.Update(1)
			} else {
				leaderGauge.Update(0)
			}
			logger.Info("cluster leader is %s", leader)
		},
	})
	logger.Info("running on Kubernetes in namespace %s as %s, incarnation %s (cluster address %s)", ns, identity, incarnation, address)
	return k, nil
}

func (k *kubeRuntime) database() (*kubedb.KubeDB, error) {
	return kubedb.New(kubedb.Config{Client: k.client, Namespace: k.namespace, Identity: k.identity})
}

// workingDirLocker returns the Lease-backed locker. onLost is called when a
// pull's lease is lost to another replica.
func (k *kubeRuntime) workingDirLocker(local events.WorkingDirLocker, onLost func(repo models.Repo, pullNum int)) events.WorkingDirLocker {
	lost := k.scope.Counter("pull_lock_lost")
	return pulllock.New(pulllock.Config{
		Local: local, Client: k.client, Namespace: k.namespace, Identity: k.identity, Incarnation: k.incarnation, Logger: k.logger,
		OnLost: func(repo models.Repo, pullNum int) {
			lost.Inc(1)
			onLost(repo, pullNum)
		},
	})
}

// kubeDeps are the server components the Kubernetes runtime wires together.
type kubeDeps struct {
	CommandRunner      events.ContextCommandRunner
	PullCleaner        events.PullCleaner
	PullClosedExecutor *events.PullClosedExecutor
	VCSClient          vcs.Client
	Database           db.Database
	WorkingDir         events.WorkingDir
	WorkingDirLocker   events.WorkingDirLocker
	// LocalWorkingDirLocker is this replica's in-process working-dir locker,
	// without the cluster-wide pull lease. Peer cleanup checks it: the owner
	// holds the lease while it asks peers to clean up.
	LocalWorkingDirLocker events.WorkingDirLocker
	JobOutput             jobs.ProjectCommandOutputHandler
	CancellationTracker   events.CancellationTracker
	// LocalLogs is the replica's own job log files; nil when logs are off.
	LocalLogs *logstore.FileLogStore
	DataDir   string
	JobLogDir string
	// SharedPlans is true when plans are in an external store.
	SharedPlans    bool
	PlanHolderWait time.Duration
	// Drainer makes shutdown wait for commands that are being routed.
	Drainer *events.Drainer
}

// wire connects the cluster runtime to the server: it returns the router
// that sends commands to the right replica, makes pull closes clean every
// replica, and prepares the replica's janitor. Call before start.
func (k *kubeRuntime) wire(d kubeDeps) *clusterrpc.Router {
	plans, _ := d.Database.(clusterrpc.PlanHolders)
	router := &clusterrpc.Router{
		Local: d.CommandRunner, LocalClean: d.PullCleaner, Owners: k.membership,
		Client: k.rpcClient, Logger: k.logger, Scope: k.scope.SubScope("routing"),
		Plans: plans, SharedPlans: d.SharedPlans, PlanHolderWait: d.PlanHolderWait,
		Notify: func(repo models.Repo, pullNum int, msg string) error {
			return d.VCSClient.CreateComment(k.logger, repo, pullNum, msg, "")
		},
	}
	if d.Drainer != nil {
		router.Drain = d.Drainer
	}
	// Closing a pull also removes the copies other replicas hold.
	d.PullClosedExecutor.Peers = router
	// Cleanup waits for the pull's running commands; a pull closed while one
	// runs keeps its closed mark (set by the webhook, see NewServer), and is
	// finished by its owner or by the leader's sweep.
	d.PullClosedExecutor.WorkingDirLocker = d.WorkingDirLocker
	if lister, ok := d.Database.(closedPullLister); ok {
		k.closedPulls = lister
		k.pullCleaner = router
	}
	k.replicaCleaner = &events.ReplicaCleaner{WorkingDir: d.WorkingDir, CancellationTracker: d.CancellationTracker, WorkingDirLocker: d.LocalWorkingDirLocker}
	if pj, ok := d.JobOutput.(events.PullJobsCleaner); ok {
		k.replicaCleaner.Jobs = pj
	}
	k.janitor = k.newJanitor(d)
	return router
}

// newJanitor returns nil when the database cannot tell whether a pull exists.
func (k *kubeRuntime) newJanitor(d kubeDeps) *janitor.Janitor {
	pulls, ok := d.Database.(interface {
		PullStatusExists(repoFullName string, pullNum int) (bool, error)
	})
	if !ok {
		return nil
	}
	cfg := janitor.Config{
		ReposDir:   filepath.Join(d.DataDir, planstore.ReposDir),
		PullExists: pulls.PullStatusExists,
		Logger:     k.logger,
		RemoveClones: func(repoFullName string, pullNum int) error {
			repo := models.Repo{FullName: repoFullName}
			return d.WorkingDir.Delete(k.logger, repo, models.PullRequest{Num: pullNum, BaseRepo: repo})
		},
		RemoveLogs: func(string, int) error { return nil },
	}
	if holder, ok := d.WorkingDirLocker.(interface{ Holds(string, int) bool }); ok {
		cfg.Busy = holder.Holds
	}
	if d.LocalLogs != nil {
		cfg.LogDir = d.JobLogDir
		cfg.RemoveLogs = func(repoFullName string, pullNum int) error {
			return d.LocalLogs.DeletePull(logstore.Pull{RepoFullName: repoFullName, Num: pullNum})
		}
	}
	return janitor.New(cfg)
}

func (k *kubeRuntime) jobProxy(output jobs.ProjectCommandOutputHandler) *clusterrpc.JobProxy {
	return &clusterrpc.JobProxy{Jobs: output, Owners: k.membership, Client: k.rpcClient, Logger: k.logger}
}

// start begins membership, leader election, and the internal listener.
func (k *kubeRuntime) start(s *Server, local events.ContextCommandRunner, localClean events.PullCleaner) error {
	jobsRouter := mux.NewRouter()
	jobsRouter.HandleFunc("/jobs/{job-id}", s.JobsController.GetProjectJobs).Methods("GET")
	jobsRouter.HandleFunc("/jobs/{job-id}/ws", s.JobsController.GetProjectJobsWS).Methods("GET")
	internal := &clusterrpc.Server{
		Token: k.token, Local: local, LocalClean: localClean,
		Jobs: s.ProjectCmdOutputHandler, JobsHandler: jobsRouter,
		Logger: k.logger, Drain: s.Drainer,
	}
	if k.replicaCleaner != nil {
		internal.Replica = k.replicaCleaner
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", k.port))
	if err != nil {
		return fmt.Errorf("listening on cluster port: %w", err)
	}
	k.srv = &http.Server{Handler: otelhttp.NewHandler(internal.Handler(), "atlantis.internal"), ReadHeaderTimeout: 10 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel
	k.wg.Go(func() {
		if err := k.srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			k.logger.Err("cluster listener: %s", err)
		}
	})
	k.wg.Go(func() { k.membership.Run(ctx) })
	if k.janitor != nil {
		k.wg.Go(func() { k.janitor.Run(ctx) })
	}
	if k.restConfig != nil {
		k.wg.Go(func() {
			if err := k.leader.Run(ctx); err != nil {
				k.logger.Err("leader election: %s", err)
			}
		})
	}
	k.started.Store(true)
	return nil
}

// ready reports whether this replica should receive traffic. It depends only
// on local state: the Kubernetes API is shared by every replica, so gating
// readiness on it would take all replicas out of the Service at once during
// an API server outage. Commands that need the API fail individually instead,
// and atlantis_cluster_api_healthy reports API reachability.
func (k *kubeRuntime) ready(draining bool) (bool, string) {
	if draining {
		return false, "draining"
	}
	if !k.started.Load() {
		return false, "starting"
	}
	if k.membership != nil && k.membership.IdentityConflict() {
		return false, "another process holds this replica's identity"
	}
	return true, ""
}

// drain stops this replica from owning pulls while its running jobs finish.
func (k *kubeRuntime) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k.membership.SetDraining(ctx)
}

// stop releases the member and leader Leases and closes the listener.
func (k *kubeRuntime) stop() {
	if k.cancel == nil {
		return
	}
	k.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = k.srv.Shutdown(ctx)
	k.wg.Wait()
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}
