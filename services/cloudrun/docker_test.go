package cloudrun_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/client"

	"github.com/monirz/cloudrig/services/cloudrun"
)

// testImage builds an image from the sample service and returns its tag.
//
// FROM scratch and a static binary, so the build pulls nothing: a test that
// reaches a registry is a test that fails when the network does.
func testImage(t *testing.T) string {
	t.Helper()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no Docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	ctx := context.Background()
	info, err := cli.Info(ctx)
	if err != nil {
		t.Skipf("Docker is not reachable: %v", err)
	}

	// Built for the daemon's architecture, not this machine's: they differ
	// whenever the daemon is a VM.
	dir := t.TempDir()
	binary := filepath.Join(dir, "service")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "../../testdata/run-hello"
	build.Env = append(os.Environ(),
		"CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goArch(info.Architecture), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the test service: %v\n%s", err, out)
	}

	const tag = "cloudrig-test-run:latest"
	if err := buildImage(ctx, cli, binary, tag); err != nil {
		t.Fatalf("building the test image: %v", err)
	}
	return tag
}

// goArch translates the daemon's uname-style architecture into Go's name for
// it. They agree on nothing: aarch64 is arm64, x86_64 is amd64.
func goArch(reported string) string {
	switch reported {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	case "armv7l", "armv6l":
		return "arm"
	}
	return reported
}

// buildImage sends a two-entry context — a Dockerfile and the binary — to the
// daemon.
func buildImage(ctx context.Context, cli *client.Client, binary, tag string) error {
	payload, err := os.ReadFile(binary)
	if err != nil {
		return err
	}

	const dockerfile = "FROM scratch\nCOPY service /service\nENTRYPOINT [\"/service\"]\n"
	var context_ bytes.Buffer
	tw := tar.NewWriter(&context_)
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{
		{"Dockerfile", []byte(dockerfile), 0o644},
		{"service", payload, 0o755},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Size: int64(len(f.body)), Mode: f.mode,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(f.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	resp, err := cli.ImageBuild(ctx, &context_, build.ImageBuildOptions{
		Tags: []string{tag}, Remove: true,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The build is only done once its progress stream ends.
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func dockerOrSkip(t *testing.T) {
	t.Helper()

	if !cloudrun.DockerAvailable(context.Background()) {
		t.Skip("no Docker daemon")
	}
}

// TestContainerRuns is what Cloud Run actually does: an image, a container, a
// request answered by it.
func TestContainerRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an image")
	}
	dockerOrSkip(t)
	t.Parallel()

	tag := testImage(t)
	r := registry(t)

	svc, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:  "boxed",
		Image: tag,
	}, cloudrun.Options{})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if svc.Revision() != "boxed-00001-cri" {
		t.Errorf("revision = %q", svc.Revision())
	}

	inst, ok := r.Instance("", "", "boxed")
	if !ok {
		t.Fatal("the service is not registered")
	}

	resp, err := http.Get(inst.URL() + "/?name=monir")
	if err != nil {
		t.Fatalf("calling the container: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := strings.TrimSpace(string(body)); got != "hello monir from boxed" {
		t.Errorf("body = %q", got)
	}
}

// TestContainerEnvironment holds the variables Cloud Run guarantees inside the
// container, which is where code actually reads them.
func TestContainerEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an image")
	}
	dockerOrSkip(t)
	t.Parallel()

	tag := testImage(t)
	r := registry(t)

	if _, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:  "boxed-env",
		Image: tag,
		Env:   []string{"GREETING=ciao"},
	}, cloudrun.Options{}); err != nil {
		t.Fatal(err)
	}

	inst, _ := r.Instance("", "", "boxed-env")
	resp, err := http.Get(inst.URL() + "/env")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := strings.TrimSpace(string(body)); got != "boxed-env|boxed-env-00001-cri|ciao" {
		t.Errorf("environment = %q", got)
	}
}

// TestContainerLogs holds that the container's own output is captured, which
// is the only account of why one failed.
func TestContainerLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an image")
	}
	dockerOrSkip(t)
	t.Parallel()

	tag := testImage(t)
	r := registry(t)

	if _, err := r.Deploy(context.Background(), cloudrun.Service{
		Name: "boxed-logs", Image: tag,
	}, cloudrun.Options{}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "boxed-logs")

	// The service logs a line as it starts; the framing Docker adds must not
	// come through with it.
	logged := waitFor(t, inst, "listening on")
	if strings.ContainsAny(logged, "\x00\x01\x02") {
		t.Errorf("the log kept Docker's stream framing: %q", logged)
	}
}

// TestDeployNeedsAnImageOrSource pins the two ways to deploy, and that they
// are not both.
func TestDeployNeedsAnImageOrSource(t *testing.T) {
	t.Parallel()

	r := registry(t)
	ctx := context.Background()

	if _, err := r.Deploy(ctx, cloudrun.Service{Name: "neither"}, cloudrun.Options{}); err == nil {
		t.Error("a deploy with no image and no source succeeded")
	}
	_, err := r.Deploy(ctx, cloudrun.Service{
		Name: "both", Image: "x", Source: "y",
	}, cloudrun.Options{})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v, want it to reject an image and a source together", err)
	}
}

// TestContainerResourceLimits is why this exists: gcloud accepts --memory, and
// a limit that is accepted but not applied means a service that would be
// killed in production runs happily here.
func TestContainerResourceLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an image")
	}
	dockerOrSkip(t)
	t.Parallel()

	tag := testImage(t)
	r := registry(t)

	if _, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:   "limited",
		Image:  tag,
		Memory: "512Mi",
		CPU:    "500m",
	}, cloudrun.Options{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	inst, _ := r.Instance("", "", "limited")
	inspected, err := cli.ContainerInspect(context.Background(), inst.ContainerID())
	if err != nil {
		t.Fatal(err)
	}

	if got := inspected.HostConfig.Memory; got != 512<<20 {
		t.Errorf("memory = %d, want %d — the limit never reached the container", got, 512<<20)
	}
	if got := inspected.HostConfig.NanoCPUs; got != 5e8 {
		t.Errorf("cpu = %d, want 5e8", got)
	}
}
