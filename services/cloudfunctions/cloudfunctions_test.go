package cloudfunctions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/functions"
	"github.com/monirz/cloudrig/services/cloudfunctions"
)

const (
	helloDir = "../../examples/hello"
	// moduleDir is a function that is its own Go module, which an uploaded
	// source has to be: gcloud zips the directory, so an enclosing go.mod does
	// not travel with it.
	moduleDir = "../../testdata/go-hello"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serve builds the API over a registry holding the given functions.
func serve(t *testing.T, fns ...functions.Function) *httptest.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a function")
	}

	reg := functions.NewRegistry(clock.NewFake(epoch), nil, functions.Options{})
	t.Cleanup(reg.StopAll)
	for _, f := range fns {
		if _, err := reg.Deploy(context.Background(), f); err != nil {
			t.Fatalf("deploy %s: %v", f.Name, err)
		}
	}

	srv := httptest.NewServer(cloudfunctions.New(reg, clock.NewFake(epoch), nil))
	t.Cleanup(srv.Close)
	return srv
}

func goFn(project, location, name string) functions.Function {
	return functions.Function{
		Project: project, Location: location, Name: name,
		Source: helloDir, EntryPoint: "HelloHTTP",
	}
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		_ = json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func TestGetFunctionV1(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "us-central1", "hello"))

	var fn map[string]any
	code := getJSON(t, srv.URL+"/v1/projects/p/locations/us-central1/functions/hello", &fn)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if fn["name"] != "projects/p/locations/us-central1/functions/hello" {
		t.Errorf("name = %v", fn["name"])
	}
	if fn["status"] != "ACTIVE" || fn["runtime"] != "go" || fn["entryPoint"] != "HelloHTTP" {
		t.Errorf("function = %+v", fn)
	}
	trigger, ok := fn["httpsTrigger"].(map[string]any)
	if !ok || !strings.HasSuffix(trigger["url"].(string), "/us-central1-p/hello") {
		t.Errorf("httpsTrigger = %v", fn["httpsTrigger"])
	}
}

func TestGetFunctionV2IsTheSameFacts(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "us-central1", "hello"))

	var fn map[string]any
	if code := getJSON(t, srv.URL+"/v2/projects/p/locations/us-central1/functions/hello", &fn); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	// Same truth, v2's nesting.
	if fn["environment"] != "GEN_1" || fn["state"] != "ACTIVE" {
		t.Errorf("function = %+v", fn)
	}
	build, ok := fn["buildConfig"].(map[string]any)
	if !ok || build["runtime"] != "go" || build["entryPoint"] != "HelloHTTP" {
		t.Errorf("buildConfig = %v", fn["buildConfig"])
	}
}

// TestNotFoundPointsAtTheRightProject guards the easiest mistake to make:
// deploying to one project and addressing another. "does not exist" alone sends
// you looking for the wrong problem.
func TestNotFoundPointsAtTheRightProject(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("my-project", "us-central1", "hello"))

	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	code := getJSON(t, srv.URL+"/v1/projects/other/locations/us-central1/functions/hello", &body)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if !strings.Contains(body.Error.Message, "projects/my-project/locations/us-central1/functions/hello") {
		t.Errorf("message = %q, want it to name where the function actually is", body.Error.Message)
	}

	// A name that exists nowhere must not gain a misleading hint.
	body.Error.Message = ""
	getJSON(t, srv.URL+"/v1/projects/other/locations/us-central1/functions/absent", &body)
	if strings.Contains(body.Error.Message, "deployed at") {
		t.Errorf("message = %q, want no hint for a name that exists nowhere", body.Error.Message)
	}
}

func TestMissingFunctionIs404(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	for _, path := range []string{
		"/v1/projects/p/locations/us-central1/functions/nope",
		"/v2/projects/p/locations/us-central1/functions/nope",
	} {
		var body map[string]any
		if code := getJSON(t, srv.URL+path, &body); code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, code)
		}
	}
}

func TestListV1(t *testing.T) {
	t.Parallel()
	srv := serve(t,
		goFn("p", "us-central1", "b"),
		goFn("p", "europe-west1", "a"),
		goFn("other", "us-central1", "elsewhere"),
	)

	var body struct{ Functions []map[string]any }
	getJSON(t, srv.URL+"/v1/projects/p/locations/-/functions", &body)
	if len(body.Functions) != 2 {
		t.Fatalf("got %d functions, want 2 (the other project must not appear)", len(body.Functions))
	}

	getJSON(t, srv.URL+"/v1/projects/p/locations/europe-west1/functions", &body)
	if len(body.Functions) != 1 {
		t.Errorf("region-scoped list returned %d, want 1", len(body.Functions))
	}
}

// TestV2ListHonoursTheEnvironmentFilter guards a duplicate-listing bug: gcloud
// lists by calling v1 for gen1 and v2 filtered to GEN_2, then merging. Ignoring
// the filter makes every function answer both calls and appear twice.
func TestV2ListHonoursTheEnvironmentFilter(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "us-central1", "hello"))

	tests := []struct {
		name   string
		filter string
		want   int
	}{
		{"gen2 filter excludes our gen1 functions", `environment="GEN_2"`, 0},
		{"gen1 filter includes them", `environment="GEN_1"`, 1},
		{"no filter returns everything", "", 1},
		{"an unrelated filter is not applied", `state="ACTIVE"`, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body struct{ Functions []map[string]any }
			url := srv.URL + "/v2/projects/p/locations/-/functions"
			if tc.filter != "" {
				url += "?filter=" + urlQueryEscape(tc.filter)
			}
			getJSON(t, url, &body)
			if len(body.Functions) != tc.want {
				t.Errorf("got %d functions, want %d", len(body.Functions), tc.want)
			}
		})
	}
}

func TestListLocations(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "europe-west1", "euro"))

	var body struct {
		Locations []map[string]any
	}
	getJSON(t, srv.URL+"/v1/projects/p/locations", &body)

	seen := map[string]bool{}
	for _, l := range body.Locations {
		seen[l["locationId"].(string)] = true
	}
	// The default plus any region actually in use, so an unusual region is
	// still discoverable.
	for _, want := range []string{functions.DefaultLocation, "europe-west1"} {
		if !seen[want] {
			t.Errorf("locations %v missing %s", seen, want)
		}
	}
}

func TestListRuntimes(t *testing.T) {
	t.Parallel()
	srv := serve(t)

	var body struct {
		Runtimes []map[string]any
	}
	getJSON(t, srv.URL+"/v2/projects/p/locations/us-central1/runtimes", &body)
	if len(body.Runtimes) == 0 {
		t.Fatal("no runtimes reported; gcloud validates --runtime against this")
	}

	names := map[string]bool{}
	for _, rt := range body.Runtimes {
		names[rt["name"].(string)] = true
	}
	for _, want := range []string{"go", "nodejs20"} {
		if !names[want] {
			t.Errorf("runtimes missing %s", want)
		}
	}
}

func TestDeleteReturnsACompletedOperation(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "us-central1", "hello"))

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/projects/p/locations/us-central1/functions/hello", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var op struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatal(err)
	}
	if !op.Done || !strings.HasPrefix(op.Name, "operations/") {
		t.Fatalf("operation = %+v", op)
	}

	// gcloud polls the operation after the fact, so it has to still resolve.
	var polled map[string]any
	id := strings.TrimPrefix(op.Name, "operations/")
	if code := getJSON(t, srv.URL+"/v1/operations/"+id, &polled); code != http.StatusOK {
		t.Errorf("polling the operation: status = %d", code)
	}
	if polled["done"] != true {
		t.Errorf("polled operation = %+v", polled)
	}

	if code := getJSON(t, srv.URL+"/v1/projects/p/locations/us-central1/functions/hello", nil); code != http.StatusNotFound {
		t.Errorf("function still present after delete: status = %d", code)
	}
}

func TestCall(t *testing.T) {
	t.Parallel()
	srv := serve(t, goFn("p", "us-central1", "hello"))
	url := srv.URL + "/v1/projects/p/locations/us-central1/functions/hello:call"

	call := func(t *testing.T, body string) (int, callResult) {
		t.Helper()
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out callResult
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	t.Run("data reaches the function as its request body", func(t *testing.T) {
		// data is a string holding JSON, which is why gcloud --data arrives
		// escaped.
		code, out := call(t, `{"data":"{\"name\":\"Monir\"}"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if !strings.Contains(out.Result, `"greeting":"hello, world"`) {
			t.Errorf("result = %q", out.Result)
		}
		if out.ExecutionID == "" {
			t.Error("no executionId")
		}
		if out.Error != "" {
			t.Errorf("error = %q", out.Error)
		}
	})

	t.Run("every call gets its own execution id", func(t *testing.T) {
		_, first := call(t, `{"data":"{}"}`)
		_, second := call(t, `{"data":"{}"}`)
		if first.ExecutionID == second.ExecutionID {
			t.Errorf("both calls reported %q", first.ExecutionID)
		}
	})

	t.Run("an empty body is accepted", func(t *testing.T) {
		if code, _ := call(t, ""); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("a malformed request is 400", func(t *testing.T) {
		if code, _ := call(t, "{not json"); code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
	})

	t.Run("calling a missing function is 404", func(t *testing.T) {
		resp, err := http.Post(
			srv.URL+"/v1/projects/p/locations/us-central1/functions/nope:call",
			"application/json", strings.NewReader(`{"data":"{}"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("an unknown custom method is 501 naming it", func(t *testing.T) {
		resp, err := http.Post(
			srv.URL+"/v1/projects/p/locations/us-central1/functions/hello:teleport",
			"application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := readAll(resp)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501", resp.StatusCode)
		}
		if !strings.Contains(body, "functions.teleport") {
			t.Errorf("body %s does not name the operation", body)
		}
	})
}

// TestCallReportsAFunctionFailure holds the v1 contract: a function that fails
// is a successful invocation reporting an error, not a failed API call.
func TestCallReportsAFunctionFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/go.mod", []byte("module example.com/boom\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/fn.go", []byte(`package boom

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "it broke", http.StatusInternalServerError)
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := serve(t, functions.Function{Project: "p", Name: "boom", Source: dir})

	resp, err := http.Post(
		srv.URL+"/v1/projects/p/locations/us-central1/functions/boom:call",
		"application/json", strings.NewReader(`{"data":"{}"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the call succeeded, the function did not", resp.StatusCode)
	}
	var out callResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Error, "it broke") {
		t.Errorf("error = %q, want the function's own message", out.Error)
	}
	if out.Result != "" {
		t.Errorf("result = %q, want empty on failure", out.Result)
	}
}

// callResult mirrors v1's CallFunctionResponse for decoding in tests.
type callResult struct {
	ExecutionID string `json:"executionId"`
	Result      string `json:"result"`
	Error       string `json:"error"`
}

func readAll(resp *http.Response) (string, error) {
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n]), nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("%22")
		case ' ':
			b.WriteString("%20")
		case '=':
			b.WriteString("%3D")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
