package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
)

// CreateBucket stores a new bucket, failing if the name is taken.
func (s *Service) CreateBucket(ctx context.Context, b Bucket) (Bucket, error) {
	if err := validBucketName(b.Name); err != nil {
		return Bucket{}, err
	}
	if b.Project == "" {
		return Bucket{}, gerr.New(gerr.InvalidArgument, "a bucket needs a project").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("required")
	}

	now := s.clk.Now()
	b.Location = orDefault(b.Location, DefaultLocation)
	b.StorageClass = orDefault(b.StorageClass, DefaultStorageClass)
	b.Metageneration = 1
	b.Created, b.Updated = now, now

	encoded, err := json.Marshal(b)
	if err != nil {
		return Bucket{}, gerr.Wrap(err, gerr.Internal, "encoding bucket metadata")
	}

	// ifVersion 0 is "must not exist", so the duplicate check and the write are
	// one step: two concurrent creates cannot both win.
	if _, err := s.kv.Put(ctx, resource.Bucket(b.Project, b.Name), encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return Bucket{}, conflict("You already own this bucket. Please select another name.", b.Name)
		}
		return Bucket{}, gerr.Wrap(err, gerr.Internal, "storing bucket metadata")
	}
	return b, nil
}

// GetBucket returns a bucket's metadata.
func (s *Service) GetBucket(ctx context.Context, project, name string) (Bucket, error) {
	b, _, err := s.bucket(ctx, project, name)
	return b, err
}

// bucket loads a bucket with the store version its metadata is at, which a
// metageneration precondition needs for its compare-and-swap.
func (s *Service) bucket(ctx context.Context, project, name string) (Bucket, uint64, error) {
	raw, version, err := s.kv.Get(ctx, resource.Bucket(project, name))
	if errors.Is(err, store.ErrNotFound) {
		return Bucket{}, 0, notFoundBucket(name)
	}
	if err != nil {
		return Bucket{}, 0, gerr.Wrap(err, gerr.Internal, "reading bucket metadata")
	}

	var b Bucket
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bucket{}, 0, gerr.Wrap(err, gerr.Internal, "decoding bucket metadata")
	}
	return b, version, nil
}

// ListBuckets returns a project's buckets, by name.
func (s *Service) ListBuckets(ctx context.Context, project string) ([]Bucket, error) {
	prefix := resource.BucketsPrefix(project)
	entries, _, err := s.kv.List(ctx, prefix, 0, "")
	if err != nil {
		return nil, gerr.Wrap(err, gerr.Internal, "listing buckets")
	}

	out := make([]Bucket, 0, len(entries))
	for _, kv := range entries {
		// The prefix also covers each bucket's contents, whose keys carry a
		// further slash. Only the bucket metadata keys are wanted here.
		if strings.Contains(strings.TrimPrefix(kv.Key, prefix), "/") {
			continue
		}
		var b Bucket
		if err := json.Unmarshal(kv.Val, &b); err != nil {
			return nil, gerr.Wrap(err, gerr.Internal, "decoding bucket metadata")
		}
		out = append(out, b)
	}
	return out, nil
}

// DeleteBucket removes an empty bucket.
func (s *Service) DeleteBucket(ctx context.Context, project, name string) error {
	if _, _, err := s.bucket(ctx, project, name); err != nil {
		return err
	}

	// GCS refuses to delete a bucket holding anything. One live pointer is
	// enough to know, so the scan stops there.
	live, _, err := s.kv.List(ctx, resource.LivePrefix(project, name), 1, "")
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "checking whether the bucket is empty")
	}
	if len(live) > 0 {
		return conflict("The bucket you tried to delete is not empty.", name)
	}

	if err := s.kv.Delete(ctx, resource.Bucket(project, name), 0); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundBucket(name)
		}
		return gerr.Wrap(err, gerr.Internal, "deleting bucket metadata")
	}
	return nil
}

// validBucketName applies the rules a client would hit first. Full GCS naming
// is stricter; these are the ones whose absence would corrupt a key.
func validBucketName(name string) error {
	switch {
	case name == "":
		return badBucketName(name, "must not be empty")
	case len(name) < 3 || len(name) > 222:
		return badBucketName(name, "must be between 3 and 222 characters")
	case strings.ContainsAny(name, "/\x00"):
		return badBucketName(name, "must not contain a slash or a NUL")
	}
	return nil
}

func badBucketName(name, why string) error {
	return gerr.Newf(gerr.InvalidArgument, "Invalid bucket name %q: %s", name, why).
		WithHTTPStatus(http.StatusBadRequest).
		WithReason("invalid")
}

func notFoundBucket(name string) error {
	return gerr.Newf(gerr.NotFound, "The specified bucket does not exist: %s", name).
		WithHTTPStatus(http.StatusNotFound).
		WithReason("notFound")
}

func conflict(message, name string) error {
	return gerr.Newf(gerr.AlreadyExists, "%s (%s)", message, name).
		WithHTTPStatus(http.StatusConflict).
		WithReason("conflict")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
