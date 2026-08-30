package cloudfunctions_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/services/cloudfunctions"
)

// zipOf builds a source archive in memory, the shape gcloud PUTs.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
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
	return buf.Bytes()
}

// upload runs generateUploadUrl then PUTs an archive to the URL it returns,
// exactly as gcloud does.
func upload(t *testing.T, base string, archive []byte) string {
	t.Helper()

	resp, err := http.Post(base+"/v1/projects/p/locations/us-central1/functions:generateUploadUrl",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generateUploadUrl: status %d", resp.StatusCode)
	}

	var out struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.UploadURL, cloudfunctions.UploadPath) {
		t.Fatalf("uploadUrl = %q, want one on this emulator", out.UploadURL)
	}

	req, _ := http.NewRequest(http.MethodPut, out.UploadURL, bytes.NewReader(archive))
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", put.StatusCode)
	}
	return out.UploadURL
}

func deploy(t *testing.T, base, name, uploadURL, runtime, entry string) *http.Response {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"name":            "projects/p/locations/us-central1/functions/" + name,
		"runtime":         runtime,
		"entryPoint":      entry,
		"sourceUploadUrl": uploadURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+"/v1/projects/p/locations/us-central1/functions",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const goSource = `package fn

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) { w.Write([]byte("deployed")) }
`

// TestDeployFromUpload is the gcloud deploy path end to end: ask for an upload
// URL, PUT a zip, create the function, and it runs.
func TestDeployFromUpload(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	url := upload(t, srv.URL, zipOf(t, map[string]string{
		"go.mod": "module example.com/fn\n\ngo 1.25\n",
		"fn.go":  goSource,
	}))

	resp := deploy(t, srv.URL, "uploaded", url, "go", "Handler")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("deploy: status %d: %s", resp.StatusCode, body)
	}

	var op struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatal(err)
	}
	if !op.Done || !strings.HasPrefix(op.Name, "operations/") {
		t.Errorf("operation = %+v", op)
	}

	// The uploaded source is what actually runs.
	callResp, err := http.Post(
		srv.URL+"/v1/projects/p/locations/us-central1/functions/uploaded:call",
		"application/json", strings.NewReader(`{"data":"{}"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer callResp.Body.Close()
	var out callResult
	if err := json.NewDecoder(callResp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Result != "deployed" {
		t.Errorf("result = %q, want deployed", out.Result)
	}
}

func TestDeployRejectsAnUnissuedUploadURL(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	resp := deploy(t, srv.URL, "x", srv.URL+cloudfunctions.UploadPath+"/never-issued", "go", "Handler")
	defer resp.Body.Close()
	body, _ := readAll(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, "generateUploadUrl") {
		t.Errorf("body %s does not say how to get a real upload URL", body)
	}
}

func TestUploadToAnUnknownTokenIs404(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+cloudfunctions.UploadPath+"/bogus",
		strings.NewReader("zip"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeployReportsABuildFailure(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	url := upload(t, srv.URL, zipOf(t, map[string]string{
		"go.mod": "module example.com/fn\n\ngo 1.25\n",
		"fn.go":  "package fn\nthis does not compile\n",
	}))

	resp := deploy(t, srv.URL, "broken", url, "go", "Handler")
	defer resp.Body.Close()
	body, _ := readAll(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: a source that does not build is the caller's fault", resp.StatusCode)
	}
	// The toolchain's own message, naming the file and line. Which tool
	// produced it depends on where the source broke — the entry-point scan
	// parses before go build ever runs.
	if !strings.Contains(body, "fn.go") {
		t.Errorf("body %s does not name the file that failed", body)
	}
}

// TestNeighbouringServiceStubs covers the methods gcloud consults during a
// deploy. Without them it reaches the real googleapis.com hosts.
func TestNeighbouringServiceStubs(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	t.Run("cloudresourcemanager projects.get", func(t *testing.T) {
		var body map[string]any
		if code := getJSON(t, srv.URL+"/v1/projects/my-project", &body); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if body["projectId"] != "my-project" || body["lifecycleState"] != "ACTIVE" {
			t.Errorf("project = %+v", body)
		}
	})

	t.Run("serviceusage services.get reports enabled", func(t *testing.T) {
		var body map[string]any
		code := getJSON(t, srv.URL+"/v1/projects/p/services/cloudbuild.googleapis.com", &body)
		if code != http.StatusOK || body["state"] != "ENABLED" {
			t.Errorf("status = %d, service = %+v", code, body)
		}
	})

	t.Run("cloudbuild defaultServiceAccount", func(t *testing.T) {
		var body map[string]any
		code := getJSON(t, srv.URL+"/v1/projects/p/locations/us-central1/defaultServiceAccount", &body)
		if code != http.StatusOK || body["serviceAccountEmail"] == "" {
			t.Errorf("status = %d, account = %+v", code, body)
		}
	})

	t.Run("projects.testIamPermissions grants what was asked", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/projects/p:testIamPermissions", "application/json",
			strings.NewReader(`{"permissions":["cloudfunctions.functions.create"]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var body struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Permissions) != 1 || body.Permissions[0] != "cloudfunctions.functions.create" {
			t.Errorf("permissions = %v, want what was requested", body.Permissions)
		}
	})

	t.Run("an unknown project verb is 501 naming it", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/v1/projects/p:teleport", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := readAll(resp)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501", resp.StatusCode)
		}
		if !strings.Contains(body, "projects.teleport") {
			t.Errorf("body %s does not name the operation", body)
		}
	})
}
