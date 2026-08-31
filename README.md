# cloudrig

A local emulator for Google Cloud. Runs as a binary, or in-process inside a Go
test — no Docker, no daemon.

**Functions really run.** `gcloud functions call` starts your code as a process
and returns what it printed — it is not a recorded response or a stubbed
handler. Upload a file and a function fires, in one process, with no Docker and
no Pub/Sub.

Requires Go 1.25+.

```sh
make build          # -> ./cloudrig
./cloudrig start    # :4599
```

Everything below assumes the emulator is running and a second terminal with:

```sh
export CLOUDRIG_ENDPOINT=http://localhost:4599
```

---

## Run a function

```sh
./cloudrig fn deploy hello \
  --source ./testdata/node-hello \
  --entry-point handler \
  --project my-project

curl "localhost:4599/us-central1-my-project/hello?name=Monir"
# Hello, Monir!

./cloudrig fn invoke hello --project my-project --data '{"name":"Monir"}'
./cloudrig fn logs hello -f
```

Go needs no flags at all when the source is a module:

```sh
./cloudrig fn deploy greet --source ./testdata/go-hello
```

The Node sample needs its dependencies once: `(cd testdata/node-hello && npm i)`.

---

## Use it from gcloud

```sh
export CLOUDSDK_CORE_PROJECT=my-project
. ./cloudrig-env.sh
```

`cloudrig-env.sh` exports the four endpoint overrides gcloud needs and disables
credentials. `CLOUDSDK_CORE_PROJECT` must match the `--project` you deployed
with.

```sh
gcloud functions call hello --region us-central1 --data '{"name":"Monir"}'
# executionId: dl242e6ly634-1
# result: Hello, Monir!

gcloud functions list
# NAME   STATE   TRIGGER       REGION       ENVIRONMENT
# hello  ACTIVE  HTTP Trigger  us-central1  1st gen

gcloud functions describe hello --region us-central1

gcloud functions deploy hello --no-gen2 --region us-central1 \
  --runtime go125 --entry-point Handler \
  --trigger-http --source ./testdata/go-hello

gcloud functions delete hello --region us-central1 --quiet
```

No `--gen2` flag is needed on `call`: functions report `GEN_1`, so gcloud routes
the invocation to the v1 API itself.

**Set all four overrides.** Without them `gcloud functions deploy` reaches the
real `cloudbuild`, `serviceusage` and `cloudresourcemanager` hosts.

**A Go source must be its own module.** gcloud zips the directory, so an
enclosing `go.mod` does not travel with it.

---

## Cloud Storage

With `gcloud storage`:

```sh
export CLOUDSDK_CORE_PROJECT=my-project
. ./cloudrig-env.sh

gcloud storage buckets create gs://my-bucket --project my-project
gcloud storage cp ./report.csv gs://my-bucket/report.csv
gcloud storage ls gs://my-bucket
gcloud storage cat gs://my-bucket/report.csv
gcloud storage cp gs://my-bucket/report.csv gs://my-bucket/copy.csv
gcloud storage rm gs://my-bucket/report.csv
```

Or over HTTP:

```sh
curl -X POST "localhost:4599/storage/v1/b?project=my-project" \
  -H 'Content-Type: application/json' -d '{"name":"my-bucket"}'

curl -X POST \
  "localhost:4599/upload/storage/v1/b/my-bucket/o?uploadType=media&name=logs%2Fapp.log" \
  -H 'Content-Type: text/plain' --data 'hello'

curl "localhost:4599/storage/v1/b/my-bucket/o/logs%2Fapp.log?alt=media"
# hello

curl "localhost:4599/storage/v1/b/my-bucket/o?delimiter=/"
# {"kind":"storage#objects","prefixes":["logs/"]}

curl -X POST localhost:4599/_emu/reset
```

Object names are percent-encoded, so `logs/app.log` is one path segment.
`generation`, `metageneration` and `size` come back as strings — that is the GCS
wire format.

From Go, with the real client:

```go
c, _ := storage.NewClient(ctx,
    option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
    option.WithoutAuthentication(),
)
```

Payload bytes never enter the heap: 2 GiB moves for about 1 MiB of allocation.

---

## Upload a file, run a function

The function is an ordinary HTTP handler; the event arrives as the body.

`on-upload/go.mod`:

```
module example.com/onupload

go 1.25
```

`on-upload/fn.go`:

```go
package onupload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type event struct {
	Data struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
		Size   string `json:"size"`
	} `json:"data"`
	Context struct {
		EventType string `json:"eventType"`
	} `json:"context"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("%s: gs://%s/%s (%s bytes)\n",
		e.Context.EventType, e.Data.Bucket, e.Data.Name, e.Data.Size)
	w.WriteHeader(http.StatusNoContent)
}
```

Deploy it against a bucket, then write an object:

```sh
./cloudrig fn deploy on-upload --source ./on-upload --trigger-bucket uploads

curl -X POST "localhost:4599/storage/v1/b?project=demo" \
  -H 'Content-Type: application/json' -d '{"name":"uploads"}'

curl -X POST \
  "localhost:4599/upload/storage/v1/b/uploads/o?uploadType=media&name=report.csv" \
  -H 'Content-Type: text/csv' --data 'a,b,c'

./cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (5 bytes)
```

`--trigger-bucket` defaults to `finalize`. Use `--trigger-event` for
`google.storage.object.delete`, `.archive` or `.metadataUpdate`.

---

## Use in a Go test

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

Shutdown is registered with `t.Cleanup`. State is never persisted under
`MustStart`.

---

## Commands

```
cloudrig start [--port N] [--runner MODE] [--data-dir DIR]

cloudrig fn deploy <name> --source DIR [--runtime R] [--entry-point F]
                          [--project P] [--region L] [--watch]
                          [--trigger-bucket B] [--trigger-event E]
cloudrig fn invoke <name> [--data JSON]
cloudrig fn logs   <name> [-f]
cloudrig fn list | describe <name> | delete <name>
cloudrig fn run <dir>      # starts its own emulator, no daemon
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
go test -short ./...    # skip the slow ones — about 6s

go test -v ./test/conformance/          # the real cloud.google.com/go client
go test -v ./services/storage/          # GCS semantics
go test -v ./services/cloudfunctions/   # the gcloud API
go test -v ./functions/                 # the function runner
```

`TestGcloudCompatibility` drives the real gcloud binary, and skips itself when
gcloud is not installed.

---

## What works

**Cloud Functions** — Go and Node, HTTP and Cloud Storage triggers, the v1 API
driven by real gcloud (`deploy`, `call`, `list`, `describe`, `delete`), hot
reload, logs.

**Cloud Storage** — buckets, objects, all three upload types, copy, rewrite,
compose, listing with prefix and delimiter, every precondition the client sends,
versioning, signed URLs, persistence. Verified against the real Go client and
`gcloud storage`.

**Not yet** — the XML API, `gsutil`, ACLs, batch, Pub/Sub, gen2 functions, and
every other GCP service. gRPC returns 501. See
[UNSUPPORTED.md](UNSUPPORTED.md).

---

## Troubleshooting

A stray `cloudrig start` will silently accept deploys meant for a new one:

```sh
pkill -f 'cloudrig start'
```

`cloudrig fn invoke` reporting "does not exist" usually means a project
mismatch — the error names where the function actually is.
