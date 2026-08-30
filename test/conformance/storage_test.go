// Package conformance drives the emulator with the real Google client, which is
// the only way to know the wire format is right: our own handlers agreeing with
// our own tests proves nothing about what a client expects.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
	storage2 "github.com/monirz/cloudrig/services/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// client points the real storage client at an in-process emulator.
func client(t *testing.T) (*storage.Client, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

func bucket(t *testing.T, c *storage.Client, ctx context.Context, name string) *storage.BucketHandle {
	t.Helper()
	b := c.Bucket(name)
	if err := b.Create(ctx, "test-project", nil); err != nil {
		t.Fatalf("creating bucket %s: %v", name, err)
	}
	return b
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	t.Fatalf("error %v is not a googleapi.Error", err)
	return 0
}

// TestRoundTrip is acceptance criterion 1: the real client writes an object,
// reads it back, and the checksums match.
func TestRoundTrip(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "round-trip")

	content := []byte("hello from a real client")
	w := b.Object("greeting.txt").NewWriter(ctx)
	w.ContentType = "text/plain"
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the writer: %v", err)
	}

	// The client verifies CRC32C on read, so a wrong checksum fails here
	// rather than silently.
	r, err := b.Object("greeting.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("opening a reader: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	attrs, err := b.Object("greeting.txt").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", attrs.Size, len(content))
	}
	if attrs.ContentType != "text/plain" {
		t.Errorf("ContentType = %q", attrs.ContentType)
	}
	if attrs.CRC32C == 0 || len(attrs.MD5) == 0 {
		t.Errorf("checksums missing: crc32c=%d md5=%x", attrs.CRC32C, attrs.MD5)
	}
	if attrs.Generation == 0 {
		t.Error("Generation is zero; the wire format spells it as a string")
	}
}

// TestObjectNameWithSlashes is acceptance criterion 2: a name holding slashes
// arrives percent-encoded and must survive get, list and delete.
func TestObjectNameWithSlashes(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "slashes")

	const name = "logs/2026/app.log"
	w := b.Object(name).NewWriter(ctx)
	w.Write([]byte("log line"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	attrs, err := b.Object(name).Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if attrs.Name != name {
		t.Errorf("Name = %q, want %q", attrs.Name, name)
	}

	it := b.Objects(ctx, nil)
	var listed []string
	for {
		o, err := it.Next()
		if err != nil {
			break
		}
		listed = append(listed, o.Name)
	}
	if len(listed) != 1 || listed[0] != name {
		t.Errorf("listed = %v, want [%s]", listed, name)
	}

	if err := b.Object(name).Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Object(name).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("err = %v, want ErrObjectNotExist", err)
	}
}

// TestDoesNotExistCondition is acceptance criterion 3.
func TestDoesNotExistCondition(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "conditions")

	first := b.Object("once").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	first.Write([]byte("first"))
	if err := first.Close(); err != nil {
		t.Fatalf("the first conditional write failed: %v", err)
	}

	second := b.Object("once").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	second.Write([]byte("second"))
	err := second.Close()
	if err == nil {
		t.Fatal("the second conditional write succeeded")
	}
	if got := statusOf(t, err); got != 412 {
		t.Errorf("status = %d, want 412", got)
	}
}

func TestGenerationCondition(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "generations")

	w := b.Object("obj").NewWriter(ctx)
	w.Write([]byte("one"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	attrs := w.Attrs()

	ok := b.Object("obj").If(storage.Conditions{GenerationMatch: attrs.Generation}).NewWriter(ctx)
	ok.Write([]byte("two"))
	if err := ok.Close(); err != nil {
		t.Fatalf("a matching generation was refused: %v", err)
	}

	stale := b.Object("obj").If(storage.Conditions{GenerationMatch: attrs.Generation}).NewWriter(ctx)
	stale.Write([]byte("three"))
	if err := stale.Close(); statusOf(t, err) != 412 {
		t.Errorf("status = %d for a stale generation, want 412", statusOf(t, err))
	}
}

// TestPrefixAndDelimiter is acceptance criterion 6, through the client.
func TestPrefixAndDelimiter(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "listing")

	for _, name := range []string{"a/1.txt", "a/2.txt", "a/deep/3.txt", "b/1.txt"} {
		w := b.Object(name).NewWriter(ctx)
		w.Write([]byte("x"))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	it := b.Objects(ctx, &storage.Query{Prefix: "a/", Delimiter: "/"})
	var objects, prefixes []string
	for {
		o, err := it.Next()
		if err != nil {
			break
		}
		if o.Prefix != "" {
			prefixes = append(prefixes, o.Prefix)
			continue
		}
		objects = append(objects, o.Name)
	}

	if len(objects) != 2 || objects[0] != "a/1.txt" || objects[1] != "a/2.txt" {
		t.Errorf("objects = %v, want a/1.txt and a/2.txt", objects)
	}
	if len(prefixes) != 1 || prefixes[0] != "a/deep/" {
		t.Errorf("prefixes = %v, want [a/deep/]", prefixes)
	}
}

func TestBucketLifecycle(t *testing.T) {
	c, ctx := client(t)

	b := c.Bucket("lifecycle")
	if err := b.Create(ctx, "test-project", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(ctx, "test-project", nil); statusOf(t, err) != 409 {
		t.Errorf("status = %d creating a duplicate, want 409", statusOf(t, err))
	}

	attrs, err := b.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Name != "lifecycle" {
		t.Errorf("Name = %q", attrs.Name)
	}

	w := b.Object("blocker").NewWriter(ctx)
	w.Write([]byte("x"))
	w.Close()
	if err := b.Delete(ctx); statusOf(t, err) != 409 {
		t.Errorf("status = %d deleting a non-empty bucket, want 409", statusOf(t, err))
	}

	if err := b.Object("blocker").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx); err != nil {
		t.Errorf("deleting an emptied bucket: %v", err)
	}
}

// TestParallelInstancesAreIsolated is acceptance criterion 7: two parallel
// tests writing the same object name must not see each other.
func TestParallelInstancesAreIsolated(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			c, ctx := client(t)
			b := bucket(t, c, ctx, "shared-name")

			w := b.Object("same-object").NewWriter(ctx)
			w.Write([]byte(name))
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			r, err := b.Object("same-object").NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, _ := io.ReadAll(r)
			if string(got) != name {
				t.Errorf("read %q, want %q — the instances share state", got, name)
			}
		})
	}
}

// clientB is client for a benchmark, which has no *testing.T to hang cleanup
// on.
func clientB(b *testing.B) (*storage.Client, context.Context) {
	b.Helper()

	emu, err := cloudrig.Start(context.Background(), cloudrig.Options{Addr: "127.0.0.1:0"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = emu.Shutdown(context.Background()) })

	ctx := context.Background()
	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

// TestResetClearsState covers the admin endpoint a test suite uses between
// cases when it shares one emulator rather than starting its own.
func TestResetClearsState(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	b := c.Bucket("resettable")
	if err := b.Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := b.Object("obj").NewWriter(ctx)
	w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(emu.BaseURL()+"/_emu/reset", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", resp.StatusCode)
	}

	if _, err := b.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("err = %v, want ErrBucketNotExist after a reset", err)
	}
	// The name is reusable, so a suite can start the next case cleanly.
	if err := b.Create(ctx, "p", nil); err != nil {
		t.Errorf("recreating the bucket after a reset: %v", err)
	}
}

// TestUploadFiresAFunction is the whole point of the event bus: an object
// written through the real client runs a function, in one process, with no
// Docker and no Pub/Sub.
func TestUploadFiresAFunction(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()

	if _, err := emu.Functions().Deploy(ctx, functions.Function{
		Name:   "on-upload",
		Source: "../../testdata/go-trigger",
		Trigger: functions.EventTrigger{
			EventType: storage2.EventFinalized,
			Resource:  "uploads",
		},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := storage.NewClient(ctx,
		option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	b := c.Bucket("uploads")
	if err := b.Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := b.Object("report.csv").NewWriter(ctx)
	w.Write([]byte("a,b,c"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Delivery is asynchronous so the upload never waits on it.
	emu.SyncEvents()

	inst, ok := emu.Functions().Instance("", "", "on-upload")
	if !ok {
		t.Fatal("the function is not deployed")
	}
	logged := strings.Join(inst.LogSnapshot(), "\n")
	for _, want := range []string{
		"FIRED",
		storage2.EventFinalized,
		"gs://uploads/report.csv",
		"storage.googleapis.com",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the function log is missing %q:\n%s", want, logged)
		}
	}
}

// TestResumableUpload covers the client's default path. Writer.ChunkSize is
// left alone: the client chunks at 16 MiB and switches to resumable above it,
// so this is what a user gets without knowing the protocol exists.
func TestResumableUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("uploads tens of megabytes")
	}

	c, ctx := client(t)
	b := bucket(t, c, ctx, "resumable")

	const size = 40 << 20 // over the default chunk, so several chunks

	w := b.Object("big.bin").NewWriter(ctx)
	if _, err := io.Copy(w, source(size)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("the default upload path failed: %v", err)
	}

	attrs, err := b.Object("big.bin").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != size {
		t.Fatalf("stored %d bytes, want %d", attrs.Size, size)
	}

	// Read it back and compare against the same generator, so the whole object
	// is never held to check it.
	r, err := b.Object("big.bin").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	want := source(size)
	got, expect := make([]byte, 64<<10), make([]byte, 64<<10)
	var read int64
	for {
		n, err := io.ReadFull(r, got)
		if n == 0 {
			break
		}
		_, _ = io.ReadFull(want, expect[:n])
		if !bytes.Equal(got[:n], expect[:n]) {
			t.Fatalf("content differs at offset %d", read)
		}
		read += int64(n)
		if err != nil {
			break
		}
	}
	if read != size {
		t.Errorf("read back %d bytes, want %d", read, size)
	}
}

// TestResumableWithPreconditions checks the conditions travel with the session
// rather than being forgotten when the first chunk lands.
func TestResumableWithPreconditions(t *testing.T) {
	if testing.Short() {
		t.Skip("uploads tens of megabytes")
	}

	c, ctx := client(t)
	b := bucket(t, c, ctx, "resumable-conditions")

	const size = 20 << 20

	first := b.Object("once").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	if _, err := io.Copy(first, source(size)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("the first conditional resumable upload failed: %v", err)
	}

	second := b.Object("once").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	if _, err := io.Copy(second, source(size)); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err == nil {
		t.Fatal("the second conditional upload succeeded")
	} else if statusOf(t, err) != 412 {
		t.Errorf("status = %d, want 412", statusOf(t, err))
	}
}
