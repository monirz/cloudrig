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

---

## Quick Start

Install CloudRig from a checkout, start it, and point `gcloud` at your local GCP
(Go 1.25+):

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
go install ./cmd/cloudrig    # puts cloudrig on your PATH

cloudrig start &
. ./cloudrig-env.sh          # points gcloud at CloudRig, with no credentials
```

Now use it exactly like the real thing:

```sh
gcloud storage buckets create gs://demo
gcloud storage cp README.md gs://demo/
gcloud storage ls gs://demo
# gs://demo/README.md
```

Point the client libraries at the same port with the standard emulator
variables:

```sh
export PUBSUB_EMULATOR_HOST=localhost:4599
export FIRESTORE_EMULATOR_HOST=localhost:4599
```

Prefer a local binary instead? `make build` produces `./cloudrig` in the repo.

---

## See It in Action

Services are wired together like real GCP. Deploy a function against a bucket,
drop a file in with `gcloud`, and the function fires, as a real process whose
stdout is the log line:

```sh
cloudrig fn deploy on-upload --source ./examples/on-upload --trigger-bucket uploads

echo "a,b,c" > report.csv
gcloud storage buckets create gs://uploads
gcloud storage cp report.csv gs://uploads/

cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (6 bytes)
```

No Docker, no Pub/Sub daemon, no polling.
→ [Full walkthrough](docs/services.md#upload-a-file-run-a-function)

---

## Why CloudRig?

- **Connected and event-driven.** A storage write triggers a function; a
  scheduler job publishes to Pub/Sub which triggers another. Whole workflows run
  locally, in one process.
- **Deterministic.** Time is injected, so a test reproduces exactly, with no
  flaky `time.Sleep`s.
- **Built for testing.** [Travel through time](docs/time-travel.md),
  [inject failures](docs/fault-injection.md), and [fork state](docs/fork-state.md).
- **Real clients.** `gcloud`, Terraform, and the Google client libraries work
  unchanged, over gRPC and REST.

---

## How To

- [Run CloudRig](docs/cli.md)
- [Use `gcloud`](docs/services.md#use-it-from-gcloud)
- [Connect a GCP client](docs/services.md#pubsub)
- [Create a Pub/Sub workflow](docs/services.md#run-a-function-on-a-pubsub-message)
- [Deploy and trigger a Function](docs/services.md#run-a-function)
- [Use Cloud Storage triggers](docs/services.md#upload-a-file-run-a-function)
- [Use Cloud Tasks](docs/services.md#cloud-tasks)
- [Use Scheduler](docs/services.md#cloud-scheduler)
- [Run GKE workloads](docs/services.md#gke)
- [Use Terraform / OpenTofu](docs/services.md#terraform)
- [Configure CloudRig](docs/configuration.md)
- [Run CloudRig in CI](docs/in-process-testing.md)

---

## Supported Services

CloudRig emulates the following services:

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

Full compatibility and limitations: [service guides](docs/services.md) ·
[what's not supported](UNSUPPORTED.md).

---

## Testing

- [Time Travel](docs/time-travel.md): fast-forward the clock; scheduled work fires instantly.
- [Fault Injection](docs/fault-injection.md): inject errors, latency, and timeouts over REST and gRPC.
- [Fork State](docs/fork-state.md): branch a seeded environment per test case.
- [In-Process Testing](docs/in-process-testing.md): one isolated emulator inside each Go test.

---

## Tutorials

- [Build your first Function](docs/services.md#run-a-function)
- [Build an event-driven application](docs/services.md#upload-a-file-run-a-function)
- [Build a Storage → Function → Pub/Sub workflow](docs/services.md#run-a-function-on-a-pubsub-message)
- [Build a complete multi-service application](docs/services.md)
- [Terraform + CloudRig](docs/services.md#terraform)
- [GKE + CloudRig](docs/services.md#gke)

---

## Guides

- [Time Travel](docs/time-travel.md)
- [Fault Injection](docs/fault-injection.md)
- [Fork State](docs/fork-state.md)
- [In-Process Testing](docs/in-process-testing.md)
- [Configuration](docs/configuration.md)
- [Authentication](docs/authentication.md)
- [Networking](docs/networking.md)

---

## Architecture

How CloudRig serves every service on one port, with an injected clock:
[ARCHITECTURE.md](ARCHITECTURE.md).

---

## CLI Reference

Every subcommand and flag: [docs/cli.md](docs/cli.md).

---

## Development

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build              # -> ./cloudrig
make check              # build, vet, lint, gofmt, tests
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for conventions.

---

## Roadmap

Planned services, distribution, and testing features: [ROADMAP.md](ROADMAP.md).

---

## Contributing

Contributions, bug reports and ideas are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT. See [LICENSE](LICENSE).
