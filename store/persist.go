package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monirz/cloudrig/core/clock"
)

// FlushInterval is how long a write waits before the snapshot is rewritten.
//
// Writes are debounced rather than flushed each time: a burst of a thousand
// objects should cost one rewrite, not a thousand.
const FlushInterval = time.Second

// Persistent is a Memory that survives a restart.
//
// Reads and writes stay in memory — durability here is a convenience for a
// developer restarting a daemon, not a database — and the whole map is
// snapshotted to one file, written to a temp path and renamed so a crash
// mid-write leaves the previous snapshot intact.
type Persistent struct {
	*Memory

	path string
	clk  clock.Clock

	mu      sync.Mutex
	pending clock.Timer
	closed  bool
}

// OpenPersistent loads a snapshot from path, or starts empty if there is none.
func OpenPersistent(path string, clk clock.Clock) (*Persistent, error) {
	p := &Persistent{Memory: NewMemory(), path: path, clk: clk}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: preparing %s: %w", filepath.Dir(path), err)
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", path, err)
	}

	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("store: %s is not a cloudrig snapshot: %w", path, err)
	}
	p.Restore(entries)
	return p, nil
}

func (p *Persistent) Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) {
	v, err := p.Memory.Put(ctx, key, val, ifVersion)
	if err == nil {
		p.touch()
	}
	return v, err
}

func (p *Persistent) Delete(ctx context.Context, key string, ifVersion uint64) error {
	err := p.Memory.Delete(ctx, key, ifVersion)
	if err == nil {
		p.touch()
	}
	return err
}

func (p *Persistent) Reset(ctx context.Context, keyPrefix string) error {
	err := p.Memory.Reset(ctx, keyPrefix)
	if err == nil {
		p.touch()
	}
	return err
}

// touch schedules a flush, coalescing with one already pending.
func (p *Persistent) touch() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || p.pending != nil {
		return
	}
	p.pending = p.clk.AfterFunc(FlushInterval, func() {
		p.mu.Lock()
		p.pending = nil
		p.mu.Unlock()
		_ = p.Flush()
	})
}

// Flush writes the snapshot now.
func (p *Persistent) Flush() error {
	entries := p.Snapshot()

	encoded, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("store: encoding the snapshot: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".snapshot-")
	if err != nil {
		return fmt.Errorf("store: staging the snapshot: %w", err)
	}
	staged := tmp.Name()
	defer os.Remove(staged)

	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("store: writing the snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: closing the snapshot: %w", err)
	}
	// Renamed rather than written in place, so a crash leaves the previous
	// snapshot whole instead of a half-written one.
	if err := os.Rename(staged, p.path); err != nil {
		return fmt.Errorf("store: replacing the snapshot: %w", err)
	}
	return nil
}

// Close cancels any pending flush and writes a final snapshot.
func (p *Persistent) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.pending != nil {
		p.pending.Stop()
		p.pending = nil
	}
	p.mu.Unlock()

	return p.Flush()
}
