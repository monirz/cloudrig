package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/services/cloudrun"
)

// deployed returns an emulator with one service running from source.
func deployed(t *testing.T, name string) *cloudrig.Emulator {
	t.Helper()

	emu := cloudrig.MustStart(t)
	if _, err := emu.CloudRun().Deploy(context.Background(), cloudrun.Service{
		Name:   name,
		Source: "../../testdata/run-hello",
	}, cloudrun.Options{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	return emu
}

// TestCloudRunServesOnTheEmulatorPort is what makes a deployed service usable:
// it answers on the one port everything else does.
func TestCloudRunServesOnTheEmulatorPort(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := deployed(t, "front")

	resp, err := http.Get(emu.BaseURL() + "/us-central1-cloudrig-local/front/?name=monir")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := strings.TrimSpace(string(body)); got != "hello monir from front" {
		t.Errorf("body = %q", got)
	}
}

// TestCloudRunKnativeAPI is the surface gcloud uses with a --region: a deploy
// is a Knative Service, and the reply has to carry a Ready status or gcloud
// waits for one that never comes.
func TestCloudRunKnativeAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := deployed(t, "seen")
	base := emu.BaseURL() + "/apis/serving.knative.dev/v1/namespaces/cloudrig-local/services"

	resp, err := http.Get(base + "/seen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var svc struct {
		Metadata struct{ Name string } `json:"metadata"`
		Status   struct {
			URL        string                          `json:"url"`
			Conditions []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&svc); err != nil {
		t.Fatal(err)
	}

	if svc.Metadata.Name != "seen" {
		t.Errorf("name = %q", svc.Metadata.Name)
	}
	var ready bool
	for _, c := range svc.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
		}
	}
	if !ready {
		t.Errorf("no Ready condition: %+v", svc.Status.Conditions)
	}

	// The URL it reports has to be one a caller can follow.
	if svc.Status.URL == "" {
		t.Fatal("status.url is empty")
	}
	follow, err := http.Get(svc.Status.URL + "/")
	if err != nil {
		t.Fatalf("following status.url %q: %v", svc.Status.URL, err)
	}
	defer follow.Body.Close()
	if follow.StatusCode != http.StatusOK {
		t.Errorf("status.url answered %d", follow.StatusCode)
	}
}

// TestCloudRunGlobalList is what `gcloud run services list` without a region
// reaches, with a literal "-" standing for every location.
func TestCloudRunGlobalList(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := deployed(t, "listed")

	resp, err := http.Get(emu.BaseURL() + "/v1/projects/cloudrig-local/locations/-/services")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The Knative list shape. gcloud reads this path as serving.knative.dev,
	// so a v2-shaped reply parses cleanly and lists nothing at all.
	var out struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "ServiceList" {
		t.Errorf("kind = %q, want ServiceList", out.Kind)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	if got := out.Items[0].Metadata.Name; got != "listed" {
		t.Errorf("name = %q", got)
	}
	// gcloud reads the region from this label; without it the REGION column
	// renders blank.
	if got := out.Items[0].Metadata.Labels["cloud.googleapis.com/location"]; got != "us-central1" {
		t.Errorf("location label = %q", got)
	}
}

// TestCloudRunDeployThroughTheAPI covers a create arriving as gcloud sends it.
func TestCloudRunDeployThroughTheAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	body := `{
	  "apiVersion": "serving.knative.dev/v1",
	  "kind": "Service",
	  "metadata": {"name": "posted", "namespace": "cloudrig-local"},
	  "spec": {"template": {"spec": {"containers": [
	    {"image": "source:../../testdata/run-hello", "env": [{"name":"GREETING","value":"hi"}]}
	  ]}}}
	}`

	// An image this emulator cannot pull is the honest failure; a source
	// deploy goes through the library. This asserts the request is understood
	// and the error names the cause rather than the shape.
	resp, err := http.Post(
		emu.BaseURL()+"/apis/serving.knative.dev/v1/namespaces/cloudrig-local/services",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("the route did not match: %s", payload)
	}
	if resp.StatusCode != http.StatusOK && !strings.Contains(string(payload), "image") {
		t.Errorf("status = %d, body = %s", resp.StatusCode, payload)
	}
}

// TestCloudRunIamVerbs covers what gcloud calls on the way to a deploy. It
// asks whether it may allow unauthenticated invocations, and a 404 there stops
// the deploy before it starts.
func TestCloudRunIamVerbs(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/cloudrig-local/locations/us-central1/services/any"

	resp, err := http.Post(base+":testIamPermissions", "application/json",
		bytes.NewBufferString(`{"permissions":["run.services.setIamPolicy"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("testIamPermissions = %d", resp.StatusCode)
	}

	var out struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Permissions) != 1 || out.Permissions[0] != "run.services.setIamPolicy" {
		t.Errorf("permissions = %v, want what was asked for", out.Permissions)
	}
}

// TestCloudRunRevisionsList holds the shape gcloud needs. A list without a
// metadata block crashes the client rather than erroring: it reads continue
// off it without checking for nil.
func TestCloudRunRevisionsList(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := deployed(t, "revised")

	resp, err := http.Get(emu.BaseURL() +
		"/apis/serving.knative.dev/v1/namespaces/cloudrig-local/revisions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Kind     string          `json:"kind"`
		Metadata *map[string]any `json:"metadata"`
		Items    []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if out.Metadata == nil {
		t.Error("no metadata block; gcloud reads continue off it and crashes without one")
	}
	if out.Kind != "RevisionList" {
		t.Errorf("kind = %q", out.Kind)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	if got := out.Items[0].Metadata.Name; got != "revised-00001-cri" {
		t.Errorf("revision = %q", got)
	}
	if got := out.Items[0].Metadata.Labels["serving.knative.dev/service"]; got != "revised" {
		t.Errorf("service label = %q", got)
	}
}

// TestCloudRunRefusesSourceOverTheAPI is a security boundary, not a
// preference. The image field is request-controlled, and reading a source
// directory out of it let any client that could reach the port run a program
// on the emulator's machine — verified by an unauthenticated POST executing
// arbitrary code as the user who started it.
func TestCloudRunRefusesSourceOverTheAPI(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	body := `{"metadata":{"name":"sneaky"},"spec":{"template":{"spec":{"containers":
	          [{"image":"source:/tmp"}]}}}}`

	resp, err := http.Post(
		emu.BaseURL()+"/apis/serving.knative.dev/v1/namespaces/cloudrig-local/services",
		"application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "emulator's machine") {
		t.Errorf("the refusal does not say why: %s", payload)
	}
	if _, running := emu.CloudRun().Describe("cloudrig-local", "us-central1", "sneaky"); running {
		t.Error("the service was deployed anyway")
	}
}

// TestCloudRunResetStopsServices holds that a reset clears what it claims to.
func TestCloudRunResetStopsServices(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	emu := deployed(t, "cleared")
	if resp, err := http.Get(emu.BaseURL() + "/us-central1-cloudrig-local/cleared/"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}

	resp, err := http.Post(emu.BaseURL()+"/_emu/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, still := emu.CloudRun().Describe("cloudrig-local", "us-central1", "cleared"); still {
		t.Error("the service survived a reset")
	}
	after, err := http.Get(emu.BaseURL() + "/us-central1-cloudrig-local/cleared/")
	if err != nil {
		t.Fatal(err)
	}
	defer after.Body.Close()
	if after.StatusCode == http.StatusOK {
		t.Error("a service that was reset is still answering requests")
	}
}
