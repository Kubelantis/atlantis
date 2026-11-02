// Package kube holds the shared plumbing for running Atlantis natively on
// Kubernetes: API client construction, object naming, and labels. The
// subpackages build on it:
//   - kubedb implements db.Database with Leases and PullStatus resources.
//   - cluster tracks replica membership, pull ownership and leader election.
//   - pulllock is a Lease-backed events.WorkingDirLocker.
package kube

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/runatlantis/atlantis/server/core/kube/apis/v1alpha1"
)

const (
	// LabelManagedBy marks every object Atlantis creates.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "atlantis"
	// LabelType distinguishes the kinds of Leases Atlantis creates.
	LabelType = "atlantis.runatlantis.io/type"
	// LabelRepo holds a hash of the repo full name (names are not valid label values).
	LabelRepo = "atlantis.runatlantis.io/repo"
	// LabelPull holds the pull request number.
	LabelPull = "atlantis.runatlantis.io/pull"

	// AnnotationKey holds the unhashed identity of the object (lock key, pull key...).
	AnnotationKey = "atlantis.runatlantis.io/key"
	// AnnotationData holds the JSON payload of a Lease.
	AnnotationData = "atlantis.runatlantis.io/data"
	// AnnotationAddress holds a member's reachable HTTP address.
	AnnotationAddress = "atlantis.runatlantis.io/address"
	// AnnotationReplica holds the replica name of a Lease's holder, whose
	// holderIdentity is <replica>/<incarnation>; see NewIncarnation.
	AnnotationReplica = "atlantis.runatlantis.io/replica"
	// AnnotationIncarnation holds the holder's incarnation on leases whose
	// holderIdentity is the replica name alone (membership leases).
	AnnotationIncarnation = "atlantis.runatlantis.io/incarnation"

	TypeProjectLock = "project-lock"
	TypeCommandLock = "command-lock"
	TypePullLock    = "pull-lock"
	TypeMember      = "member"

	namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// NewScheme returns a scheme with the core types and the Atlantis types.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// NewClient builds an uncached client from in-cluster config or KUBECONFIG.
// It is deliberately uncached: lock decisions must see the latest state.
func NewClient(qps float32, burst int) (client.Client, *rest.Config, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubernetes config: %w", err)
	}
	if qps > 0 {
		cfg.QPS = qps
	}
	if burst > 0 {
		cfg.Burst = burst
	}
	cfg.UserAgent = "atlantis"
	s, err := NewScheme()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return c, cfg, nil
}

// ResolveNamespace returns explicit if set, then $POD_NAMESPACE, then the
// service account namespace.
func ResolveNamespace(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, nil
	}
	b, err := os.ReadFile(namespaceFile)
	if err != nil {
		return "", fmt.Errorf("namespace not set: pass --kubernetes-namespace or set POD_NAMESPACE: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ResolveIdentity returns explicit if set, then $POD_NAME, then the hostname.
func ResolveIdentity(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if n := os.Getenv("POD_NAME"); n != "" {
		return n, nil
	}
	return os.Hostname()
}

// NewIncarnation returns a random ID for one run of a process. A replica's
// identity (its pod name) is reused when the pod restarts, and two processes
// can run under one name (a force-deleted pod that is still running, or the
// same --kubernetes-identity on every replica). A Lease's holderIdentity is
// <identity>/<incarnation>, or <identity> with the incarnation in
// AnnotationIncarnation for membership leases, so these processes never
// share one.
func NewIncarnation() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand never fails on supported platforms
	return hex.EncodeToString(b)
}

// Hash returns a DNS-label-safe digest of s.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:40]
}

// Name builds a deterministic object name "<prefix>-<hash(key)>".
func Name(prefix, key string) string {
	return prefix + "-" + Hash(key)
}

// PullKey identifies a pull request across VCS hosts.
func PullKey(vcsHostname, repoFullName string, pullNum int) string {
	return fmt.Sprintf("%s::%s::%d", vcsHostname, repoFullName, pullNum)
}

// Labels returns the common labels for an object of the given type.
func Labels(typ string, extra map[string]string) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue, LabelType: typ}
	for k, v := range extra {
		l[k] = v
	}
	return l
}
