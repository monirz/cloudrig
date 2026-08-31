package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
)

// TestGcloudStorage drives the real gcloud storage CLI.
//
// It is here for the same reason the functions one is: a compatibility claim
// with no test that runs the client is worth nothing a week later. It found
// two gaps on its first run — an unrouted storageLayout, and gcloud quoting a
// multipart boundary in a way Go's parser refuses.
func TestGcloudStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("runs gcloud against a real emulator")
	}
	if _, err := exec.LookPath("gcloud"); err != nil {
		t.Skip("gcloud is not installed")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	env := append(os.Environ(),
		"CLOUDSDK_CORE_PROJECT=my-project",
		"CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE="+emu.BaseURL()+"/storage/v1/",
	)

	run := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.Command("gcloud", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	dir := t.TempDir()
	local := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(local, []byte("a,b,c\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run(t, "storage", "buckets", "create", "gs://cli-bucket", "--project", "my-project")

	t.Run("ls shows the bucket", func(t *testing.T) {
		if out := run(t, "storage", "ls"); !strings.Contains(out, "gs://cli-bucket/") {
			t.Errorf("ls = %q", out)
		}
	})

	t.Run("cp uploads", func(t *testing.T) {
		run(t, "storage", "cp", local, "gs://cli-bucket/report.csv")
		if out := run(t, "storage", "ls", "gs://cli-bucket"); !strings.Contains(out, "report.csv") {
			t.Errorf("ls = %q", out)
		}
	})

	t.Run("cat reads it back", func(t *testing.T) {
		if out := run(t, "storage", "cat", "gs://cli-bucket/report.csv"); !strings.Contains(out, "a,b,c") {
			t.Errorf("cat = %q", out)
		}
	})

	t.Run("cp downloads", func(t *testing.T) {
		back := filepath.Join(dir, "back.csv")
		run(t, "storage", "cp", "gs://cli-bucket/report.csv", back)

		got, err := os.ReadFile(back)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "a,b,c" {
			t.Errorf("downloaded %q", got)
		}
	})

	t.Run("cp copies server-side", func(t *testing.T) {
		run(t, "storage", "cp", "gs://cli-bucket/report.csv", "gs://cli-bucket/copy.csv")
		if out := run(t, "storage", "cat", "gs://cli-bucket/copy.csv"); !strings.Contains(out, "a,b,c") {
			t.Errorf("cat = %q", out)
		}
	})

	t.Run("rm deletes", func(t *testing.T) {
		run(t, "storage", "rm", "gs://cli-bucket/report.csv")
		out := run(t, "storage", "ls", "gs://cli-bucket")
		if strings.Contains(out, "report.csv") {
			t.Errorf("the object survived rm: %q", out)
		}
		if !strings.Contains(out, "copy.csv") {
			t.Errorf("rm took the copy with it: %q", out)
		}
	})
}
