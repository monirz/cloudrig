package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func openAt(t *testing.T, path string, clk clock.Clock) *store.Persistent {
	t.Helper()
	p, err := store.OpenPersistent(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPersistentSurvivesReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	ctx := context.Background()
	clk := clock.NewFake(epoch)

	first := openAt(t, path, clk)
	if _, err := first.Put(ctx, "a", []byte("one"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Put(ctx, "b", []byte("two"), 0); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openAt(t, path, clk)
	val, version, err := second.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "one" {
		t.Errorf("value = %q, want one", val)
	}
	// The version travels with the entry: a precondition that held before the
	// restart must still hold after it.
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if _, err := second.Put(ctx, "a", []byte("three"), version); err != nil {
		t.Errorf("a compare-and-swap against the restored version failed: %v", err)
	}
}

// TestPersistentPreservesVersions guards the subtle half: restoring at version
// 1 when the entry had reached 5 would let a stale precondition succeed.
func TestPersistentPreservesVersions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	ctx := context.Background()

	first := openAt(t, path, clock.NewFake(epoch))
	var version uint64
	for i := 0; i < 5; i++ {
		var err error
		version, err = first.Put(ctx, "k", []byte("v"), version)
		if err != nil {
			t.Fatal(err)
		}
	}
	first.Close()

	second := openAt(t, path, clock.NewFake(epoch))
	if _, _, err := second.Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	// A stale version must still be refused.
	if _, err := second.Put(ctx, "k", []byte("stale"), 1); !errors.Is(err, store.ErrVersionMismatch) {
		t.Errorf("err = %v for a stale version after a restart, want ErrVersionMismatch", err)
	}
	if _, err := second.Put(ctx, "k", []byte("fresh"), version); err != nil {
		t.Errorf("the current version was refused after a restart: %v", err)
	}
}

// TestPersistentDebouncesWrites checks that a burst costs one rewrite, not one
// per key. The flush is scheduled on the injected clock, so the test drives it.
func TestPersistentDebouncesWrites(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	ctx := context.Background()
	clk := clock.NewFake(epoch)

	p := openAt(t, path, clk)
	for i := 0; i < 100; i++ {
		if _, err := p.Put(ctx, string(rune('a'+i%26))+string(rune(i)), []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
	}

	// Nothing on disk yet: a hundred writes scheduled one flush.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the snapshot was written before the interval elapsed: %v", err)
	}
	if clk.Pending() != 1 {
		t.Errorf("pending flushes = %d, want 1", clk.Pending())
	}

	clk.Advance(store.FlushInterval)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no snapshot after the interval: %v", err)
	}
}

func TestPersistentDeleteAndReset(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	ctx := context.Background()

	p := openAt(t, path, clock.NewFake(epoch))
	p.Put(ctx, "keep", []byte("v"), 0)
	p.Put(ctx, "drop", []byte("v"), 0)
	if err := p.Delete(ctx, "drop", 0); err != nil {
		t.Fatal(err)
	}
	p.Close()

	reopened := openAt(t, path, clock.NewFake(epoch))
	if _, _, err := reopened.Get(ctx, "drop"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a deleted key came back after a restart")
	}
	if _, _, err := reopened.Get(ctx, "keep"); err != nil {
		t.Errorf("a kept key did not survive: %v", err)
	}

	if err := reopened.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}
	reopened.Close()

	empty := openAt(t, path, clock.NewFake(epoch))
	if entries, _, _ := empty.List(ctx, "", 0, ""); len(entries) != 0 {
		t.Errorf("%d keys survived a reset", len(entries))
	}
}

func TestPersistentStartsEmptyWithoutASnapshot(t *testing.T) {
	t.Parallel()

	p := openAt(t, filepath.Join(t.TempDir(), "absent.json"), clock.NewFake(epoch))
	if entries, _, err := p.List(context.Background(), "", 0, ""); err != nil || len(entries) != 0 {
		t.Errorf("List = %d entries, %v; want an empty store", len(entries), err)
	}
}

func TestPersistentRejectsAMalformedSnapshot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not a snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Louder than starting empty: silently discarding a developer's data
	// because a file was unreadable is the worse failure.
	if _, err := store.OpenPersistent(path, clock.NewFake(epoch)); err == nil {
		t.Error("a malformed snapshot was accepted")
	}
}

func TestPersistentCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := openAt(t, filepath.Join(t.TempDir(), "s.json"), clock.NewFake(epoch))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
