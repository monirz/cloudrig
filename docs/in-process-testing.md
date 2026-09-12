# In-process testing

No Docker, no daemon, one isolated instance per test:

```go
func TestUpload(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t)

	emu.BaseURL()                        // http://127.0.0.1:53412
	emu.FakeClock(t).Advance(time.Hour)  // deterministic, never sleeps
}
```

With a function, built and served in-process:

```go
emu := cloudrig.MustStart(t, cloudrig.Options{
	Functions: []functions.Function{{
		Name: "hello", Source: "./examples/hello", EntryPoint: "HelloHTTP",
	}},
})

http.Get(emu.FunctionURL("hello") + "?name=monir")
```

Deploying into a running instance, and waiting for an event to be delivered:

```go
emu.Functions().Deploy(ctx, functions.Function{...})
emu.SyncEvents()
```

`SyncTasks` is the Cloud Tasks equivalent: a task due now dispatches on its own
goroutine, so `SyncTasks` waits for that before you advance the clock for a
scheduled retry. (Scheduled tasks need no Sync — a timer fires synchronously
inside `Advance`.)

`SyncEvents` waits for delivery — the handler has run and answered. It does
**not** wait for the handler's output: a function is a child process whose
stdout is drained by another goroutine, so a log line can arrive shortly
after. Assert on a function's log by polling for what you expect, not by
reading it once.

Shutdown is registered with `t.Cleanup`. State is never persisted under
`MustStart`.
