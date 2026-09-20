# Configuration

`cloudrig start` takes a few flags, and every flag has a `CLOUDRIG_` environment
twin (an explicit flag wins).

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--host H` | `CLOUDRIG_HOST` | `127.0.0.1` | Address to listen on. Empty means every interface: there is no auth, so only on a network you trust. |
| `--port N` | `CLOUDRIG_PORT` | `4599` | Port to serve everything on. `0` picks a free one. |
| `--runner MODE` | `CLOUDRIG_RUNNER` | `auto` | Function runner: `auto`, `subprocess` or `none`. |
| `--data-dir DIR` | `CLOUDRIG_DATA_DIR` | (memory) | Persist Cloud Storage across restarts. |
| `--clock MODE` | `CLOUDRIG_CLOCK` | `real` | `virtual` freezes time so you can travel it. |
| `--clock-start T` | `CLOUDRIG_CLOCK_START` | now | RFC3339 seed for the virtual clock. |

```sh
cloudrig start --port 8080 --clock virtual --clock-start 2026-01-01T00:00:00Z
```

Client-side, point tools at the running server:

```sh
export CLOUDRIG_ENDPOINT=http://localhost:4599    # cloudrig CLI and HTTP calls
export PUBSUB_EMULATOR_HOST=localhost:4599        # Pub/Sub client libraries
export FIRESTORE_EMULATOR_HOST=localhost:4599     # Firestore client libraries
```

For gcloud and Terraform, source `cloudrig-env.sh`, which sets the endpoint
overrides and disables credentials. See [Networking](networking.md) for how the
overrides map to services, and the [CLI reference](cli.md) for every subcommand.
