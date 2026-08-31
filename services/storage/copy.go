package storage

import (
	"context"
	"io"
	"net/http"
	"os"

	"github.com/monirz/cloudrig/core/gerr"
)

// Source names an object to read from.
type Source struct {
	Bucket     string
	Name       string
	Generation *int64
}

// CopyObject writes an existing object's content under a new name.
//
// No bytes move. Content is addressed by its hash, so the copy points at the
// same file and only the reference count changes — a copy of a gibibyte costs
// what a copy of a byte costs.
func (s *Service) CopyObject(ctx context.Context, project string, src Source, dst Write) (Object, error) {
	from, err := s.GetObject(ctx, project, src.Bucket, src.Name, src.Generation)
	if err != nil {
		return Object{}, err
	}
	if _, _, err := s.bucket(ctx, project, dst.Bucket); err != nil {
		return Object{}, err
	}

	// Unstated metadata is inherited, which is what copy means; a caller that
	// wants it changed says so.
	if dst.ContentType == "" {
		dst.ContentType = from.ContentType
	}
	if dst.Metadata == nil {
		dst.Metadata = from.Metadata
	}

	return s.publishRef(ctx, project, dst, blobRef{
		SHA256: from.Blob,
		Size:   from.Size,
		CRC32C: from.CRC32C,
		MD5:    from.MD5,
	})
}

// ComposeObject concatenates sources into one object.
//
// The parts are streamed one after another into the blob tree, so composing
// objects far larger than memory costs the same fixed buffer as any other
// write.
func (s *Service) ComposeObject(ctx context.Context, project string, sources []Source, dst Write) (Object, error) {
	if len(sources) == 0 {
		return Object{}, gerr.New(gerr.InvalidArgument, "compose needs at least one source object").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("required")
	}
	// Real GCS composes at most 32 sources at a time.
	if len(sources) > 32 {
		return Object{}, gerr.Newf(gerr.InvalidArgument,
			"compose accepts at most 32 source objects, got %d", len(sources)).
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("invalid")
	}
	if _, _, err := s.bucket(ctx, project, dst.Bucket); err != nil {
		return Object{}, err
	}

	readers := make([]io.Reader, 0, len(sources))
	open := make([]*os.File, 0, len(sources))
	defer func() {
		for _, f := range open {
			f.Close()
		}
	}()

	for _, src := range sources {
		bucket := src.Bucket
		if bucket == "" {
			bucket = dst.Bucket
		}
		_, f, err := s.OpenObject(ctx, project, bucket, src.Name, src.Generation, Preconditions{})
		if err != nil {
			return Object{}, err
		}
		open = append(open, f)
		readers = append(readers, f)
	}

	if dst.ContentType == "" {
		dst.ContentType = "application/octet-stream"
	}
	return s.WriteObject(ctx, project, dst, io.MultiReader(readers...))
}
