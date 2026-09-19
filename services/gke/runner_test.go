package gke

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// fakeCLI puts stand-ins for kind and k3d on PATH. They answer the commands the
// runners send, and fail every command when FAKE_CLI_FAIL is set.
func fakeCLI(t *testing.T, fail bool) {
	t.Helper()
	dir := t.TempDir()
	const script = `#!/bin/sh
[ -n "$FAKE_CLI_FAIL" ] && { echo boom; exit 1; }
case "$*" in
  version) echo "${0##*/} v0.0.0-fake" ;;
  *kubeconfig*) printf 'apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n' ;;
esac
`
	for _, name := range []string{"kind", "k3d"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if fail {
		t.Setenv("FAKE_CLI_FAIL", "1")
	}
}

func TestRunnersDriveTheirCLI(t *testing.T) {
	fakeCLI(t, false)
	ctx := context.Background()

	for name, r := range map[string]clusterRunner{"kind": kindRunner{}, "k3d": k3dRunner{}} {
		if !r.available(ctx) {
			t.Errorf("%s: not available with the CLI on PATH", name)
		}
		endpoint, err := r.create(ctx, "dev")
		if err != nil || endpoint != "127.0.0.1:6443" {
			t.Errorf("%s: create = %q, %v", name, endpoint, err)
		}
		cfg, err := r.kubeconfig(ctx, "dev")
		if err != nil || !strings.Contains(string(cfg), "server:") {
			t.Errorf("%s: kubeconfig = %q, %v", name, cfg, err)
		}
		if err := r.delete(ctx, "dev"); err != nil {
			t.Errorf("%s: delete: %v", name, err)
		}
	}
	if _, ok := chooseRunner(ctx).(k3dRunner); !ok {
		t.Error("chooseRunner did not prefer k3d")
	}
}

func TestRunnersReportCLIFailures(t *testing.T) {
	fakeCLI(t, true)
	ctx := context.Background()

	for name, r := range map[string]clusterRunner{"kind": kindRunner{}, "k3d": k3dRunner{}} {
		if r.available(ctx) {
			t.Errorf("%s: available although the CLI fails", name)
		}
		if _, err := r.create(ctx, "dev"); err == nil {
			t.Errorf("%s: create succeeded", name)
		}
		if _, err := r.kubeconfig(ctx, "dev"); err == nil {
			t.Errorf("%s: kubeconfig succeeded", name)
		}
		if err := r.delete(ctx, "dev"); err == nil {
			t.Errorf("%s: delete succeeded", name)
		}
	}
	if _, ok := chooseRunner(ctx).(kindRunner); !ok {
		t.Error("chooseRunner did not fall back to kind")
	}
}

func TestRunnersNeedTheirCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	if (kindRunner{}).available(ctx) || (k3dRunner{}).available(ctx) {
		t.Error("a runner is available with nothing on PATH")
	}
}

func TestEndpointFromKubeconfig(t *testing.T) {
	t.Parallel()
	if got := endpointFromKubeconfig("server: https://10.0.0.1:443\n"); got != "10.0.0.1:443" {
		t.Errorf("endpoint = %q", got)
	}
	if got := endpointFromKubeconfig("apiVersion: v1\n"); got != "" {
		t.Errorf("endpoint of a kubeconfig without a server = %q", got)
	}
}

func TestRESTRefusesBadBodies(t *testing.T) {
	t.Parallel()
	srv, _ := restServer(t)
	url := srv.URL + "/v1/projects/p/locations/us-central1/clusters"

	if code, _ := do(t, http.MethodPost, url, `{`); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(strings.Repeat(" ", MaxBodyBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d, want 413", resp.StatusCode)
	}
}

func TestHTTPStatusOf(t *testing.T) {
	t.Parallel()
	for code, want := range map[codes.Code]int{
		codes.OK:                 200,
		codes.FailedPrecondition: 400,
		codes.OutOfRange:         400,
		codes.Aborted:            409,
		codes.Unimplemented:      501,
		codes.Unavailable:        500,
	} {
		if got := httpStatusOf(code); got != want {
			t.Errorf("httpStatusOf(%v) = %d, want %d", code, got, want)
		}
	}
}
