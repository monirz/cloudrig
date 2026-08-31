package conformance

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// fsClient points the real Firestore client at an in-process emulator.
func fsClient(t *testing.T) (*firestore.Client, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := firestore.NewClient(ctx, "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

type person struct {
	Name  string    `firestore:"name"`
	Age   int64     `firestore:"age"`
	Tags  []string  `firestore:"tags"`
	Since time.Time `firestore:"since"`
}

// TestFirestoreSetAndGet is the vertical slice: a document written by the real
// client, read back through it, with the types intact.
func TestFirestoreSetAndGet(t *testing.T) {
	c, ctx := fsClient(t)

	want := person{
		Name:  "Ada",
		Age:   36,
		Tags:  []string{"maths", "engines"},
		Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := c.Collection("people").Doc("ada").Set(ctx, want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	snap, err := c.Collection("people").Doc("ada").Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var got person
	if err := snap.DataTo(&got); err != nil {
		t.Fatalf("DataTo: %v", err)
	}

	if got.Name != want.Name || got.Age != want.Age {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "maths" {
		t.Errorf("tags = %v", got.Tags)
	}
	if !got.Since.Equal(want.Since) {
		t.Errorf("since = %v, want %v", got.Since, want.Since)
	}
	if !snap.Exists() {
		t.Error("Exists() = false for a document just written")
	}
}

// TestFirestoreMissingDocument pins the error the client turns a missing
// document into, which is how application code tests for absence.
func TestFirestoreMissingDocument(t *testing.T) {
	c, ctx := fsClient(t)

	_, err := c.Collection("people").Doc("nobody").Get(ctx)
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

// TestFirestoreCreateIsExclusive covers the precondition that makes Create
// different from Set.
func TestFirestoreCreateIsExclusive(t *testing.T) {
	c, ctx := fsClient(t)

	doc := c.Collection("people").Doc("once")
	if _, err := doc.Create(ctx, map[string]any{"n": 1}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := doc.Create(ctx, map[string]any{"n": 2}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second Create = %v, want AlreadyExists", err)
	}
}

// TestFirestoreUpdateTouchesNamedFields is the difference between Set and
// Update: an unmentioned field must survive.
func TestFirestoreUpdateTouchesNamedFields(t *testing.T) {
	c, ctx := fsClient(t)

	doc := c.Collection("people").Doc("grace")
	if _, err := doc.Set(ctx, map[string]any{"name": "Grace", "age": int64(45)}); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Update(ctx, []firestore.Update{{Path: "age", Value: int64(46)}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	snap, err := doc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Data()["age"]; got != int64(46) {
		t.Errorf("age = %v, want 46", got)
	}
	if got := snap.Data()["name"]; got != "Grace" {
		t.Errorf("name = %v, want it untouched by the update", got)
	}
}

// TestFirestoreSetReplaces holds the other half: Set without options replaces
// the document rather than merging into it.
func TestFirestoreSetReplaces(t *testing.T) {
	c, ctx := fsClient(t)

	doc := c.Collection("people").Doc("alan")
	if _, err := doc.Set(ctx, map[string]any{"name": "Alan", "age": int64(41)}); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Set(ctx, map[string]any{"name": "Alan"}); err != nil {
		t.Fatal(err)
	}

	snap, _ := doc.Get(ctx)
	if _, ok := snap.Data()["age"]; ok {
		t.Errorf("age survived a replacing Set: %v", snap.Data())
	}
}

// TestFirestoreUpdateNeedsADocument covers Update's own precondition.
func TestFirestoreUpdateNeedsADocument(t *testing.T) {
	c, ctx := fsClient(t)

	_, err := c.Collection("people").Doc("ghost").
		Update(ctx, []firestore.Update{{Path: "x", Value: 1}})
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestFirestoreDelete(t *testing.T) {
	c, ctx := fsClient(t)

	doc := c.Collection("people").Doc("temp")
	if _, err := doc.Set(ctx, map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := doc.Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("the document survived a delete: %v", err)
	}
	// Deleting again is not an error, as in real Firestore.
	if _, err := doc.Delete(ctx); err != nil {
		t.Errorf("deleting an absent document: %v", err)
	}
}

// TestFirestoreBatch covers several writes arriving as one Commit.
func TestFirestoreBatch(t *testing.T) {
	c, ctx := fsClient(t)

	b := c.BulkWriter(ctx)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := b.Set(c.Collection("batch").Doc(name), map[string]any{"id": name}); err != nil {
			t.Fatal(err)
		}
	}
	b.End()

	for _, name := range []string{"a", "b", "c"} {
		snap, err := c.Collection("batch").Doc(name).Get(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if snap.Data()["id"] != name {
			t.Errorf("%s: data = %v", name, snap.Data())
		}
	}
}

// TestFirestoreEmulatorHost is the promise the env var makes: existing code,
// unmodified, finds the emulator.
//
// Not parallel: it sets an environment variable, which is process-wide.
func TestFirestoreEmulatorHost(t *testing.T) {
	emu := cloudrig.MustStart(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", emu.Endpoint())

	ctx := context.Background()
	c, err := firestore.NewClient(ctx, "test-project")
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Collection("env").Doc("d").Set(ctx, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Set through the env var: %v", err)
	}
	snap, err := c.Collection("env").Doc("d").Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Data()["ok"] != true {
		t.Errorf("data = %v", snap.Data())
	}
}

// TestFirestoreNestedValues covers the encoding that is most of the work: a
// Value is a oneof, and nested maps and arrays must survive the store.
func TestFirestoreNestedValues(t *testing.T) {
	c, ctx := fsClient(t)

	in := map[string]any{
		"nil":    nil,
		"bool":   true,
		"float":  1.5,
		"bytes":  []byte{1, 2, 3},
		"nested": map[string]any{"deep": map[string]any{"n": int64(7)}},
		"list":   []any{int64(1), "two", map[string]any{"three": true}},
	}
	if _, err := c.Collection("shapes").Doc("all").Set(ctx, in); err != nil {
		t.Fatalf("Set: %v", err)
	}

	snap, err := c.Collection("shapes").Doc("all").Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Data()

	if got["nil"] != nil || got["bool"] != true || got["float"] != 1.5 {
		t.Errorf("scalars = %v", got)
	}
	if b, ok := got["bytes"].([]byte); !ok || len(b) != 3 || b[0] != 1 {
		t.Errorf("bytes = %#v", got["bytes"])
	}
	deep, _ := got["nested"].(map[string]any)
	inner, _ := deep["deep"].(map[string]any)
	if inner["n"] != int64(7) {
		t.Errorf("nested = %#v", got["nested"])
	}
	list, _ := got["list"].([]any)
	if len(list) != 3 || list[1] != "two" {
		t.Errorf("list = %#v", got["list"])
	}
}
