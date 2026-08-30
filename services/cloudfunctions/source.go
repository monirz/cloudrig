package cloudfunctions

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// extract unpacks a source archive into a fresh directory and returns it.
//
// gcloud zips the source tree and hands over the archive, so the emulator never
// sees the path the developer edited — the extracted copy is what runs.
func extract(archive, into string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("reading the uploaded source: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		dest, err := safeJoin(into, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := copyEntry(f, dest); err != nil {
			return err
		}
	}
	return nil
}

func copyEntry(f *zip.File, dest string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	// Copied in chunks rather than read whole: an archive entry can be large,
	// and there is no reason for it to sit in the heap.
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return nil
}

// safeJoin refuses an archive entry that would escape the destination, the
// classic zip-slip: a crafted "../../etc/x" would otherwise be written outside.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return filepath.Join(root, clean), nil
}

// installDeps runs npm install when an extracted Node source has none.
//
// gcloud excludes node_modules from the archive, expecting Cloud Build to
// install; without this a deployed Node function has no functions-framework to
// run. Go needs no equivalent — go build fetches its own modules.
func installDeps(dir string, log func(string, ...any)) error {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		return nil
	}

	log("installing node dependencies")
	cmd := exec.Command("npm", "install", "--no-audit", "--no-fund")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install: %w\n%s", err, lastLines(string(out), 20))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
