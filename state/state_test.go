package state_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

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
	if err := state.Write(&buf, entries, srcBlobs); err != nil {
		t.Fatalf("write: %v", err)
	}

	dst := store.NewMemory()
	dstBlobs := tempBlobs(t)
	if err := state.Read(&buf, dst, dstBlobs); err != nil {
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
