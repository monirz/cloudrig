package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

var errBoom = errors.New("the store is unwell")

// faultyStore fails or rewrites chosen operations, so the store-error paths a
// healthy store never reaches are reachable from a test.
type faultyStore struct {
	store.Store
	failGet    func(key string) error
	failPut    func(key string) error
	failDelete func(key string) error
	tamper     func(key string, val []byte) []byte
}

func (f *faultyStore) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	if f.failGet != nil {
		if err := f.failGet(key); err != nil {
			return nil, 0, err
		}
	}
	val, version, err := f.Store.Get(ctx, key)
	if err == nil && f.tamper != nil {
		val = f.tamper(key, val)
	}
	return val, version, err
}

func (f *faultyStore) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	if f.failPut != nil {
		if err := f.failPut(key); err != nil {
			return 0, err
		}
	}
	return f.Store.Put(ctx, key, val, ifVersion)
}

func (f *faultyStore) Delete(ctx context.Context, key string, ifVersion uint64) error {
	if f.failDelete != nil {
		if err := f.failDelete(key); err != nil {
			return err
		}
	}
	return f.Store.Delete(ctx, key, ifVersion)
}

// onKeys returns a fault that fires only for keys holding one of the markers,
// so a test can break the live pointer without breaking metadata, or vice versa.
func onKeys(err error, markers ...string) func(string) error {
	return func(key string) error {
		for _, m := range markers {
			if strings.Contains(key, m) {
				return err
			}
		}
		return nil
	}
}

const (
	liveKey = "/live/"
	genKey  = "/o/"
)

func withFaultyBucket(t *testing.T) (*storage.Service, *faultyStore, context.Context) {
	t.Helper()

	blobs, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	fs := &faultyStore{Store: store.NewMemory()}
	s := storage.New(fs, blobs, clock.NewFake(epoch), nil)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	return s, fs, ctx
}

// wantInternal asserts the store failure was wrapped rather than leaking out
// as a bare error, and that the wrap names the operation.
func wantInternal(t *testing.T, err error, mentions string) {
	t.Helper()
	if got := status(t, err); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Errorf("error %q does not mention %q", err, mentions)
	}
}

func TestWriteObjectNeedsAName(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	_, err := s.WriteObject(ctx, "p", storage.Write{Bucket: "bkt"}, strings.NewReader("x"))
	if got := status(t, err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (err: %v)", got, err)
	}
}

func TestWriteObjectReportsAFailedRead(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	_, err := s.WriteObject(ctx, "p", storage.Write{Bucket: "bkt", Name: "x"},
		iotest.ErrReader(errBoom))
	wantInternal(t, err, "storing object content")
}

func TestWriteObjectReportsStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		arm      func(fs *faultyStore)
		mentions string
	}{
		{"reading the live pointer", func(fs *faultyStore) {
			fs.failGet = onKeys(errBoom, liveKey)
		}, "reading the live pointer"},
		{"storing metadata", func(fs *faultyStore) {
			fs.failPut = onKeys(errBoom, genKey)
		}, "storing object metadata"},
		{"publishing the pointer", func(fs *faultyStore) {
			fs.failPut = onKeys(errBoom, liveKey)
		}, "publishing the live pointer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, fs, _ := withFaultyBucket(t)
			tc.arm(fs)

			_, err := write(t, s, "x", "hello", storage.Preconditions{})
			wantInternal(t, err, tc.mentions)
		})
	}
}

func TestReadReportsStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		arm      func(fs *faultyStore)
		mentions string
	}{
		{"reading metadata", func(fs *faultyStore) {
			fs.failGet = onKeys(errBoom, genKey)
		}, "reading object metadata"},
		{"decoding the live pointer", func(fs *faultyStore) {
			fs.tamper = tamperOn(liveKey, []byte("{not json"))
		}, "decoding the live pointer"},
		{"decoding metadata", func(fs *faultyStore) {
			fs.tamper = tamperOn(genKey, []byte("{not json"))
		}, "decoding object metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, fs, ctx := withFaultyBucket(t)
			if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
				t.Fatal(err)
			}
			tc.arm(fs)

			_, err := s.GetObject(ctx, "p", "bkt", "x", nil)
			wantInternal(t, err, tc.mentions)
		})
	}
}

func tamperOn(marker string, with []byte) func(string, []byte) []byte {
	return func(key string, val []byte) []byte {
		if strings.Contains(key, marker) {
			return with
		}
		return val
	}
}

// An overwrite without versioning drops the superseded generation; a store that
// cannot delete has to say so rather than leak the blob silently.
func TestOverwriteReportsAFailedDrop(t *testing.T) {
	t.Parallel()
	s, fs, _ := withFaultyBucket(t)

	if _, err := write(t, s, "x", "first", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	fs.failDelete = onKeys(errBoom, genKey)

	_, err := write(t, s, "x", "second", storage.Preconditions{})
	wantInternal(t, err, "removing a superseded generation")
}

// The pointer keeps moving out from under the reader: bounded retries, then a
// conflict rather than a spin.
func TestResolveGivesUpOnAMovingTarget(t *testing.T) {
	t.Parallel()
	s, fs, ctx := withFaultyBucket(t)

	if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	fs.failGet = onKeys(store.ErrNotFound, genKey)

	_, err := s.GetObject(ctx, "p", "bkt", "x", nil)
	if got := status(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (err: %v)", got, err)
	}
}

// Every probed generation is already taken, so the claim has to give up.
func TestClaimGivesUpWhenEveryGenerationIsTaken(t *testing.T) {
	t.Parallel()
	s, fs, _ := withFaultyBucket(t)

	fs.failPut = onKeys(store.ErrVersionMismatch, genKey)

	_, err := write(t, s, "x", "hello", storage.Preconditions{})
	if got := status(t, err); got != http.StatusConflict {
		t.Errorf("status = %d, want 409 (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "generation") {
		t.Errorf("error %q does not mention the generation it could not allocate", err)
	}
}

// A pointer that never stops moving: unconditional writes retry and then report
// contention, while a conditional write fails its precondition at once.
func TestContentionOnTheLivePointer(t *testing.T) {
	t.Parallel()

	t.Run("unconditional retries then gives up", func(t *testing.T) {
		t.Parallel()
		s, fs, _ := withFaultyBucket(t)
		fs.failPut = onKeys(store.ErrVersionMismatch, liveKey)

		_, err := write(t, s, "x", "hello", storage.Preconditions{})
		if got := status(t, err); got != http.StatusConflict {
			t.Errorf("status = %d, want 409 (err: %v)", got, err)
		}
		if !strings.Contains(err.Error(), "contention") {
			t.Errorf("error %q does not name contention", err)
		}
	})

	t.Run("conditional fails its precondition", func(t *testing.T) {
		t.Parallel()
		s, fs, _ := withFaultyBucket(t)
		fs.failPut = onKeys(store.ErrVersionMismatch, liveKey)

		_, err := write(t, s, "x", "hello", storage.Preconditions{IfGenerationMatch: gen(0)})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})
}

// A condition against an object that is not there is a failed precondition,
// not a 404, because the caller asked conditionally.
func TestConditionalReadOfAMissingObject(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	_, err := s.GetObjectIf(ctx, "p", "bkt", "absent", nil,
		storage.Preconditions{IfGenerationMatch: gen(5)})
	if got := status(t, err); got != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (err: %v)", got, err)
	}
}

// The metadata survives but the content it names does not.
func TestOpenReportsMissingContent(t *testing.T) {
	t.Parallel()
	s, fs, ctx := withFaultyBucket(t)

	if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	fs.tamper = func(key string, val []byte) []byte {
		if !strings.Contains(key, genKey) {
			return val
		}
		var o storage.Object
		if json.Unmarshal(val, &o) != nil {
			return val
		}
		o.Blob = strings.Repeat("0", 64)
		rewritten, err := json.Marshal(o)
		if err != nil {
			return val
		}
		return rewritten
	}

	_, _, err := s.OpenObject(ctx, "p", "bkt", "x", nil, storage.Preconditions{})
	wantInternal(t, err, "opening object content")
}

func TestUpdateObjectReportsStoreFailures(t *testing.T) {
	t.Parallel()

	t.Run("a pointer that moved is a failed precondition", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failPut = onKeys(store.ErrVersionMismatch, genKey)

		_, err := s.UpdateObject(ctx, "p", "bkt", "x", storage.Write{ContentType: "text/plain"})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})

	t.Run("a broken store is reported", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failPut = onKeys(errBoom, genKey)

		_, err := s.UpdateObject(ctx, "p", "bkt", "x", storage.Write{ContentType: "text/plain"})
		wantInternal(t, err, "storing object metadata")
	})

	t.Run("a generation that vanished is a failed precondition", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failGet = onKeys(store.ErrNotFound, genKey)

		_, err := s.UpdateObject(ctx, "p", "bkt", "x", storage.Write{ContentType: "text/plain"})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})
}

func TestDeleteObjectReportsStoreFailures(t *testing.T) {
	t.Parallel()

	t.Run("a pointer that moved is a failed precondition", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failDelete = onKeys(store.ErrVersionMismatch, liveKey)

		err := s.DeleteObject(ctx, "p", "bkt", "x", storage.Preconditions{})
		if got := status(t, err); got != http.StatusPreconditionFailed {
			t.Errorf("status = %d, want 412 (err: %v)", got, err)
		}
	})

	t.Run("a broken store is reported", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failDelete = onKeys(errBoom, liveKey)

		err := s.DeleteObject(ctx, "p", "bkt", "x", storage.Preconditions{})
		wantInternal(t, err, "deleting the live pointer")
	})

	t.Run("a superseded generation that cannot be dropped is reported", func(t *testing.T) {
		t.Parallel()
		s, fs, ctx := withFaultyBucket(t)
		if _, err := write(t, s, "x", "hello", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
		fs.failDelete = onKeys(errBoom, genKey)

		err := s.DeleteObject(ctx, "p", "bkt", "x", storage.Preconditions{})
		wantInternal(t, err, "removing a superseded generation")
	})
}
