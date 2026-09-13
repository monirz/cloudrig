# CloudRig

### A local Google Cloud environment for realistic, deterministic integration testing.

CloudRig is a local Google Cloud emulator for developing and testing cloud
applications without touching real GCP. Unlike a box of isolated emulators, its
services are wired together the way GCP wires them: upload a file to a bucket and
the function deployed against it fires; a scheduled job publishes to Pub/Sub and
triggers another. You build and test event-driven apps locally, then control the
things real GCP makes hard: **time, failure, and state.**

![License: MIT](https://img.shields.io/badge/license-MIT-blue)
![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8)

> **Build locally. Test realistically. Break things on purpose.**

---

# Why CloudRig?

### [Real Cloud Functions](docs/services.md#run-a-function)

Run your actual functions locally. Cloud Storage and
Pub/Sub events trigger them and run real application code:

```sh
cloudrig fn deploy on-upload --source ./examples/on-upload --trigger-bucket uploads
curl -X POST "localhost:4599/upload/storage/v1/b/uploads/o?uploadType=media&name=report.csv" \
  -H 'Content-Type: text/csv' --data 'a,b,c'
cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (5 bytes)
```

No Docker, no daemon, no polling: one process wired the storage write to the
function.

### [Connected, event-driven services](docs/services.md#upload-a-file-run-a-function)

Services are wired together like a real GCP
environment, so a whole workflow runs locally instead of you stitching separate
emulators together:

```text
Storage → Function → Pub/Sub → Function → Cloud Tasks → Function
```

### [GKE workloads](docs/services.md#gke)

Run real Kubernetes workloads (k3d/kind) alongside your local
GCP services and test how they talk to Google Cloud APIs:

```sh
gcloud container clusters create demo --location=us-central1
kubectl create deployment web --image=nginx
```

### [Terraform / OpenTofu](docs/services.md#terraform)

Provision your local environment with the same
infrastructure-as-code workflow you use in production:

```sh
terraform -chdir=examples/terraform/services apply
```

### [Time travel](docs/time-travel.md)

Fast-forward minutes, days or months without waiting for real
time. Scheduled jobs, retries, deadlines and TTLs fire deterministically:

```sh
cloudrig clock advance 7d
```

### [Fault injection](docs/fault-injection.md)

Break your infrastructure on purpose. Inject errors, latency
and timeouts, over REST and gRPC, to test retries and resilience:

```sh
cloudrig fault pubsub --error 503
```

### [Fork state](docs/fork-state.md)

Seed an environment once, then fork it into isolated states for
different tests or scenarios:

```go
emu := base.Fork(t)
```

### [In-process testing](docs/in-process-testing.md)

Start an isolated CloudRig directly inside your Go tests,
with no separate emulator process or shared infrastructure:

```go
emu := cloudrig.MustStart(t)
```

### [Real GCP clients](docs/services.md#use-it-from-gcloud)

Point the Google Cloud SDKs, gcloud and Terraform at your
local environment with no code changes:

```sh
export PUBSUB_EMULATOR_HOST=localhost:4599
```

---

# Install

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

Prebuilt binaries and a Homebrew tap are planned.

---

# How to Use

**1. Start CloudRig.**

```sh
cloudrig start        # serves everything on :4599
```

**2. Point your tools at it.** No code changes; the client libraries and gcloud
just need an endpoint:

```sh
export CLOUDRIG_ENDPOINT=http://localhost:4599    # cloudrig CLI and HTTP calls
export PUBSUB_EMULATOR_HOST=localhost:4599        # Pub/Sub client libraries
export FIRESTORE_EMULATOR_HOST=localhost:4599     # Firestore client libraries
. ./cloudrig-env.sh                               # gcloud and Terraform (from a checkout)
```

**3. Use any service** as you would in production:

```sh
gcloud storage buckets create gs://my-bucket
gcloud storage cp report.csv gs://my-bucket/
```

**4. Test the hard things.** Inject failures on any server; start with
`--clock virtual` to also fast-forward time:

```sh
cloudrig fault storage --latency 2s
cloudrig clock advance 7d
```

Full walkthroughs are in the [service guides](docs/services.md) and the
[CLI reference](docs/cli.md).

---

# Supported services

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

# Documentation

**Guides**
- [Service guides](docs/services.md): Storage, Pub/Sub, Firestore, Secret Manager, Tasks, Scheduler, Cloud Run, GKE, Terraform, and the gcloud flows
- [CLI reference](docs/cli.md): every subcommand and flag

**Testing**
- [Time travel](docs/time-travel.md) · [Fault injection](docs/fault-injection.md) · [Fork state](docs/fork-state.md) · [In-process testing](docs/in-process-testing.md)

**Project**
- [Architecture](ARCHITECTURE.md): how CloudRig is put together
- [Unsupported](UNSUPPORTED.md): every gap, in one place

---

# Development

CloudRig is written in Go, with no runtime dependencies for its in-process
services (Cloud Run and GKE need a container runtime).

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build              # -> ./cloudrig
make check              # build, vet, lint, gofmt, tests
```

Contributions, bug reports and ideas are welcome.

---

# License

MIT. See [LICENSE](LICENSE).
