package gke

import (
	"context"
	"sort"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CreateCluster starts a real local cluster and returns the operation that
// tracks it. GKE is asynchronous: the cluster is PROVISIONING until the runner
// finishes, then RUNNING, and the caller polls the operation to learn when.
func (s *Service) CreateCluster(ctx context.Context, req *containerpb.CreateClusterRequest) (*containerpb.Operation, error) {
	cluster := req.GetCluster()
	if cluster == nil || cluster.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster.name is required")
	}
	sc := scopeOf(req.GetParent(), req.GetProjectId(), req.GetZone())
	if sc.project == "" || sc.location == "" {
		return nil, status.Error(codes.InvalidArgument, "a project and location are required")
	}

	if !s.runner.available(ctx) {
		return nil, status.Error(codes.FailedPrecondition,
			"no local Kubernetes backend is available; install kind and a container runtime")
	}

	s.mu.Lock()
	if _, _, err := s.kv.Get(ctx, clusterKey(sc.project, sc.location, cluster.GetName())); err == nil {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "cluster %q already exists", cluster.GetName())
	}

	cluster.Status = containerpb.Cluster_PROVISIONING
	cluster.SelfLink = "projects/" + sc.project + "/locations/" + sc.location + "/clusters/" + cluster.GetName()
	cluster.Location = sc.location
	cluster.CreateTime = s.clk.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := s.putCluster(ctx, sc, cluster); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	op := s.newOperation(sc, containerpb.Operation_CREATE_CLUSTER, cluster.GetName())
	s.mu.Unlock()

	// The real cluster comes up on its own goroutine; the operation reports
	// PROVISIONING until it is ready. Sync waits on this.
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		endpoint, err := s.runner.create(context.WithoutCancel(ctx), cluster.GetName())
		s.mu.Lock()
		stored, gerr := s.getCluster(ctx, sc, cluster.GetName())
		if gerr == nil {
			if err != nil {
				stored.Status = containerpb.Cluster_ERROR
				stored.StatusMessage = err.Error()
			} else {
				stored.Status = containerpb.Cluster_RUNNING
				stored.Endpoint = endpoint
			}
			_ = s.putCluster(ctx, sc, stored)
		}
		s.mu.Unlock()
		s.finish(op.GetName(), err)
	}()
	return op, nil
}

func (s *Service) GetCluster(ctx context.Context, req *containerpb.GetClusterRequest) (*containerpb.Cluster, error) {
	sc := scopeOf(req.GetName(), req.GetProjectId(), req.GetZone())
	return s.getCluster(ctx, sc, clusterID(req.GetName(), req.GetClusterId()))
}

func (s *Service) ListClusters(ctx context.Context, req *containerpb.ListClustersRequest) (*containerpb.ListClustersResponse, error) {
	sc := scopeOf(req.GetParent(), req.GetProjectId(), req.GetZone())
	entries, _, err := s.kv.List(ctx, clusterPrefix(sc.project, sc.location), 0, "")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing clusters: %v", err)
	}
	out := make([]*containerpb.Cluster, 0, len(entries))
	for _, kv := range entries {
		var c containerpb.Cluster
		if err := unmarshal.Unmarshal(kv.Val, &c); err != nil {
			return nil, status.Errorf(codes.Internal, "decoding cluster: %v", err)
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return &containerpb.ListClustersResponse{Clusters: out}, nil
}

// DeleteCluster tears down the real cluster and returns the tracking operation.
func (s *Service) DeleteCluster(ctx context.Context, req *containerpb.DeleteClusterRequest) (*containerpb.Operation, error) {
	sc := scopeOf(req.GetName(), req.GetProjectId(), req.GetZone())
	name := clusterID(req.GetName(), req.GetClusterId())

	s.mu.Lock()
	cluster, err := s.getCluster(ctx, sc, name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	cluster.Status = containerpb.Cluster_STOPPING
	_ = s.putCluster(ctx, sc, cluster)
	op := s.newOperation(sc, containerpb.Operation_DELETE_CLUSTER, name)
	s.mu.Unlock()

	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		err := s.runner.delete(context.WithoutCancel(ctx), name)
		s.mu.Lock()
		_ = s.kv.Delete(ctx, clusterKey(sc.project, sc.location, name), 0)
		s.mu.Unlock()
		s.finish(op.GetName(), err)
	}()
	return op, nil
}

func (s *Service) GetOperation(ctx context.Context, req *containerpb.GetOperationRequest) (*containerpb.Operation, error) {
	name := req.GetOperationId()
	if name == "" {
		name = lastSegment(req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[name]
	if op == nil {
		return nil, status.Errorf(codes.NotFound, "operation %q not found", name)
	}
	// A clone: the stored op is still being mutated by a running action.
	return proto.Clone(op.pb).(*containerpb.Operation), nil
}

// clusterID takes the cluster name from the modern name field or the legacy id.
func clusterID(name, id string) string {
	if id != "" {
		return id
	}
	return lastSegment(name)
}

func lastSegment(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return name
}
