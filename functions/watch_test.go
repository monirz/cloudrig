package functions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
)

func TestFingerprint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package a\n")
	first := fingerprint(dir)

	t.Run("stable when nothing changes", func(t *testing.T) {
		if fingerprint(dir) != first {
			t.Error("fingerprint moved with no change")
		}
	})

	t.Run("changes when content changes", func(t *testing.T) {
		writeFile(t, dir, "a.go", "package a // edited\n")
		if fingerprint(dir) == first {
			t.Error("fingerprint did not move after an edit")
		}
	})

	t.Run("changes when a file is added", func(t *testing.T) {
		before := fingerprint(dir)
		writeFile(t, dir, "b.go", "package a\n")
		if fingerprint(dir) == before {
			t.Error("fingerprint did not move after a file was added")
		}
	})

	t.Run("changes when a file is removed", func(t *testing.T) {
		before := fingerprint(dir)
		if err := os.Remove(filepath.Join(dir, "b.go")); err != nil {
			t.Fatal(err)
		}
		if fingerprint(dir) == before {
			t.Error("fingerprint did not move after a file was removed")
		}
	})

	t.Run("ignores node_modules", func(t *testing.T) {
		before := fingerprint(dir)
		nm := filepath.Join(dir, "node_modules", "pkg")
		if err := os.MkdirAll(nm, 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, nm, "index.js", "module.exports = {}\n")
		if fingerprint(dir) != before {
			t.Error("node_modules moved the fingerprint; a walk of it would be enormous and pointless")
		}
	})
}

// TestWatchIsDrivenByTheClock is why the watcher schedules through Clock rather
// than a ticker: a test triggers a rescan by advancing time, with no sleeping.
func TestWatchIsDrivenByTheClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "a.go", "package a\n")

	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	changes := 0
	stop := watch(clk, dir, WatchInterval, func() { changes++ })
	defer stop()

	t.Run("no change means no callback", func(t *testing.T) {
		clk.Advance(10 * WatchInterval)
		if changes != 0 {
			t.Errorf("fired %d times with no edit", changes)
		}
	})

	t.Run("an edit fires once", func(t *testing.T) {
		writeFile(t, dir, "a.go", "package a // one\n")
		clk.Advance(WatchInterval)
		if changes != 1 {
			t.Fatalf("fired %d times, want 1", changes)
		}
		// The same edit must not fire again: the fingerprint is now the
		// baseline.
		clk.Advance(10 * WatchInterval)
		if changes != 1 {
			t.Errorf("fired %d times after one edit, want 1", changes)
		}
	})

	t.Run("a second edit fires again", func(t *testing.T) {
		writeFile(t, dir, "a.go", "package a // two\n")
		clk.Advance(WatchInterval)
		if changes != 2 {
			t.Errorf("fired %d times, want 2", changes)
		}
	})

	t.Run("stop ends it", func(t *testing.T) {
		stop()
		writeFile(t, dir, "a.go", "package a // three\n")
		clk.Advance(10 * WatchInterval)
		if changes != 2 {
			t.Errorf("fired %d times after stop, want 2", changes)
		}
	})
}

func TestWatchStopIsIdempotent(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(time.Now())
	stop := watch(clk, t.TempDir(), WatchInterval, func() {})
	stop()
	stop()
}

func TestWatchLeavesNoTimerBehind(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	stop := watch(clk, t.TempDir(), WatchInterval, func() {})
	if clk.Pending() != 1 {
		t.Fatalf("pending timers = %d, want 1", clk.Pending())
	}
	stop()
	if clk.Pending() != 0 {
		t.Errorf("pending timers = %d after stop, want 0", clk.Pending())
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// mtime has coarse resolution on some filesystems, so make each write
	// unmistakably later than the last.
	future := time.Now().Add(time.Duration(len(content)) * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(content) == "" {
		t.Fatal("empty content makes the fingerprint ambiguous")
	}
}
