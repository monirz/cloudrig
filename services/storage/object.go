package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/core/resource"
	"github.com/monirz/cloudrig/store"
)

// Object is one generation's metadata. Payload bytes live in the blob tree;
// Blob is their address.
type Object struct {
	Bucket         string            `json:"bucket"`
	Name           string            `json:"name"`
	Generation     int64             `json:"generation"`
	Metageneration int64             `json:"metageneration"`
	Size           int64             `json:"size"`
	ContentType    string            `json:"contentType"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CRC32C         string            `json:"crc32c"`
	MD5            string            `json:"md5Hash"`
	ETag           string            `json:"etag"`
	Blob           string            `json:"blob"`
	Created        time.Time         `json:"timeCreated"`
	Updated        time.Time         `json:"updated"`
}

// live is the pointer to an object's current generation.
//
// Highest is every generation ever allocated for this name, not merely the
// current one: allocation takes max(now, Highest+1), so a write under a fake
// clock still produces a generation above any that came before.
type live struct {
	Generation int64 `json:"generation"`
	Highest    int64 `json:"highest"`
}

// Preconditions gate a write. A nil field is no condition.
type Preconditions struct {
	// IfGenerationMatch of 0 means the object must not exist — what
	// storage.Conditions{DoesNotExist: true} compiles to.
	IfGenerationMatch     *int64
	IfMetagenerationMatch *int64
}

// Write is a request to store an object.
type Write struct {
	Bucket        string
	Name          string
	ContentType   string
	Metadata      map[string]string
	Preconditions Preconditions
}

// maxWriteAttempts bounds the retry an unconditional write does when it loses
// the live-pointer race. Without a precondition GCS is last-write-wins, so
// losing means trying again rather than failing.
//
// A writer only retries while another is actively winning, so the budget bounds
// concurrent writers to one object rather than time. Past it the answer is
// contention, not a wrong result.
const maxWriteAttempts = 64

// maxGenerationProbes bounds the search for an unclaimed generation. Writers
// that read the same live pointer compute the same number, and each takes the
// next free one; the search is over as soon as it finds a gap.
const maxGenerationProbes = 1024

// WriteObject streams r into the blob tree and publishes it as a new
// generation.
//
// The payload is stored before the pointer is claimed, so a writer that loses
// the race leaves an unreferenced blob behind. That is deliberate: the
// alternative is holding the bytes until the outcome is known, which is exactly
// the memory behaviour this project exists to avoid. Reclaimed by Reset.
func (s *Service) WriteObject(ctx context.Context, project string, w Write, r io.Reader) (Object, error) {
	if _, _, err := s.bucket(ctx, project, w.Bucket); err != nil {
		return Object{}, err
	}
	if w.Name == "" {
		return Object{}, gerr.New(gerr.InvalidArgument, "an object needs a name").
			WithHTTPStatus(http.StatusBadRequest).
			WithReason("required")
	}

	ref, err := s.blobs.Put(ctx, r)
	if err != nil {
		return Object{}, gerr.Wrap(err, gerr.Internal, "storing object content")
	}

	conditional := w.Preconditions.IfGenerationMatch != nil || w.Preconditions.IfMetagenerationMatch != nil
	for attempt := 0; ; attempt++ {
		obj, err := s.publish(ctx, project, w, ref)
		if err == nil {
			return obj, nil
		}
		// A lost race under a precondition is a precondition failure: the state
		// the caller required is no longer current.
		if !errors.Is(err, errRaceLost) {
			return Object{}, err
		}
		if conditional {
			return Object{}, preconditionFailed(w.Bucket, w.Name)
		}
		if attempt+1 >= maxWriteAttempts {
			return Object{}, gerr.New(gerr.Aborted, "too much contention on the object").
				WithHTTPStatus(http.StatusConflict).
				WithReason("conflict")
		}
	}
}

// errRaceLost signals that the live pointer moved between read and write.
var errRaceLost = errors.New("storage: live pointer moved")

func (s *Service) publish(ctx context.Context, project string, w Write, ref blobRef) (Object, error) {
	current, version, err := s.readLive(ctx, project, w.Bucket, w.Name)
	if err != nil {
		return Object{}, err
	}

	var existing *Object
	if version != 0 {
		obj, err := s.readGeneration(ctx, project, w.Bucket, w.Name, current.Generation)
		if err != nil {
			return Object{}, err
		}
		existing = &obj
	}
	if err := checkPreconditions(w.Preconditions, existing, w.Bucket, w.Name); err != nil {
		return Object{}, err
	}

	now := s.clk.Now()
	obj := Object{
		Bucket:         w.Bucket,
		Name:           w.Name,
		Metageneration: 1,
		Size:           ref.Size,
		ContentType:    orDefault(w.ContentType, "application/octet-stream"),
		Metadata:       w.Metadata,
		CRC32C:         ref.CRC32C,
		MD5:            ref.MD5,
		Blob:           ref.SHA256,
		Created:        now,
		Updated:        now,
	}

	// The metadata is written before the pointer is moved, so a live pointer
	// never names a generation whose metadata is missing.
	generation, err := s.claimGeneration(ctx, project, obj, nextGeneration(now, current.Highest))
	if err != nil {
		return Object{}, err
	}
	obj.Generation = generation
	obj.ETag = etag(obj)

	next := live{Generation: generation, Highest: generation}
	if err := s.casLive(ctx, project, w.Bucket, w.Name, next, version); err != nil {
		return Object{}, err
	}
	return obj, nil
}

// claimGeneration writes the metadata under the first free generation at or
// above start, and returns the number it took.
func (s *Service) claimGeneration(ctx context.Context, project string, o Object, start int64) (int64, error) {
	for generation := start; generation < start+maxGenerationProbes; generation++ {
		o.Generation = generation
		o.ETag = etag(o)

		err := s.putGeneration(ctx, project, o)
		if err == nil {
			return generation, nil
		}
		if !errors.Is(err, errRaceLost) {
			return 0, err
		}
	}
	return 0, gerr.New(gerr.Aborted, "could not allocate a generation").
		WithHTTPStatus(http.StatusConflict).
		WithReason("conflict")
}

// nextGeneration keeps generations unique and increasing.
//
// GCS derives a generation from the write time in microseconds. Under a fake
// clock time does not move, so every write in a test would collide; taking the
// max with highest+1 walks past the collision while keeping a later real-time
// write above anything a fake clock produced.
func nextGeneration(now time.Time, highest int64) int64 {
	candidate := now.UnixMicro()
	if highest >= candidate {
		return highest + 1
	}
	return candidate
}

func checkPreconditions(p Preconditions, existing *Object, bucket, name string) error {
	if p.IfGenerationMatch != nil {
		want := *p.IfGenerationMatch
		switch {
		case want == 0 && existing != nil:
			return preconditionFailed(bucket, name)
		case want != 0 && (existing == nil || existing.Generation != want):
			return preconditionFailed(bucket, name)
		}
	}
	if p.IfMetagenerationMatch != nil {
		if existing == nil || existing.Metageneration != *p.IfMetagenerationMatch {
			return preconditionFailed(bucket, name)
		}
	}
	return nil
}

// etag is opaque to clients, so any stable function of the identity will do.
func etag(o Object) string {
	return fmt.Sprintf("CN%dEA%dE=", o.Generation, o.Metageneration)
}

func preconditionFailed(bucket, name string) error {
	return gerr.Newf(gerr.FailedPrecondition,
		"At least one of the pre-conditions you specified did not hold (%s/%s)", bucket, name).
		// 412, not the canonical 400 for FAILED_PRECONDITION: GCS answers a
		// failed precondition with 412 and clients branch on it.
		WithHTTPStatus(http.StatusPreconditionFailed).
		WithReason("conditionNotMet")
}

func notFoundObject(bucket, name string) error {
	return gerr.Newf(gerr.NotFound, "No such object: %s/%s", bucket, name).
		WithHTTPStatus(http.StatusNotFound).
		WithReason("notFound")
}

// readLive returns the live pointer and the store version it is at. A version
// of 0 means the object does not exist.
func (s *Service) readLive(ctx context.Context, project, bucket, name string) (live, uint64, error) {
	raw, version, err := s.kv.Get(ctx, resource.Live(project, bucket, name))
	if errors.Is(err, store.ErrNotFound) {
		return live{}, 0, nil
	}
	if err != nil {
		return live{}, 0, gerr.Wrap(err, gerr.Internal, "reading the live pointer")
	}

	l, err := decodeLive(raw)
	if err != nil {
		return live{}, 0, gerr.Wrap(err, gerr.Internal, "decoding the live pointer")
	}
	return l, version, nil
}

func decodeLive(raw []byte) (live, error) {
	var l live
	err := json.Unmarshal(raw, &l)
	return l, err
}

func (s *Service) casLive(ctx context.Context, project, bucket, name string, l live, version uint64) error {
	encoded, err := json.Marshal(l)
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "encoding the live pointer")
	}
	if _, err := s.kv.Put(ctx, resource.Live(project, bucket, name), encoded, version); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return errRaceLost
		}
		return gerr.Wrap(err, gerr.Internal, "publishing the live pointer")
	}
	return nil
}

func (s *Service) putGeneration(ctx context.Context, project string, o Object) error {
	encoded, err := json.Marshal(o)
	if err != nil {
		return gerr.Wrap(err, gerr.Internal, "encoding object metadata")
	}
	key := resource.Object(project, o.Bucket, o.Name, o.Generation)

	// ifVersion 0 claims the generation: two writers that read the same live
	// pointer allocate the same number, and only one may have it. The loser
	// retries and allocates above the winner.
	// ifVersion 0 claims the generation: two writers that read the same live
	// pointer compute the same number, and only one may have it.
	if _, err := s.kv.Put(ctx, key, encoded, 0); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return errRaceLost
		}
		return gerr.Wrap(err, gerr.Internal, "storing object metadata")
	}
	return nil
}

func (s *Service) readGeneration(ctx context.Context, project, bucket, name string, generation int64) (Object, error) {
	o, _, err := s.readGenerationVersion(ctx, project, bucket, name, generation)
	return o, err
}

// readGenerationVersion also returns the store version, which a metadata
// update needs for its compare-and-swap.
func (s *Service) readGenerationVersion(ctx context.Context, project, bucket, name string, generation int64) (Object, uint64, error) {
	raw, version, err := s.kv.Get(ctx, resource.Object(project, bucket, name, generation))
	if errors.Is(err, store.ErrNotFound) {
		return Object{}, 0, notFoundObject(bucket, name)
	}
	if err != nil {
		return Object{}, 0, gerr.Wrap(err, gerr.Internal, "reading object metadata")
	}

	var o Object
	if err := json.Unmarshal(raw, &o); err != nil {
		return Object{}, 0, gerr.Wrap(err, gerr.Internal, "decoding object metadata")
	}
	return o, version, nil
}

// GetObject returns metadata for the live generation, or a specific one.
func (s *Service) GetObject(ctx context.Context, project, bucket, name string, generation *int64) (Object, error) {
	if generation != nil {
		return s.readGeneration(ctx, project, bucket, name, *generation)
	}

	current, version, err := s.readLive(ctx, project, bucket, name)
	if err != nil {
		return Object{}, err
	}
	if version == 0 {
		return Object{}, notFoundObject(bucket, name)
	}
	return s.readGeneration(ctx, project, bucket, name, current.Generation)
}

// OpenObject returns metadata and an open file of the content. The caller
// closes the file; handing back the file rather than bytes is what keeps a
// download's memory constant.
func (s *Service) OpenObject(ctx context.Context, project, bucket, name string, generation *int64) (Object, *os.File, error) {
	obj, err := s.GetObject(ctx, project, bucket, name, generation)
	if err != nil {
		return Object{}, nil, err
	}
	f, err := s.blobs.Open(obj.Blob)
	if err != nil {
		return Object{}, nil, gerr.Wrap(err, gerr.Internal, "opening object content")
	}
	return obj, f, nil
}

// UpdateObject replaces an object's mutable metadata.
//
// Metageneration moves, generation does not: the content is untouched, and a
// client watching for a content change must not see one.
func (s *Service) UpdateObject(ctx context.Context, project, bucket, name string, patch Write) (Object, error) {
	current, version, err := s.readLive(ctx, project, bucket, name)
	if err != nil {
		return Object{}, err
	}
	if version == 0 {
		return Object{}, notFoundObject(bucket, name)
	}

	obj, objVersion, err := s.readGenerationVersion(ctx, project, bucket, name, current.Generation)
	if err != nil {
		return Object{}, err
	}
	if err := checkPreconditions(patch.Preconditions, &obj, bucket, name); err != nil {
		return Object{}, err
	}

	if patch.ContentType != "" {
		obj.ContentType = patch.ContentType
	}
	if patch.Metadata != nil {
		obj.Metadata = patch.Metadata
	}
	obj.Metageneration++
	obj.Updated = s.clk.Now()
	obj.ETag = etag(obj)

	encoded, err := json.Marshal(obj)
	if err != nil {
		return Object{}, gerr.Wrap(err, gerr.Internal, "encoding object metadata")
	}
	// Compare-and-swap on the version we read, so two concurrent metadata
	// updates cannot both bump metageneration to the same number.
	key := resource.Object(project, bucket, name, obj.Generation)
	if _, err := s.kv.Put(ctx, key, encoded, objVersion); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return Object{}, preconditionFailed(bucket, name)
		}
		return Object{}, gerr.Wrap(err, gerr.Internal, "storing object metadata")
	}
	return obj, nil
}

// DeleteObject removes the live generation.
func (s *Service) DeleteObject(ctx context.Context, project, bucket, name string, p Preconditions) error {
	current, version, err := s.readLive(ctx, project, bucket, name)
	if err != nil {
		return err
	}
	if version == 0 {
		return notFoundObject(bucket, name)
	}

	obj, err := s.readGeneration(ctx, project, bucket, name, current.Generation)
	if err != nil {
		return err
	}
	if err := checkPreconditions(p, &obj, bucket, name); err != nil {
		return err
	}

	if err := s.kv.Delete(ctx, resource.Live(project, bucket, name), version); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			return preconditionFailed(bucket, name)
		}
		return gerr.Wrap(err, gerr.Internal, "deleting the live pointer")
	}
	// The generation's metadata and its blob stay: a later read by explicit
	// generation still resolves, and Reset reclaims both.
	return nil
}
