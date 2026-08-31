package conformance

import (
	"context"
	"errors"
	"io"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
)

// gcs points the real client at one emulator.
func gcs(t *testing.T, emu *cloudrig.Emulator) (*storage.Client, context.Context) {
	t.Helper()

	ctx := context.Background()
	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

func read(t *testing.T, c *storage.Client, ctx context.Context, bucket, object string) string {
	t.Helper()

	r, err := c.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, object, err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestForkCarriesState is the base case: a fork starts from what the parent
// had, objects and all.
func TestForkCarriesState(t *testing.T) {
	t.Parallel()

	parent := cloudrig.MustStart(t)
	pc, ctx := gcs(t, parent)

	if err := pc.Bucket("shared").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := pc.Bucket("shared").Object("data.txt").NewWriter(ctx)
	w.Write([]byte("from the parent"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fork := parent.Fork(t)
	if fork.BaseURL() == parent.BaseURL() {
		t.Fatal("the fork is the same emulator")
	}

	fc, fctx := gcs(t, fork)
	if got := read(t, fc, fctx, "shared", "data.txt"); got != "from the parent" {
		t.Errorf("the fork read %q", got)
	}
}

// TestForkIsIndependent is the point of forking: two branches from one state
// that cannot see each other's writes.
func TestForkIsIndependent(t *testing.T) {
	t.Parallel()

	parent := cloudrig.MustStart(t)
	pc, ctx := gcs(t, parent)

	if err := pc.Bucket("branch").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := pc.Bucket("branch").Object("base.txt").NewWriter(ctx)
	w.Write([]byte("base"))
	w.Close()

	fork := parent.Fork(t)
	fc, fctx := gcs(t, fork)

	// Each side writes an object the other must never see.
	fw := fc.Bucket("branch").Object("only-in-fork.txt").NewWriter(fctx)
	fw.Write([]byte("fork"))
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	pw := pc.Bucket("branch").Object("only-in-parent.txt").NewWriter(ctx)
	pw.Write([]byte("parent"))
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := pc.Bucket("branch").Object("only-in-fork.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the parent can see the fork's write: %v", err)
	}
	if _, err := fc.Bucket("branch").Object("only-in-parent.txt").Attrs(fctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the fork can see the parent's later write: %v", err)
	}
	// The state they branched from is still readable on both sides.
	if got := read(t, fc, fctx, "branch", "base.txt"); got != "base" {
		t.Errorf("fork lost the shared base: %q", got)
	}
	if got := read(t, pc, ctx, "branch", "base.txt"); got != "base" {
		t.Errorf("parent lost its own base: %q", got)
	}
}

// TestForkSharesPayloads holds the reason a fork is cheap: object bytes are
// content-addressed, so the copy is metadata only and a fork still reads the
// parent's payloads after the parent has written more.
func TestForkSharesPayloads(t *testing.T) {
	t.Parallel()

	parent := cloudrig.MustStart(t)
	pc, ctx := gcs(t, parent)

	if err := pc.Bucket("big").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	const body = "the same bytes under the same name"
	w := pc.Bucket("big").Object("payload").NewWriter(ctx)
	w.Write([]byte(body))
	w.Close()

	fork := parent.Fork(t)
	fc, fctx := gcs(t, fork)

	// The parent carries on working; the fork's view must not move with it.
	pw := pc.Bucket("big").Object("payload").NewWriter(ctx)
	pw.Write([]byte("overwritten in the parent"))
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}

	if got := read(t, fc, fctx, "big", "payload"); got != body {
		t.Errorf("the fork saw the parent's overwrite: %q", got)
	}
}
