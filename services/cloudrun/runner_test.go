package cloudrun_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/services/cloudrun"
)

func registry(t *testing.T) *cloudrun.Registry {
	t.Helper()

	r := cloudrun.NewRegistry()
	t.Cleanup(r.StopAll)
	return r
}

// TestServiceRuns is the whole claim: source in, a process out, answering a
// real request. No container, no image, no Docker.
func TestServiceRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	r := registry(t)
	svc, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:   "hello",
		Source: "../../testdata/run-hello",
	}, cloudrun.Options{})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if svc.Revision() != "hello-00001-cri" {
		t.Errorf("revision = %q", svc.Revision())
	}

	inst, ok := r.Instance("", "", "hello")
	if !ok {
		t.Fatal("the service is not registered")
	}

	resp, err := http.Get(inst.URL() + "/?name=monir")
	if err != nil {
		t.Fatalf("calling the service: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := strings.TrimSpace(string(body)); got != "hello monir from hello" {
		t.Errorf("body = %q", got)
	}
}

// TestServiceEnvironment holds the variables Cloud Run guarantees, plus what
// the deploy set: code that reads them must find what it expects.
func TestServiceEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	r := registry(t)
	if _, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:   "envy",
		Source: "../../testdata/run-hello",
		Env:    []string{"GREETING=bonjour"},
	}, cloudrun.Options{}); err != nil {
		t.Fatal(err)
	}

	inst, _ := r.Instance("", "", "envy")
	resp, err := http.Get(inst.URL() + "/env")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := strings.TrimSpace(string(body)); got != "envy|envy-00001-cri|bonjour" {
		t.Errorf("environment = %q, want the service, its revision and GREETING", got)
	}
}

// TestRedeployMakesANewRevision covers a second deploy of the same service.
func TestRedeployMakesANewRevision(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	r := registry(t)
	ctx := context.Background()
	svc := cloudrun.Service{Name: "twice", Source: "../../testdata/run-hello"}

	first, err := r.Deploy(ctx, svc, cloudrun.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Deploy(ctx, svc, cloudrun.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if first.Revision() == second.Revision() {
		t.Errorf("both deploys are revision %q", first.Revision())
	}
	if second.Revision() != "twice-00002-cri" {
		t.Errorf("second revision = %q", second.Revision())
	}
	// The old revision is gone, and the new one answers.
	inst, _ := r.Instance("", "", "twice")
	resp, err := http.Get(inst.URL() + "/")
	if err != nil {
		t.Fatalf("the redeployed service does not answer: %v", err)
	}
	resp.Body.Close()
}

// TestRouting is how a request reaches a service through the emulator's front
// door, mirroring the functions layout.
func TestRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	r := registry(t)
	if _, err := r.Deploy(context.Background(), cloudrun.Service{
		Name:   "routed",
		Source: "../../testdata/run-hello",
	}, cloudrun.Options{}); err != nil {
		t.Fatal(err)
	}

	handler, rest, ok := r.Route("/us-central1-cloudrig-local/routed/env")
	if !ok {
		t.Fatal("the path did not route to a service")
	}
	if rest != "/env" {
		t.Errorf("rest = %q, want /env", rest)
	}

	req := httptest.NewRequest(http.MethodGet, rest, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.HasPrefix(rec.Body.String(), "routed|") {
		t.Errorf("body = %q", rec.Body.String())
	}
	if _, _, ok := r.Route("/us-central1-cloudrig-local/absent/"); ok {
		t.Error("a path routed to a service that is not deployed")
	}
}

// TestSourceThatDoesNotListen is the failure a startup timeout exists for: the
// error has to say what happened and carry the service's own output.
func TestSourceThatDoesNotListen(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir+"/go.mod", "module example.com/silent\n\ngo 1.25\n")
	write(t, dir+"/main.go", `package main

import "fmt"

func main() { fmt.Println("I am not a server") }
`)

	r := registry(t)
	_, err := r.Deploy(context.Background(), cloudrun.Service{
		Name: "silent", Source: dir,
	}, cloudrun.Options{})
	if err == nil {
		t.Fatal("a service that never listens deployed successfully")
	}
	if !strings.Contains(err.Error(), "exited before it listened") {
		t.Errorf("err = %v, want it to name the cause", err)
	}
	if !strings.Contains(err.Error(), "I am not a server") {
		t.Errorf("err = %v, want the service's own output", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
