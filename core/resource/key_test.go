package resource_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/monirz/cloudrig/core/resource"
)

// awkwardNames are the object names that break a naive encoding. GCS permits
// almost any UTF-8, and every one of these is legal.
var awkwardNames = []string{
	"simple.txt",
	"logs/2026/app.log",
	"a#b",
	"a#b#5",
	"has space.txt",
	"café.txt",
	"100%.txt",
	"a/b/c/d/e",
	"trailing/",
	strings.Repeat("deep/", 20) + "leaf",
}

func TestObjectKeyRoundTrip(t *testing.T) {
	t.Parallel()

	for _, name := range awkwardNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key := resource.Object("proj", "bkt", name, 1735689600000000)

			project, bucket, object, generation, err := resource.ParseObject(key)
			if err != nil {
				t.Fatalf("ParseObject(%q): %v", key, err)
			}
			if project != "proj" || bucket != "bkt" {
				t.Errorf("project/bucket = %q/%q", project, bucket)
			}
			if object != name {
				t.Errorf("object = %q, want %q", object, name)
			}
			if generation != 1735689600000000 {
				t.Errorf("generation = %d", generation)
			}
		})
	}
}

// TestHashIsNotAmbiguous is the reason the separator is NUL. With '#', the two
// keys below would be identical, and one object would shadow the other.
func TestHashIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	a := resource.Object("p", "b", "a#b", 5)
	bKey := resource.Object("p", "b", "a", 5)

	if a == bKey {
		t.Fatal("two different objects encode to the same key")
	}
	name, _, _, _, _ := parse(t, a)
	if name != "a#b" {
		t.Errorf("name = %q, want a#b", name)
	}
}

func TestGenerationsSortInOrder(t *testing.T) {
	t.Parallel()

	// Zero-padding is what makes a prefix scan return generations in order; a
	// bare decimal would sort 10 before 9.
	gens := []int64{1, 9, 10, 100, 1735689600000000}
	keys := make([]string, len(gens))
	for i, g := range gens {
		keys[i] = resource.Object("p", "b", "obj", g)
	}

	shuffled := []string{keys[3], keys[0], keys[4], keys[2], keys[1]}
	sort.Strings(shuffled)

	for i := range keys {
		if shuffled[i] != keys[i] {
			t.Fatalf("sorted position %d = generation out of order", i)
		}
	}
}

// TestPrefixScopes checks that each prefix covers exactly what a scan of it
// should return, and nothing adjacent.
func TestPrefixScopes(t *testing.T) {
	t.Parallel()

	obj := resource.Object("p", "bkt", "logs/app.log", 7)
	live := resource.Live("p", "bkt", "logs/app.log")

	tests := []struct {
		name   string
		prefix string
		covers []string
		misses []string
	}{
		{
			name:   "one object's generations",
			prefix: resource.ObjectPrefix("p", "bkt", "logs/app.log"),
			covers: []string{obj, resource.Object("p", "bkt", "logs/app.log", 8)},
			// A longer name sharing the prefix must not be swept up.
			misses: []string{resource.Object("p", "bkt", "logs/app.log.bak", 7)},
		},
		{
			name:   "a bucket's live pointers",
			prefix: resource.LivePrefix("p", "bkt"),
			covers: []string{live},
			misses: []string{obj, resource.Live("p", "other", "logs/app.log")},
		},
		{
			name:   "everything in a bucket",
			prefix: resource.BucketPrefix("p", "bkt"),
			covers: []string{obj, live},
			misses: []string{resource.Bucket("p", "other"), resource.Object("p2", "bkt", "x", 1)},
		},
		{
			name:   "everything in a project",
			prefix: resource.ProjectPrefix("p"),
			covers: []string{obj, live, resource.Bucket("p", "bkt"), resource.Bucket("p", "other")},
			misses: []string{resource.Bucket("p2", "bkt")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, key := range tc.covers {
				if !strings.HasPrefix(key, tc.prefix) {
					t.Errorf("prefix %q does not cover %q", tc.prefix, key)
				}
			}
			for _, key := range tc.misses {
				if strings.HasPrefix(key, tc.prefix) {
					t.Errorf("prefix %q wrongly covers %q", tc.prefix, key)
				}
			}
		})
	}
}

// TestNulSortsBeforeSlash is why the separator matters for ordering: listing
// must return "a" before "a/b", as GCS does.
func TestNulSortsBeforeSlash(t *testing.T) {
	t.Parallel()

	keys := []string{
		resource.Object("p", "b", "a/b", 1),
		resource.Object("p", "b", "a", 1),
	}
	sort.Strings(keys)

	name, _, _, _, _ := parse(t, keys[0])
	if name != "a" {
		t.Errorf("first sorted name = %q, want a", name)
	}
}

func TestObjectName(t *testing.T) {
	t.Parallel()

	for _, name := range awkwardNames {
		got, err := resource.ObjectName("p", "bkt", resource.Live("p", "bkt", name))
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("ObjectName = %q, want %q", got, name)
		}
	}

	if _, err := resource.ObjectName("p", "bkt", "p/p/b/other/live/x"); err == nil {
		t.Error("accepted a live pointer from another bucket")
	}
}

func TestParseObjectRejectsMalformedKeys(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"not a key":           "nonsense",
		"a bucket key":        resource.Bucket("p", "b"),
		"a live pointer":      resource.Live("p", "b", "x"),
		"no generation":       "p/p/b/bkt/o/name",
		"a bad generation":    "p/p/b/bkt/o/name\x00notanumber",
		"an empty generation": "p/p/b/bkt/o/name\x00",
	}

	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, _, _, err := resource.ParseObject(key); err == nil {
				t.Errorf("accepted %q", key)
			}
		})
	}
}

func parse(t *testing.T, key string) (object, project, bucket string, generation int64, err error) {
	t.Helper()
	project, bucket, object, generation, err = resource.ParseObject(key)
	if err != nil {
		t.Fatalf("ParseObject(%q): %v", key, err)
	}
	return object, project, bucket, generation, nil
}
