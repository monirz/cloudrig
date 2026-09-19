package cloudrun_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/services/cloudrun"
)

// TestAPI walks both API surfaces over one source-deployed service, so the
// build runs once and no Docker is needed.
func TestAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a service")
	}
	t.Parallel()

	reg := registry(t)
	if _, err := reg.Deploy(context.Background(), cloudrun.Service{
		Name:   "api",
		Source: "../../testdata/run-hello",
		Env:    []string{"GREETING=hi", "MALFORMED"},
		Memory: "512Mi",
		CPU:    "1",
	}, cloudrun.Options{}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	inst, ok := reg.Instance("", "", "api")
	if !ok {
		t.Fatal("the service is not registered")
	}
	if inst.Name() != "api" || inst.Project() != cloudrun.DefaultProject ||
		inst.Location() != cloudrun.DefaultLocation || inst.Revision() != "api-00001-cri" {
		t.Errorf("instance = %s %s %s %s", inst.Name(), inst.Project(), inst.Location(), inst.Revision())
	}
	if inst.ContainerID() != "" {
		t.Errorf("a source deploy has container %q", inst.ContainerID())
	}
	_ = inst.LogSnapshot()

	srv := httptest.NewServer(cloudrun.NewAPI(reg))
	t.Cleanup(srv.Close)

	const knative = "/apis/serving.knative.dev/v1/namespaces/cloudrig-local"
	const global = "/v1/projects/cloudrig-local/locations/us-central1/services"

	t.Run("get renders the deploy", func(t *testing.T) {
		var svc struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image     string
							Env       []struct{ Name, Value string }
							Resources struct{ Limits map[string]string }
						}
					}
				}
			}
			Status struct{ URL string }
		}
		getJSON(t, srv.URL+knative+"/services/api?region=us-central1", http.StatusOK, &svc)

		c := svc.Spec.Template.Spec.Containers
		if len(c) != 1 {
			t.Fatalf("containers = %+v", c)
		}
		if c[0].Image != "source:../../testdata/run-hello" {
			t.Errorf("image = %q", c[0].Image)
		}
		if len(c[0].Env) != 1 || c[0].Env[0].Name != "GREETING" || c[0].Env[0].Value != "hi" {
			t.Errorf("env = %+v, want only the well-formed entry", c[0].Env)
		}
		if l := c[0].Resources.Limits; l["memory"] != "512Mi" || l["cpu"] != "1" {
			t.Errorf("limits = %v", l)
		}
		if !strings.HasSuffix(svc.Status.URL, "/us-central1-cloudrig-local/api") {
			t.Errorf("url = %q", svc.Status.URL)
		}
	})

	t.Run("region comes from the request params header", func(t *testing.T) {
		for _, params := range []string{"location=us-central1", "location=us-central1&name=api"} {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+knative+"/services/api", nil)
			req.Header.Set("X-Goog-Request-Params", params)
			if got := do(t, req); got != http.StatusOK {
				t.Errorf("%s: status = %d", params, got)
			}
		}
	})

	t.Run("another region does not see it", func(t *testing.T) {
		getJSON(t, srv.URL+knative+"/services/api?region=europe-west1", http.StatusNotFound, nil)
	})

	t.Run("list", func(t *testing.T) {
		var out struct {
			Items []struct{ Metadata struct{ Name string } }
		}
		getJSON(t, srv.URL+knative+"/services", http.StatusOK, &out)
		if len(out.Items) != 1 || out.Items[0].Metadata.Name != "api" {
			t.Errorf("items = %+v", out.Items)
		}
	})

	t.Run("revision", func(t *testing.T) {
		var rev struct{ Metadata struct{ Name string } }
		getJSON(t, srv.URL+knative+"/revisions/api-00001-cri", http.StatusOK, &rev)
		if rev.Metadata.Name != "api-00001-cri" {
			t.Errorf("revision = %q", rev.Metadata.Name)
		}
		getJSON(t, srv.URL+knative+"/revisions/api-00009-cri", http.StatusNotFound, nil)
	})

	t.Run("global get", func(t *testing.T) {
		var svc struct{ Name, URI string }
		getJSON(t, srv.URL+global+"/api", http.StatusOK, &svc)
		if svc.Name != "projects/cloudrig-local/locations/us-central1/services/api" {
			t.Errorf("name = %q", svc.Name)
		}
		if svc.URI == "" {
			t.Error("uri is empty")
		}
		getJSON(t, srv.URL+global+"/missing", http.StatusNotFound, nil)
	})

	t.Run("iam", func(t *testing.T) {
		var policy map[string]any
		getJSON(t, srv.URL+global+"/api:getIamPolicy", http.StatusOK, &policy)
		if policy["etag"] != "cloudrig" {
			t.Errorf("getIamPolicy = %v", policy)
		}

		for _, body := range []string{`{"policy":{"bindings":[]}}`, `{}`} {
			var set map[string]any
			postJSON(t, srv.URL+global+"/api:setIamPolicy", body, http.StatusOK, &set)
			if set["etag"] != "cloudrig" {
				t.Errorf("setIamPolicy(%s) = %v", body, set)
			}
		}

		postJSON(t, srv.URL+global+"/api:undeploy", `{}`, http.StatusNotImplemented, nil)
		postJSON(t, srv.URL+global+"/api", `{}`, http.StatusBadRequest, nil)
	})

	t.Run("create refusals", func(t *testing.T) {
		url := srv.URL + knative + "/services"
		postJSON(t, url, `{`, http.StatusBadRequest, nil)
		postJSON(t, url, `{"spec":{"template":{"spec":{"containers":[{"image":"x"}]}}}}`,
			http.StatusBadRequest, nil)
		postJSON(t, url, `{"metadata":{"name":"api"},"spec":{"template":{"spec":{"containers":[{"image":"x"}]}}}}`,
			http.StatusConflict, nil)
	})

	t.Run("replace refusals", func(t *testing.T) {
		url := srv.URL + knative + "/services/api"
		send(t, http.MethodPut, url, `{`, http.StatusBadRequest)
		send(t, http.MethodPut, url,
			`{"spec":{"template":{"spec":{"containers":[{"image":"source:/tmp"}]}}}}`, http.StatusBadRequest)
		send(t, http.MethodPut, url, `{"spec":{"template":{"spec":{"containers":[]}}}}`, http.StatusBadRequest)
	})

	t.Run("delete", func(t *testing.T) {
		send(t, http.MethodDelete, srv.URL+global+"/api", "", http.StatusOK)
		send(t, http.MethodDelete, srv.URL+global+"/api", "", http.StatusNotFound)
		send(t, http.MethodDelete, srv.URL+knative+"/services/api", "", http.StatusNotFound)
		if _, ok := reg.Describe("", "", "api"); ok {
			t.Error("the service survived its delete")
		}
	})
}

func getJSON(t *testing.T, url string, want int, out any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	decode(t, req, want, out)
}

func postJSON(t *testing.T, url, body string, want int, out any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	decode(t, req, want, out)
}

func send(t *testing.T, method, url, body string, want int) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	decode(t, req, want, nil)
}

func do(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func decode(t *testing.T, req *http.Request, want int, out any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d", req.Method, req.URL.Path, resp.StatusCode, want)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}
