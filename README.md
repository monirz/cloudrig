# CloudRig

### A local Google Cloud environment for realistic, deterministic integration testing.

CloudRig runs Google Cloud services on your machine as a **connected
environment** — not a box of isolated emulators. Upload a file and the function
deployed against it fires; a scheduled job publishes to Pub/Sub and triggers
another function. Build and test event-driven apps locally, then control the
things real GCP makes hard: **time, failure, and state.**

![License: MIT](https://img.shields.io/badge/license-MIT-blue)
![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8)

> **Build locally. Test realistically. Break things on purpose.**

---

## Run a Cloud Function — for real

Deploy a function, trigger it by writing a file to a bucket, and read what it
printed. It is compiled and run as a real process — the log line is its actual
stdout, not a canned response:

```sh
./cloudrig start &     # serves everything on :4599

./cloudrig fn deploy on-upload --source ./examples/on-upload --trigger-bucket uploads

curl -X POST "localhost:4599/storage/v1/b?project=demo" -d '{"name":"uploads"}'
curl -X POST "localhost:4599/upload/storage/v1/b/uploads/o?uploadType=media&name=report.csv" \
  -H 'Content-Type: text/csv' --data 'a,b,c'

./cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (5 bytes)
```

No Docker, no Pub/Sub daemon, no polling — one process wired the storage write
to the function. → [Full walkthrough](docs/services.md#upload-a-file-run-a-function)

---

## Install

Requires Go 1.25+.

```sh
# with Go
go install github.com/monirz/cloudrig/cmd/cloudrig@latest

# or from source
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build          # -> ./cloudrig
```

Then start it and point your tools at the one endpoint:

```sh
cloudrig start                                # :4599
export CLOUDRIG_ENDPOINT=http://localhost:4599
```

Prebuilt binaries and a Homebrew tap are planned. → [Getting started](docs/services.md)

---

## Why CloudRig?

**Real Cloud Functions.** Your function runs as an actual process and is
triggered by the services it depends on — not mocked.

**Connected, event-driven services.** Services are wired together like real GCP,
so whole workflows run locally instead of you stitching separate emulators
yourself:

```text
Cloud Storage ──(object created)──► Function ──► Firestore
                                        └──► Pub/Sub ──► Function ──► Cloud Tasks ──► Function
```

**Real GCP clients.** `gcloud`, Terraform/OpenTofu and the Google client
libraries work unchanged, over gRPC and REST on a single port.

**GKE + Terraform.** Provision your local environment with infrastructure-as-code,
and run real Kubernetes workloads (k3d/kind) alongside your GCP services.

---

## Built for testing

CloudRig gives you control over what is slow or impossible to reproduce against
real infrastructure.

**⏩ [Time travel](docs/time-travel.md)** — fast-forward minutes, days or months
without waiting. Scheduled jobs, retries, backoff, ack deadlines and TTLs all
fire instantly and deterministically.

```sh
cloudrig start --clock virtual
cloudrig clock advance 7d      # everything due in those 7 days runs now
```

**💥 [Fault injection](docs/fault-injection.md)** — break your local
infrastructure on purpose to test retries, backoff and circuit breakers. Errors,
latency and timeouts, over REST *and* gRPC.

```sh
cloudrig fault pubsub --error 503
cloudrig fault tasks  --failure-rate 20%
```

**🌿 [Fork state](docs/fork-state.md)** — seed an environment once, then branch
it into isolated scenarios per test, cheaply.

**🧪 [In-process testing](docs/in-process-testing.md)** — run CloudRig directly
inside a Go test, one isolated instance per test:

```go
emu := cloudrig.MustStart(t)
```

The combination is the point: fast-forward 30 days, make Pub/Sub fail half the
time, and see whether your retries actually hold — deterministically.

---

## Supported services

| Service | Status | Notes |
|---|:--:|---|
| Cloud Functions | ✅ | Real local execution (Go & Node), HTTP & event triggers |
| Cloud Storage | ✅ | Buckets, objects, uploads, versioning, signed URLs |
| Pub/Sub | ✅ | Publish, streaming pull, ack/nack, redelivery |
| Firestore | ✅ | Documents, queries, transactions, over gRPC |
| Cloud Tasks | ✅ | Deferred HTTP work, dispatched on the clock |
| Cloud Scheduler | ✅ | Cron jobs firing HTTP or Pub/Sub, on the clock |
| Secret Manager | ✅ | Secrets, versions, the `latest` alias |
| Cloud Logging | ✅ | Write and read structured entries with a filter |
| Service Usage | ✅ | `gcloud services enable/disable/list` |
| Cloud Run | ✅ | Runs a real container (needs Docker) |
| GKE | ✅ | Real local Kubernetes (needs k3d or kind) |

Full compatibility, supported APIs and limitations:
→ [Service guides](docs/services.md) · [What's not supported](UNSUPPORTED.md)

---

## Documentation

**Guides**
- [Service guides](docs/services.md) — Storage, Pub/Sub, Firestore, Secret Manager, Tasks, Scheduler, Cloud Run, GKE, Terraform, and the gcloud flows
- [CLI reference](docs/cli.md) — every subcommand and flag

**Testing**
- [Time travel](docs/time-travel.md) · [Fault injection](docs/fault-injection.md) · [Fork state](docs/fork-state.md) · [In-process testing](docs/in-process-testing.md)

**Project**
- [Architecture](ARCHITECTURE.md) — how CloudRig is put together
- [Unsupported](UNSUPPORTED.md) — every gap, in one place

---

## Development

CloudRig is written in Go, with no runtime dependencies for its in-process
services (Cloud Run and GKE need a container runtime).

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build              # -> ./cloudrig
make check              # build, vet, lint, gofmt, tests
```

Contributions, bug reports and ideas are welcome.

---

## License

MIT. See [LICENSE](LICENSE).
