package gke

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Service is the GKE cluster admin API over a real local-cluster runner.
type Service struct {
	containerpb.UnimplementedClusterManagerServer

	kv     store.Store
	clk    clock.Clock
	runner clusterRunner

	mu         sync.Mutex
	operations map[string]*operation // name -> live operation
	inFlight   sync.WaitGroup
}

// operation tracks an async cluster action, which is how GKE reports create and
// delete: the call returns an operation, and the caller polls it to DONE.
type operation struct {
	pb *containerpb.Operation
}

// New wires a service. runner defaults to kind; a test injects a fake.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{
		kv:         kv,
		clk:        clk,
		runner:     kindRunner{},
		operations: map[string]*operation{},
	}
}

// Sync waits for every in-flight cluster action (create/delete) to finish, so a
// test can drive an operation to completion deterministically.
func (s *Service) Sync() { s.inFlight.Wait() }

var (
	marshal   = protojson.MarshalOptions{}
	unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
)

func clusterKey(project, location, name string) string {
	return "gke/c/" + project + "/" + location + "/" + name
}
func clusterPrefix(project, location string) string {
	return "gke/c/" + project + "/" + location + "/"
}

// scope is the project and location a request names, from either the modern
// name field or the legacy project/zone triple.
type scope struct{ project, location string }

func scopeOf(name, project, zone string) scope {
	// The name form wins: projects/{p}/locations/{l}/clusters/{c}.
	if parts := strings.Split(name, "/"); len(parts) >= 4 && parts[0] == "projects" {
		return scope{parts[1], parts[3]}
	}
	return scope{project, zone}
}

func (s *Service) getCluster(ctx context.Context, sc scope, name string) (*containerpb.Cluster, error) {
	raw, _, err := s.kv.Get(ctx, clusterKey(sc.project, sc.location, name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", name)
	}
	var c containerpb.Cluster
	if err := unmarshal.Unmarshal(raw, &c); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding cluster: %v", err)
	}
	return &c, nil
}

func (s *Service) putCluster(ctx context.Context, sc scope, c *containerpb.Cluster) error {
	encoded, err := marshal.Marshal(c)
	if err != nil {
		return status.Errorf(codes.Internal, "encoding cluster: %v", err)
	}
	key := clusterKey(sc.project, sc.location, c.GetName())
	_, v, err := s.kv.Get(ctx, key)
	var ifVersion uint64
	if err == nil {
		ifVersion = v
	}
	_, err = s.kv.Put(ctx, key, encoded, ifVersion)
	return err
}

// newOperation records a pending operation and returns it.
func (s *Service) newOperation(sc scope, opType containerpb.Operation_Type, cluster string) *containerpb.Operation {
	id := fmt.Sprintf("operation-%d", s.clk.Now().UnixNano())
	pb := &containerpb.Operation{
		Name:          id,
		OperationType: opType,
		Status:        containerpb.Operation_RUNNING,
		SelfLink:      fmt.Sprintf("projects/%s/locations/%s/operations/%s", sc.project, sc.location, id),
		TargetLink:    fmt.Sprintf("projects/%s/locations/%s/clusters/%s", sc.project, sc.location, cluster),
		Zone:          sc.location,
		StartTime:     s.clk.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	s.operations[id] = &operation{pb: pb}
	// A clone for the response: the stored one is mutated by finish under the
	// lock, and a caller must not read a proto another goroutine is writing.
	return proto.Clone(pb).(*containerpb.Operation)
}

// finish flips an operation to DONE, recording an error message if the action
// failed. Safe to call from the action goroutine.
func (s *Service) finish(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[name]
	if op == nil {
		return
	}
	op.pb.Status = containerpb.Operation_DONE
	op.pb.EndTime = s.clk.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err != nil {
		op.pb.Error = &statuspb.Status{Message: err.Error()}
		op.pb.StatusMessage = err.Error()
	}
}
