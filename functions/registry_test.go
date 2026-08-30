package functions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/functions"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newRegistry(t *testing.T) *functions.Registry {
	t.Helper()
	r := functions.NewRegistry(clock.NewFake(epoch), functions.Options{})
	t.Cleanup(r.StopAll)
	return r
}

func goFn(name string) functions.Function {
	return functions.Function{Name: name, Source: helloDir, EntryPoint: "HelloHTTP"}
}

func TestRegistryDeployAndLookup(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	if _, ok := r.Handler("", "", "hello"); ok {
		t.Fatal("empty registry resolved a name")
	}

	desc, err := r.Deploy(context.Background(), goFn("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if desc.Runtime != functions.RuntimeGo || desc.State != "ACTIVE" {
		t.Errorf("descriptor = %+v", desc)
	}
	// UpdateTime comes from the injected clock, so it is the fake epoch.
	if !desc.UpdateTime.Equal(epoch) {
		t.Errorf("UpdateTime = %v, want the fake epoch %v", desc.UpdateTime, epoch)
	}
	if _, ok := r.Handler("", "", "hello"); !ok {
		t.Error("deployed function is not resolvable")
	}
	if got, ok := r.Get("", "", "hello"); !ok || got.Name != "hello" {
		t.Errorf("Get = %+v, %v", got, ok)
	}
}

func TestRegistryRedeployReplaces(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}
	first, _ := r.Handler("", "", "hello")

	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}
	second, _ := r.Handler("", "", "hello")

	if first == second {
		t.Error("redeploy reused the old instance")
	}
	if got := r.List("", ""); len(got) != 1 {
		t.Errorf("List has %d entries after a redeploy, want 1", len(got))
	}
}

// TestRegistryFailedDeployKeepsTheOldOne is the reason Deploy starts the
// replacement before stopping the incumbent: a broken deploy must not take the
// name down.
func TestRegistryFailedDeployKeepsTheOldOne(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}

	bad := goFn("hello")
	bad.EntryPoint = "NoSuchHandler"
	if _, err := r.Deploy(ctx, bad); err == nil {
		t.Fatal("a broken deploy succeeded")
	}

	h, ok := r.Handler("", "", "hello")
	if !ok {
		t.Fatal("the failed deploy took the function down")
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if body := get(t, srv.URL+"/?name=x"); !strings.Contains(body, "hello, x") {
		t.Errorf("body = %s", body)
	}
}

func TestRegistryListAndDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	for _, name := range []string{"b", "a"} {
		if _, err := r.Deploy(ctx, goFn(name)); err != nil {
			t.Fatal(err)
		}
	}

	got := r.List("", "")
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("List = %+v, want a then b", got)
	}

	if err := r.Delete("", "", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Handler("", "", "a"); ok {
		t.Error("deleted function is still resolvable")
	}
	if err := r.Delete("", "", "a"); err == nil {
		t.Error("deleting a missing function succeeded")
	}
}

func TestAdminAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	srv := httptest.NewServer(r.Admin())
	t.Cleanup(srv.Close)

	deploy := func(t *testing.T, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+functions.AdminPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("deploy", func(t *testing.T) {
		resp := deploy(t, `{"name":"hello","source":"`+helloDir+`","entryPoint":"HelloHTTP"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var desc functions.Descriptor
		if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
			t.Fatal(err)
		}
		if desc.Name != "hello" || desc.Runtime != functions.RuntimeGo {
			t.Errorf("descriptor = %+v", desc)
		}
	})

	t.Run("list", func(t *testing.T) {
		var body struct {
			Functions []functions.Descriptor `json:"functions"`
		}
		decode(t, srv.URL+functions.AdminPath, &body)
		if len(body.Functions) != 1 {
			t.Errorf("functions = %+v, want 1", body.Functions)
		}
	})

	t.Run("describe", func(t *testing.T) {
		var desc functions.Descriptor
		decode(t, srv.URL+functions.AdminPath+"/hello", &desc)
		if desc.EntryPoint != "HelloHTTP" {
			t.Errorf("descriptor = %+v", desc)
		}
	})

	t.Run("describe a missing function is 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + functions.AdminPath + "/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("a broken deploy is 400 with the reason", func(t *testing.T) {
		resp := deploy(t, `{"name":"bad","source":"`+helloDir+`","entryPoint":"NoSuchHandler"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		var env struct {
			Error struct{ Message string } `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(env.Error.Message, "NoSuchHandler") {
			t.Errorf("message = %q, want it to name the entry point", env.Error.Message)
		}
	})

	t.Run("malformed body is 400", func(t *testing.T) {
		resp := deploy(t, `{not json`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("delete", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+functions.AdminPath+"/hello", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
	})

	t.Run("unsupported method is 405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+functions.AdminPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
}

// TestStartupFailureExplainsItself covers the tail buffer: a child that dies
// before listening has usually already said why.
func TestStartupFailureExplainsItself(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a node subprocess")
	}
	if !exists(nodeDir + "/node_modules/.bin/functions-framework") {
		t.Skipf("run: (cd %s && npm i)", nodeDir)
	}
	t.Parallel()

	_, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: nodeDir, Runtime: functions.RuntimeNode20, EntryPoint: "nosuchhandler",
	}, functions.Options{})
	if err == nil {
		t.Fatal("a missing node entry point started successfully")
	}
	if !strings.Contains(err.Error(), "nosuchhandler") {
		t.Errorf("err = %q, want the child's own explanation", err)
	}
}

func decode(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

// TestDescriptorReportsResolvedValues guards a bug where a fully auto-detected
// deploy reported an empty entry point: the descriptor was built from the
// request rather than from what the resolved function actually runs.
func TestDescriptorReportsResolvedValues(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/solo\n\ngo 1.25\n")
	write(t, dir, "fn.go", `package solo

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }
`)

	r := newRegistry(t)
	// Neither runtime nor entry point given: both must come back resolved.
	desc, err := r.Deploy(context.Background(), functions.Function{Name: "solo", Source: dir})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Runtime != functions.RuntimeGo {
		t.Errorf("Runtime = %q, want go", desc.Runtime)
	}
	if desc.EntryPoint != "Handler" {
		t.Errorf("EntryPoint = %q, want Handler", desc.EntryPoint)
	}
}
