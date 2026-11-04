package kubetest

import (
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A stop on SIGINT/SIGTERM can arrive while envtest is still starting etcd
// and the API server. It must wait for the start to finish, so it stops
// every process that start created, and no start may follow it.
func TestStopWaitsForStartInProgress(t *testing.T) {
	origStart, origStop := envStart, envStop
	t.Cleanup(func() {
		envStart, envStop = origStart, origStop
		env, stopped = nil, false
	})

	inStart, finishStart := make(chan struct{}), make(chan struct{})
	started := false
	envStart = func(*envtest.Environment) (*rest.Config, error) {
		close(inStart)
		<-finishStart
		started = true
		return &rest.Config{}, nil
	}
	stoppedAfterStart := make(chan bool, 1)
	envStop = func(*envtest.Environment) error {
		stoppedAfterStart <- started
		return nil
	}

	go func() { _, _ = start(&envtest.Environment{}) }()
	<-inStart
	stopDone := make(chan struct{})
	go func() { stop(); close(stopDone) }()

	select {
	case <-stopDone:
		t.Fatal("stop finished while start was still creating processes")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishStart)
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not finish after start")
	}
	if !<-stoppedAfterStart {
		t.Fatal("stop ran before start finished")
	}

	envStart = func(*envtest.Environment) (*rest.Config, error) {
		t.Fatal("started after stop")
		return nil, nil
	}
	if _, err := start(&envtest.Environment{}); err == nil {
		t.Fatal("start after stop did not fail")
	}
}
