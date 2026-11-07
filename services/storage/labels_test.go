package storage_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/services/storage"
	"github.com/monirz/cloudrig/store"
	"github.com/monirz/cloudrig/store/blob"
)

// TestBucketLabelsArePersisted guards a bug where labels were dropped on create
// and patch while the patch still answered 200 and bumped metageneration. It
// walks create, patch, get, restart, get over a persistent store.
func TestBucketLabelsArePersisted(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "metadata.db")
	blobs, err := blob.NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	clk := clock.NewFake(epoch)

	// start opens the store at path and serves the JSON API over it. stop
	// closes both, as a daemon shutdown would.
	start := func() (url string, stop func()) {
		kv, err := store.OpenBolt(path)
		if err != nil {
			t.Fatal(err)
		}
		api := storage.NewAPI(storage.New(kv, blobs, clk, nil))
		srv := httptest.NewServer(api)
		return srv.URL + "/storage/v1/b", func() {
			srv.Close()
			_ = api.Close()
			if err := kv.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}

	type bucket struct {
		Labels         map[string]string `json:"labels"`
		Metageneration string            `json:"metageneration"`
	}
	call := func(method, url, body string) bucket {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d", method, url, resp.StatusCode)
		}
		var b bucket
		if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	check := func(step string, got bucket, want map[string]string) {
		t.Helper()
		if !maps.Equal(got.Labels, want) {
			t.Errorf("%s: labels = %v, want %v", step, got.Labels, want)
		}
	}

	u, stop := start()
	check("create", call("POST", u+"?project=p", `{"name":"label-two","labels":{"a":"1","c":"3"}}`),
		map[string]string{"a": "1", "c": "3"})

	// A patch merges: named keys are set, a null removes one, others stay.
	patched := call("PATCH", u+"/label-two", `{"labels":{"b":"2","c":null}}`)
	want := map[string]string{"a": "1", "b": "2"}
	check("patch", patched, want)
	if patched.Metageneration != "2" {
		t.Errorf("patch: metageneration = %s, want 2", patched.Metageneration)
	}
	check("get", call("GET", u+"/label-two", ""), want)

	// A patch that does not mention labels leaves them alone.
	check("unrelated patch", call("PATCH", u+"/label-two", `{"storageClass":"NEARLINE"}`), want)
	stop()

	u, stop = start()
	defer stop()
	check("get after restart", call("GET", u+"/label-two", ""), want)
}
