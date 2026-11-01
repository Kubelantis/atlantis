package cluster_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/core/kube/kubetest"
)

// TestMain stops the envtest API server and etcd after the tests.
func TestMain(m *testing.M) { kubetest.Run(m) }
