# Fault injection

Break your local GCP on purpose. Inject latency, errors, timeouts and transient
failures — over **REST and gRPC** — to test how your app behaves when a service
fails, without touching real infrastructure.

**From the CLI**, against a running server:

```sh
cloudrig fault pubsub  --error 500          # gRPC: returned as INTERNAL
cloudrig fault tasks   --latency 2s         # slow, then succeeds
cloudrig fault storage --failure-rate 20%   # fail 1 in 5, deterministically
cloudrig fault gke     --timeout            # 504 / DEADLINE_EXCEEDED
cloudrig fault list
cloudrig fault clear
```

A named service maps to the path its requests carry: gRPC-first services
(`pubsub`, `firestore`, `tasks`, `scheduler`, `secretmanager`, `gke`, `logging`)
match the gRPC method a client library sends; `storage` matches its REST prefix.
Use `--path <prefix>` for anything else. Two things worth knowing:

- **gRPC faults are real gRPC statuses.** An `--error 500` becomes `INTERNAL`,
  `--timeout` becomes `DEADLINE_EXCEEDED` — the code mapped from the HTTP status,
  never an HTTP number smuggled over gRPC.
- **`--failure-rate` is deterministic.** 20% fails exactly one in five matching
  requests, in a fixed pattern — so a fault composes with time travel into a
  reproducible test rather than a coin flip. A bare `--latency` (no error) slows
  the request and then lets it succeed.

**In a Go test**, arm rules directly:

```go
emu := cloudrig.MustStart(t)

emu.Faults().Add(faults.Rule{
    Path:   "/storage/v1/*",   // trailing * is a prefix
    Status: http.StatusTooManyRequests,
    Count:  1,                 // fail once, then let the retry through
})
```

`Count: 1` is the useful one: the first call fails, the client retries, the
second succeeds — which proves the retry happened rather than assuming it.

| Field | Meaning |
|---|---|
| `Method` | HTTP method, or empty for any |
| `Path` | Escaped path or gRPC method; a trailing `*` makes it a prefix; empty matches all |
| `Status` | HTTP status to answer with (default 503); gRPC maps it to a canonical code |
| `Code` | Canonical error code (default: derived from `Status`) |
| `Message` | Error text |
| `Latency` | Delay before responding, on the injected clock |
| `Count` | How many requests to fail; zero means every one |
| `Rate` | Fraction of requests to fail, 0–1, applied deterministically |

`emu.Faults().Clear()` disarms everything. `/_emu/` is never faulted, so a
match-everything rule cannot lock a test out of its own controls. `Latency` runs
on the emulator's clock: under a `FakeClock` the request waits until the test
advances time, so a slow backend is something to assert on rather than sit
through.

### Time travel + fault injection

The combination is the point. Fast-forward 30 days, make Pub/Sub fail half the
time, and watch whether your retries and dead-letter handling actually hold —
deterministically, in milliseconds:

```sh
cloudrig fault pubsub --error 503 --failure-rate 50%
cloudrig clock advance 30d
```
