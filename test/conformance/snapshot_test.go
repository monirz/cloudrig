package conformance

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/monirz/cloudrig"
)

// TestSnapshotRestoreAcrossEmulators is the operator-facing round trip: a
// bucket and object saved from one emulator reappear in a fresh one after its
// snapshot is restored, metadata and payload both.
func TestSnapshotRestoreAcrossEmulators(t *testing.T) {
	t.Parallel()

	src := cloudrig.MustStart(t)
	sc, ctx := gcs(t, src)
	if err := sc.Bucket("keep").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := sc.Bucket("keep").Object("hello.txt").NewWriter(ctx)
	w.Write([]byte("payload"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Save src's state.
	resp, err := http.Get(src.BaseURL() + "/_emu/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot GET = %d, want 200", resp.StatusCode)
	}
	snap, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Load it into a fresh emulator that has never seen the bucket.
	dst := cloudrig.MustStart(t)
	post, err := http.Post(dst.BaseURL()+"/_emu/snapshot", "application/x-tar", bytes.NewReader(snap))
	if err != nil {
		t.Fatal(err)
	}
	if post.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(post.Body)
		t.Fatalf("restore POST = %d (%s), want 204", post.StatusCode, body)
	}
	post.Body.Close()

	dc, dctx := gcs(t, dst)
	if got := read(t, dc, dctx, "keep", "hello.txt"); got != "payload" {
		t.Errorf("restored object = %q, want %q", got, "payload")
	}
}
