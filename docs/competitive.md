# Competitive landscape

Researched 2026-08-29. This exists because the framing in `spec.md` was partly
wrong about floci-gcp, and the wrong framing leads to the wrong engineering.

## floci-gcp

<https://github.com/floci-io/floci-gcp> — MIT, v0.7.0, releases roughly every
two to three weeks.

The spec described it as "a Java container." It is a **Quarkus / GraalVM native
image**, and the difference matters: published figures are **24 ms startup** and
**~13 MiB idle**. REST and gRPC share one port (4588) via ALPN negotiation
(`quarkus.grpc.server.use-separate-server=false`). Wire compatibility comes from
subclassing Google's own generated protobuf service base classes off Maven
Central, so message types are byte-identical to production by construction and
track upstream through dependency bumps. 25 services, validated by 186 SDK
compatibility tests across Java, Python, Node.js and Go.

Its GCS emulation is well ahead of cloudrig M1:

- JSON **and** XML APIs
- media, multipart **and** resumable uploads; batch via `/batch/storage/v1`
- compose (up to 32 sources), copy, move, head
- object metadata PATCH
- customer-supplied encryption keys
- bucket and object ACLs, default object ACLs
- V4 signed URLs, path and virtual-hosted style
- object-change notifications into Pub/Sub
- versioning, and the full precondition set — `ifGenerationMatch`,
  `ifGenerationNotMatch`, `ifMetagenerationMatch`, `ifMetagenerationNotMatch`,
  plus source-level conditions on move

They document explicitly that "concurrent writers with `ifGenerationMatch=0`
race safely." Our acceptance criterion 4 is their table stakes, not a
differentiator.

Four storage modes: `memory` (default), `persistent`, `hybrid` (async 5 s
flush), `wal`.

Documented deviations from real GCS: any chunk size accepted where real GCS
demands 256 KiB multiples for non-final chunks; completed resumable sessions
retained for 1024 uploads rather than a week; special handling for the Go SDK's
`X-GUploader-No-308` header.

### What it does not have

- **No clock control.** Nothing in the docs, and the architecture gives no hook
  for one. Anything time-shaped — lifecycle, retention, TTL, signed-URL expiry,
  generation ordering — is flaky or untestable against it.
- **No fault injection.**
- **No state forking or snapshots.**
- **No embedding.** It is a container. Testcontainers is the integration story;
  a Go test binary cannot import it.

## fake-gcs-server

<https://github.com/fsouza/fake-gcs-server> — Go, and genuinely embeddable,
which makes it the closest thing to a direct competitor. It has no controllable
clock, no fault injection, no state forking, and covers only GCS.

Its memory behaviour is the opening. Long-standing, still-open issues:

- [#397](https://github.com/fsouza/fake-gcs-server/issues/397) — a 2.06 GB file
  in a bind mount drives the container to **12.4 GB** RSS at startup, climbing
  to 15.2 GB when objects are queried.
- [#669](https://github.com/fsouza/fake-gcs-server/issues/669) — `multipartUpload`
  and `uploadFileContent` both go through `ioutil.ReadAll`, pulling the entire
  request body into the heap.
- [#828](https://github.com/fsouza/fake-gcs-server/issues/828) — downloads load
  the whole object into memory, which compounds under concurrent requests.
- [#671](https://github.com/fsouza/fake-gcs-server/issues/671) — objects are
  stored JSON-encoded rather than as binary.

## Where cloudrig wins

Not on request latency, and not on breadth. Both fights are already lost, and
pretending otherwise produces a worse project.

**1. Per-test setup cost — three to four orders of magnitude.** floci's 24 ms is
the process. The cost that a test suite actually pays is Docker: image resolve,
container create, start, then a testcontainers health-poll against `/health`.
Call it 0.5-3 s. The consequence shows up in their own API surface — because a
container per test is unaffordable, everyone shares one across the suite, which
is precisely why floci ships `POST /_floci-gcp/state/reset`. Reset-between-tests
is the workaround for isolation you cannot afford to buy honestly.

`MustStart(t)` is an `httptest.NewServer` in-process: sub-millisecond, one per
test, genuinely parallel. A 500-test suite pays about a second of setup instead
of ten minutes, or instead of shared mutable state.

**2. Determinism.** `FakeClock.Advance()` is not a feature floci can bolt on; it
requires the injected-clock discipline from the first commit, which is why
`spec.md` makes it non-negotiable.

**3. Constant memory on large objects.** floci's docs never state a streaming
strategy and `memory` is the default mode, so large objects are probably
resident. fake-gcs-server is provably catastrophic. A benchmarked flat-heap
guarantee is defensible and, as far as this research found, unmatched.

**4. No daemon.** `go test ./...` on a macOS CI runner with no Docker at all.
Neither competitor can do this.

**5. Forking.** Neither has it.

The pitch is **zero-cost per-test isolation plus determinism**, not "faster
server."
