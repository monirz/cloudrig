package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/store"
)

// openBolt opens a database at path, closed when the test ends. Closing it
// again first, as the restart tests do, is harmless.
func openBolt(t *testing.T, path string) *store.Bolt {
	t.Helper()
	b, err := store.OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func closeBolt(t *testing.T, b *store.Bolt) {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoltSurvivesReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kv.db")
	ctx := context.Background()

	first := openBolt(t, path)
	if _, err := first.Put(ctx, "a", []byte("one"), 0); err != nil {
		t.Fatal(err)
	}
	closeBolt(t, first)

	second := openBolt(t, path)
	val, version, err := second.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "one" || version != 1 {
		t.Errorf("Get = %q at %d, want one at 1", val, version)
	}
}

// TestBoltPreservesVersions guards the subtle half of a restart: coming back
// at version 1 when the entry had reached 5 would let a stale precondition
// succeed.
func TestBoltPreservesVersions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kv.db")
	ctx := context.Background()

	first := openBolt(t, path)
	var version uint64
	for range 5 {
		var err error
		if version, err = first.Put(ctx, "k", []byte("v"), version); err != nil {
			t.Fatal(err)
		}
	}
	closeBolt(t, first)

	second := openBolt(t, path)
	if _, err := second.Put(ctx, "k", []byte("stale"), 1); !errors.Is(err, store.ErrVersionMismatch) {
		t.Errorf("err = %v for a stale version after a restart, want ErrVersionMismatch", err)
	}
	if _, err := second.Put(ctx, "k", []byte("fresh"), version); err != nil {
		t.Errorf("the current version was refused after a restart: %v", err)
	}
}

func TestBoltDeleteAndResetSurviveReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kv.db")
	ctx := context.Background()

	b := openBolt(t, path)
	for _, k := range []string{"keep", "drop", "gone/a", "gone/b"} {
		if _, err := b.Put(ctx, k, []byte("v"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Delete(ctx, "drop", 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Reset(ctx, "gone/"); err != nil {
		t.Fatal(err)
	}
	closeBolt(t, b)

	reopened := openBolt(t, path)
	entries, _, err := reopened.List(ctx, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := joinKeys(entries); got != "keep" {
		t.Errorf("keys after a restart = %q, want keep", got)
	}
}

// TestBoltImportsAnOldSnapshot covers the upgrade: a --data-dir written by the
// JSON store keeps its contents, versions included, and is imported once.
func TestBoltImportsAnOldSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "metadata.json")
	writeSnapshot(t, snapshot, []store.Entry{
		{Key: "b/one", Val: []byte("1"), Version: 3},
		{Key: "b/two", Val: []byte("2"), Version: 1},
	})
	ctx := context.Background()

	b := openBolt(t, filepath.Join(dir, "metadata.db"))
	if err := b.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	val, version, err := b.Get(ctx, "b/one")
	if err != nil || string(val) != "1" || version != 3 {
		t.Errorf("Get = %q at %d, %v; want 1 at 3", val, version, err)
	}
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Error("the snapshot is still in place, so it would be imported again")
	}
	if _, err := os.Stat(snapshot + ".imported"); err != nil {
		t.Errorf("the snapshot was not kept beside the database: %v", err)
	}
	// Run again, as the next start does: nothing to import, nothing changes.
	if err := b.ImportSnapshot(snapshot); err != nil {
		t.Errorf("a second import: %v", err)
	}
}

func TestBoltImportRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("into a database that has data", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		snapshot := filepath.Join(dir, "metadata.json")
		writeSnapshot(t, snapshot, []store.Entry{{Key: "old", Val: []byte("x"), Version: 1}})
		b := openBolt(t, filepath.Join(dir, "metadata.db"))
		if _, err := b.Put(ctx, "new", []byte("y"), 0); err != nil {
			t.Fatal(err)
		}
		if err := b.ImportSnapshot(snapshot); err == nil {
			t.Error("an old snapshot was imported over newer data")
		}
	})

	t.Run("a malformed snapshot", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		snapshot := filepath.Join(dir, "metadata.json")
		if err := os.WriteFile(snapshot, []byte("not a snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Louder than starting empty: discarding a developer's data because a
		// file was unreadable is the worse failure.
		b := openBolt(t, filepath.Join(dir, "metadata.db"))
		if err := b.ImportSnapshot(snapshot); err == nil {
			t.Error("a malformed snapshot was accepted")
		}
	})
}

func TestBoltRefusesASecondOpener(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kv.db")
	openBolt(t, path)

	_, err := store.OpenBolt(path)
	if err == nil || !strings.Contains(err.Error(), "another cloudrig") {
		t.Errorf("second open err = %v, want one naming the lock", err)
	}
}

func TestBoltCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	b := openBolt(t, filepath.Join(t.TempDir(), "kv.db"))
	closeBolt(t, b)
	if err := b.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func writeSnapshot(t *testing.T, path string, entries []store.Entry) {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
