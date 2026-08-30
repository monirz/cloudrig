package conformance

import (
	"io"
	"runtime"
	"testing"
)

// repeating yields the same block forever without allocating, so a large upload
// costs nothing to produce. Building the payload with bytes.Repeat would put it
// in the heap and defeat the measurement.
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

// TestLargeObjectMemory is acceptance criterion 8, through the whole HTTP stack
// with the real client: a gibibyte in and out without ever holding it.
//
// The gate is memory still held after a GC, not HeapAlloc mid-flight. HeapAlloc
// counts allocated-but-uncollected objects, so it measures how recently the
// collector ran as much as anything the code did — under -race it reads over a
// hundred megabytes for a transfer that retains kilobytes. Churn is reported
// but not gated: streaming a gibibyte through 32 KiB buffers allocates, and
// that is fine. What must not happen is retaining it.
//
// Writer.ChunkSize is 0 on purpose. The client buffers a chunk at a time by
// default, so leaving it at 16 MiB would measure the client's buffer rather
// than the emulator; 0 disables chunking and streams the object as one
// unbuffered multipart request, which is also what makes resumable unnecessary
// (decision D1).
func TestLargeObjectMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streams a gibibyte")
	}

	c, ctx := client(t)
	b := bucket(t, c, ctx, "large")

	const (
		size    = 1 << 30 // 1 GiB
		ceiling = 64 << 20
	)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	w := b.Object("big.bin").NewWriter(ctx)
	w.ChunkSize = 0
	if _, err := io.Copy(w, source(size)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the writer: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	churn := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("uploaded %d MiB: retained %d KiB, allocated %d MiB in total",
		size>>20, retained>>10, churn>>20)
	if retained > ceiling {
		t.Errorf("uploading %d bytes retained %d; want under %d", int64(size), retained, int64(ceiling))
	}

	attrs, err := b.Object("big.bin").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != size {
		t.Fatalf("stored %d bytes, want %d", attrs.Size, size)
	}

	runtime.GC()
	runtime.ReadMemStats(&before)

	r, err := b.Object("big.bin").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	if n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	retained = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	churn = int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("downloaded %d MiB: retained %d KiB, allocated %d MiB in total",
		size>>20, retained>>10, churn>>20)
	if retained > ceiling {
		t.Errorf("downloading %d bytes retained %d; want under %d", int64(size), retained, int64(ceiling))
	}
}

// BenchmarkUpload reports upload throughput through the full HTTP path.
func BenchmarkUpload(b *testing.B) {
	c, ctx := clientB(b)
	bkt := c.Bucket("bench")
	if err := bkt.Create(ctx, "p", nil); err != nil {
		b.Fatal(err)
	}

	const size = 64 << 20
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		w := bkt.Object("obj").NewWriter(ctx)
		w.ChunkSize = 0
		if _, err := io.Copy(w, source(size)); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
