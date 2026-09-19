<p align="center">
  <img src="assets/logo.png" alt="CloudRig" width="200">
</p>

# CloudRig

### A local Google Cloud environment for realistic, deterministic integration testing.

CloudRig gives you a connected GCP environment on your machine. Your real
application code and GCP clients interact with realistic local services, while
tests can deterministically control time, failures, and state.

```text
                         CloudRig
                            │
       ┌────────────────────┼────────────────────┐
       │                    │                    │
       ▼                    ▼                    ▼
   GCP Services         Real Workloads       Test Controls
       │                    │                    │
       │                    │              ┌─────┼─────┐
       │                    │              ▼     ▼     ▼
 Storage / PubSub       Functions/GKE    Time  Fault  Fork
 Firestore / Tasks      Cloud Run
 Scheduler / Secrets
       │
       └────────── Connected Event-Driven ──────────┘
```

<p align="center">
  <a href="https://github.com/monirz/cloudrig/actions/workflows/ci.yml"><img src="https://github.com/monirz/cloudrig/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://codecov.io/gh/monirz/cloudrig"><img src="https://codecov.io/gh/monirz/cloudrig/branch/main/graph/badge.svg" alt="Coverage"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/monirz/cloudrig"><img src="https://api.scorecard.dev/projects/github.com/monirz/cloudrig/badge" alt="OpenSSF Scorecard"></a>
  <a href="https://pkg.go.dev/github.com/monirz/cloudrig"><img src="https://pkg.go.dev/badge/github.com/monirz/cloudrig.svg" alt="Go Reference"></a>
  <a href="https://github.com/monirz/cloudrig/releases"><img src="https://img.shields.io/github/v/release/monirz/cloudrig" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/monirz/cloudrig" alt="License"></a>
</p>

---

## Test controls

Provoke the failures, delays, and starting conditions a test needs, all against
one live pipeline:

<p align="center">
  <img src="assets/test-controls.svg" alt="One pipeline (app to function to Pub/Sub to Cloud Task) with three controls acting on it: fault injection fails the Pub/Sub publish, time travel fast-forwards the clock to fire the task retries, and snapshot/restore saves and reloads the whole state." width="820">
</p>

```sh
cloudrig fault pubsub --failure-rate 0.2   # make a dependency flake, prove your retries hold
cloudrig clock advance 1h                  # fire every scheduled job, retry and TTL now
cloudrig snapshot seed.tar                 # save a seeded world; restore it before each test
```

More: [fault injection](docs/fault-injection.md), [time travel](docs/time-travel.md),
and [snapshot and restore](docs/snapshot.md).

---

## Install

Pick one. All three put a `cloudrig` binary on your PATH.

**Prebuilt binary (no Go needed).** Downloads the release for your OS and CPU,
on macOS and Linux:

```sh
VERSION=v0.1.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')                 # darwin or linux
ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
curl -sSL "https://github.com/monirz/cloudrig/releases/download/$VERSION/cloudrig_${VERSION#v}_${OS}_${ARCH}.tar.gz" | tar -xz cloudrig
sudo mv cloudrig /usr/local/bin/
```

On macOS the binary is unsigned, so clear the quarantine flag once:
`xattr -d com.apple.quarantine /usr/local/bin/cloudrig`.

**With Go (1.25 or newer):**

```sh
go install github.com/monirz/cloudrig/cmd/cloudrig@latest
```

**From source:**

```sh
git clone https://github.com/monirz/cloudrig && cd cloudrig
make build              # produces ./cloudrig
```

---

## Quick Start

Start CloudRig and point `gcloud` at your local GCP:

```sh
cloudrig start &
curl -sSO https://raw.githubusercontent.com/monirz/cloudrig/main/cloudrig-env.sh
. ./cloudrig-env.sh          # points gcloud at CloudRig, with no credentials
```

If you installed from source, `cloudrig-env.sh` is already in the checkout, so
skip the `curl` line.

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

---

## See It in Action

One workflow, end to end: an object landing in a bucket triggers a function that
publishes to Pub/Sub, a worker enqueues a Cloud Task, and fast-forwarding the
clock fires its retries. Run the whole thing with
[`examples/pipeline/run.sh`](examples/pipeline/run.sh), or step through it:

```sh
# Start CloudRig with a virtual clock. It points functions at its own services
# automatically; SINK_URL is this app's own setting.
SINK_URL=http://localhost:8080/ cloudrig start --clock virtual &
. ./cloudrig-env.sh

# Provision a bucket, a Pub/Sub topic, and a task queue.
gcloud storage buckets create gs://intake
curl -X PUT localhost:4599/v1/projects/cloudrig-local/topics/orders -d '{}'
gcloud tasks queues create pipeline --location=us-central1 --max-attempts=3

# Deploy the pipeline: ingest (storage -> Pub/Sub), worker (Pub/Sub -> Cloud Task).
cloudrig fn deploy ingest --source ./examples/pipeline/ingest --trigger-bucket intake
cloudrig fn deploy worker --source ./examples/pipeline/worker --trigger-topic orders

# Drop an object in. It flows through the whole chain and enqueues a task.
echo '{"id":42}' > order.json
gcloud storage cp order.json gs://intake/

# Fast-forward time. The task fires and retries, deterministically.
cloudrig clock advance 1h
#   sink: task attempt      <- the retries, with no real waiting
#   sink: task attempt
```

Every stage is real: the functions run as processes, Pub/Sub and Cloud Tasks are
live, and the clock is injected so the retries need no real waiting.

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
- [Snapshot and Restore](docs/snapshot.md): save a running emulator's state to a file and load it back.
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
- [Snapshot and Restore](docs/snapshot.md)
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

## Support

If CloudRig saves you time, you can buy me a coffee. Recent messages appear
below, updated automatically.

<!--START_SECTION:buy-me-a-coffee-->
<!--END_SECTION:buy-me-a-coffe-->

---

## License

MIT. See [LICENSE](LICENSE).
