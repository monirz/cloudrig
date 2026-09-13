# Event-driven pipeline

One CloudRig workflow, end to end:

```
object -> ingest function -> Pub/Sub -> worker function -> Cloud Task -> retry
```

- **ingest/** is a Storage-triggered function. On a new object it publishes to
  the `orders` Pub/Sub topic (through `PUBSUB_EMULATOR_HOST`).
- **worker/** is a Pub/Sub-triggered function. It enqueues a Cloud Task (through
  `CLOUDRIG_ENDPOINT`) that hits `SINK_URL`.

The task target returns 500, so advancing the clock fires its retries, which is
the point: time-dependent behaviour runs in milliseconds and deterministically.

## Run it

From the repo root, with `cloudrig` on your PATH:

```sh
./examples/pipeline/run.sh
```

The functions reach CloudRig's own services because it is started with
`PUBSUB_EMULATOR_HOST`, `CLOUDRIG_ENDPOINT` and `SINK_URL` in its environment,
which the deployed functions inherit.
