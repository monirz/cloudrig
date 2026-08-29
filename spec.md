# Claude Code kickoff prompt — cloudrig M1

Paste everything below the line into Claude Code as the first message in a fresh
repo. It is scoped to one verifiable milestone, not the whole project. Later
milestones get their own prompts against the same architecture rules.

---

Build the first milestone of **cloudrig**, a local emulator for Google Cloud
APIs written in Go. Module path `github.com/monirz/cloudrig`, Go 1.23+.

Read this whole brief before writing any code. Then propose the package layout
and the `core` interfaces, wait for my confirmation, and only then implement.

## What cloudrig is

A single-binary, single-port emulator of GCP APIs for local development and CI.
Two entry points into the same code:

1. A binary / Docker image (`cloudrig start`) that GCP SDKs and `gcloud` point at
   via the standard endpoint-override env vars.
2. A Go library (`cloudrig.MustStart(t)`) that runs **in-process inside a Go
   test** — no Docker daemon, no container, random free port, per-test isolation.

Entry point 2 is the reason the project exists. Competing emulators are a Java
container (floci-gcp) or a Python container (LocalStack) and structurally cannot
be imported into a Go test binary. `fsouza/fake-gcs-server` is Go and does embed,
but it has no controllable clock, no fault injection, no state forking, and
covers only GCS. Everything below is designed to make those four things possible.

## Non-negotiable architecture rules

These matter more than any individual feature. Enforce them from the first
commit; violating them later is a rewrite.

1. **Dependency direction is one-way.** `core` imports no service package.
   Service packages import `core`. Service packages never import each other —
   cross-service interaction happens only by publishing to the event bus in
   `core`.

2. **Nothing in the codebase calls `time.Now`, `time.Sleep`, `time.After`, or
   `time.NewTimer`.** All time comes from an injected `Clock` interface that owns
   the timer queue. Add a `go vet`-style check or a lint rule in CI that fails
   the build on direct `time` package timer use outside `core/clock`.

3. **Object payload bytes never enter the Go heap in full.** Uploads stream from
   `r.Body` to a temp file through a `TeeReader` into the hashers, one pass.
   Downloads use `os.Open` + `http.ServeContent`. Memory must stay constant
   regardless of object size. This is a hard requirement — `fake-gcs-server` has
   had open issues since 2022 where a 2 GB file causes 12 GB of RSS, and beating
   that is a headline property of this project.

4. **`store` and `runner` are interfaces defined in `core`**, implemented at the
   edge. The library entry point must work with no Docker daemon present.

5. **Unimplemented means loud.** Any operation not implemented returns HTTP 501 /
   `codes.Unimplemented` naming the operation. Never accept-and-ignore a field
   silently. Maintain `UNSUPPORTED.md` listing every field accepted but ignored.

## Package layout (propose adjustments if you disagree)

```
cmd/cloudrig/            main: flags, env, wiring, graceful shutdown
internal/transport/      h2c mux (gRPC + REST on one port), codecs
internal/services/
  storage/               GCS: handlers + semantics, no proto/HTTP types leaking down
internal/core/
  resource/              GCP resource-name parsing + hierarchical registry
  clock/                 Clock interface, real clock, FakeClock with Advance()
  gerr/                  canonical errors -> google.rpc.Status + GCS JSON envelope
  events/                in-process event bus (Publish / Subscribe / Sync)
  faults/                fault rules + matcher
  trace/                 ring buffer of recent requests
internal/store/          Store interface + memory impl + blob tree
pkg/emulator/            public library API: Start, MustStart, Fork, Clock, Faults
test/conformance/        one suite, runs against emulator or real GCP by env var
```

## Scope of this milestone

### Transport
- Single port (default 4599). One `h2c` handler: dispatch to gRPC when
  `ProtoMajor == 2` and content-type is `application/grpc`, otherwise to the HTTP
  mux. No gRPC services exist yet — ship the mux anyway.
- Route on `r.URL.EscapedPath()`, **not** `r.URL.Path`. GCS object names contain
  slashes and arrive percent-encoded (`/storage/v1/b/bkt/o/logs%2Fapp.log`);
  `net/http` decodes `Path` before handlers see it, making `a%2Fb` and `a/b`
  indistinguishable. Unescape only the object segment yourself. Include a test
  with an object named `logs/2026/app.log`.

### Store
```go
type Store interface {
    Get(ctx context.Context, key string) (val []byte, version uint64, err error)
    Put(ctx context.Context, key string, val []byte, ifVersion uint64) (uint64, error) // 0 = must not exist
    Delete(ctx context.Context, key string, ifVersion uint64) error
    List(ctx context.Context, prefix string, limit int, pageToken string) ([][]byte, string, error)
    Reset(ctx context.Context, keyPrefix string) error
}
```
Memory implementation only in this milestone. `ifVersion` is real CAS — GCS
preconditions depend on it, so no read-then-write.

Blobs are separate from the KV store: content-addressed by SHA-256 under
`{dataDir}/blobs/{sha[0:2]}/{sha}`, or a temp dir in memory mode. Metadata holds
the hash. Copy is metadata-only.

### GCS (subset)
Key layout:
```
b/{bucket}                          bucket metadata
b/{bucket}/live/{name}              pointer to current generation
b/{bucket}/o/{name}#{generation}    object metadata
```

Endpoints for this milestone:
- `POST /storage/v1/b?project=` — create bucket (409 on duplicate)
- `GET  /storage/v1/b?project=` — list buckets
- `GET  /storage/v1/b/{bucket}` — get bucket
- `DELETE /storage/v1/b/{bucket}` — 409 if non-empty
- `POST /upload/storage/v1/b/{bucket}/o?uploadType=media&name=` — upload bytes
- `GET  /storage/v1/b/{bucket}/o/{object}` — metadata; `?alt=media` returns bytes
- `GET  /storage/v1/b/{bucket}/o` — list with `prefix`, `delimiter`, `maxResults`,
  `pageToken`; `delimiter` synthesizes the `prefixes[]` rollup
- `DELETE /storage/v1/b/{bucket}/o/{object}`

Object model: bucket, name, generation, metageneration, size, contentType,
metadata map, crc32c (Castagnoli, base64 big-endian), md5, etag, created,
updated, blobRef.

Generation is `clock.Now().UnixMicro()`, incremented on collision so it stays
unique and monotonic. Metageneration starts at 1.

Preconditions in this milestone: `ifGenerationMatch` (including `=0` meaning
"must not exist" — this is what `storage.Conditions{DoesNotExist: true}`
compiles to) and `ifMetagenerationMatch`. Mismatch is HTTP 412,
`FAILED_PRECONDITION`, reason `conditionNotMet`.

### Clock
```go
type Clock interface {
    Now() time.Time
    AfterFunc(d time.Duration, f func()) Timer
}
```
`FakeClock.Advance(d)` walks the timer queue in timestamp order, runs every due
callback **synchronously**, and returns only when the queue is drained past `d`.
Deterministic, no sleeping, no polling.

### pkg/emulator
```go
func Start(ctx context.Context, o Options) (*Emulator, error)
func MustStart(t testing.TB, o ...Options) *Emulator   // t.Cleanup registered

func (e *Emulator) Endpoint() string
func (e *Emulator) BaseURL() string
func (e *Emulator) StorageClient(t testing.TB) *storage.Client
func (e *Emulator) Clock() *clock.FakeClock
func (e *Emulator) Reset(ctx context.Context) error
```
`MustStart` binds a random free port via `httptest.NewServer`. Each call is a
fully isolated instance so tests can run `t.Parallel()`.

### Admin
- `GET /_emu/health`
- `POST /_emu/reset` (optional `?project=`)

## Acceptance criteria

The milestone is done when all of these pass:

1. A Go test using the real `cloud.google.com/go/storage` client, pointed at
   `MustStart(t)`, creates a bucket, writes an object, reads it back, and the
   CRC32C and MD5 match.
2. An object named `logs/2026/app.log` round-trips through get, list, and delete.
3. `storage.Conditions{DoesNotExist: true}` succeeds on first write and returns
   412 on the second.
4. Two goroutines writing the same object with `ifGenerationMatch` — exactly one
   succeeds.
5. Overwrite bumps generation; a metadata-only change bumps metageneration only.
6. `prefix=a/&delimiter=/` returns the expected `items[]` and `prefixes[]`.
7. Two `t.Parallel()` tests each calling `MustStart(t)` and writing the same
   object name do not interfere.
8. A benchmark uploads a 1 GiB object and asserts peak RSS stays under 64 MiB.
9. `go test ./...` passes with no Docker daemon running.
10. `go vet ./...` and the timer lint rule pass.

## Explicitly out of scope

Do not build these now, and return 501 for them: resumable and multipart
uploads, the XML API, compose, rewrite, versioning, persistent/WAL storage
modes, Cloud Functions, Pub/Sub, IAM enforcement, auth of any kind, fault
injection, tracing, the event bus wiring (define the interface, leave it
unsubscribed).

## Conventions

- Standard library first. Justify every third-party dependency; `testify` is
  fine, an HTTP router framework is not.
- Table-driven tests. Test the semantics layer directly, not only through HTTP.
- Errors via `gerr`, never bare `errors.New` in handlers.
- Comments explain *why*, not *what* — especially for the `EscapedPath` routing
  and the generation-collision handling, where the reason is non-obvious.
- No `panic` outside `MustStart`.

## How to work

Propose the layout and the `core` interfaces first. Once I confirm, implement in
this order, running tests at each step: `core/clock` → `store/memory` →
`core/gerr` → `internal/services/storage` semantics → `internal/transport` →
`pkg/emulator` → conformance tests → the RSS benchmark.

Ask me before adding a dependency or deviating from the layout.