// Package kubetest starts a real kube-apiserver and etcd (envtest) for tests
// of the Kubernetes-native backends. Tests are skipped unless
// KUBEBUILDER_ASSETS points at the control-plane binaries; run
// `make envtest` to install them.
//
// The API server and etcd are separate processes that outlive the test
// binary unless they are stopped, so every package that calls Client needs
//
//	func TestMain(m *testing.M) { kubetest.Run(m) }
package kubetest

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/runatlantis/atlantis/server/core/kube"
)

var (
	once sync.Once
	// lifecycle serializes starting and stopping env: envtest starts etcd
	// and the API server one after the other without synchronization, so a
	// stop during the start could miss a process it is still creating.
	lifecycle sync.Mutex
	// env and stopped, set by the first stop, are guarded by lifecycle.
	env     *envtest.Environment
	stopped bool
	// envStart and envStop start and stop env; tests replace them.
	envStart = (*envtest.Environment).Start
	envStop  = (*envtest.Environment).Stop
	// running is set by Run; Client refuses to start the server without it.
	running atomic.Bool
	shared  client.Client
	restCfg *rest.Config
	initErr error
	nsSeq   atomic.Int64
)

// Client returns a client for a shared envtest API server and a fresh
// namespace for this test. The server is started once per test binary and
// stopped by Run when the tests finish.
func Client(t testing.TB) (client.Client, string) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make envtest` to enable Kubernetes integration tests")
	}
	if !running.Load() {
		t.Fatal("kubetest: add `func TestMain(m *testing.M) { kubetest.Run(m) }` to this package; without it the envtest API server and etcd keep running after the tests")
	}
	once.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
		stopOnSignal()
		cfg, err := start(&envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join(root, "deploy", "crds")},
			ErrorIfCRDPathMissing: true,
		})
		restCfg = cfg
		if err != nil {
			initErr = fmt.Errorf("starting envtest: %w", err)
			return
		}
		s, err := kube.NewScheme()
		if err != nil {
			initErr = err
			return
		}
		shared, initErr = client.New(cfg, client.Options{Scheme: s})
	})
	if initErr != nil {
		t.Fatal(initErr)
	}
	ns := fmt.Sprintf("test-%d-%d", os.Getpid(), nsSeq.Add(1))
	if err := shared.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return shared, ns
}

// RestConfig returns the rest config of the shared API server. Call Client first.
func RestConfig() *rest.Config { return rest.CopyConfig(restCfg) }

// Run runs the package's tests and then stops the shared API server and
// etcd, if Client started them. Call it from TestMain.
func Run(m *testing.M) {
	running.Store(true)
	defer stop()
	m.Run()
}

// start starts e as env unless env was already stopped. A start that fails
// part-way may leave processes running; stop stops them.
func start(e *envtest.Environment) (*rest.Config, error) {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if stopped {
		return nil, fmt.Errorf("the test binary is shutting down")
	}
	env = e
	return envStart(env)
}

// stop stops the shared API server and etcd once, after any start in
// progress has finished.
func stop() {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if stopped {
		return
	}
	stopped = true
	if env != nil {
		_ = envStop(env)
	}
}

// stopOnSignal stops them too when the test binary is interrupted or
// terminated, which would otherwise leave them running. A signal during
// startup waits for it to finish, then stops whatever it started. A second
// signal exits at once, without waiting, as an escape hatch for a hung
// startup or stop; the API server and etcd may then keep running.
func stopOnSignal() {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		go func() {
			<-sig
			fmt.Fprintln(os.Stderr, "kubetest: second signal, exiting without stopping the envtest API server and etcd")
			os.Exit(1)
		}()
		stop()
		os.Exit(1)
	}()
}
