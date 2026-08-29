# cloudrig M1 — implementation plan

Companion to [spec.md](spec.md). The spec is the brief; this file records the
decisions taken against it and the order of work. Where the two disagree, this
file wins — every deviation below has a stated reason.

## Positioning

See [docs/competitive.md](docs/competitive.md). Short version: cloudrig does not
win on request latency or service breadth, and should not try to. It wins on
**per-test setup cost** (in-process, no container) and **determinism**
(injected clock), with **constant memory on large objects** as the supporting
claim. Every decision below is downstream of that.

## Decisions against the spec

### D1 — Upload types: `media` + `multipart` in scope, resumable stays 501

The spec scoped only `uploadType=media`, but the real `cloud.google.com/go/storage`
client never sends it. In `google.golang.org/api/internal/gensupport`,
`MediaInfo.UploadType()` returns exactly `"multipart"` or `"resumable"` — there
is no `media` path. `Writer.ChunkSize` defaults to `googleapi.DefaultUploadChunkSize`
(16 MiB), so objects under 16 MiB go out as `multipart` and larger ones as
`resumable`. Acceptance criteria 1-8 all drive the real client, so as written
none of them could pass.

Resolution: implement `media` and `multipart`. The multipart body is parsed with
`mime/multipart.Reader` and the media part streams straight into the TeeReader,
so it costs nothing in heap terms.

Resumable stays unimplemented (501). It is not needed even for the 1 GiB
benchmark: `PrepareUpload` returns `(media, nil, singleChunk=true)` when
`chunkSize == 0`, and `UploadType()` reports `"multipart"` for a single chunk.
So a benchmark that sets `Writer.ChunkSize = 0` streams the whole gibibyte as one
unbuffered multipart request. (Side effect: that request is non-retryable
client-side. Fine in a test.)

### D2 — Add object PATCH

`PATCH /storage/v1/b/{bucket}/o/{object}` joins the milestone. Acceptance
criterion 5 requires a metadata-only change that bumps metageneration without
bumping generation, and no endpoint in the original scope could produce one.

### D3 — Key encoding uses NUL, not `#`

`#` is a legal character in a GCS object name, so `b/{bucket}/o/{name}#{gen}` is
ambiguous: an object named `a#b` at generation 5 and an object named `a` at
generation `b#5` produce the same key. Prefix scans get it wrong too.

New layout, with generation zero-padded to 20 digits so keys sort
lexicographically in generation order:

```
p/{project}/b/{bucket}                             bucket metadata
p/{project}/b/{bucket}/live/{name}                 pointer to current generation
p/{project}/b/{bucket}/o/{name}\x00{gen:020d}      object metadata
```

NUL cannot appear in an object name, so the separator is unambiguous. It also
sorts before every other byte, which keeps `a` ordered ahead of `a/b`.

### D4 — Project is part of the key path

The spec keyed buckets as `b/{bucket}` while also specifying
`POST /_emu/reset?project=`. The project appeared only inside the bucket
metadata *value*, so reset-by-project could not be expressed as a prefix
operation against `Store.Reset(ctx, keyPrefix)`. Buckets now live under
`p/{project}/`, as shown above.

### Decisions not requiring sign-off

- **Generation allocation.** `gen = max(clock.Now().UnixMicro(), lastGenEverSeen+1)`,
  where `lastGenEverSeen` is tracked on the live pointer — not merely the current
  live generation. Under `FakeClock` time does not advance unless a test advances
  it, so every write in a test collides and walks the counter forward; taking the
  max keeps a later real-time write from landing below an earlier faked one.
- **Store versions start at 1.** That makes `ifVersion == 0` unambiguously "must
  not exist" on `Put`, and "unconditional" on `Delete`.
- **`Store.List` returns keys with values** (`[]KV`), not bare `[][]byte`.
  Delimiter rollup needs the key to synthesize `prefixes[]`; decoding every
  candidate's JSON just to recover its own name would be absurd.
- **`Store.List`'s `limit` is a scan budget, not a page size.** Collapsing ten
  thousand keys under one delimiter prefix must not consume the caller's page.
  The handler paginates above the store.
- **CAS losers leave orphans.** A goroutine that loses the live-pointer CAS has
  already written its object metadata and blob. In M1 those are garbage, swept
  only by `Reset`. Recorded in `UNSUPPORTED.md`.
- **`core/faults` and `core/trace` are omitted**, not stubbed.
  The spec listed both in the layout and on the out-of-scope list. Empty packages
  are noise; they arrive in M2 with their implementations.
- **Timer lint needs no dependency.** A test in `lint` walks the module
  with stdlib `go/parser` and `go/ast` and fails on any `time` timer call outside
  `core/clock`. No `x/tools`, no golangci-lint. `go vet` has no such
  analyzer, so the spec's "go vet-style check" could not have been literal.
- **The RSS benchmark gates on heap, not RSS.** Go does not return memory to the
  OS promptly enough for a 64 MiB RSS assertion to be anything but flaky. The
  hard gate is a `runtime.ReadMemStats` `HeapAlloc` delta; RSS is reported
  alongside it for information. The 1 GiB source is an `io.LimitReader` over a
  repeating reader, never a `make([]byte, 1<<30)`.

## Package layout

No `internal/` tree: every package sits at the module root. cloudrig is meant
to be imported, and `internal/` would make each of these unreachable to anyone
embedding it — which is the entry point the project exists for.

```
cloudrig.go              package cloudrig: Start, MustStart, Clock, Reset
cmd/cloudrig/            package main: flags, env, wiring, graceful shutdown
transport/               h2c mux (gRPC + REST on one port), codecs
services/
  storage/               GCS: handlers + semantics, no proto/HTTP types leak down
core/
  resource/              GCP resource-name parsing + key codec
  clock/                 Clock interface, real clock, FakeClock with Advance()
  gerr/                  canonical errors -> google.rpc.Status + GCS JSON envelope
  events/                in-process event bus (defined, left unsubscribed)
store/                   Store interface + memory impl + blob tree
lint/                    stdlib AST check for forbidden time calls
test/conformance/        one suite, runs against emulator or real GCP by env var
```

The public library API is the **module root**, not `pkg/emulator`. The call site
is the product here — `cloudrig.MustStart(t)` is what spec.md itself writes, and
it is what a reader of someone else's test has to recognise instantly.
`emulator.MustStart(t)` reads like a detail of a library rather than the library.
`pkg/` would also be a second layer of nesting-by-convention immediately after
removing the first.

`cmd/cloudrig` imports the root package and adds only flags, env and signal
handling; every behaviour it exposes has to be reachable from the library entry
point too, or the binary has grown a feature tests cannot use.

This is a deviation from the layout in spec.md, which nested everything under
`internal/`.

## Build order

Each step lands green before the next begins.

| # | Step | Done when |
|---|------|-----------|
| 0 | `go.mod`, `lint` AST checker, CI | lint test passes on an empty tree |
| 1 | `core/clock` | `Advance` runs due callbacks synchronously, in timestamp order |
| 2 | `store` interface + memory impl, real CAS | concurrent CAS: exactly one winner of N |
| 3 | `core/resource` + key codec (D3/D4) | round-trips names with `#`, `/`, `%`, unicode |
| 4 | `core/gerr` | 412/`conditionNotMet`, 409, 404, 501 envelopes match real GCS |
| 5 | blob tree: SHA-256 content-addressed, one-pass TeeReader into crc32c + md5 | 1 GiB through the writer, flat `HeapAlloc` |
| 6 | `services/storage` semantics, no HTTP types | criteria 3, 4, 5, 6 tested at this layer directly |
| 7 | `transport`: h2c mux, `EscapedPath()` routing, multipart parse | criterion 2 (`logs/2026/app.log`) |
| 8 | root `package cloudrig` | criteria 1, 7 |
| 9 | `test/conformance` + admin endpoints | criteria 9, 10 |
| 10 | RSS/heap benchmark | criterion 8 |

Steps 6 and 7 are where the spec's "test the semantics layer directly, not only
through HTTP" pays off: criteria 3-6 become fast unit tests with no server at all.

## Addition to M1

A committed `BenchmarkStartup` measuring `MustStart(t)` wall time, plus a
documented comparison against a testcontainers-floci fixture. The project's
central claim is a performance claim, so it belongs in a test that fails on
regression rather than in a sentence in the README.

## Acceptance criteria, as amended

1. Real `storage.Client` against `MustStart(t)`: create bucket, write, read back,
   CRC32C and MD5 match.
2. `logs/2026/app.log` round-trips through get, list, delete.
3. `storage.Conditions{DoesNotExist: true}` succeeds once, then 412.
4. Two goroutines with `ifGenerationMatch` — exactly one succeeds.
5. Overwrite bumps generation; PATCH (D2) bumps metageneration only.
6. `prefix=a/&delimiter=/` returns the expected `items[]` and `prefixes[]`.
7. Two `t.Parallel()` tests, each `MustStart(t)`, same object name, no interference.
8. 1 GiB upload with `Writer.ChunkSize = 0` (D1) holds `HeapAlloc` flat.
9. `go test ./...` passes with no Docker daemon running.
10. `go vet ./...` and the `lint` timer check pass.
