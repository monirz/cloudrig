package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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
	opSeq      uint64                // monotonic, so op IDs are unique regardless of the clock
	inFlight   sync.WaitGroup
}

// operation tracks an async cluster action, which is how GKE reports create and
// delete: the call returns an operation, and the caller polls it to DONE.
type operation struct {
	pb *containerpb.Operation
}

// opKey scopes an operation by project and location, so a caller cannot poll an
// operation identifier that belongs to a different project and read back its
// cluster, status and error.
func opKey(sc scope, id string) string {
	return sc.project + "/" + sc.location + "/" + id
}

// New wires a service. runner defaults to kind; a test injects a fake.
func New(kv store.Store, clk clock.Clock) *Service {
	return &Service{
		kv:         kv,
		clk:        clk,
		runner:     chooseRunner(context.Background()),
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

// physKeyPrefix namespaces the cluster-tuple -> physical-name records.
const physKeyPrefix = "gke/phys/"

// physMaxLen is the physical name budget: k3d caps a cluster name at 32 chars,
// and the runner prefixes it with "cloudrig-" (9).
const physMaxLen = 32 - len("cloudrig-")

func physKey(sc scope, name string) string {
	return physKeyPrefix + sc.project + "/" + sc.location + "/" + name
}

// candidateName is the preferred backend (k3d/kind) cluster name for a scoped
// cluster: a readable label from the cluster name plus a hash of the full scope
// so two projects do not start from the same string. It is only a starting
// point — assignPhysical guarantees uniqueness, because a hash alone can
// collide. Bounded to physMaxLen with room for a disambiguating suffix.
func candidateName(sc scope, name string) string {
	sum := sha256.Sum256([]byte(sc.project + "\x00" + sc.location + "\x00" + name))
	label := sanitizeLabel(name)
	if len(label) > 8 {
		label = strings.TrimRight(label[:8], "-")
	}
	if label == "" {
		label = "c"
	}
	return label + "-" + hex.EncodeToString(sum[:6]) // <= 8 + 1 + 12 = 21
}

// assignPhysical picks a backend cluster name no other cluster is using and
// records it under the cluster's tuple. Called under s.mu, so two concurrent
// creates cannot claim the same name. Storing the assignment (rather than
// recomputing a hash on demand) makes the mapping injective: distinct clusters
// never share one physical cluster even when their names hash alike, and the
// record survives a restart so delete resolves the same name.
func (s *Service) assignPhysical(ctx context.Context, sc scope, name string) (string, error) {
	entries, _, err := s.kv.List(ctx, physKeyPrefix, 0, "")
	if err != nil {
		return "", status.Errorf(codes.Internal, "listing physical names: %v", err)
	}
	used := make(map[string]bool, len(entries))
	for _, e := range entries {
		used[string(e.Val)] = true
	}

	base := candidateName(sc, name)
	cand := base
	for i := 2; used[cand]; i++ {
		suffix := "-" + strconv.Itoa(i)
		trimmed := base
		if len(trimmed) > physMaxLen-len(suffix) {
			trimmed = strings.TrimRight(trimmed[:physMaxLen-len(suffix)], "-")
		}
		cand = trimmed + suffix
	}
	if _, err := s.kv.Put(ctx, physKey(sc, name), []byte(cand), 0); err != nil {
		return "", status.Errorf(codes.Internal, "recording physical name: %v", err)
	}
	return cand, nil
}

// physicalOf resolves a cluster's assigned backend name.
func (s *Service) physicalOf(ctx context.Context, sc scope, name string) (string, error) {
	v, _, err := s.kv.Get(ctx, physKey(sc, name))
	if err != nil {
		return "", status.Errorf(codes.NotFound, "no physical name for cluster %q", name)
	}
	return string(v), nil
}

// sanitizeLabel lowercases name and keeps only DNS-label characters.
func sanitizeLabel(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteByte(byte(r))
		}
	}
	return strings.Trim(b.String(), "-")
}

// newOperation records a pending operation and returns it. Callers hold s.mu.
func (s *Service) newOperation(sc scope, opType containerpb.Operation_Type, cluster string) *containerpb.Operation {
	s.opSeq++
	id := fmt.Sprintf("operation-%d-%d", s.clk.Now().UnixNano(), s.opSeq)
	pb := &containerpb.Operation{
		Name:          id,
		OperationType: opType,
		Status:        containerpb.Operation_RUNNING,
		SelfLink:      fmt.Sprintf("projects/%s/locations/%s/operations/%s", sc.project, sc.location, id),
		TargetLink:    fmt.Sprintf("projects/%s/locations/%s/clusters/%s", sc.project, sc.location, cluster),
		Zone:          sc.location,
		StartTime:     s.clk.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	s.operations[opKey(sc, id)] = &operation{pb: pb}
	// A clone for the response: the stored one is mutated by finish under the
	// lock, and a caller must not read a proto another goroutine is writing.
	return proto.Clone(pb).(*containerpb.Operation)
}

// finish flips an operation to DONE, recording an error message if the action
// failed. Safe to call from the action goroutine.
func (s *Service) finish(sc scope, name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[opKey(sc, name)]
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
