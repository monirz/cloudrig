package blob_test

import (
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/monirz/cloudrig/store/blob"
)

// repeating yields the same block forever without allocating, so a large test
// input costs nothing to produce. Generating it with bytes.Repeat would put the
// whole thing in the heap and defeat the measurement.
type repeating struct{ block []byte }

func (r repeating) Read(p []byte) (int, error) {
	n := copy(p, r.block)
	for n < len(p) {
		n += copy(p[n:], r.block)
	}
	return n, nil
}

func source(size int64) io.Reader {
	block := make([]byte, 64<<10)
	for i := range block {
		block[i] = byte(i)
	}
	return io.LimitReader(repeating{block}, size)
}

// TestMemoryStaysFlat is spec rule 3: payload bytes never enter the heap in
// full. It is asserted on HeapAlloc rather than RSS because Go does not return
// memory to the OS promptly enough for an RSS bound to be anything but flaky.
//
// fake-gcs-server is the counterexample this exists to avoid: a 2 GB object has
// driven its RSS past 12 GB, because upload reads the body whole.
func TestMemoryStaysFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a large object")
	}
	t.Parallel()

	s := newStore(t)
	ctx := context.Background()

	const (
		size    = 256 << 20 // 256 MiB
		ceiling = 16 << 20  // three orders of magnitude below the payload
	)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	ref, err := s.Put(ctx, source(size))
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	if ref.Size != size {
		t.Fatalf("stored %d bytes, want %d", ref.Size, size)
	}

	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > ceiling {
		t.Errorf("heap grew %d bytes streaming %d; want under %d",
			growth, int64(size), int64(ceiling))
	}
	t.Logf("streamed %d MiB, heap grew %d KiB", size>>20, growth>>10)

	// Reading it back must be constant too: a download that buffers is the
	// same bug on the way out.
	runtime.GC()
	runtime.ReadMemStats(&before)

	f, err := s.Open(ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := io.Copy(io.Discard, f)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	if n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	growth = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > ceiling {
		t.Errorf("heap grew %d bytes reading %d back; want under %d",
			growth, int64(size), int64(ceiling))
	}
	t.Logf("read back %d MiB, heap grew %d KiB", size>>20, growth>>10)
}

// BenchmarkPut reports throughput and allocations per byte stored.
func BenchmarkPut(b *testing.B) {
	s, err := blob.NewTemp()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	const size = 64 << 20
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Each iteration writes distinct content, so dedup does not turn the
		// benchmark into a stat() loop.
		block := make([]byte, 64<<10)
		block[0] = byte(i)
		if _, err := s.Put(context.Background(), io.LimitReader(repeating{block}, size)); err != nil {
			b.Fatal(err)
		}
	}
}
