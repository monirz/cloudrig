package storage_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/monirz/cloudrig/services/storage"
)

// TestMixedConcurrency runs every mutating operation against one object at
// once, which is where a race the single-operation tests miss would show.
func TestMixedConcurrency(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	const rounds = 40
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(5)
		go func() { defer wg.Done(); _, _ = write(t, s, "hot", "content", storage.Preconditions{}) }()
		go func() { defer wg.Done(); _ = s.DeleteObject(ctx, "p", "bkt", "hot", storage.Preconditions{}) }()
		go func() {
			defer wg.Done()
			_, _ = s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "bkt"})
		}()
		go func() { defer wg.Done(); _, _ = s.GetObject(ctx, "p", "bkt", "hot", nil) }()
		go func() {
			defer wg.Done()
			_, _ = s.UpdateObject(ctx, "p", "bkt", "hot", storage.Write{Metadata: map[string]string{"n": "x"}})
		}()
	}
	wg.Wait()
}

// TestResetDuringTraffic checks that clearing state under load neither races
// nor wedges.
func TestResetDuringTraffic(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.WriteObject(ctx, "p", storage.Write{Bucket: "bkt", Name: "x"}, strings.NewReader("v"))
				_, _ = s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "bkt"})
			}
		}()
	}

	for i := 0; i < 20; i++ {
		if err := s.Reset(ctx, "p"); err != nil {
			t.Errorf("reset under load: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestConcurrentBucketDeleteAndWrite races emptiness checking against writing.
func TestConcurrentBucketDeleteAndWrite(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = write(t, s, "obj", "c", storage.Preconditions{}) }()
		go func() { defer wg.Done(); _ = s.DeleteBucket(ctx, "p", "bkt") }()
	}
	wg.Wait()
}
