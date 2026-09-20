# Snapshot and restore

Save a running emulator's state to a file, and load it back later or into
another emulator:

```sh
cloudrig snapshot state.tar     # save the running emulator's state
cloudrig restore state.tar      # load it back, replacing current state
```

Use `-` for stdout or stdin, so a snapshot can be piped:

```sh
cloudrig snapshot - | cloudrig restore -   # against two --endpoint targets
```

A snapshot is a tar of the key-value metadata plus every object payload, so it
captures store state: buckets and objects, Pub/Sub, Firestore, Cloud Tasks,
Secret Manager and the rest. Payloads are content-addressed, so restoring a
blob lands it at the address its metadata already references.

A snapshot also carries the deterministic testing state around the store:
armed faults, and a virtual clock's reading. A restored emulator fails the
same way and reads the same time as the one it came from. A real clock carries
nothing — the restoring emulator's wall clock is already the right answer.

Deployed functions stay behind: they are environment, not state, so restoring
never rebuilds or restarts a process. An in-process [fork](fork-state.md)
copies the store only. Restoring replaces the target's store contents with the
file's.

Seed an environment once, snapshot it, and each run or test case restores from
the same file for an identical starting point:

```sh
cloudrig restore fixtures/seeded.tar
```

Only an in-memory emulator can snapshot; one started with `--data-dir` already
persists on disk and is refused with a clear error.
