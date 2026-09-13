# Roadmap

CloudRig is actively evolving. Rough direction, not commitments.

**Distribution**
- Prebuilt binaries and a Homebrew tap, via GoReleaser on tagged releases.

**Services**
- More Google Cloud services (candidates: Managed Kafka, BigQuery, Cloud SQL).
- Pub/Sub push subscriptions, dead-letter topics and ordering keys.
- Second-generation (CloudEvents) function triggers.
- GKE node pools and a `get-credentials` bridge so gcloud's kubeconfig works.

**Testing**
- Streaming-RPC fault injection (today faults cover unary gRPC and REST).
- Snapshot/restore of a running environment from the CLI.

See [UNSUPPORTED.md](UNSUPPORTED.md) for the current gaps, and open an issue to
propose or upvote anything here.
