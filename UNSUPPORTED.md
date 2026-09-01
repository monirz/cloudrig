# Unsupported

Fields and behaviours cloudrig accepts but does not honour, and operations it
declines outright.

The rule this file exists for: unimplemented must be loud, and nothing may be
accepted-and-ignored silently without appearing here. A gap you can read is
worth more than a surface that looks complete.

## Returns 501 / `codes.Unimplemented`

Cloud Functions v1:

- `functions.create`, `functions.patch`, `functions.generateUploadUrl` — real
  gcloud uploads a source zip, which the emulator cannot accept yet. The error
  points at `cloudrig fn deploy NAME --source DIR`, which works.

Cloud Storage:

- The XML API, except the `/{bucket}/{object}` media download the Go client's
  reader requires.
- ACLs, notifications, lifecycle, CORS, retention, CSEK.
- **IAM policies are stored, never enforced.** Every request here is
  unauthenticated, so there is no identity to evaluate a binding against; an
  emulator refusing a request on a policy would be inventing an authority it
  does not have. They are stored because Terraform reads and writes them, and
  `testIamPermissions` grants whatever is asked.
- **Signed URLs are accepted but never verified**, neither signature nor
  expiry. There is no key to check a signature against, and a URL carries the
  client's wall clock while the emulator's time is injected — under a fake
  clock every real signed URL looks issued in the future. An expired URL
  therefore still works.
- `GenerateSignedPostPolicyV4` — the form-upload flow is not routed.
- `ifSource*` preconditions on copy and compose. They return 501 naming the
  parameter rather than being ignored: a condition silently dropped is worse
  than one refused.
- Resumable sessions live in memory and in a temp directory, so one interrupted
  by a restart cannot be resumed. Real GCS keeps a session usable for a week.
- Any chunk size is accepted. Real GCS requires non-final chunks to be a
  multiple of 256 KiB.

Event triggers:

- Cloud Storage events only. Pub/Sub, Firestore and Eventarc triggers need
  those services first.
- The first-generation envelope only. CloudEvents arrives with gen2, which
  needs an identity-token issuer.
- Delivery is at-least-once within one process and not durable: an event
  published while the emulator stops is gone.

Pub/Sub:

- Push subscriptions, ordering keys, dead-letter topics, retry policies,
  snapshots, seek and schemas.
- **Pull is implemented but the Go client never calls it.** Subscriber.Receive
  opens a StreamingPull, so that is the path that matters; Pull exists for
  other clients.
- Ack deadlines expire through the injected clock, so a test drives
  redelivery by advancing time. Under a FakeClock nothing expires on its own,
  which is the point: delivery stays deterministic.
- Messages are held in memory and are not persisted by --data-dir.
- A --trigger-topic function fires on the publish, not through a subscription:
  it sees every message on the topic and has no backlog, no ack and no
  redelivery of its own. Real GCF creates a push subscription, so a function
  that is down there misses nothing; here it does.

Pub/Sub REST: only what Terraform drives — create, get, list, patch and delete
for topics and subscriptions. No publish, pull or acknowledge over JSON; use
gRPC for those.

gRPC: everything except Pub/Sub.

Firestore:

- `Listen` (real-time updates), query cursors (`StartAt`/`EndAt`),
  collection-group queries, and aggregation queries.
- A transaction is serialised by the commit lock, not multi-version
  concurrency. Real Firestore aborts a transaction whose reads were
  invalidated; that abort never happens here, so a test written to observe
  contention will pass here and fail in production.

Cloud Run:

- Building an image from source (`gcloud run deploy --source`). An image runs
  as a container; a source directory runs as a process, which is a convenience
  rather than an emulation of Cloud Run.
- A source directory cannot be deployed over the API, only through the Go
  library: the image field is request-controlled, and reading a host path out
  of it would let any client that reaches the port run a program on this
  machine.
- Traffic splitting between revisions. One revision exists at a time, so a
  deploy replaces what was running and there is nothing to roll back to.
- Jobs, and authenticated invocation — every IAM permission asked for is
  granted, because there is no authentication here to enforce.

## Accepted but ignored

- **v2 list `filter`** — only the `environment=` term is understood. Any other
  filter matches everything, which returns too much rather than silently hiding
  a function.
- **`pageSize` and `pageToken`** on every list — all results are returned in one
  page. An emulator holds few enough functions that paging would only be
  ceremony.
- **Bucket `location` and `storageClass`** — stored and echoed back, but nothing
  behaves differently for them.
- **`projection`, `fields`, `userProject`** on storage requests.
- **Cloud Run `--concurrency` and `--max-instances`** — one container is one
  container, and faking local autoscaling would teach the wrong thing.
- **Firestore `pageSize` and `pageToken`**, as for every other list.

- **v2 `generateUploadUrl`, IAM policy methods** — not routed at all; they 404
  rather than 501, because the v2 surface is deliberately read-only.

## Known divergence from real GCS

- **Functions report `GEN_1`.** They are served through the v1 API, the only
  generation with an invoke method. Reporting `GEN_2` would make gcloud mint an
  identity token and call a Cloud Run URL that does not exist.
- **`gcloud functions deploy` does not work**, by the above. Deploy with
  `cloudrig fn deploy`; everything else is then reachable from gcloud.

- **Bucket names are globally unique**, as in GCS, so a name taken in one
  project is taken everywhere. Per-test isolation comes from each MustStart
  being its own emulator rather than from partitioning names by project.
- **A scoped reset leaves blobs behind.** They are content-addressed and shared,
  so knowing which became unreferenced would need refcounting nothing else
  wants. A full reset removes them.
- **Only Cloud Storage is persisted.** `--data-dir` keeps buckets and objects;
  deployed functions live in memory and go with the process, since their source
  is on disk already and redeploying is a command.
- **Versioning is honoured, and off by default.** Without it an overwrite
  discards the superseded generation and a delete removes it, both metadata and
  content, as GCS does. With `versioning.enabled` both are kept and readable by
  explicit generation. Content is reference-counted because identical bytes are
  stored once and shared.

- **Orphaned generations.** A writer that loses an `ifGenerationMatch` CAS has
  already written its object metadata and blob. In M1 those are left as garbage
  and reclaimed only by `Reset`. Real GCS leaves nothing behind. See PLAN.md.
