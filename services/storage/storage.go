// Package storage is the Cloud Storage semantics layer: buckets, objects,
// generations and preconditions.
//
// It speaks no HTTP and no JSON envelopes — handlers translate. Keeping the
// rules here means the awkward parts (generation allocation, precondition
// atomicity, delimiter rollup) are testable without a server.
package storage

import (
	"context"
	"time"

	"strconv"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/events"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

// blobRef is blob.Ref, aliased so this package's signatures do not spell the
// blob package in every line.
type blobRef = blob.Ref

// Service holds the metadata store, the blob tree and the clock.
type Service struct {
	kv    store.Store
	blobs *blob.Store
	clk   clock.Clock
	bus   *events.Bus
}

// New wires a service. The blob tree holds payloads; kv holds only metadata.
// A nil bus publishes nothing.
func New(kv store.Store, blobs *blob.Store, clk clock.Clock, bus *events.Bus) *Service {
	return &Service{kv: kv, blobs: blobs, clk: clk, bus: bus}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Bucket is a bucket's metadata.
type Bucket struct {
	Name         string `json:"name"`
	Project      string `json:"project"`
	Location     string `json:"location"`
	StorageClass string `json:"storageClass"`
	// Versioning off is the GCS default: an overwrite discards the previous
	// generation and a delete removes it. On, both are kept.
	Versioning     bool      `json:"versioning"`
	Metageneration int64     `json:"metageneration"`
	Created        time.Time `json:"timeCreated"`
	Updated        time.Time `json:"updated"`
}

// Defaults applied when a caller does not say, matching what GCS assumes.
const (
	DefaultLocation     = "US"
	DefaultStorageClass = "STANDARD"
)

// Reset clears state. An empty project clears everything.
//
// Blobs are only reclaimed on a full reset: they are content-addressed and
// shared, so knowing which are unreferenced after clearing one project would
// need refcounting that nothing else wants. A scoped reset leaves them, and a
// full one removes the tree.
func (s *Service) Reset(ctx context.Context, project string) error {
	prefix := ""
	if project != "" {
		prefix = resource.ProjectPrefix(project)
	}
	if err := s.kv.Reset(ctx, prefix); err != nil {
		return gerr.Wrap(err, gerr.Internal, "clearing metadata")
	}

	if project == "" {
		// The bucket index lives outside the per-project tree, so an empty
		// prefix has already taken it; a scoped reset must leave other
		// projects' claims alone.
		if err := s.blobs.Reset(); err != nil {
			return gerr.Wrap(err, gerr.Internal, "clearing stored content")
		}
		return nil
	}
	return s.dropBucketClaims(ctx, project)
}

// dropBucketClaims releases the global name claims a project held, which a
// prefix reset cannot reach.
func (s *Service) dropBucketClaims(ctx context.Context, project string) error {
	entries, _, err := s.kv.List(ctx, "bx/", 0, "")
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "listing bucket claims")
	}
	for _, kv := range entries {
		if string(kv.Val) != project {
			continue
		}
		if err := s.kv.Delete(ctx, kv.Key, 0); err != nil {
			return gerr.Wrap(err, gerr.Internal, "releasing a bucket claim")
		}
	}
	return nil
}
