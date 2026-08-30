package storage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/services/storage"
)

// seed writes the tree the listing tests work over.
func seed(t *testing.T, names ...string) (*storage.Service, context.Context) {
	t.Helper()
	s, ctx := withBucket(t)
	for _, name := range names {
		if _, err := write(t, s, name, "content", storage.Preconditions{}); err != nil {
			t.Fatal(err)
		}
	}
	return s, ctx
}

func namesOf(objects []storage.Object) string {
	out := make([]string, len(objects))
	for i, o := range objects {
		out[i] = o.Name
	}
	return strings.Join(out, ",")
}

// TestListWithDelimiter is acceptance criterion 6.
func TestListWithDelimiter(t *testing.T) {
	t.Parallel()
	s, ctx := seed(t,
		"a/1.txt", "a/2.txt", "a/deep/3.txt", "a/deep/4.txt",
		"b/1.txt", "top.txt",
	)

	tests := []struct {
		name         string
		req          storage.ListRequest
		wantObjects  string
		wantPrefixes string
	}{
		{
			name:         "no prefix or delimiter lists everything",
			req:          storage.ListRequest{Bucket: "bkt"},
			wantObjects:  "a/1.txt,a/2.txt,a/deep/3.txt,a/deep/4.txt,b/1.txt,top.txt",
			wantPrefixes: "",
		},
		{
			name:         "delimiter alone rolls up the top level",
			req:          storage.ListRequest{Bucket: "bkt", Delimiter: "/"},
			wantObjects:  "top.txt",
			wantPrefixes: "a/,b/",
		},
		{
			// The spec's example: items[] and prefixes[] side by side.
			name:         "prefix and delimiter",
			req:          storage.ListRequest{Bucket: "bkt", Prefix: "a/", Delimiter: "/"},
			wantObjects:  "a/1.txt,a/2.txt",
			wantPrefixes: "a/deep/",
		},
		{
			name:         "prefix without delimiter is a flat scan",
			req:          storage.ListRequest{Bucket: "bkt", Prefix: "a/"},
			wantObjects:  "a/1.txt,a/2.txt,a/deep/3.txt,a/deep/4.txt",
			wantPrefixes: "",
		},
		{
			name:         "a deeper prefix",
			req:          storage.ListRequest{Bucket: "bkt", Prefix: "a/deep/", Delimiter: "/"},
			wantObjects:  "a/deep/3.txt,a/deep/4.txt",
			wantPrefixes: "",
		},
		{
			name:         "a prefix matching nothing",
			req:          storage.ListRequest{Bucket: "bkt", Prefix: "zz"},
			wantObjects:  "",
			wantPrefixes: "",
		},
		{
			name:         "a prefix that is not a path segment",
			req:          storage.ListRequest{Bucket: "bkt", Prefix: "to"},
			wantObjects:  "top.txt",
			wantPrefixes: "",
		},
		{
			name:         "a non-slash delimiter",
			req:          storage.ListRequest{Bucket: "bkt", Delimiter: "."},
			wantObjects:  "",
			wantPrefixes: "a/1.,a/2.,a/deep/3.,a/deep/4.,b/1.,top.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListObjects(ctx, "p", tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if names := namesOf(got.Objects); names != tc.wantObjects {
				t.Errorf("objects = %q, want %q", names, tc.wantObjects)
			}
			if prefixes := strings.Join(got.Prefixes, ","); prefixes != tc.wantPrefixes {
				t.Errorf("prefixes = %q, want %q", prefixes, tc.wantPrefixes)
			}
		})
	}
}

func TestListPaginates(t *testing.T) {
	t.Parallel()
	s, ctx := seed(t, "a", "b", "c", "d", "e")

	var seen []string
	token := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
		got, err := s.ListObjects(ctx, "p", storage.ListRequest{
			Bucket: "bkt", MaxResults: 2, PageToken: token,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Objects) > 2 {
			t.Fatalf("page held %d objects, want at most 2", len(got.Objects))
		}
		for _, o := range got.Objects {
			seen = append(seen, o.Name)
		}
		if got.NextPageToken == "" {
			break
		}
		token = got.NextPageToken
	}

	if want := "a,b,c,d,e"; strings.Join(seen, ",") != want {
		t.Errorf("paged names = %q, want %q", strings.Join(seen, ","), want)
	}
}

// TestMaxResultsCountsPrefixes holds the rule that a rollup consumes a slot
// just as an object does, which is how GCS counts.
func TestMaxResultsCountsPrefixes(t *testing.T) {
	t.Parallel()
	s, ctx := seed(t, "a/1", "b/1", "c/1", "top")

	got, err := s.ListObjects(ctx, "p", storage.ListRequest{
		Bucket: "bkt", Delimiter: "/", MaxResults: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total := len(got.Objects) + len(got.Prefixes); total != 2 {
		t.Errorf("page held %d objects and %d prefixes, want 2 in total",
			len(got.Objects), len(got.Prefixes))
	}
	if got.NextPageToken == "" {
		t.Error("no token despite more results remaining")
	}
}

// TestListCollapsesManyKeysIntoOnePrefix is why the store's List limit is a
// scan budget: hundreds of keys under one rollup must still yield a full page.
func TestListCollapsesManyKeysIntoOnePrefix(t *testing.T) {
	t.Parallel()

	names := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		names = append(names, "bulk/"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+"/x")
	}
	names = append(names, "visible.txt")

	s, ctx := seed(t, names...)
	got, err := s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "bkt", Delimiter: "/"})
	if err != nil {
		t.Fatal(err)
	}

	// 600 keys collapse to one rollup, and the object after them must still
	// appear rather than being lost past a scan boundary.
	if strings.Join(got.Prefixes, ",") != "bulk/" {
		t.Errorf("prefixes = %v, want just bulk/", got.Prefixes)
	}
	if namesOf(got.Objects) != "visible.txt" {
		t.Errorf("objects = %q, want visible.txt", namesOf(got.Objects))
	}
}

func TestListSkipsDeletedObjects(t *testing.T) {
	t.Parallel()
	s, ctx := seed(t, "kept", "removed")

	if err := s.DeleteObject(ctx, "p", "bkt", "removed", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	if namesOf(got.Objects) != "kept" {
		t.Errorf("objects = %q, want kept", namesOf(got.Objects))
	}
}

func TestListReportsTheLiveGeneration(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	if _, err := write(t, s, "obj", "one", storage.Preconditions{}); err != nil {
		t.Fatal(err)
	}
	second, err := write(t, s, "obj", "two", storage.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	// One entry per visible object, not one per generation.
	if len(got.Objects) != 1 {
		t.Fatalf("objects = %d, want 1", len(got.Objects))
	}
	if got.Objects[0].Generation != second.Generation {
		t.Errorf("generation = %d, want the live one %d",
			got.Objects[0].Generation, second.Generation)
	}
}

func TestListRequiresTheBucket(t *testing.T) {
	t.Parallel()
	s, ctx := withBucket(t)

	_, err := s.ListObjects(ctx, "p", storage.ListRequest{Bucket: "missing"})
	if got := status(t, err); got != 404 {
		t.Errorf("status = %d, want 404", got)
	}
}
