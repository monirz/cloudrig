package functions

import (
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirz/cloudrig/core/clock"
)

// WatchInterval is how often a watched source tree is rescanned.
const WatchInterval = 300 * time.Millisecond

// skipDirs are never walked. node_modules alone can hold tens of thousands of
// files, and none of them are the source you are editing.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
}

// watcher rescans a source tree and calls onChange when its fingerprint moves.
//
// Polling rather than fsnotify keeps the dependency count at zero, and the poll
// is scheduled through the injected Clock rather than a ticker — so a test with
// a FakeClock triggers a rescan by advancing time, with no sleeping and no
// flakiness.
type watcher struct {
	clk      clock.Clock
	dir      string
	interval time.Duration
	onChange func()

	mu      sync.Mutex
	last    string
	timer   clock.Timer
	stopped bool
}

// watch starts polling dir. The returned function stops it.
func watch(clk clock.Clock, dir string, interval time.Duration, onChange func()) func() {
	w := &watcher{clk: clk, dir: dir, interval: interval, onChange: onChange}
	w.last = fingerprint(dir)
	w.schedule()
	return w.stop
}

func (w *watcher) schedule() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.timer = w.clk.AfterFunc(w.interval, w.tick)
}

func (w *watcher) tick() {
	current := fingerprint(w.dir)

	w.mu.Lock()
	changed := current != w.last
	w.last = current
	stopped := w.stopped
	w.mu.Unlock()

	if stopped {
		return
	}
	// Reschedule before the callback: a redeploy stops this watcher and starts
	// a new one, and the stop must find a timer to cancel.
	w.schedule()
	if changed {
		w.onChange()
	}
}

func (w *watcher) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

// fingerprint summarises a tree as its files' sizes and modification times.
// Contents are not read: a source tree can be large, and mtime is what an
// editor changes on save.
func fingerprint(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is itself a change; recording the
			// error keeps the fingerprint different from the run before.
			b.WriteString(path)
			b.WriteString("!\n")
			return nil
		}
		if d.IsDir() {
			if path != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		b.WriteString(path)
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(info.Size(), 10))
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}
