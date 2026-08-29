package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/monirz/cloudrig/store"
)

func TestPutPreconditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        int // how many times to write the key first
		ifVersion   uint64
		wantErr     error
		wantVersion uint64
	}{
		{name: "create with must-not-exist", ifVersion: 0, wantVersion: 1},
		{name: "create with a required version", ifVersion: 1, wantErr: store.ErrVersionMismatch},
		{name: "must-not-exist over an existing key", seed: 1, ifVersion: 0, wantErr: store.ErrVersionMismatch},
		{name: "matching version", seed: 1, ifVersion: 1, wantVersion: 2},
		{name: "stale version", seed: 2, ifVersion: 1, wantErr: store.ErrVersionMismatch},
		{name: "future version", seed: 1, ifVersion: 9, wantErr: store.ErrVersionMismatch},
		{name: "version increases on every write", seed: 3, ifVersion: 3, wantVersion: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := store.NewMemory()

			var prev uint64
			for i := 0; i < tc.seed; i++ {
				var err error
				prev, err = s.Put(ctx, "k", []byte("seed"), prev)
				if err != nil {
					t.Fatalf("seeding write %d: %v", i, err)
				}
			}

			got, err := s.Put(ctx, "k", []byte("v"), tc.ifVersion)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Put err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.wantVersion {
				t.Errorf("version = %d, want %d", got, tc.wantVersion)
			}
		})
	}
}

func TestGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewMemory()

	if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get on absent key = %v, want ErrNotFound", err)
	}

	if _, err := s.Put(ctx, "k", []byte("v1"), 0); err != nil {
		t.Fatal(err)
	}
	val, ver, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "v1" || ver != 1 {
		t.Errorf("Get = (%q, %d), want (\"v1\", 1)", val, ver)
	}

	// A caller mutating what Get returned must not reach the store.
	val[0] = 'X'
	again, _, _ := s.Get(ctx, "k")
	if string(again) != "v1" {
		t.Errorf("stored value = %q after caller mutated its copy, want \"v1\"", again)
	}
}

func TestPutCopiesTheCallerSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewMemory()

	v := []byte("v1")
	if _, err := s.Put(ctx, "k", v, 0); err != nil {
		t.Fatal(err)
	}
	v[0] = 'X'

	got, _, _ := s.Get(ctx, "k")
	if string(got) != "v1" {
		t.Errorf("stored value = %q after caller mutated its slice, want \"v1\"", got)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("absent key", func(t *testing.T) {
		s := store.NewMemory()
		if err := s.Delete(ctx, "k", 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("unconditional", func(t *testing.T) {
		s := store.NewMemory()
		s.Put(ctx, "k", []byte("v"), 0)
		if err := s.Delete(ctx, "k", 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, "k"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("key survived an unconditional delete")
		}
	})

	t.Run("stale version", func(t *testing.T) {
		s := store.NewMemory()
		s.Put(ctx, "k", []byte("v"), 0)
		s.Put(ctx, "k", []byte("v"), 1)
		if err := s.Delete(ctx, "k", 1); !errors.Is(err, store.ErrVersionMismatch) {
			t.Errorf("err = %v, want ErrVersionMismatch", err)
		}
		if _, _, err := s.Get(ctx, "k"); err != nil {
			t.Errorf("key was removed despite a failed precondition")
		}
	})

	t.Run("frees the key for must-not-exist", func(t *testing.T) {
		s := store.NewMemory()
		s.Put(ctx, "k", []byte("v"), 0)
		s.Delete(ctx, "k", 1)
		if _, err := s.Put(ctx, "k", []byte("v"), 0); err != nil {
			t.Errorf("must-not-exist failed after delete: %v", err)
		}
	})
}

func TestListOrderAndPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewMemory()

	// Written out of order; List must return them sorted.
	for _, k := range []string{"b/2", "a/3", "b/1", "a/1", "c", "a/2"} {
		if _, err := s.Put(ctx, k, []byte(k), 0); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "everything", prefix: "", want: "a/1,a/2,a/3,b/1,b/2,c"},
		{name: "one branch", prefix: "a/", want: "a/1,a/2,a/3"},
		{name: "prefix is not a path segment", prefix: "b", want: "b/1,b/2"},
		{name: "no matches", prefix: "z", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, tok, err := s.List(ctx, tc.prefix, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			if tok != "" {
				t.Errorf("token = %q on an unbudgeted scan, want empty", tok)
			}
			if joined := joinKeys(got); joined != tc.want {
				t.Errorf("keys = %q, want %q", joined, tc.want)
			}
		})
	}
}

func TestListPaginates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewMemory()

	for _, k := range []string{"a/1", "a/2", "a/3", "a/4", "a/5"} {
		s.Put(ctx, k, []byte(k), 0)
	}

	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		got, next, err := s.List(ctx, "a/", 2, token)
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range got {
			seen = append(seen, kv.Key)
		}
		if next == "" {
			break
		}
		token = next
	}

	if want := "a/1,a/2,a/3,a/4,a/5"; strings.Join(seen, ",") != want {
		t.Errorf("paged keys = %q, want %q", strings.Join(seen, ","), want)
	}
}

func TestListRejectsForeignTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := store.NewMemory()
	for _, k := range []string{"a/1", "a/2", "b/1", "b/2"} {
		s.Put(ctx, k, []byte(k), 0)
	}

	_, tok, err := s.List(ctx, "a/", 1, "")
	if err != nil || tok == "" {
		t.Fatalf("setup: token = %q, err = %v", tok, err)
	}

	if _, _, err := s.List(ctx, "b/", 1, tok); !errors.Is(err, store.ErrInvalidPageToken) {
		t.Errorf("token from another prefix: err = %v, want ErrInvalidPageToken", err)
	}
	if _, _, err := s.List(ctx, "a/", 1, "not base64!!"); !errors.Is(err, store.ErrInvalidPageToken) {
		t.Errorf("malformed token: err = %v, want ErrInvalidPageToken", err)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	seed := func() *store.Memory {
		s := store.NewMemory()
		for _, k := range []string{"p/x/a", "p/x/b", "p/y/a"} {
			s.Put(ctx, k, []byte(k), 0)
		}
		return s
	}

	t.Run("by prefix", func(t *testing.T) {
		s := seed()
		if err := s.Reset(ctx, "p/x/"); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.List(ctx, "", 0, "")
		if want := "p/y/a"; joinKeys(got) != want {
			t.Errorf("keys = %q, want %q", joinKeys(got), want)
		}
	})

	t.Run("everything", func(t *testing.T) {
		s := seed()
		if err := s.Reset(ctx, ""); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.List(ctx, "", 0, "")
		if len(got) != 0 {
			t.Errorf("keys = %q, want none", joinKeys(got))
		}
	})

	t.Run("leaves the key set usable", func(t *testing.T) {
		s := seed()
		s.Reset(ctx, "p/x/")
		if _, err := s.Put(ctx, "p/x/a", []byte("v"), 0); err != nil {
			t.Fatalf("rewriting a reset key: %v", err)
		}
		got, _, _ := s.List(ctx, "", 0, "")
		if want := "p/x/a,p/y/a"; joinKeys(got) != want {
			t.Errorf("keys = %q, want %q", joinKeys(got), want)
		}
	})
}

// TestConcurrentCASHasExactlyOneWinner is acceptance criterion 4 at the store
// layer: GCS ifGenerationMatch is built directly on this.
func TestConcurrentCASHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("creating the same key", func(t *testing.T) {
		t.Parallel()
		s := store.NewMemory()

		const writers = 64
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, lost := 0, 0

		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.Put(ctx, "k", []byte("v"), 0)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, store.ErrVersionMismatch):
					lost++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		if won != 1 || lost != writers-1 {
			t.Errorf("won = %d, lost = %d; want 1 and %d", won, lost, writers-1)
		}
	})

	t.Run("overwriting the same version", func(t *testing.T) {
		t.Parallel()
		s := store.NewMemory()
		if _, err := s.Put(ctx, "k", []byte("v"), 0); err != nil {
			t.Fatal(err)
		}

		const writers = 64
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0

		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Put(ctx, "k", []byte("v"), 1); err == nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if won != 1 {
			t.Errorf("won = %d, want 1", won)
		}
		if _, ver, _ := s.Get(ctx, "k"); ver != 2 {
			t.Errorf("version = %d after one successful overwrite, want 2", ver)
		}
	})
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := store.NewMemory()
	if _, _, err := s.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v, want context.Canceled", err)
	}
	if _, err := s.Put(ctx, "k", nil, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v, want context.Canceled", err)
	}
	if err := s.Delete(ctx, "k", 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v, want context.Canceled", err)
	}
	if _, _, err := s.List(ctx, "", 0, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v, want context.Canceled", err)
	}
	if err := s.Reset(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Reset = %v, want context.Canceled", err)
	}
}

func joinKeys(kvs []store.KV) string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.Key
	}
	return strings.Join(out, ",")
}
