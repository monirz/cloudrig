package storage

import (
	"context"
	"errors"
	"strconv"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
)

// maxRefAttempts bounds the compare-and-swap retry on a reference count. It
// only spins while another writer is changing the same count.
const maxRefAttempts = 64

// retainBlob records one more reference to stored content.
func (s *Service) retainBlob(ctx context.Context, sha string) error {
	return s.adjustRefs(ctx, sha, +1)
}

// releaseBlob drops a reference and removes the content at zero.
func (s *Service) releaseBlob(ctx context.Context, sha string) error {
	return s.adjustRefs(ctx, sha, -1)
}

func (s *Service) adjustRefs(ctx context.Context, sha string, delta int) error {
	if sha == "" {
		return nil
	}
	key := resource.BlobRefs(sha)

	for attempt := 0; attempt < maxRefAttempts; attempt++ {
		raw, version, err := s.kv.Get(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return gerr.Wrap(err, gerr.Internal, "reading a content reference count")
		}

		count := 0
		if version != 0 {
			count, _ = strconv.Atoi(string(raw))
		}
		count += delta

		if count > 0 {
			if _, err := s.kv.Put(ctx, key, []byte(strconv.Itoa(count)), version); err != nil {
				if errors.Is(err, store.ErrVersionMismatch) {
					continue
				}
				return gerr.Wrap(err, gerr.Internal, "updating a content reference count")
			}
			return nil
		}

		// Nothing references it any more. The count goes first: if the process
		// stops between the two, an orphaned file is harmless where a count
		// naming a missing file is not.
		if version != 0 {
			if err := s.kv.Delete(ctx, key, version); err != nil {
				if errors.Is(err, store.ErrVersionMismatch) {
					continue
				}
				if !errors.Is(err, store.ErrNotFound) {
					return gerr.Wrap(err, gerr.Internal, "clearing a content reference count")
				}
			}
		}
		if err := s.blobs.Delete(sha); err != nil {
			return gerr.Wrap(err, gerr.Internal, "removing unreferenced content")
		}
		return nil
	}
	return gerr.New(gerr.Aborted, "too much contention on a content reference count").
		WithHTTPStatus(409).
		WithReason("conflict")
}
