# CLI reference

## Commands

```
cloudrig start [--host H] [--port N] [--runner MODE] [--data-dir DIR]

cloudrig fn deploy <name> --source DIR [--runtime R] [--entry-point F]
                          [--project P] [--region L] [--watch]
                          [--trigger-bucket B] [--trigger-topic T]
                          [--trigger-event E]
cloudrig fn invoke <name> [--data JSON]
cloudrig fn logs   <name> [-f]
cloudrig fn list | describe <name> | delete <name>
cloudrig fn run <dir>      # starts its own emulator, no daemon

cloudrig snapshot <file>   # save running state ("-" for stdout)
cloudrig restore <file>    # load state back ("-" for stdin)
```

- `--runtime` is detected from the source: `package.json` → nodejs20, `go.mod`
  → go. Accepts `go121`-`go125`, `nodejs18/20/22`.
- `--entry-point` is detected for Go when the package has one exported
  `func(http.ResponseWriter, *http.Request)`. Node must be told.
- `--watch` redeploys when the source changes; a build failure leaves the
  previous version serving.
- `--data-dir` persists Cloud Storage across restarts. Without it, everything
  is in memory.
- Every flag has a `CLOUDRIG_` environment twin.

---

## Test

```sh
make check              # build, vet, lint, gofmt, race — about 40s
make vuln               # govulncheck, call paths only
go test -short ./...    # skip the slow ones — about 6s

go test -v ./test/conformance/          # the real cloud.google.com/go client
go test -v ./services/storage/          # GCS semantics
go test -v ./services/cloudfunctions/   # the gcloud API
go test -v ./functions/                 # the function runner
```

`TestGcloudCompatibility` drives the real gcloud binary, and skips itself when
gcloud is not installed.

---

## Troubleshooting

A stray `cloudrig start` will silently accept deploys meant for a new one:

```sh
pkill -f 'cloudrig start'
```

`cloudrig fn invoke` reporting "does not exist" usually means a project
mismatch — the error names where the function actually is.
