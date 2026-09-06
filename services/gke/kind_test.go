package gke

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// TestKindClusterIsReal is the honest claim: a created cluster is a working
// Kubernetes, not a stub. It is slow (a real kind cluster comes up), needs kind
// and a container runtime, and is skipped without them or under -short.
func TestKindClusterIsReal(t *testing.T) {
	// Opt-in: creating a real kind cluster takes a minute and spins Docker
	// containers, too heavy for every `make check`. Run it deliberately with
	// CLOUDRIG_KIND_TEST=1.
	if os.Getenv("CLOUDRIG_KIND_TEST") == "" {
		t.Skip("set CLOUDRIG_KIND_TEST=1 to run the real-cluster test")
	}
	if !(kindRunner{}).available(context.Background()) {
		t.Skip("kind and a container runtime are not available")
	}

	s := New(store.NewMemory(), clock.Real())
	ctx := context.Background()
	const name = "conformance"

	if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  "projects/p/locations/us-central1",
		Cluster: &containerpb.Cluster{Name: name, InitialNodeCount: 1},
	}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
			Name: "projects/p/locations/us-central1/clusters/" + name,
		})
		s.Sync()
	})
	s.Sync() // wait for the real cluster to come up

	cluster, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: "projects/p/locations/us-central1/clusters/" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cluster.GetStatus() != containerpb.Cluster_RUNNING {
		t.Fatalf("cluster status = %v: %s", cluster.GetStatus(), cluster.GetStatusMessage())
	}

	// The proof it is real: kubectl against the cluster's kubeconfig lists a
	// running node.
	kubeconfig, err := (kindRunner{}).kubeconfig(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := dir + "/kubeconfig"
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("kubectl", "--kubeconfig", path, "get", "nodes").CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get nodes: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Ready") {
		t.Errorf("no Ready node in the cluster:\n%s", out)
	}
}
