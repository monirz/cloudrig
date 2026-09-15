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
	"fmt"
	"io"
	"strings"

	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

const (
	manifestName = "manifest.json"
	blobPrefix   = "blobs/"
	formatVer    = 1
)

type manifest struct {
	Version int           `json:"version"`
	Entries []store.Entry `json:"entries"`
}

// Write packs entries and every blob into a tar stream on w.
func Write(w io.Writer, entries []store.Entry, blobs *blob.Store) error {
	tw := tar.NewWriter(w)

	m, err := json.Marshal(manifest{Version: formatVer, Entries: entries})
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
// manifest and adds every blob to blobs.
func Read(r io.Reader, kv *store.Memory, blobs *blob.Store) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("state: read archive: %w", err)
		}
		switch {
		case hdr.Name == manifestName:
			var m manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return fmt.Errorf("state: decode manifest: %w", err)
			}
			if m.Version != formatVer {
				return fmt.Errorf("state: unsupported snapshot version %d", m.Version)
			}
			kv.Restore(m.Entries)
		case strings.HasPrefix(hdr.Name, blobPrefix):
			if _, err := blobs.Put(context.Background(), tr); err != nil {
				return fmt.Errorf("state: restore blob: %w", err)
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
