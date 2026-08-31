# Architecture

How cloudrig is put together.

- [Shape](#shape) — one binary, one port, two entry points
- [Packages](#packages)
- [Rules the code holds to](#rules-the-code-holds-to)
- [How behaviour is verified](#how-behaviour-is-verified)

---

## Shape

One binary, one port, two ways in:

```
  gcloud / client libraries / Terraform / curl        go test
                     |                                   |
                     v                                   v
              cloudrig start                  cloudrig.MustStart(t)
                     \                                   /
                      \_________ transport _____________/
                                     |
             +-----------+-----------+-----------+
             |           |           |           |
        functions    storage      pubsub      _emu admin
             |           |           |
             +-----> store / blob <--+
```

**The in-process entry point is the reason the project exists.** Competing
emulators are containers and cannot be imported into a Go test binary.
`MustStart(t)` gives one isolated emulator per test on a random port, with a
fake clock and `t.Cleanup` already registered.

**One port** carries HTTP/1.1 and h2c, so gRPC and REST share it. The transport
dispatches on content type; a Pub/Sub client and a `curl` reach the same
process the same way they would reach Google.

---

## Packages

| Package | What it is |
|---|---|
| `transport` | The front door: one router, service mounts, h2c gRPC dispatch |
| `core/clock` | The only place permitted to read wall-clock time |
| `core/gerr` | The canonical error: a code, an explicit HTTP status, a reason |
| `core/events` | The in-process bus; the only path between services |
| `core/faults` | Rules that fail chosen requests, matched before routing |
| `core/resource` | GCP resource names encoded as store keys |
| `core/tmp` | One temp-directory root per process, so a crash leaves one thing |
| `store` | Versioned compare-and-swap KV, in memory or persisted |
| `store/blob` | Object payloads as content-addressed files; forks by hardlink |
| `functions` | Builds and runs functions as child processes |
| `services/cloudfunctions` | The Cloud Functions v1 REST API |
| `services/storage` | Cloud Storage semantics: buckets, objects, generations |
| `services/pubsub` | Pub/Sub over gRPC, plus the JSON API Terraform drives |
| `lint` | Build-time invariants enforced as tests |

---

## Rules the code holds to

**Time is injected.** Nothing outside `core/clock` may call `time.Now`,
`Sleep`, `After`, `NewTimer`, `NewTicker`, `Since` or `Until`. A test in
`lint/` walks the tree and fails the build otherwise. This is what makes an ack
deadline or a retry backoff something a test drives by advancing a clock rather
than waiting out.

**Payload bytes never enter the heap.** Object content is streamed to
content-addressed files, hashed in one pass. 2 GiB moves for about 1 MiB of
allocation.

**Services do not know about each other.** A bucket knows nothing about
functions; a function knows nothing about buckets. Both know the event bus, and
that is the only thing that crosses a service boundary.

**Errors carry their own HTTP status.** Every construction site states it,
rather than inheriting a default that is right for most cases and wrong for the
one that matters.

**Compare-and-swap, never read-then-write.** The store has no unconditional
`Put`: a write states the version it expects. Concurrent writers cannot claim
the same object generation.

---

## How behaviour is verified

`test/conformance/` drives the emulator with **real clients** — the Go storage
and Pub/Sub libraries, real `gcloud`, real Terraform — not with hand-written
requests that agree with the implementation by construction. A request the
emulator answers correctly in a unit test can still be one no client sends.

```sh
make check     # build, vet, lint invariants, and the whole suite under -race
```
