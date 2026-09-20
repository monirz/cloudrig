// Package state packs a running emulator's state into a portable archive and
// loads it back: the key-value metadata plus every object payload.
//
// Blobs are content-addressed, so restoring a payload with Put lands it at the
// same address the metadata already references; the archive need not carry the
// hash or preserve any on-disk layout.
package state

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/monirz/cloudrig/core/faults"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

const (
	manifestName = "manifest.json"
	blobPrefix   = "blobs/"
	formatVer    = 2
)

// Runtime is the deterministic testing state that sits beside the store:
// armed faults and, under a virtual clock, the time. Deployed functions are
// environment, not state, and stay behind.
type Runtime struct {
	Faults []faults.Rule `json:"faults,omitempty"`
	Clock  *ClockState   `json:"clock,omitempty"`
}

// ClockState is a virtual clock's reading. A real clock writes none.
type ClockState struct {
	Now time.Time `json:"now"`
}

type manifest struct {
	Version int           `json:"version"`
	Entries []store.Entry `json:"entries"`
	Runtime Runtime       `json:"runtime,omitzero"`
}

// Write packs entries, the runtime state and every blob into a tar stream on w.
func Write(w io.Writer, entries []store.Entry, blobs *blob.Store, rt Runtime) error {
	tw := tar.NewWriter(w)

	m, err := json.Marshal(manifest{Version: formatVer, Entries: entries, Runtime: rt})
	if err != nil {
		return fmt.Errorf("state: marshal manifest: %w", err)
	}
	if err := writeHeader(tw, manifestName, int64(len(m))); err != nil {
		return err
	}
	if _, err := tw.Write(m); err != nil {
		return fmt.Errorf("state: write manifest: %w", err)
	}

	i := 0
	if err := blobs.Each(func(size int64, r io.Reader) error {
		if err := writeHeader(tw, fmt.Sprintf("%s%d", blobPrefix, i), size); err != nil {
			return err
		}
		i++
		if _, err := io.Copy(tw, r); err != nil {
			return fmt.Errorf("state: write blob: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return tw.Close()
}

// Read loads a stream written by Write: it replaces kv's contents with the
// manifest, adds every blob to blobs, and returns the runtime state for the
// caller to apply. A version 1 archive carries no runtime state.
func Read(r io.Reader, kv *store.Memory, blobs *blob.Store) (Runtime, error) {
	var rt Runtime
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return rt, nil
		}
		if err != nil {
			return Runtime{}, fmt.Errorf("state: read archive: %w", err)
		}
		switch {
		case hdr.Name == manifestName:
			var m manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return Runtime{}, fmt.Errorf("state: decode manifest: %w", err)
			}
			if m.Version < 1 || m.Version > formatVer {
				return Runtime{}, fmt.Errorf("state: unsupported snapshot version %d", m.Version)
			}
			kv.Restore(m.Entries)
			rt = m.Runtime
		case strings.HasPrefix(hdr.Name, blobPrefix):
			if _, err := blobs.Put(context.Background(), tr); err != nil {
				return Runtime{}, fmt.Errorf("state: restore blob: %w", err)
			}
		}
	}
}

func writeHeader(tw *tar.Writer, name string, size int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size}); err != nil {
		return fmt.Errorf("state: write header %q: %w", name, err)
	}
	return nil
}
