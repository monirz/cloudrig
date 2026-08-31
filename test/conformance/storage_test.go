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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/monirz/cloudrig"
	"github.com/monirz/cloudrig/functions"
	storage2 "github.com/monirz/cloudrig/services/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// bases records each test's emulator address, so a test that builds a URL by
// hand can find the instance its client is talking to.
var bases sync.Map

// baseOf is the emulator URL for this test.
func baseOf(t *testing.T) string {
	t.Helper()
	v, ok := bases.Load(t.Name())
	if !ok {
		t.Fatal("no emulator for this test; call client(t) first")
	}
	return v.(string)
}

// client points the real storage client at an in-process emulator.
func client(t *testing.T) (*storage.Client, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	bases.Store(t.Name(), emu.BaseURL())
	t.Cleanup(func() { bases.Delete(t.Name()) })
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
	logged := waitForLog(t, inst, "FIRED")
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

// TestNotMatchConditions covers the conditions the client sends and we used to
// ignore. Ignoring them is worse than refusing them: a conditional request
// silently succeeded when it should not have.
func TestNotMatchConditions(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "notmatch")

	w := b.Object("obj").NewWriter(ctx)
	w.Write([]byte("one"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	first := w.Attrs()

	t.Run("GenerationNotMatch on a read is 304 when it matches", func(t *testing.T) {
		_, err := b.Object("obj").If(storage.Conditions{
			GenerationNotMatch: first.Generation,
		}).Attrs(ctx)
		if err == nil {
			t.Fatal("a read conditioned on a different generation returned the same one")
		}
		if got := statusOf(t, err); got != 304 {
			t.Errorf("status = %d, want 304", got)
		}
	})

	t.Run("GenerationNotMatch on a read passes when it differs", func(t *testing.T) {
		if _, err := b.Object("obj").If(storage.Conditions{
			GenerationNotMatch: first.Generation + 1,
		}).Attrs(ctx); err != nil {
			t.Errorf("a read conditioned on another generation was refused: %v", err)
		}
	})

	t.Run("MetagenerationNotMatch on a read is 304 when it matches", func(t *testing.T) {
		_, err := b.Object("obj").If(storage.Conditions{
			MetagenerationNotMatch: first.Metageneration,
		}).Attrs(ctx)
		if got := statusOf(t, err); got != 304 {
			t.Errorf("status = %d, want 304", got)
		}
	})

	t.Run("GenerationNotMatch on a write is 412 when it matches", func(t *testing.T) {
		blocked := b.Object("obj").If(storage.Conditions{
			GenerationNotMatch: first.Generation,
		}).NewWriter(ctx)
		blocked.Write([]byte("two"))
		if err := blocked.Close(); err == nil {
			t.Fatal("a write conditioned against the current generation succeeded")
		} else if got := statusOf(t, err); got != 412 {
			t.Errorf("status = %d, want 412", got)
		}
	})

	t.Run("GenerationNotMatch on a delete is 412 when it matches", func(t *testing.T) {
		err := b.Object("obj").If(storage.Conditions{
			GenerationNotMatch: first.Generation,
		}).Delete(ctx)
		if got := statusOf(t, err); got != 412 {
			t.Errorf("status = %d, want 412", got)
		}
	})
}

// TestBucketUpdate covers turning versioning on after creation, which was not
// routed at all.
func TestBucketUpdate(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "patchable")

	attrs, err := b.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: true})
	if err != nil {
		t.Fatalf("enabling versioning: %v", err)
	}
	if !attrs.VersioningEnabled {
		t.Error("versioning did not take effect")
	}
	if attrs.MetaGeneration != 2 {
		t.Errorf("MetaGeneration = %d after a patch, want 2", attrs.MetaGeneration)
	}

	// It has to survive a re-read, not just come back from the patch.
	reread, err := b.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reread.VersioningEnabled {
		t.Error("versioning was not persisted")
	}

	// And it has to actually change behaviour: an overwrite now keeps the old
	// generation instead of discarding it.
	w := b.Object("obj").NewWriter(ctx)
	w.Write([]byte("one"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	firstGen := w.Attrs().Generation

	w2 := b.Object("obj").NewWriter(ctx)
	w2.Write([]byte("two"))
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Object("obj").Generation(firstGen).Attrs(ctx); err != nil {
		t.Errorf("the previous generation is gone despite versioning: %v", err)
	}
}

// TestCopyAndCompose covers the object-to-object operations. The client's
// Copier uses rewriteTo, and ComposerFrom uses compose.
func TestCopyAndCompose(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "copying")

	put := func(t *testing.T, name, content string) {
		t.Helper()
		w := b.Object(name).NewWriter(ctx)
		w.Write([]byte(content))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, name string) string {
		t.Helper()
		r, err := b.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out, _ := io.ReadAll(r)
		return string(out)
	}

	t.Run("copy", func(t *testing.T) {
		put(t, "source.txt", "original content")

		attrs, err := b.Object("dest.txt").CopierFrom(b.Object("source.txt")).Run(ctx)
		if err != nil {
			t.Fatalf("copy: %v", err)
		}
		if attrs.Name != "dest.txt" || attrs.Size != 16 {
			t.Errorf("attrs = %+v", attrs)
		}
		if got := read(t, "dest.txt"); got != "original content" {
			t.Errorf("copied content = %q", got)
		}
		// The source is untouched.
		if got := read(t, "source.txt"); got != "original content" {
			t.Errorf("source content = %q", got)
		}
	})

	t.Run("copy carries metadata across", func(t *testing.T) {
		w := b.Object("tagged.txt").NewWriter(ctx)
		w.ContentType = "text/plain"
		w.Metadata = map[string]string{"team": "infra"}
		w.Write([]byte("x"))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		attrs, err := b.Object("tagged-copy.txt").CopierFrom(b.Object("tagged.txt")).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if attrs.ContentType != "text/plain" || attrs.Metadata["team"] != "infra" {
			t.Errorf("copy lost its metadata: %+v", attrs)
		}
	})

	t.Run("copy into another bucket", func(t *testing.T) {
		other := bucket(t, c, ctx, "copying-dest")
		put(t, "cross.txt", "across buckets")

		if _, err := other.Object("arrived.txt").CopierFrom(b.Object("cross.txt")).Run(ctx); err != nil {
			t.Fatalf("cross-bucket copy: %v", err)
		}
		r, err := other.Object("arrived.txt").NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, _ := io.ReadAll(r)
		if string(got) != "across buckets" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("copy a missing object is 404", func(t *testing.T) {
		_, err := b.Object("x").CopierFrom(b.Object("absent")).Run(ctx)
		if got := statusOf(t, err); got != 404 {
			t.Errorf("status = %d, want 404", got)
		}
	})

	t.Run("compose", func(t *testing.T) {
		put(t, "part-a", "hello ")
		put(t, "part-b", "composed ")
		put(t, "part-c", "world")

		attrs, err := b.Object("whole.txt").ComposerFrom(
			b.Object("part-a"), b.Object("part-b"), b.Object("part-c"),
		).Run(ctx)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		if attrs.Size != 20 {
			t.Errorf("Size = %d, want 20", attrs.Size)
		}
		if got := read(t, "whole.txt"); got != "hello composed world" {
			t.Errorf("composed content = %q", got)
		}
	})

	t.Run("compose a missing part is 404", func(t *testing.T) {
		_, err := b.Object("bad.txt").ComposerFrom(b.Object("part-a"), b.Object("absent")).Run(ctx)
		if got := statusOf(t, err); got != 404 {
			t.Errorf("status = %d, want 404", got)
		}
	})
}

// TestCopyIsMetadataOnly is the property content addressing buys: copying does
// not duplicate the bytes, so a copy of a large object is as cheap as a small
// one.
func TestCopyIsMetadataOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("uploads tens of megabytes")
	}

	c, ctx := client(t)
	b := bucket(t, c, ctx, "cheap-copy")

	const size = 32 << 20
	w := b.Object("big.bin").NewWriter(ctx)
	w.ChunkSize = 0
	if _, err := io.Copy(w, source(size)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if _, err := b.Object("big-copy.bin").CopierFrom(b.Object("big.bin")).Run(ctx); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	churn := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	t.Logf("copying %d MiB allocated %d KiB in total", size>>20, churn>>10)
	if churn > 1<<20 {
		t.Errorf("copying %d bytes allocated %d; a copy should not move the content", size, churn)
	}
}

// TestIAMPolicy drives the policy endpoints through the real client's IAM
// handle. Nothing is enforced — there is no identity here — but Terraform's
// google_storage_bucket_iam_member does read-modify-write against these, and
// an unrouted endpoint made it hang rather than fail.
func TestIAMPolicy(t *testing.T) {
	c, ctx := client(t)
	b := bucket(t, c, ctx, "iam-bucket")

	policy, err := b.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("reading an unset policy: %v", err)
	}
	if len(policy.Roles()) != 0 {
		t.Errorf("a fresh bucket has roles: %v", policy.Roles())
	}

	policy.Add("allUsers", "roles/storage.objectViewer")
	if err := b.IAM().SetPolicy(ctx, policy); err != nil {
		t.Fatalf("setting a policy: %v", err)
	}

	got, err := b.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members := got.Members("roles/storage.objectViewer")
	if len(members) != 1 || members[0] != "allUsers" {
		t.Errorf("members = %v, want [allUsers]", members)
	}
}

// waitForLog returns a function's log once it contains want.
//
// SyncEvents proves the event was delivered and the handler answered. It
// cannot prove the handler's output has arrived: a function is a child
// process, its stdout is a pipe, and the goroutine draining that pipe runs
// after the response. On a loaded machine that gap is wide enough to read an
// empty log, which is what made this flaky in CI and never here.
func waitForLog(t *testing.T, inst *functions.Instance, want string) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		logged := strings.Join(inst.LogSnapshot(), "\n")
		if strings.Contains(logged, want) {
			return logged
		}
		if time.Now().After(deadline) {
			return logged
		}
		time.Sleep(5 * time.Millisecond)
	}
}
