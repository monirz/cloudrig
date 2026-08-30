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
// full.
//
// The gate is memory still held after a GC, not RSS and not mid-flight
// HeapAlloc. Go does not return memory to the OS promptly enough for an RSS
// bound to hold, and HeapAlloc counts allocated-but-uncollected objects, so it
// measures how recently the collector ran as much as what the code did. Churn
// is reported but not gated: streaming through a 32 KiB buffer allocates, and
// that is fine — retaining the object is not.
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
	runtime.GC()
	runtime.ReadMemStats(&after)

	if ref.Size != size {
		t.Fatalf("stored %d bytes, want %d", ref.Size, size)
	}

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	churn := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("streamed %d MiB: retained %d KiB, allocated %d KiB in total",
		size>>20, retained>>10, churn>>10)
	if retained > ceiling {
		t.Errorf("streaming %d bytes retained %d; want under %d",
			int64(size), retained, int64(ceiling))
	}

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
	runtime.GC()
	runtime.ReadMemStats(&after)

	if n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	retained = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("read back %d MiB: retained %d KiB", size>>20, retained>>10)
	if retained > ceiling {
		t.Errorf("reading %d bytes back retained %d; want under %d",
			int64(size), retained, int64(ceiling))
	}
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
