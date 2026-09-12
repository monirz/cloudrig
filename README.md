# CloudRig

### A local Google Cloud environment for realistic, deterministic integration testing.

CloudRig runs Google Cloud services on your machine as a **connected
environment**, not a box of isolated emulators. Upload a file and the function
deployed against it fires; a scheduled job publishes to Pub/Sub and triggers
another function. Build and test event-driven apps locally, then control the
things real GCP makes hard: **time, failure, and state.**

![License: MIT](https://img.shields.io/badge/license-MIT-blue)
![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8)

> **Build locally. Test realistically. Break things on purpose.**

---

## Why CloudRig?

**⚡ Real Cloud Functions.** Run your actual functions locally. Cloud Storage and
Pub/Sub events trigger them and run real application code:

```bash
./cloudrig start &     # serves everything on :4599

./cloudrig fn deploy on-upload --source ./examples/on-upload --trigger-bucket uploads

curl -X POST "localhost:4599/storage/v1/b?project=demo" -d '{"name":"uploads"}'
curl -X POST "localhost:4599/upload/storage/v1/b/uploads/o?uploadType=media&name=report.csv" \
  -H 'Content-Type: text/csv' --data 'a,b,c'

./cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (5 bytes)
```

No Docker, no daemon, no polling: one process wired the storage write to the
function.

**🔄 Connected, event-driven services.** Services are wired together like a real
GCP environment. Upload a file to a bucket and the function deployed against it
fires, in the same local environment. Chain Storage → Functions → Pub/Sub →
Tasks → Functions and test the whole workflow locally.

**☸️ GKE workloads.** Run Kubernetes workloads (k3d/kind) alongside your local
GCP services and test how they interact with Google Cloud APIs.

**🏗️ Terraform / OpenTofu.** Provision your local environment using the same
infrastructure-as-code workflow you use in production.

**⏩ Time travel.** Fast-forward minutes, days or months without waiting for real
time. Test scheduled jobs, task retries, deadlines, TTLs and other
time-dependent behaviour deterministically.

**💥 Fault injection.** Break your infrastructure on purpose. Inject errors,
latency, timeouts and transient failures, over REST and gRPC, to test retries,
resilience and failure handling.

**🌿 Fork state.** Create an environment once, then fork it into isolated states
for different tests, scenarios or experiments.

**🧪 In-process testing.** Start an isolated CloudRig environment directly inside
your Go tests, with no separate emulator process or shared infrastructure.

**🔌 Real GCP clients.** Use the Google Cloud SDKs, gcloud, Terraform and the
familiar GCP APIs against your local environment.

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
- [Service guides](docs/services.md): Storage, Pub/Sub, Firestore, Secret Manager, Tasks, Scheduler, Cloud Run, GKE, Terraform, and the gcloud flows
- [CLI reference](docs/cli.md): every subcommand and flag

**Testing**
- [Time travel](docs/time-travel.md) · [Fault injection](docs/fault-injection.md) · [Fork state](docs/fork-state.md) · [In-process testing](docs/in-process-testing.md)

**Project**
- [Architecture](ARCHITECTURE.md): how CloudRig is put together
- [Unsupported](UNSUPPORTED.md): every gap, in one place

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
