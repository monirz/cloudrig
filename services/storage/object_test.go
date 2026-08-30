package storage_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/monirz/cloudrig/services/storage"
)

func withBucket(t *testing.T) (*storage.Service, context.Context) {
	t.Helper()
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, storage.Bucket{Name: "bkt", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func write(t *testing.T, s *storage.Service, name, content string, p storage.Preconditions) (storage.Object, error) {
	t.Helper()
	return s.WriteObject(context.Background(), "p", storage.Write{
		Bucket: "bkt", Name: name, Preconditions: p,
	}, strings.NewReader(content))
}

func gen(n int64) *int64 { return &n }

func TestWriteAndRead(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	obj, err := s.WriteObject(ctx, "p", storage.Write{
		Bucket: "bkt", Name: "logs/2026/app.log",
		ContentType: "text/plain",
		Metadata:    map[string]string{"team": "infra"},
	}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}

	if obj.Size != 5 || obj.ContentType != "text/plain" || obj.Metadata["team"] != "infra" {
		t.Errorf("object = %+v", obj)
	}
	if obj.Metageneration != 1 {
		t.Errorf("Metageneration = %d, want 1", obj.Metageneration)
	}
	// Checksums come from the single hashing pass in the blob store.
	if obj.CRC32C == "" || obj.MD5 == "" || obj.ETag == "" {
		t.Errorf("checksums or etag missing: %+v", obj)
	}

	got, f, err := s.OpenObject(ctx, "p", "bkt", "logs/2026/app.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if string(body) != "hello" {
		t.Errorf("content = %q", body)
	}
	if got.Generation != obj.Generation {
		t.Errorf("generation = %d, want %d", got.Generation, obj.Generation)
	}
}

func TestWriteRequiresAnExistingBucket(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	_, err := s.WriteObject(ctx, "p", storage.Write{Bucket: "nope", Name: "x"}, strings.NewReader(""))
	if got := status(t, err); got != 404 {
		t.Errorf("status = %d, want 404", got)
	}
}

// TestOverwriteBumpsGeneration is acceptance criterion 5's first half.
func TestOverwriteBumpsGeneration(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	first, err := write(t, s, "obj", "one", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := write(t, s, "obj", "two", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	if second.Generation <= first.Generation {
		t.Errorf("generation went %d -> %d, want an increase", first.Generation, second.Generation)
	}
	if second.Metageneration != 1 {
		t.Errorf("Metageneration = %d after an overwrite, want 1", second.Metageneration)
	}

	// Versioning is off by default, so the superseded generation is gone —
	// as in GCS, and as fake-gcs-server does with versioning disabled.
	if _, err := s.GetObject(context.Background(), "p", "bkt", "obj", gen(first.Generation)); status(t, err) != 404 {
		t.Errorf("status = %d reading a superseded generation, want 404", status(t, err))
	}
}

// TestGenerationsIncreaseUnderAFakeClock is why allocation takes
// max(now, highest+1): a fake clock does not move, so every write would
// otherwise collide on the same microsecond.
func TestGenerationsIncreaseUnderAFakeClock(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	var last int64
	for i := 0; i < 5; i++ {
		obj, err := write(t, s, "obj", "content", storage.Preconditions{})
		if err != nil {
			t.Fatal(err)
		}
		if obj.Generation <= last {
			t.Fatalf("write %d produced generation %d, not above %d", i, obj.Generation, last)
		}
		last = obj.Generation
	}
}

// TestMetadataUpdateBumpsOnlyMetageneration is acceptance criterion 5's second
// half: the content did not change, so a client watching for a content change
// must not see one.
func TestMetadataUpdateBumpsOnlyMetageneration(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	obj, err := write(t, s, "obj", "content", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateObject(ctx, "p", "bkt", "obj", storage.Write{
		ContentType: "application/json",
		Metadata:    map[string]string{"reviewed": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if updated.Generation != obj.Generation {
		t.Errorf("generation moved from %d to %d on a metadata-only change",
			obj.Generation, updated.Generation)
	}
	if updated.Metageneration != 2 {
		t.Errorf("Metageneration = %d, want 2", updated.Metageneration)
	}
	if updated.ContentType != "application/json" || updated.Metadata["reviewed"] != "yes" {
		t.Errorf("patch not applied: %+v", updated)
	}
	if updated.ETag == obj.ETag {
		t.Error("etag did not change with metageneration")
	}

	// A second update keeps moving.
	again, err := s.UpdateObject(ctx, "p", "bkt", "obj", storage.Write{Metadata: map[string]string{"n": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Metageneration != 3 {
		t.Errorf("Metageneration = %d, want 3", again.Metageneration)
	}
}

// TestDoesNotExistPrecondition is acceptance criterion 3: what
// storage.Conditions{DoesNotExist: true} compiles to.
func TestDoesNotExistPrecondition(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	if _, err := write(t, s, "obj", "first", storage.Preconditions{IfGenerationMatch: gen(0)}); err != nil {
		t.Fatalf("the first write was refused: %v", err)
	}
	_, err := write(t, s, "obj", "second", storage.Preconditions{IfGenerationMatch: gen(0)})
	if got := status(t, err); got != 412 {
		t.Errorf("status = %d, want 412", got)
	}
}

func TestGenerationPreconditions(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	obj, err := write(t, s, "obj", "one", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("matching generation succeeds", func(t *testing.T) {
		if _, err := write(t, s, "obj", "two",
			storage.Preconditions{IfGenerationMatch: gen(obj.Generation)}); err != nil {
			t.Errorf("a matching precondition was refused: %v", err)
		}
	})

	t.Run("stale generation is 412", func(t *testing.T) {
		_, err := write(t, s, "obj", "three",
			storage.Preconditions{IfGenerationMatch: gen(obj.Generation)})
		if got := status(t, err); got != 412 {
			t.Errorf("status = %d, want 412", got)
		}
	})

	t.Run("a generation on a missing object is 412", func(t *testing.T) {
		_, err := write(t, s, "absent", "x", storage.Preconditions{IfGenerationMatch: gen(123)})
		if got := status(t, err); got != 412 {
			t.Errorf("status = %d, want 412", got)
		}
	})
}

func TestMetagenerationPrecondition(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "obj", "one", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateObject(ctx, "p", "bkt", "obj", storage.Write{
		Metadata:      map[string]string{"n": "1"},
		Preconditions: storage.Preconditions{IfMetagenerationMatch: gen(1)},
	}); err != nil {
		t.Errorf("a matching metageneration was refused: %v", err)
	}

	_, err := s.UpdateObject(ctx, "p", "bkt", "obj", storage.Write{
		Metadata:      map[string]string{"n": "2"},
		Preconditions: storage.Preconditions{IfMetagenerationMatch: gen(1)},
	})
	if got := status(t, err); got != 412 {
		t.Errorf("status = %d, want 412", got)
	}
}

// TestConcurrentConditionalWrites is acceptance criterion 4: exactly one of two
// racing conditional writers wins.
func TestConcurrentConditionalWrites(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	obj, err := write(t, s, "obj", "seed", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	const racers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := write(t, s, "obj", "contended",
				storage.Preconditions{IfGenerationMatch: gen(obj.Generation)})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
				return
			}
			if status(t, err) == 412 {
				lost++
			}
		}()
	}
	wg.Wait()

	if won != 1 || lost != racers-1 {
		t.Errorf("won = %d, lost = %d; want 1 and %d", won, lost, racers-1)
	}
}

// TestConcurrentUnconditionalWrites checks the other side: with no
// precondition GCS is last-write-wins, so every writer must succeed.
func TestConcurrentUnconditionalWrites(t *testing.T) {
	t.Parallel()
	s, _ := withBucket(t)

	const racers = 16
	var wg sync.WaitGroup
	errs := make(chan error, racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := write(t, s, "obj", "content", storage.Preconditions{}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("an unconditional write failed: %v", err)
	}
	if _, err := s.GetObject(context.Background(), "p", "bkt", "obj", nil); err != nil {
		t.Errorf("no live object after %d writes: %v", racers, err)
	}
}

func TestDeleteObject(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	obj, err := write(t, s, "obj", "content", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a stale precondition is 412", func(t *testing.T) {
		err := s.DeleteObject(ctx, "p", "bkt", "obj", storage.Preconditions{IfGenerationMatch: gen(1)})
		if got := status(t, err); got != 412 {
			t.Errorf("status = %d, want 412", got)
		}
	})

	t.Run("deletes", func(t *testing.T) {
		if err := s.DeleteObject(ctx, "p", "bkt", "obj",
			storage.Preconditions{IfGenerationMatch: gen(obj.Generation)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetObject(ctx, "p", "bkt", "obj", nil); status(t, err) != 404 {
			t.Error("object still live after delete")
		}
		// Unversioned, so the generation went with it.
		if _, err := s.GetObject(ctx, "p", "bkt", "obj", gen(obj.Generation)); status(t, err) != 404 {
			t.Error("the deleted generation is still readable in an unversioned bucket")
		}
	})

	t.Run("deleting again is 404", func(t *testing.T) {
		if err := s.DeleteObject(ctx, "p", "bkt", "obj", storage.Preconditions{}); status(t, err) != 404 {
			t.Errorf("status = %d, want 404", status(t, err))
		}
	})
}

func TestDeleteBucketRefusesWhileObjectsRemain(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "obj", "content", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); status(t, err) != 409 {
		t.Errorf("status = %d deleting a non-empty bucket, want 409", status(t, err))
	}

	if err := s.DeleteObject(ctx, "p", "bkt", "obj", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBucket(ctx, "p", "bkt"); err != nil {
		t.Errorf("bucket still refused after its object was deleted: %v", err)
	}
}

// TestBlobAddressSurvivesAReload guards a bug where the content address was
// tagged json:"-" and so never persisted: the metadata came back with no blob
// to open.
func TestBlobAddressSurvivesAReload(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "obj", "content", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	// GetObject decodes from the store rather than returning what Write built.
	obj, err := s.GetObject(ctx, "p", "bkt", "obj", nil)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Blob == "" {
		t.Fatal("the content address was not persisted")
	}

	_, f, err := s.OpenObject(ctx, "p", "bkt", "obj", nil)
	if err != nil {
		t.Fatalf("opening content after a reload: %v", err)
	}
	f.Close()
}

// TestConcurrentWritesAllocateDistinctGenerations guards the other half of that
// race: two writers reading the same live pointer compute the same generation,
// and only one may claim it.
func TestConcurrentWritesAllocateDistinctGenerations(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	const racers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int64]int{}

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obj, err := write(t, s, "obj", "content", storage.Preconditions{})
			if err != nil {
				t.Errorf("write failed: %v", err)
				return
			}
			mu.Lock()
			seen[obj.Generation]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != racers {
		t.Errorf("%d writes produced %d distinct generations", racers, len(seen))
	}
	for generation, count := range seen {
		if count != 1 {
			t.Errorf("generation %d was handed out %d times", generation, count)
		}
	}

	// Exactly one survives: the winner's. The rest were superseded and
	// dropped, because the bucket is not versioned.
	live, err := s.GetObject(ctx, "p", "bkt", "obj", nil)
	if err != nil {
		t.Fatalf("no live object after %d writes: %v", racers, err)
	}
	if seen[live.Generation] != 1 {
		t.Errorf("the live generation %d was never handed out", live.Generation)
	}
}
