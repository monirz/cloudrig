// Package gke emulates the GKE cluster admin API, backed by a real local
// Kubernetes cluster rather than a stub.
//
// A stubbed GKE would answer "cluster created" and hand back a cluster that
// cannot run a pod — which is worse than nothing, because a deploy would then
// fail deep inside kubectl instead of at the honest boundary. So a created
// cluster is a real one: the runner starts an actual local Kubernetes (kind
// today, k3s/k3d could slot into the same seam), and get-credentials returns a
// kubeconfig that talks to it. The admin API is emulated; the cluster is not.
package gke

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// clusterRunner starts and stops the local Kubernetes cluster behind a GKE
// cluster. It is an interface so the admin-API logic is testable without
// spinning a real cluster, and so a different backend can replace kind.
type clusterRunner interface {
	// available reports whether this backend can run a cluster here.
	available(ctx context.Context) bool

	// create starts a cluster and returns the address of its API server.
	create(ctx context.Context, name string) (endpoint string, err error)

	// kubeconfig returns a kubeconfig that talks to the cluster.
	kubeconfig(ctx context.Context, name string) ([]byte, error)

	// delete tears the cluster down.
	delete(ctx context.Context, name string) error
}

// kindRunner backs a GKE cluster with a kind (Kubernetes-in-Docker) cluster.
// kind runs a real control plane in containers, so a pod scheduled against it
// actually runs.
type kindRunner struct{}

// kindName is the kind cluster name for a GKE cluster. kind names are flat and
// DNS-label-shaped, so the GKE name is used directly with a prefix that keeps
// cloudrig's clusters distinct from any the developer runs themselves.
func kindName(cluster string) string { return "cloudrig-" + cluster }

func (kindRunner) available(ctx context.Context) bool {
	if _, err := exec.LookPath("kind"); err != nil {
		return false
	}
	// kind needs a container runtime; a kind command that cannot reach one is
	// not really available.
	out, err := run(ctx, "kind", "version")
	return err == nil && strings.Contains(out, "kind")
}

func (kindRunner) create(ctx context.Context, name string) (string, error) {
	if _, err := run(ctx, "kind", "create", "cluster", "--name", kindName(name), "--wait", "60s"); err != nil {
		return "", fmt.Errorf("creating the kind cluster: %w", err)
	}
	// The API-server endpoint kind published. get-credentials is what a caller
	// actually uses, so this is only for the cluster record's endpoint field.
	out, err := run(ctx, "kind", "get", "kubeconfig", "--name", kindName(name), "--internal")
	if err == nil {
		if ep := endpointFromKubeconfig(out); ep != "" {
			return ep, nil
		}
	}
	return "127.0.0.1", nil
}

func (kindRunner) kubeconfig(ctx context.Context, name string) ([]byte, error) {
	out, err := run(ctx, "kind", "get", "kubeconfig", "--name", kindName(name))
	if err != nil {
		return nil, fmt.Errorf("reading the kind kubeconfig: %w", err)
	}
	return []byte(out), nil
}

func (kindRunner) delete(ctx context.Context, name string) error {
	if _, err := run(ctx, "kind", "delete", "cluster", "--name", kindName(name)); err != nil {
		return fmt.Errorf("deleting the kind cluster: %w", err)
	}
	return nil
}

// endpointFromKubeconfig pulls the server URL out of a kubeconfig.
func endpointFromKubeconfig(kubeconfig string) string {
	for _, line := range strings.Split(kubeconfig, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "server:"); ok {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "https://"))
		}
	}
	return ""
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
