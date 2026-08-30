package storage

import (
	"context"
	"sort"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
)

// DefaultMaxResults matches what GCS returns when a client does not ask.
const DefaultMaxResults = 1000

// scanChunk is how many live pointers are read from the store per round.
//
// It is a scan budget, not a page size: collapsing a thousand keys under one
// delimiter prefix yields a single result, so a listing may have to read far
// more entries than it emits.
const scanChunk = 512

// ListRequest describes an object listing.
type ListRequest struct {
	Bucket     string
	Prefix     string
	Delimiter  string
	MaxResults int
	PageToken  string
}

// ListResult is one page.
type ListResult struct {
	Objects []Object
	// Prefixes are the delimiter rollups: the synthetic "directories" GCS
	// reports in prefixes[].
	Prefixes      []string
	NextPageToken string
}

// ListObjects returns one page of a bucket's live objects.
//
// Listing walks live pointers, not generation keys: one entry per visible
// object, rather than every version it has ever had.
func (s *Service) ListObjects(ctx context.Context, project string, req ListRequest) (ListResult, error) {
	if _, _, err := s.bucket(ctx, project, req.Bucket); err != nil {
		return ListResult{}, err
	}

	limit := req.MaxResults
	if limit <= 0 {
		limit = DefaultMaxResults
	}

	livePrefix := resource.LivePrefix(project, req.Bucket)
	scanPrefix := livePrefix + req.Prefix

	var (
		result    ListResult
		seen      = map[string]bool{} // rollups already emitted
		token     = req.PageToken
		exhausted bool
	)

	for !exhausted {
		entries, next, err := s.kv.List(ctx, scanPrefix, scanChunk, token)
		if err != nil {
			return ListResult{}, gerr.Wrap(err, gerr.InvalidArgument, "%s", err.Error()).
				WithHTTPStatus(400).
				WithReason("invalid")
		}
		exhausted = next == ""
		token = next

		for _, kv := range entries {
			name, err := resource.ObjectName(project, req.Bucket, kv.Key)
			if err != nil {
				return ListResult{}, gerr.Wrap(err, gerr.Internal, "decoding a live pointer key")
			}

			if rollup, ok := rollupOf(name, req.Prefix, req.Delimiter); ok {
				if seen[rollup] {
					continue
				}
				seen[rollup] = true
				result.Prefixes = append(result.Prefixes, rollup)
			} else {
				obj, err := s.objectFor(ctx, project, req.Bucket, name, kv.Val)
				if err != nil {
					return ListResult{}, err
				}
				result.Objects = append(result.Objects, obj)
			}

			// maxResults counts objects and rollups together, as GCS does.
			if len(result.Objects)+len(result.Prefixes) >= limit {
				// More may remain under this same key's prefix, so the token
				// names where this page stopped rather than where the store's
				// page did.
				result.NextPageToken = store.PageToken(kv.Key)
				sort.Strings(result.Prefixes)
				return result, nil
			}
		}
	}

	sort.Strings(result.Prefixes)
	return result, nil
}

// rollupOf reports the synthetic prefix a name collapses into, if any.
//
// Only the part after the request prefix is examined: listing "a/" with
// delimiter "/" must roll up "a/b/c" to "a/b/", not to "a/".
func rollupOf(name, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return "", false
	}
	rest := strings.TrimPrefix(name, prefix)
	i := strings.Index(rest, delimiter)
	if i < 0 {
		return "", false
	}
	return prefix + rest[:i+len(delimiter)], true
}

// objectFor resolves a live pointer to its generation's metadata.
func (s *Service) objectFor(ctx context.Context, project, bucket, name string, pointer []byte) (Object, error) {
	l, err := decodeLive(pointer)
	if err != nil {
		return Object{}, gerr.Wrap(err, gerr.Internal, "decoding the live pointer")
	}
	return s.readGeneration(ctx, project, bucket, name, l.Generation)
}
