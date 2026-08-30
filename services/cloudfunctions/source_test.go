package cloudfunctions

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{name: "a plain file", entry: "index.js"},
		{name: "a nested file", entry: "lib/util.js"},
		{name: "a dot-slash prefix", entry: "./index.js"},
		// zip-slip: a crafted entry escaping the destination is the classic
		// archive vulnerability, and it writes outside the sandbox silently.
		{name: "parent traversal", entry: "../escaped.js", wantErr: true},
		{name: "deep traversal", entry: "a/../../escaped.js", wantErr: true},
		{name: "absolute path", entry: "/etc/passwd", wantErr: true},
	}

	root := t.TempDir()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := safeJoin(root, tc.entry)
			if tc.wantErr {
				if err == nil {
					t.Errorf("accepted %q, resolving to %q", tc.entry, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected %q: %v", tc.entry, err)
			}
			if !strings.HasPrefix(got, root) {
				t.Errorf("%q resolved to %q, outside %q", tc.entry, got, root)
			}
		})
	}
}

func TestExtract(t *testing.T) {
	t.Parallel()

	archive := filepath.Join(t.TempDir(), "src.zip")
	writeZip(t, archive, map[string]string{
		"index.js":     "exports.handler = () => {}\n",
		"lib/util.js":  "module.exports = 1\n",
		"package.json": `{"name":"x"}`,
	})

	into := t.TempDir()
	if err := extract(archive, into); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"index.js":    "exports.handler = () => {}\n",
		"lib/util.js": "module.exports = 1\n",
	} {
		got, err := os.ReadFile(filepath.Join(into, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestExtractRefusesAnEscapingEntry(t *testing.T) {
	t.Parallel()

	archive := filepath.Join(t.TempDir(), "evil.zip")
	writeZip(t, archive, map[string]string{"../escaped.js": "pwned\n"})

	into := t.TempDir()
	if err := extract(archive, into); err == nil {
		t.Fatal("extracted an entry that escapes the destination")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(into), "escaped.js")); err == nil {
		t.Error("the escaping entry was written outside the destination")
	}
}

func TestExtractRejectsAMalformedArchive(t *testing.T) {
	t.Parallel()

	bad := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extract(bad, t.TempDir()); err == nil {
		t.Error("accepted something that is not an archive")
	}
}

func TestInstallDeps(t *testing.T) {
	t.Parallel()

	t.Run("skipped without a package.json", func(t *testing.T) {
		t.Parallel()
		if err := installDeps(t.TempDir(), func(string, ...any) {}); err != nil {
			t.Errorf("err = %v, want nil for a non-Node source", err)
		}
	})

	t.Run("skipped when node_modules is already present", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "node_modules"), 0o750); err != nil {
			t.Fatal(err)
		}
		logged := false
		if err := installDeps(dir, func(string, ...any) { logged = true }); err != nil {
			t.Errorf("err = %v", err)
		}
		if logged {
			t.Error("ran an install for a source that already has its dependencies")
		}
	})
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	for name, content := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
