// Package storage is the Cloud Storage semantics layer: buckets, objects,
// generations and preconditions.
//
// It speaks no HTTP and no JSON envelopes — handlers translate. Keeping the
// rules here means the awkward parts (generation allocation, precondition
// atomicity, delimiter rollup) are testable without a server.
package storage

import (
	"time"

	"github.com/monirz/cloudrig/core/clock"
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
}

// New wires a service. The blob tree holds payloads; kv holds only metadata.
func New(kv store.Store, blobs *blob.Store, clk clock.Clock) *Service {
	return &Service{kv: kv, blobs: blobs, clk: clk}
}

// Bucket is a bucket's metadata.
type Bucket struct {
	Name           string    `json:"name"`
	Project        string    `json:"project"`
	Location       string    `json:"location"`
	StorageClass   string    `json:"storageClass"`
	Metageneration int64     `json:"metageneration"`
	Created        time.Time `json:"timeCreated"`
	Updated        time.Time `json:"updated"`
}

// Defaults applied when a caller does not say, matching what GCS assumes.
const (
	DefaultLocation     = "US"
	DefaultStorageClass = "STANDARD"
)
