package conformance

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/faults"
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

// TestSnapshotCarriesFaultsAndClock is the deterministic-state contract: a
// restored emulator fails the same way and reads the same time. Deployed
// functions are environment, not state, and stay behind.
func TestSnapshotCarriesFaultsAndClock(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	src := cloudrig.MustStart(t, cloudrig.Options{Clock: clock.NewFake(at)})
	src.Faults().Add(faults.Rule{Path: "/storage/v1/*", Status: 503})

	resp, err := http.Get(src.BaseURL() + "/_emu/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	dst := cloudrig.MustStart(t, cloudrig.Options{Clock: clock.NewFake(at.Add(100 * time.Hour))})
	post, err := http.Post(dst.BaseURL()+"/_emu/snapshot", "application/x-tar", bytes.NewReader(snap))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()

	if got := dst.Clock().Now(); !got.Equal(at) {
		t.Errorf("restored clock = %s, want the snapshotted %s", got, at)
	}
	if n := dst.Faults().Len(); n != 1 {
		t.Fatalf("restored emulator has %d faults armed, want 1", n)
	}

	hit, err := http.Get(dst.BaseURL() + "/storage/v1/b/anything")
	if err != nil {
		t.Fatal(err)
	}
	hit.Body.Close()
	if hit.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("restored fault answered %d, want 503", hit.StatusCode)
	}
}
