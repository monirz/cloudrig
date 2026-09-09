package gke

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// k3dRunner backs a GKE cluster with k3s, run in Docker by k3d. k3s is the
// lightweight Kubernetes distribution — smaller and faster to start than the
// upstream cluster kind runs — and is what most local GCP emulation settles on.
// This is the preferred backend; kind is the fallback when k3d is absent.
type k3dRunner struct{}

// k3dName is the k3d cluster name for a GKE cluster. k3d prefixes containers
// with "k3d-"; the cloudrig- prefix keeps these distinct from a developer's own.
func k3dName(cluster string) string { return "cloudrig-" + cluster }

func (k3dRunner) available(ctx context.Context) bool {
	if _, err := exec.LookPath("k3d"); err != nil {
		return false
	}
	out, err := run(ctx, "k3d", "version")
	return err == nil && strings.Contains(out, "k3d")
}

func (k3dRunner) create(ctx context.Context, name string) (string, error) {
	// --wait blocks until the API server is ready, so a cluster reported
	// RUNNING actually accepts kubectl.
	if _, err := run(ctx, "k3d", "cluster", "create", k3dName(name), "--wait"); err != nil {
		return "", fmt.Errorf("creating the k3d cluster: %w", err)
	}
	out, err := run(ctx, "k3d", "kubeconfig", "get", k3dName(name))
	if err == nil {
		if ep := endpointFromKubeconfig(out); ep != "" {
			return ep, nil
		}
	}
	return "127.0.0.1", nil
}

func (k3dRunner) kubeconfig(ctx context.Context, name string) ([]byte, error) {
	out, err := run(ctx, "k3d", "kubeconfig", "get", k3dName(name))
	if err != nil {
		return nil, fmt.Errorf("reading the k3d kubeconfig: %w", err)
	}
	return []byte(out), nil
}

func (k3dRunner) delete(ctx context.Context, name string) error {
	if _, err := run(ctx, "k3d", "cluster", "delete", k3dName(name)); err != nil {
		return fmt.Errorf("deleting the k3d cluster: %w", err)
	}
	return nil
}

// chooseRunner picks the local Kubernetes backend, preferring k3s (via k3d) and
// falling back to kind. The choice is made once at startup; a runner that is
// present but has no container runtime still fails at create with a clear error.
func chooseRunner(ctx context.Context) clusterRunner {
	if (k3dRunner{}).available(ctx) {
		return k3dRunner{}
	}
	return kindRunner{}
}
