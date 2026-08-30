package functions_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/functions"
)

func TestRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	// Same name, two projects: only the prefixed form can tell them apart.
	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}
	other := goFn("hello")
	other.Project, other.Location = "other-project", "europe-west1"
	if _, err := r.Deploy(ctx, other); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h, rest, ok := r.Route(req.URL.EscapedPath())
		if !ok {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Rest", rest)
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantRest   string
	}{
		{"short form uses the defaults", "/hello", 200, "/"},
		{"prefixed form, default project", "/us-central1-cloudrig-local/hello", 200, "/"},
		{"prefixed form, another project", "/europe-west1-other-project/hello", 200, "/"},
		{"subtree path is passed through", "/hello/a/b", 200, "/a/b"},
		{"prefixed subtree", "/us-central1-cloudrig-local/hello/a/b", 200, "/a/b"},
		{"unknown name", "/nope", 404, ""},
		{"known name, wrong project", "/us-central1-nope/hello", 404, ""},
		{"empty path", "/", 404, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == 200 && resp.Header.Get("X-Rest") != tc.wantRest {
				t.Errorf("rest = %q, want %q", resp.Header.Get("X-Rest"), tc.wantRest)
			}
		})
	}
}

func TestResourceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		project, location, name string
		want                    string
	}{
		{"p", "l", "f", "projects/p/locations/l/functions/f"},
		{"", "", "f", "projects/cloudrig-local/locations/us-central1/functions/f"},
		{"p", "", "f", "projects/p/locations/us-central1/functions/f"},
	}
	for _, tc := range tests {
		if got := functions.ResourceName(tc.project, tc.location, tc.name); got != tc.want {
			t.Errorf("ResourceName(%q,%q,%q) = %q, want %q", tc.project, tc.location, tc.name, got, tc.want)
		}
	}
}

func TestSameNameDifferentProjectsAreDistinct(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	a := goFn("hello")
	b := goFn("hello")
	b.Project = "second"

	if _, err := r.Deploy(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Deploy(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Two entries, not one overwritten: the key is the resource name.
	if got := r.List("", ""); len(got) != 2 {
		t.Fatalf("List = %d entries, want 2", len(got))
	}
	if got := r.List("second", ""); len(got) != 1 || got[0].Project != "second" {
		t.Errorf("List(second) = %+v", got)
	}

	// Deleting one must leave the other.
	if err := r.Delete("second", "", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("", "", "hello"); !ok {
		t.Error("deleting one project's function removed the other")
	}
}

func TestDescriptorCarriesIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	desc, err := r.Deploy(context.Background(), goFn("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if desc.Project != functions.DefaultProject || desc.Location != functions.DefaultLocation {
		t.Errorf("descriptor = %+v, want the defaults filled in", desc)
	}
	if !strings.HasSuffix(desc.ResourceName(), "/functions/hello") {
		t.Errorf("ResourceName = %q", desc.ResourceName())
	}
}
