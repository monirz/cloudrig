# Fork state

Build a fixture once, then branch it per case:

```go
base := cloudrig.MustStart(t)
seed(base)                     // buckets, objects, whatever the suite needs

t.Run("deletes it", func(t *testing.T) {
    emu := base.Fork(t)        // its own port, its own state
    ...
})
t.Run("overwrites it", func(t *testing.T) {
    emu := base.Fork(t)        // unaffected by the case above
    ...
})
```

A fork copies metadata and **hardlinks** object payloads, so branching a
hundred gigabytes of objects costs the size of the metadata. Neither side can
see the other's writes, and neither can delete the other's bytes.

What travels is store state: deployed functions, armed faults and the clock
stay behind. The fork starts with none deployed and gets its own port, event
bus and fault set. A [snapshot](snapshot.md) is wider — it carries armed
faults and a virtual clock's reading too.

Only an in-memory emulator can fork; one started with `--data-dir` cannot.
