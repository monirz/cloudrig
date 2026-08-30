# cloudrig

A local emulator for Google Cloud APIs. Runs as a binary, or in-process inside a
Go test — no Docker, no daemon.

Cloud Functions actually execute: `gcloud functions call` runs your code.

Requires Go 1.25+.

## Quick start

```sh
make build                                    # -> ./cloudrig
./cloudrig start                              # :4599, leave running
```

In a second terminal:

```sh
./cloudrig fn deploy hello \
  --source ./testdata/node-hello \
  --entry-point handler \
  --project my-project

curl "localhost:4599/us-central1-my-project/hello?name=Monir"
# Hello, Monir!
```

The Node sample needs its dependencies once: `(cd testdata/node-hello && npm i)`.

## gcloud, step by step

Complete walkthrough from an empty shell.

### 1. Build

```sh
make build
```

Produces `./cloudrig`.

### 2. Install the Node sample's dependencies

```sh
(cd testdata/node-hello && npm i)
```

Once only. `node_modules` is gitignored, so a fresh clone needs this. Skip it if
you are deploying a Go function.

### 3. Start the emulator

```sh
./cloudrig start
```

Listens on `:4599`. Leave it running; use a second terminal from here.

### 4. Deploy a function

```sh
./cloudrig fn deploy hello \
  --source ./testdata/node-hello \
  --entry-point handler \
  --project my-project
```

```
deployed projects/my-project/locations/us-central1/functions/hello (nodejs20, handler)
url: http://localhost:4599/us-central1-my-project/hello
```

`gcloud functions deploy` cannot be used here — see step 9.

### 5. Point gcloud at the emulator

```sh
export CLOUDSDK_CORE_PROJECT=my-project
. ./cloudrig-env.sh
```

`cloudrig-env.sh` exports the overrides for the four services gcloud consults —
cloudfunctions, cloudbuild, serviceusage and cloudresourcemanager — plus
`CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true`.

`CLOUDSDK_CORE_PROJECT` must match the `--project` from step 4, or gcloud looks
in the wrong project and finds nothing.

**Set all four.** Without them `gcloud functions deploy` reaches
`cloudbuild.googleapis.com`, `serviceusage.googleapis.com` and
`cloudresourcemanager.googleapis.com` on the public internet. It fails on auth
today, but with live credentials it would touch a real project.

### 6. Call the function

```sh
gcloud functions call hello --region us-central1 --data '{"name":"Monir"}'
```

```
executionId: dl242e6ly634-1
result: Hello, Monir!
```

That is your code running. No `--gen2` flag is needed: functions report
`GEN_1`, so gcloud routes the invocation to the v1 `call` method itself.

### 7. List and describe

```sh
gcloud functions list
```

```
NAME   STATE   TRIGGER       REGION       ENVIRONMENT
hello  ACTIVE  HTTP Trigger  us-central1  1st gen
```

```sh
gcloud functions describe hello --region us-central1
```

```
entryPoint: handler
httpsTrigger:
  securityLevel: SECURE_OPTIONAL
  url: http://localhost:4599/us-central1-my-project/hello
name: projects/my-project/locations/us-central1/functions/hello
runtime: nodejs20
status: ACTIVE
```

The `url` it reports really serves the function:

```sh
curl "http://localhost:4599/us-central1-my-project/hello?name=Monir"
# Hello, Monir!
```

### 8. Delete

```sh
gcloud functions delete hello --region us-central1 --quiet
```

```
Deleted [projects/my-project/locations/us-central1/functions/hello].
```

### 9. Deploying with gcloud

`gcloud functions deploy` works too — it zips the source, uploads it, and the
emulator builds and runs what arrived:

```sh
gcloud functions deploy gohello --no-gen2 --region us-central1 \
  --runtime go125 --entry-point Handler \
  --trigger-http --source ./testdata/go-hello
```

```
name: projects/my-project/locations/us-central1/functions/gohello
runtime: go125
status: ACTIVE
```

Two things to know:

- **A Go source must be its own module.** gcloud zips the directory, so an
  enclosing `go.mod` does not travel with it. Without one you get
  `has no go.mod; a Go function's source must be a module`.
- **Node dependencies are installed on deploy.** gcloud excludes
  `node_modules` from the archive, so the emulator runs `npm install` on the
  extracted source — the first deploy needs network access.

`cloudrig fn deploy` remains the faster path: it takes a local directory, so
nothing is zipped, uploaded or reinstalled.

### Without gcloud

```sh
./cloudrig fn invoke hello --data '{"name":"Monir"}'
# Hello, Monir!
```

### Calling the API with curl

```sh
curl -X POST \
  "localhost:4599/v1/projects/my-project/locations/us-central1/functions/hello:call" \
  -H 'Content-Type: application/json' \
  -d '{"data":"{\"name\":\"Monir\"}"}'
# {"executionId":"...","result":"Hello, Monir!"}
```

`data` is a string containing JSON, not an object — that is the v1 contract, and
why the inner quotes are escaped.

Served surface: v1 `get`, `list`, `delete`, `call`, `operations`, and
`projects/{p}/locations`; v2 `get` and `list` read-only, and
`locations/{l}/runtimes`. `create`, `patch` and `generateUploadUrl` return 501.

## Deploying

```sh
./cloudrig fn deploy NAME --source DIR [--runtime R] [--entry-point F]
                          [--project P] [--region L] [--watch]
./cloudrig fn invoke NAME [--data JSON]
./cloudrig fn logs   NAME [-f]
./cloudrig fn list        [--project P] [--region L]
./cloudrig fn describe NAME
./cloudrig fn delete NAME
```

`fn invoke` runs a function and prints what it returned:

```sh
./cloudrig fn invoke hello --data '{"name":"Monir"}'   # Hello, Monir!
echo '{"name":"piped"}' | ./cloudrig fn invoke hello --data -
```

It goes through the same v1 `:call` endpoint gcloud uses, so it exercises the
compatibility surface rather than a private shortcut. A function that returns an
error status prints it on stderr and exits non-zero.

`fn logs` shows what a function wrote, and follows it with `-f`:

```sh
./cloudrig fn logs hello
./cloudrig fn logs hello -f          # streams until Ctrl-C
```

```
Function: handler
Signature type: http
POST / -> Monir
GET /?name=curled -> curled
```

The last 1000 lines of each function's stdout and stderr are kept, merged in the
order the process produced them. Output is not persisted: it lives as long as
the function does.

Go needs no flags at all when the source is a module:

```sh
./cloudrig fn deploy gohello --source ./testdata/go-hello
curl "localhost:4599/gohello?name=Monir"      # {"hello":"Monir"}
```

- `--runtime` is detected from the source: `package.json` -> `nodejs20`,
  `go.mod` or `.go` files -> `go`. Accepts `go121`-`go125`, `nodejs18/20/22`.
- `--entry-point` is detected for Go when the package has exactly one exported
  `func(http.ResponseWriter, *http.Request)`. Node must be told.
- `--project` defaults to `cloudrig-local`, `--region` to `us-central1`.

Redeploying a name replaces it in place. A deploy that fails to build leaves the
previous version serving.

### Hot reload

```sh
./cloudrig fn deploy hello --source ./testdata/node-hello \
  --entry-point handler --watch
```

Edit a file, save, and the next request runs the new code — no redeploy:

```
watch: hello redeployed
```

The source tree is rescanned every 300ms (`node_modules`, `.git`, `vendor`,
`dist` and `build` are skipped). A save that does not build is reported and the
previous version keeps serving:

```
watch: hello: function hello: exited before it started listening:
SyntaxError: Unexpected end of input
```

Under `MustStart` the watcher runs on the test's `FakeClock`, so a rescan
happens when the test advances time and never on its own.

These commands are HTTP clients of a running emulator (`/_emu/functions`). Point
them elsewhere with `--endpoint` or `CLOUDRIG_ENDPOINT`.

### Function URLs

```
/{name}                          the default project and region
/{region}-{project}/{name}       any project
```

A function is the root of its own URL space, so `/hello/a/b` reaches it as
`/a/b`.

## Without a daemon

Builds and serves one function, then exits on Ctrl-C:

```sh
./cloudrig fn run ./examples/hello --entry-point HelloHTTP
```

## Run the emulator

```sh
./cloudrig start                             # :4599
./cloudrig start --port 5000
CLOUDRIG_PORT=5000 ./cloudrig start
./cloudrig --help
```

```sh
curl -s localhost:4599/_emu/health
curl -s --http2-prior-knowledge localhost:4599/_emu/health   # same port, HTTP/2
```

Every flag has a `CLOUDRIG_` environment twin; an explicit flag wins.

## Use in a test

```go
func TestUpload(t *testing.T) {
	t.Parallel()
	emu := cloudrig.MustStart(t)

	emu.BaseURL()                        // http://127.0.0.1:53412
	emu.FakeClock(t).Advance(time.Hour)  // deterministic, never sleeps
}
```

With a function, built and served in-process — no Docker, no daemon:

```go
emu := cloudrig.MustStart(t, cloudrig.Options{
	Functions: []functions.Function{{
		Name: "hello", Source: "./examples/hello", EntryPoint: "HelloHTTP",
	}},
})

http.Get(emu.FunctionURL("hello") + "?name=monir")
```

Or deploy into a running one:

```go
emu.Functions().Deploy(ctx, functions.Function{
	Name: "hello", Source: "./testdata/node-hello",
	Runtime: functions.RuntimeNode20, EntryPoint: "handler",
})
```

Each `MustStart` is an isolated instance on its own port, with its own function
processes. Shutdown is registered with `t.Cleanup`.

## Test

Everything, the way CI runs it:

```sh
make check
```

Build, vet, the timer lint rule, gofmt, then `go test -race ./...`.

### Plain go test

```sh
go test ./...                    # all packages
go test -race ./...              # with the race detector
go test -v ./...                 # name every test as it runs
go test -short ./...             # skip anything that compiles or spawns a process
go test -count=1 ./...           # ignore the cache and really re-run
```

### One package at a time

```sh
go test -v ./functions/                  # the runner: build, launch, proxy, registry
go test -v ./services/cloudfunctions/    # the gcloud API: v1, v2, :call
go test -v ./transport/                  # h2c, escaped-path routing, health
go test -v ./core/clock/                 # FakeClock ordering and draining
go test -v ./core/gerr/                  # error envelopes and status codes
go test -v ./store/                      # compare-and-swap
go test -v ./cmd/cloudrig/               # flags, env, signals, the real binary
go test -v ./lint/                       # the forbidden-time-call check
go test -v .                             # MustStart, isolation, in-process functions
```

### One test at a time

```sh
go test -v -run TestGcloudCompatibility ./services/cloudfunctions/
go test -v -run TestCall ./services/cloudfunctions/
go test -v -run TestNodeFunction ./functions/
go test -v -run 'TestRoute$' ./functions/
```

`-run` takes a regular expression, so `TestCall` also matches
`TestCallReportsAFunctionFailure`. Anchor it with `$` to pin one test.

### Notes

- `TestGcloudCompatibility` drives the **real gcloud binary** against a live
  emulator. It skips itself when gcloud is not installed, so CI stays green
  without it.
- Tests that compile a function or spawn `node` are the slow ones; `-short`
  skips them and takes the suite from ~15s to ~2s (~40s under -race).
- Everything runs in parallel with its own emulator instance, so the suite needs
  no ports reserved and no cleanup between runs.

## Build

```sh
make build     # ./cloudrig, version stamped from git describe
```

## Status

Working: transport, injected clock, canonical errors, the library entry point,
HTTP Cloud Functions on Go and Node, and the Cloud Functions v1 API driven by
real gcloud.

Not yet: `gcloud functions deploy`, event triggers (HTTP only), runtimes beyond Go and Node, and any other
GCP service. gRPC returns 501.

## Troubleshooting

A stray `cloudrig start` holding `:4599` will silently accept deploys meant for
a new one:

```sh
pkill -f 'cloudrig start'
```
