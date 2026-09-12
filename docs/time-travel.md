# Time travel

Test time-dependent GCP workloads without waiting. Fast-forward minutes, days or
months and let scheduled jobs, task retries, message deadlines, TTLs and other
time-dependent behaviour fire — without waiting for real time to pass.

```sh
# Start cloudrig with a virtual clock
cloudrig start --clock virtual

# Travel 7 days into the future
cloudrig clock advance 7d
```

Instead of waiting 7 days, cloudrig immediately processes everything that became
due during those 7 days. Time is injected, not read from the wall clock, so this
is deterministic — no flaky `time.Sleep` in sight.

**In a Go test** (`MustStart` already runs on a virtual clock):

```go
emu := cloudrig.MustStart(t)
// ... create a Cloud Scheduler job due in an hour ...
emu.FakeClock(t).Advance(time.Hour) // the job fires now, deterministically
```

**From the CLI**, against a server started with a virtual clock:

```sh
./cloudrig start --clock virtual                       # time freezes at startup
# reproducible runs: pin where time starts
./cloudrig start --clock virtual --clock-start 2026-01-01T00:00:00Z

# in another shell
cloudrig clock                         # clock: virtual  now: …  pending: 3
cloudrig clock advance 7d              # fire everything due in the next 7 days
cloudrig clock goto 2026-12-25T00:00:00Z
```

Time only moves **forward** — a fired timer cannot un-fire, so travelling to a
past time is refused. Without `--clock virtual` the server tracks the real clock
and `cloudrig clock advance` says so rather than pretending. `pending` is how
many timers are still waiting for the clock to reach them.
