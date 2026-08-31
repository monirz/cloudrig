package firestore

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const base = "projects/p/databases/(default)/documents"

func TestValidDocName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"a document", base + "/people/ada", true},
		{"a subcollection document", base + "/people/ada/pets/cat", true},
		{"a collection is not a document", base + "/people", false},
		{"a deeper collection", base + "/people/ada/pets", false},
		{"no documents marker", "projects/p/people/ada", false},
		{"an empty segment", base + "/people//ada", false},
		{"nothing at all", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validDocName(c.in)
			if c.ok && err != nil {
				t.Errorf("validDocName(%q) = %v, want ok", c.in, err)
			}
			if !c.ok && status.Code(err) != codes.InvalidArgument {
				t.Errorf("validDocName(%q) = %v, want InvalidArgument", c.in, err)
			}
		})
	}
}

// TestKeysAreNamespaced keeps documents from colliding with any other
// service's keys in the shared store.
func TestKeysAreNamespaced(t *testing.T) {
	t.Parallel()

	if got := docKey(base + "/people/ada"); got != "fs/d/"+base+"/people/ada" {
		t.Errorf("docKey = %q", got)
	}
	// A collection's prefix must not also match a document whose name merely
	// starts with the same letters.
	if got := collectionPrefix(base, "people"); got != "fs/d/"+base+"/people/" {
		t.Errorf("collectionPrefix = %q", got)
	}
}
