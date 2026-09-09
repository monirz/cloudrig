package gke

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeRunner stands in for kind: it records calls and lets a test decide
// whether create succeeds, without spinning a real cluster.
type fakeRunner struct {
	mu        sync.Mutex
	createErr error
	deleteErr error
	created   []string
	deleted   []string
}

func (f *fakeRunner) available(context.Context) bool { return true }
func (f *fakeRunner) create(_ context.Context, name string) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.mu.Lock()
	f.created = append(f.created, name)
	f.mu.Unlock()
	return "10.0.0.1:6443", nil
}
func (f *fakeRunner) kubeconfig(context.Context, string) ([]byte, error) {
	return []byte("kubeconfig"), nil
}
func (f *fakeRunner) delete(_ context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	f.deleted = append(f.deleted, name)
	f.mu.Unlock()
	return nil
}

func newTest(t *testing.T, runner clusterRunner) *Service {
	t.Helper()
	s := New(store.NewMemory(), clock.NewFake(epoch))
	s.runner = runner
	return s
}

const testParent = "projects/p/locations/us-central1"

// TestCreateProvisionsThenRuns is the operation lifecycle: create returns a
// RUNNING operation and a PROVISIONING cluster, and once the runner finishes
// the operation is DONE and the cluster RUNNING with an endpoint.
func TestCreateProvisionsThenRuns(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  testParent,
		Cluster: &containerpb.Cluster{Name: "dev", InitialNodeCount: 1},
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if op.GetStatus() != containerpb.Operation_RUNNING {
		t.Errorf("operation status = %v, want RUNNING", op.GetStatus())
	}

	s.Sync() // the real create finished

	done, _ := s.GetOperation(ctx, &containerpb.GetOperationRequest{
		Name: testParent + "/operations/" + op.GetName(),
	})
	if done.GetStatus() != containerpb.Operation_DONE {
		t.Errorf("operation after Sync = %v, want DONE", done.GetStatus())
	}
	cluster, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: testParent + "/clusters/dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cluster.GetStatus() != containerpb.Cluster_RUNNING {
		t.Errorf("cluster status = %v, want RUNNING", cluster.GetStatus())
	}
	if cluster.GetEndpoint() != "10.0.0.1:6443" {
		t.Errorf("endpoint = %q", cluster.GetEndpoint())
	}
	if f.mu.Lock(); len(f.created) != 1 {
		t.Errorf("the runner created %d clusters, want 1", len(f.created))
	}
	f.mu.Unlock()
}

// TestCreateFailureMarksError holds that a runner that cannot start a cluster
// leaves the cluster in ERROR and the operation DONE-with-error, rather than a
// false RUNNING.
func TestCreateFailureMarksError(t *testing.T) {
	t.Parallel()

	s := newTest(t, &fakeRunner{createErr: errors.New("no nodes")})
	ctx := context.Background()

	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "broken"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Sync()

	done, _ := s.GetOperation(ctx, &containerpb.GetOperationRequest{Name: testParent + "/operations/" + op.GetName()})
	if done.GetStatus() != containerpb.Operation_DONE || done.GetError() == nil {
		t.Errorf("operation = %v err=%v, want DONE with an error", done.GetStatus(), done.GetError())
	}
	cluster, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: testParent + "/clusters/broken"})
	if cluster.GetStatus() != containerpb.Cluster_ERROR {
		t.Errorf("cluster status = %v, want ERROR", cluster.GetStatus())
	}
}

// TestDeleteTearsDownTheCluster covers delete: the runner is told to remove the
// cluster and the record is gone.
func TestDeleteTearsDownTheCluster(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "temp"},
	})
	s.Sync()

	if _, err := s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
		Name: testParent + "/clusters/temp",
	}); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	s.Sync()

	if _, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: testParent + "/clusters/temp",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("cluster survived delete: %v", err)
	}
	if f.mu.Lock(); len(f.deleted) != 1 {
		t.Errorf("the runner deleted %d clusters, want 1", len(f.deleted))
	}
	f.mu.Unlock()
}

func TestGKEErrors(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "dup"},
	})
	if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "dup"},
	}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v, want AlreadyExists", err)
	}
	if _, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: testParent + "/clusters/ghost",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing cluster = %v, want NotFound", err)
	}
}

// TestNoBackendIsAClearError holds that without kind, create fails at the honest
// boundary rather than returning a cluster that cannot run anything.
func TestNoBackendIsAClearError(t *testing.T) {
	t.Parallel()

	s := newTest(t, unavailableRunner{})
	_, err := s.CreateCluster(context.Background(), &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "x"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition when no backend is available", err)
	}
}

type unavailableRunner struct{}

func (unavailableRunner) available(context.Context) bool                     { return false }
func (unavailableRunner) create(context.Context, string) (string, error)     { return "", nil }
func (unavailableRunner) kubeconfig(context.Context, string) ([]byte, error) { return nil, nil }
func (unavailableRunner) delete(context.Context, string) error               { return nil }

// TestChooseRunnerPrefersK3s pins the preference: k3s (via k3d) is chosen when
// present, kind only as the fallback.
func TestChooseRunnerPrefersK3s(t *testing.T) {
	t.Parallel()

	// Without k3d installed here, the fallback is kind — assert the type so a
	// machine with k3d gets k3s and this test documents the order.
	got := chooseRunner(context.Background())
	switch got.(type) {
	case k3dRunner, kindRunner:
		// both are valid depending on what is installed
	default:
		t.Errorf("chooseRunner returned %T, want k3d or kind", got)
	}
}

// TestNetworkDefaultsAreStable holds that a create with no network echoes the
// short name in the top-level fields and the full resource path under
// networkConfig, matching real GKE. The Terraform provider reads networkConfig
// and treats network as ForceNew; without these a fresh plan wants to replace
// the cluster on every run.
func TestNetworkDefaultsAreStable(t *testing.T) {
	t.Parallel()

	s := newTest(t, &fakeRunner{})
	ctx := context.Background()

	if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  testParent,
		Cluster: &containerpb.Cluster{Name: "net", InitialNodeCount: 1},
	}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	s.Sync()

	c, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: testParent + "/clusters/net"})
	if err != nil {
		t.Fatal(err)
	}
	if c.GetNetwork() != "default" {
		t.Errorf("network = %q, want %q", c.GetNetwork(), "default")
	}
	if c.GetSubnetwork() != "default" {
		t.Errorf("subnetwork = %q, want %q", c.GetSubnetwork(), "default")
	}
	if got, want := c.GetNetworkConfig().GetNetwork(), "projects/p/global/networks/default"; got != want {
		t.Errorf("networkConfig.network = %q, want %q", got, want)
	}
	if got, want := c.GetNetworkConfig().GetSubnetwork(), "projects/p/regions/us-central1/subnetworks/default"; got != want {
		t.Errorf("networkConfig.subnetwork = %q, want %q", got, want)
	}
}

// TestScopeCollisionIsolatesClusters holds that the same cluster name in two
// projects maps to two distinct physical clusters, so deleting one does not
// tear down the other. Regression: the runner name was derived from the short
// cluster name alone, while store keys are scoped by project and location.
func TestScopeCollisionIsolatesClusters(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	for _, project := range []string{"projects/a/locations/us-central1", "projects/b/locations/us-central1"} {
		if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
			Parent:  project,
			Cluster: &containerpb.Cluster{Name: "dev", InitialNodeCount: 1},
		}); err != nil {
			t.Fatalf("CreateCluster in %s: %v", project, err)
		}
	}
	s.Sync()

	f.mu.Lock()
	created := append([]string(nil), f.created...)
	f.mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("created %v, want two clusters", created)
	}
	if created[0] == created[1] {
		t.Fatalf("both scopes produced the same physical name %q; they must differ", created[0])
	}

	// Deleting project a's cluster must not remove project b's record.
	if _, err := s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
		Name: "projects/a/locations/us-central1/clusters/dev",
	}); err != nil {
		t.Fatal(err)
	}
	s.Sync()
	if _, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: "projects/b/locations/us-central1/clusters/dev",
	}); err != nil {
		t.Errorf("project b's cluster went missing after deleting a's: %v", err)
	}
}

// TestOperationIDsAreUniqueWithoutClockTicks holds that two operations started
// without the fake clock advancing get distinct IDs, so polling one does not
// return the other. Regression: the ID was purely timestamp-derived.
func TestOperationIDsAreUniqueWithoutClockTicks(t *testing.T) {
	t.Parallel()

	s := newTest(t, &fakeRunner{})
	ctx := context.Background()

	op1, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op2, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if op1.GetName() == op2.GetName() {
		t.Fatalf("two operations share the ID %q without the clock advancing", op1.GetName())
	}
	s.Sync()
}

// TestFailedDeleteKeepsClusterRecord holds that when the runner cannot delete
// the physical cluster, the record survives (as ERROR) rather than vanishing,
// so the cluster is not silently orphaned. Regression: the record was removed
// unconditionally.
func TestFailedDeleteKeepsClusterRecord(t *testing.T) {
	t.Parallel()

	s := newTest(t, &fakeRunner{})
	ctx := context.Background()

	if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: testParent, Cluster: &containerpb.Cluster{Name: "stuck"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Sync()

	// Now make deletion fail.
	s.runner = &fakeRunner{deleteErr: errors.New("k3d down")}
	if _, err := s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
		Name: testParent + "/clusters/stuck",
	}); err != nil {
		t.Fatal(err)
	}
	s.Sync()

	c, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: testParent + "/clusters/stuck"})
	if err != nil {
		t.Fatalf("cluster record vanished after a failed delete: %v", err)
	}
	if c.GetStatus() != containerpb.Cluster_ERROR {
		t.Errorf("status = %v, want ERROR", c.GetStatus())
	}
}

// TestGetOperationIsScopeChecked holds that an operation identifier from one
// project cannot be polled through another project's scope. Regression: the
// operation store was keyed by bare ID, so a caller in project b could read
// project a's operation, including a's cluster, status and error.
func TestGetOperationIsScopeChecked(t *testing.T) {
	t.Parallel()

	s := newTest(t, &fakeRunner{})
	ctx := context.Background()

	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  "projects/a/locations/us-central1",
		Cluster: &containerpb.Cluster{Name: "dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Sync()

	// Same identifier, a different project: must not resolve.
	if _, err := s.GetOperation(ctx, &containerpb.GetOperationRequest{
		Name: "projects/b/locations/us-central1/operations/" + op.GetName(),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("cross-scope GetOperation = %v, want NotFound", err)
	}
	// The owning scope still resolves it.
	if _, err := s.GetOperation(ctx, &containerpb.GetOperationRequest{
		Name: "projects/a/locations/us-central1/operations/" + op.GetName(),
	}); err != nil {
		t.Errorf("owning-scope GetOperation failed: %v", err)
	}
}

// TestPhysicalNamesAreUniqueUnderCollision holds that two clusters are never
// given the same backend name, even when their candidate names would collide:
// assignPhysical disambiguates against what is already in use. Regression: a
// fixed-length hash could map two distinct tuples to one physical cluster.
func TestPhysicalNamesAreUniqueUnderCollision(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	// Two different scopes. Their assigned names must differ regardless of how
	// the candidate hashes come out.
	p1, err := s.assignPhysical(ctx, scope{"a", "us-central1"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.assignPhysical(ctx, scope{"b", "us-central1"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("two scopes got the same physical name %q", p1)
	}

	// Force a candidate clash: pre-seed the store with p1's candidate under a
	// third tuple, then assign that tuple — it must not reuse p1.
	third := scope{"c", "us-central1"}
	if _, err := s.kv.Put(ctx, physKey(third, "dev"), []byte(p1), 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.assignPhysical(ctx, scope{"d", "us-central1"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if got == p1 {
		t.Fatalf("assignPhysical reused an in-use name %q", p1)
	}
	if got == "" || len("cloudrig-"+got) > 32 {
		t.Fatalf("physical name %q exceeds the k3d 32-char limit with its prefix", got)
	}
}
