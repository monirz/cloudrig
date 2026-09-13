# Networking

CloudRig serves **everything on one port** (default `4599`): REST over HTTP/1.1
and gRPC over cleartext HTTP/2 (h2c), told apart by content type. The binary
binds every interface so it is reachable from a container; an in-process
`MustStart` takes a random free loopback port instead.

Different clients find the same port differently:

| Client | How it is pointed at CloudRig |
|---|---|
| `cloudrig` CLI, raw HTTP | `CLOUDRIG_ENDPOINT=http://localhost:4599` |
| Pub/Sub libraries | `PUBSUB_EMULATOR_HOST=localhost:4599` (host:port, no scheme) |
| Firestore libraries | `FIRESTORE_EMULATOR_HOST=localhost:4599` |
| Storage / Secret Manager libraries | `option.WithEndpoint(...)` |
| gcloud, Terraform | endpoint overrides from `cloudrig-env.sh` |

Cloud Run is regional, and gcloud builds its endpoint by prefixing the region
onto the host: `localhost` becomes `us-central1-localhost`, which does not
resolve. `cloudrig-env.sh` points it at `127.0.0.1.nip.io` instead, whose
wildcard DNS answers the prefixed name with `127.0.0.1`.

See [Configuration](configuration.md) for the full flag and env list.
