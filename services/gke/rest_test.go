package gke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
)

// restServer wraps the REST surface over a service with a fake runner, so the
// JSON API is tested without spinning a real cluster.
func restServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	s := New(store.NewMemory(), clock.NewFake(epoch))
	s.runner = &fakeRunner{}
	srv := httptest.NewServer(NewREST(s))
	t.Cleanup(srv.Close)
	return srv, s
}

func do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: %q: %v", method, url, raw, err)
		}
	}
	return resp.StatusCode, out
}

// TestRESTLifecycle walks what gcloud does: create returns an operation, the
// operation polls to DONE, the cluster reads RUNNING, then delete.
func TestRESTLifecycle(t *testing.T) {
	t.Parallel()

	srv, svc := restServer(t)
	base := srv.URL + "/v1/projects/p/locations/us-central1"

	code, op := do(t, http.MethodPost, base+"/clusters",
		`{"cluster":{"name":"dev","initialNodeCount":1}}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d %v", code, op)
	}
	opName, _ := op["name"].(string)
	if opName == "" || op["status"] != "RUNNING" {
		t.Fatalf("operation = %v", op)
	}

	svc.Sync() // the fake runner finished

	_, done := do(t, http.MethodGet, base+"/operations/"+opName, "")
	if done["status"] != "DONE" {
		t.Errorf("operation after Sync = %v", done["status"])
	}

	code, cluster := do(t, http.MethodGet, base+"/clusters/dev", "")
	if code != http.StatusOK || cluster["status"] != "RUNNING" {
		t.Errorf("cluster = %d %v", code, cluster)
	}
	if cluster["endpoint"] != "10.0.0.1:6443" {
		t.Errorf("endpoint = %v", cluster["endpoint"])
	}

	_, list := do(t, http.MethodGet, base+"/clusters", "")
	if clusters, _ := list["clusters"].([]any); len(clusters) != 1 {
		t.Errorf("list = %v", list)
	}

	if code, _ := do(t, http.MethodDelete, base+"/clusters/dev", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	svc.Sync()
	if code, _ := do(t, http.MethodGet, base+"/clusters/dev", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// TestRESTAndGRPCShareState holds one service behind two APIs.
func TestRESTAndGRPCShareState(t *testing.T) {
	t.Parallel()

	srv, svc := restServer(t)
	base := srv.URL + "/v1/projects/p/locations/us-central1"

	do(t, http.MethodPost, base+"/clusters", `{"cluster":{"name":"shared"}}`)
	svc.Sync()

	// Visible to the gRPC service.
	if _, err := svc.GetCluster(context.Background(), &containerpb.GetClusterRequest{
		Name: "projects/p/locations/us-central1/clusters/shared",
	}); err != nil {
		t.Fatalf("a cluster made over REST is invisible to gRPC: %v", err)
	}
}
