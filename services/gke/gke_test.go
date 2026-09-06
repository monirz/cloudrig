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

const parent = "projects/p/locations/us-central1"

// TestCreateProvisionsThenRuns is the operation lifecycle: create returns a
// RUNNING operation and a PROVISIONING cluster, and once the runner finishes
// the operation is DONE and the cluster RUNNING with an endpoint.
func TestCreateProvisionsThenRuns(t *testing.T) {
	t.Parallel()

	f := &fakeRunner{}
	s := newTest(t, f)
	ctx := context.Background()

	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent:  parent,
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
		Name: parent + "/operations/" + op.GetName(),
	})
	if done.GetStatus() != containerpb.Operation_DONE {
		t.Errorf("operation after Sync = %v, want DONE", done.GetStatus())
	}
	cluster, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/dev"})
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
		Parent: parent, Cluster: &containerpb.Cluster{Name: "broken"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Sync()

	done, _ := s.GetOperation(ctx, &containerpb.GetOperationRequest{OperationId: op.GetName()})
	if done.GetStatus() != containerpb.Operation_DONE || done.GetError() == nil {
		t.Errorf("operation = %v err=%v, want DONE with an error", done.GetStatus(), done.GetError())
	}
	cluster, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/broken"})
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
		Parent: parent, Cluster: &containerpb.Cluster{Name: "temp"},
	})
	s.Sync()

	if _, err := s.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{
		Name: parent + "/clusters/temp",
	}); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	s.Sync()

	if _, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: parent + "/clusters/temp",
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
		Parent: parent, Cluster: &containerpb.Cluster{Name: "dup"},
	})
	if _, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{
		Parent: parent, Cluster: &containerpb.Cluster{Name: "dup"},
	}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v, want AlreadyExists", err)
	}
	if _, err := s.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: parent + "/clusters/ghost",
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
		Parent: parent, Cluster: &containerpb.Cluster{Name: "x"},
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
