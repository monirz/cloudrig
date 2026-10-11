# Replay a CI failure

An integration test fails in CI and passes on your laptop. Usually the
difference is the environment: data the job seeded, a dependency that
happened to be slow, the time of day. With CloudRig, all of that is one file.
Take a snapshot before the tests run, upload it when they fail, and restore it
locally to get the same failure.

[`examples/ci-replay`](../examples/ci-replay) is a complete case: an order
service that saves to Cloud Storage and then publishes to Pub/Sub. With
Publish made slow for one call in five, orders 5 and 10 are saved but never
announced, on every run, in CI and on a laptop.

## In CI

Start CloudRig, set up the conditions under test, snapshot, then test:

```yaml
- name: Start CloudRig
  run: |
    go install github.com/monirz/cloudrig/cmd/cloudrig@latest
    cloudrig start &
    until curl -sf http://localhost:4599/_emu/health >/dev/null; do sleep 0.2; done

- name: Arm faults and save the starting state
  run: |
    cloudrig fault pubsub --path /google.pubsub.v1.Publisher/Publish --latency 3s --failure-rate 20%
    cloudrig snapshot start.tar

- name: Test
  env:
    STORAGE_EMULATOR_HOST: localhost:4599
    PUBSUB_EMULATOR_HOST: localhost:4599
  run: go test ./...

- name: Upload the starting state
  if: failure()
  uses: actions/upload-artifact@v4
  with:
    name: cloudrig-start
    path: start.tar
```

Seed data goes before the snapshot too, so it travels with it. This
repository runs the same steps against the example in
[`.github/workflows/ci-replay.yml`](../.github/workflows/ci-replay.yml),
started by hand from the Actions tab.

## On your laptop

```sh
gh run download <run-id> -n cloudrig-start
cloudrig start &
cloudrig restore start.tar
# restored
cloudrig fault list
# /google.pubsub.v1.Publisher/Publish → latency 3s, 20% of requests  (/google.pubsub.v1.Publisher/Publish)

go test ./...
# --- FAIL: TestEveryOrderIsAnnounced
#     orders_test.go:72: order-10 is saved but was never announced
#     orders_test.go:72: order-5 is saved but was never announced
```

## Why snapshot before the tests

A snapshot taken after a failure holds what the failure left behind, which is
useful for inspecting but is not a starting point: rerunning against it runs
into leftover state. A fault's `--failure-rate` pattern also starts again on
restore, so only a snapshot taken before the first request lines it up with
the run it came from.

To inspect the wreckage as well, snapshot again in a step with
`if: failure()` and upload both files.

## What replays and what doesn't

A snapshot carries the store (buckets and objects, topics and subscriptions,
Firestore, tasks, secrets), armed faults with the firings each has left, and a
virtual clock's reading. See [snapshot and restore](snapshot.md).

It does not carry:

- **Undelivered Pub/Sub messages.** They are held in memory, outside the
  store. Publish seed messages in the test, not before the snapshot.
- **Deployed functions.** They are environment, not state. Deploy them the
  same way in CI and locally.
- **Wall-clock time.** Under the default real clock, code that reads the time
  sees a different one on replay. Start with `cloudrig start --clock virtual`
  if the test depends on time; the clock's reading then travels.
- **Request order across goroutines.** A `--failure-rate` fault fails every
  Nth matching request. If the code under test sends those requests
  concurrently, which request is Nth can differ between runs. `--count`
  (fail the first N) does not depend on order.

An emulator started with `--data-dir` cannot snapshot; run CI against an
in-memory one.
