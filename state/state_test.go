package state_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/faults"
	"github.com/monirz/cloudrig/state"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

// TestRoundTrip is the vertical slice: write kv metadata and a blob to an
// archive, load it into a fresh store, and get both back intact.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()

	srcBlobs := tempBlobs(t)
	ref, err := srcBlobs.Put(ctx, strings.NewReader("hello payload"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	entries := []store.Entry{
		{Key: "buckets/demo", Val: []byte(`{"name":"demo"}`), Version: 1},
		{Key: "objects/demo/file", Val: []byte(ref.SHA256), Version: 3},
	}

	var buf bytes.Buffer
	if err := state.Write(&buf, entries, srcBlobs, state.Runtime{}); err != nil {
		t.Fatalf("write: %v", err)
	}

	dst := store.NewMemory()
	dstBlobs := tempBlobs(t)
	if _, err := state.Read(&buf, dst, dstBlobs); err != nil {
		t.Fatalf("read: %v", err)
	}

	val, ver, err := dst.Get(ctx, "objects/demo/file")
	if err != nil {
		t.Fatalf("get restored entry: %v", err)
	}
	if string(val) != ref.SHA256 || ver != 3 {
		t.Errorf("entry = %q v%d, want %q v3", val, ver, ref.SHA256)
	}

	f, err := dstBlobs.Open(ref.SHA256)
	if err != nil {
		t.Fatalf("open restored blob: %v", err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "hello payload" {
		t.Errorf("blob = %q, want %q", got, "hello payload")
	}
}

func tempBlobs(t *testing.T) *blob.Store {
	t.Helper()
	b, err := blob.NewTemp()
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestRuntimeRoundTrip is the deterministic-state contract: a restored
// emulator fails the same way and reads the same time as the one snapshotted.
func TestRuntimeRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rt := state.Runtime{
		Faults: []faults.Rule{{Path: "/storage/v1/*", Status: 503, Count: 2}},
		Clock:  &state.ClockState{Now: at},
	}

	var buf bytes.Buffer
	if err := state.Write(&buf, nil, tempBlobs(t), rt); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := state.Read(&buf, store.NewMemory(), tempBlobs(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Faults) != 1 || got.Faults[0].Path != "/storage/v1/*" || got.Faults[0].Count != 2 {
		t.Errorf("faults = %+v, want the armed rule with 2 firings left", got.Faults)
	}
	if got.Clock == nil || !got.Clock.Now.Equal(at) {
		t.Errorf("clock = %+v, want %s", got.Clock, at)
	}
}

// TestReadRejectsBadArchives covers what a caller can hand Read that is not a
// snapshot this build understands.
func TestReadRejectsBadArchives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body func() []byte
	}{
		{name: "not a tar", body: func() []byte { return []byte("this is not an archive") }},
		{name: "truncated", body: func() []byte {
			var buf bytes.Buffer
			if err := state.Write(&buf, []store.Entry{{Key: "k", Val: []byte("v")}}, tempBlobs(t), state.Runtime{}); err != nil {
				t.Fatalf("write: %v", err)
			}
			return buf.Bytes()[:40]
		}},
		{name: "malformed manifest", body: func() []byte { return tarOf(t, "manifest.json", "{not json") }},
		{name: "future version", body: func() []byte {
			return tarOf(t, "manifest.json", `{"version":99,"entries":[]}`)
		}},
		{name: "version zero", body: func() []byte {
			return tarOf(t, "manifest.json", `{"version":0,"entries":[]}`)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := state.Read(bytes.NewReader(tc.body()), store.NewMemory(), tempBlobs(t)); err == nil {
				t.Error("accepted an archive that is not a usable snapshot")
			}
		})
	}
}

// TestReadsVersionOne keeps archives written before runtime state loadable.
func TestReadsVersionOne(t *testing.T) {
	t.Parallel()

	archive := tarOf(t, "manifest.json",
		`{"version":1,"entries":[{"Key":"buckets/old","Val":"eyJuYW1lIjoib2xkIn0=","Version":1}]}`)

	kv := store.NewMemory()
	rt, err := state.Read(bytes.NewReader(archive), kv, tempBlobs(t))
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if rt.Faults != nil || rt.Clock != nil {
		t.Errorf("v1 archive carried runtime state: %+v", rt)
	}
	if _, _, err := kv.Get(context.Background(), "buckets/old"); err != nil {
		t.Errorf("v1 entry did not restore: %v", err)
	}
}

// tarOf builds a one-entry archive, for the cases Write cannot produce.
func tarOf(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// failAfter is a writer that dies mid-archive, which is what a full disk or a
// closed connection looks like to Write.
type failAfter struct {
	left int
}

func (w *failAfter) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, errors.New("disk full")
	}
	if len(p) > w.left {
		n := w.left
		w.left = 0
		return n, errors.New("disk full")
	}
	w.left -= len(p)
	return len(p), nil
}

// TestWriteReportsAFailingSink holds Write to reporting the sink's error
// rather than returning a half-written archive as success.
func TestWriteReportsAFailingSink(t *testing.T) {
	t.Parallel()

	blobs := tempBlobs(t)
	if _, err := blobs.Put(context.Background(), strings.NewReader("payload")); err != nil {
		t.Fatalf("put: %v", err)
	}
	entries := []store.Entry{{Key: "buckets/demo", Val: []byte(`{"name":"demo"}`), Version: 1}}

	// 0 fails on the tar header, 512 once the manifest body starts, 1024 with
	// the blob, so each stage of Write is the one that reports.
	for _, budget := range []int{0, 512, 1024} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			t.Parallel()
			if err := state.Write(&failAfter{left: budget}, entries, blobs, state.Runtime{}); err == nil {
				t.Errorf("budget %d: Write reported success on a failing sink", budget)
			}
		})
	}
}
