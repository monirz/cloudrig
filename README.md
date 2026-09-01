# cloudrig

A local emulator for Google Cloud. Runs as a binary, or in-process inside a Go
test. Cloud Storage, Cloud Functions and Pub/Sub run natively — a subprocess
per function, everything else in-process.

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

## Guides

Each one is a sequence you can paste, in order, against a running emulator.

| | |
|---|---|
| [Run a function](#run-a-function) | Deploy a Node or Go function and call it |
| [Use it from gcloud](#use-it-from-gcloud) | The same function through real `gcloud` |
| [Cloud Storage](#cloud-storage) | Buckets and objects, via `gcloud storage` or HTTP |
| [Terraform](#terraform) | `terraform apply` against the emulator |
| [Pub/Sub](#pubsub) | Topics, subscriptions, publish and receive |
| [Upload a file, run a function](#upload-a-file-run-a-function) | A storage trigger, end to end |
| [Run a function on a Pub/Sub message](#run-a-function-on-a-pubsub-message) | A topic trigger, end to end |
| [Watch an unacknowledged message come back](#watch-an-unacknowledged-message-come-back) | Ack deadlines and redelivery |
| [Use in a Go test](#use-in-a-go-test) | In-process, one isolated emulator per test |
| [Inject failures](#inject-failures) | Make a request fail, to test your error handling |
| [Fork state](#fork-state) | Branch an emulator, cheaply, mid-test |
| [Firestore](#firestore) | Documents and queries, over gRPC |
| [Cloud Run](#cloud-run) | Deploy a container, and call it |
| [Run a service without Docker](#run-a-service-without-docker) | The same service as a process |

## Reference

| | |
|---|---|
| [Architecture](ARCHITECTURE.md) | How cloudrig is put together |
| [Commands](#commands) | Every subcommand and flag |
| [Test](#test) | Running cloudrig's own suite |
| [What works](#what-works) | Supported surface, and what is missing |
| [Troubleshooting](#troubleshooting) | When something does not start |
| [License](#license) | MIT |

---

## Run a function

**1. Deploy it.** The Node sample needs its dependencies once,
`(cd testdata/node-hello && npm i)`:

```sh
./cloudrig fn deploy hello \
  --source ./testdata/node-hello \
  --entry-point handler \
  --project my-project
```

**2. Call it,** over HTTP or through the CLI:

```sh
curl "localhost:4599/us-central1-my-project/hello?name=Monir"
# Hello, Monir!

./cloudrig fn invoke hello --project my-project --data '{"name":"Monir"}'
```

**3. Watch what it printed.** `-f` follows:

```sh
./cloudrig fn logs hello -f
```

Go needs no flags at all when the source is a module:

```sh
./cloudrig fn deploy greet --source ./testdata/go-hello
```

---

## Use it from gcloud

**1. Point gcloud at the emulator.** `cloudrig-env.sh` exports the four
endpoint overrides gcloud needs and disables credentials.
`CLOUDSDK_CORE_PROJECT` must match the `--project` you deployed with:

```sh
export CLOUDSDK_CORE_PROJECT=my-project
. ./cloudrig-env.sh
```

**2. Use it as you would the real thing:**

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

**1. Point gcloud at the emulator:**

```sh
export CLOUDSDK_CORE_PROJECT=my-project
. ./cloudrig-env.sh
```

**2. Create a bucket and move objects around:**

```sh
gcloud storage buckets create gs://my-bucket --project my-project
gcloud storage cp ./report.csv gs://my-bucket/report.csv
gcloud storage ls gs://my-bucket
gcloud storage cat gs://my-bucket/report.csv
gcloud storage cp gs://my-bucket/report.csv gs://my-bucket/copy.csv
gcloud storage rm gs://my-bucket/report.csv
```

**Or do the same over HTTP,** with no gcloud at all:

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

## Terraform

**1. Apply the example stack:**

```sh
cd examples/terraform
terraform init
terraform apply -auto-approve
```

```
Apply complete! Resources: 3 added, 0 changed, 0 destroyed.

Outputs:
object_url = "http://localhost:4599/storage/v1/b/tf-bucket/o/hello.txt?alt=media"
```

**2. Read back what it made, confirm the plan is clean, then tear it down:**

```sh
curl "$(terraform output -raw object_url)"    # from terraform
terraform plan                                 # no changes
terraform destroy -auto-approve
```

Two lines in the provider block point Terraform at the emulator:

```hcl
provider "google" {
  access_token            = "cloudrig-local"
  storage_custom_endpoint = "http://localhost:4599/storage/v1/"
}
```

`access_token` skips credentials — real ones make the provider sign a JWT and
exchange it at `oauth2.googleapis.com`. The emulator never looks at the token.
Each service you use needs its own `*_custom_endpoint`.

Works: `google_storage_bucket`, `google_storage_bucket_object`,
`google_storage_bucket_iam_member`, `google_pubsub_topic`,
`google_pubsub_subscription` — create, update in place, and destroy. IAM
policies are stored but not enforced.

The provider speaks REST for every resource, so Pub/Sub needs
`pubsub_custom_endpoint` even though the client libraries reach the same
service over gRPC.

---

## Pub/Sub

gRPC, on the same port as everything else.

**1. Point the client at it** with the same environment variable the Google
emulator uses. Existing code needs no change at all:

```sh
export PUBSUB_EMULATOR_HOST=localhost:4599   # host:port, no scheme
```

**2. Use the client as you would against Google:**

```go
c, _ := pubsub.NewClient(ctx, "cloudrig-local")

c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{
    Name: "projects/cloudrig-local/topics/orders",
})
c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
    Name:  "projects/cloudrig-local/subscriptions/worker",
    Topic: "projects/cloudrig-local/topics/orders",
})

c.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("order-42")})

c.Subscriber(sub).Receive(ctx, func(_ context.Context, m *pubsub.Message) {
    fmt.Println(string(m.Data))
    m.Ack()
})
```

In a Go test the port is chosen for you, so pass it as options instead of
setting the variable:

```go
c, _ := pubsub.NewClient(ctx, "test-project",
    option.WithEndpoint(emu.Endpoint()),
    option.WithoutAuthentication(),
    option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
)
```

Topics, subscriptions, publish, streaming pull, ack and nack, over gRPC — plus
a JSON API on the same port for Terraform. Each subscription gets its own copy
of a message. A message that is nacked, or
whose ack deadline passes, is redelivered. A published message can also run a
function.

`examples/pubsub` is a runnable client for the two scenarios below.

Not supported: push subscriptions, ordering keys, dead-letter topics, retry
policies, snapshots, seek, schemas.

---

## Upload a file, run a function

**1. Write the handler.** It is an ordinary HTTP handler; the event arrives as
the body.

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

**2. Deploy it against a bucket:**

```sh
./cloudrig fn deploy on-upload --source ./on-upload --trigger-bucket uploads
```

**3. Create the bucket and write an object into it:**

```sh
curl -X POST "localhost:4599/storage/v1/b?project=demo" \
  -H 'Content-Type: application/json' -d '{"name":"uploads"}'

curl -X POST \
  "localhost:4599/upload/storage/v1/b/uploads/o?uploadType=media&name=report.csv" \
  -H 'Content-Type: text/csv' --data 'a,b,c'
```

**4. The function ran.** Nothing polled and nothing was stubbed:

```sh
./cloudrig fn logs on-upload
# google.storage.object.finalize: gs://uploads/report.csv (5 bytes)
```

`--trigger-bucket` defaults to `finalize`. Use `--trigger-event` for
`google.storage.object.delete`, `.archive` or `.metadataUpdate`.

---

## Run a function on a Pub/Sub message

Verified end to end; `examples/pubsub` is the publishing client.

**1. A handler.** It reads the gen1 envelope, with the payload base64-encoded
the way the wire carries it:

```sh
mkdir -p /tmp/on-message && cd /tmp/on-message
printf 'module example.com/onmessage\n\ngo 1.25\n' > go.mod
cat > on-message.go <<'EOF'
package onmessage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

type event struct {
	Data struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
	} `json:"data"`
	Context struct {
		Resource struct{ Name string } `json:"resource"`
	} `json:"context"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	var e event
	json.NewDecoder(r.Body).Decode(&e)
	body, _ := base64.StdEncoding.DecodeString(e.Data.Data)
	fmt.Printf("GOT %s from %s (%v)\n", body, e.Context.Resource.Name, e.Data.Attributes)
	w.WriteHeader(http.StatusNoContent)
}
EOF
```

**2. Deploy it against a topic and publish.** The publisher never names
cloudrig; `PUBSUB_EMULATOR_HOST` is its only configuration:

```sh
cd -                                        # back to the cloudrig checkout
export PUBSUB_EMULATOR_HOST=localhost:4599  # host:port, no scheme

./cloudrig fn deploy on-message --source /tmp/on-message \
    --entry-point Handler --trigger-topic orders

go run ./examples/pubsub -mode send -data "hello-from-pubsub"
./cloudrig fn logs on-message
```

```
trigger: google.pubsub.topic.publish on orders
published 1 to projects/cloudrig-local/topics/orders
GOT hello-from-pubsub from projects/cloudrig-local/topics/orders (map[source:pubsub-demo])
```

The function is compiled and run for real, in a subprocess, and the trigger
fires on the publish itself rather than through a subscription: it sees every
message on the topic whether or not anything is subscribed. A `Receive` loop
still gets its own copy; the two do not consume each other.

---

## Watch an unacknowledged message come back

The subscription's ack deadline is ten seconds. `-mode crash` takes a message
and exits while still holding it, which is what a worker dying mid-handler
looks like:

```sh
go run ./examples/pubsub -mode send    -topic dl -data "survives-a-crash"
go run ./examples/pubsub -mode crash   -topic dl
go run ./examples/pubsub -mode receive -topic dl -wait 20s
```

```
15:36:24  id=2  survives-a-crash  <- taken, now crashing
15:36:34  id=2  survives-a-crash
```

Ten seconds apart: the deadline lapsed and the message was redelivered.

It has to be a crash, not a handler that politely returns without acking. The
Go client waits forever for a message that is neither acked nor nacked, so a
graceful exit hangs instead of demonstrating anything.

In a Go test the deadline is on the injected clock, so redelivery happens when
the test advances time rather than after a real ten seconds.

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

`SyncEvents` waits for delivery — the handler has run and answered. It does
**not** wait for the handler's output: a function is a child process whose
stdout is drained by another goroutine, so a log line can arrive shortly
after. Assert on a function's log by polling for what you expect, not by
reading it once.

Shutdown is registered with `t.Cleanup`. State is never persisted under
`MustStart`.

---

## Inject failures

A retry loop is only tested by a request that actually fails. Arm a rule and
the emulator fails matching requests before they reach any service:

```go
emu := cloudrig.MustStart(t)

emu.Faults().Add(faults.Rule{
    Path:   "/storage/v1/*",   // trailing * is a prefix
    Status: http.StatusTooManyRequests,
    Count:  1,                 // fail once, then let the retry through
})
```

`Count: 1` is the useful one: the first call fails, the client retries, the
second succeeds — which proves the retry happened rather than assuming it.

| Field | Meaning |
|---|---|
| `Method` | HTTP method, or empty for any |
| `Path` | Escaped path; a trailing `*` makes it a prefix; empty matches all |
| `Status` | HTTP status to answer with (default 503) |
| `Code` | Canonical error code (default: derived from `Status`) |
| `Message` | Error text |
| `Latency` | Delay before responding, on the injected clock |
| `Count` | How many requests to fail; zero means every one |

`emu.Faults().Clear()` disarms everything. `/_emu/` is never faulted, so a
match-everything rule cannot lock a test out of its own controls.

`Latency` runs on the emulator's clock: under a `FakeClock` the request waits
until the test advances time, so a slow backend is something to assert on
rather than sit through.

---

## Fork state

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

What travels is state, not processes: deployed functions, armed faults and the
clock stay behind. The fork starts with none deployed and gets its own port,
event bus and fault set.

Only an in-memory emulator can fork; one started with `--data-dir` cannot.

---

## Firestore

gRPC, on the same port. The env var is all the configuration a client needs:

```sh
export FIRESTORE_EMULATOR_HOST=localhost:4599   # host:port, no scheme
```

```go
c, _ := firestore.NewClient(ctx, "cloudrig-local")

c.Collection("crew").Doc("ada").Set(ctx, map[string]any{
    "name": "ada", "age": 36, "role": "eng",
})

snap, _ := c.Collection("crew").Doc("ada").Get(ctx)

docs, _ := c.Collection("crew").
    Where("role", "==", "eng").
    OrderBy("age", firestore.Desc).
    Limit(10).
    Documents(ctx).GetAll()
```

Documents, subcollections, `Set`, `Create`, `Update` with field masks,
`Delete`, `BulkWriter`, and queries: `==`, `!=`, `<`, `<=`, `>`, `>=`, `in`,
`not-in`, `array-contains`, `array-contains-any`, `AND`/`OR`, ordering, limit
and offset. Values sort the way Firestore sorts them, across types.

`RunTransaction` works, and so do the field transforms real schemas rely on —
`ServerTimestamp`, `Increment`, `ArrayUnion`, `ArrayRemove`:

```go
c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
    snap, _ := tx.Get(doc)
    return tx.Set(doc, map[string]any{"balance": snap.Data()["balance"].(int64) - 30})
})
```

A transaction here is serialised by the commit lock, not multi-version
concurrency: writes apply as a unit, one commit at a time. Real Firestore also
aborts a transaction whose reads were invalidated, and that abort never
happens here — a test written to observe contention will not see it.

Not supported: `Listen` (real-time updates), cursors (`StartAt`/`EndAt`),
collection-group queries, and aggregations.

---

## Cloud Run

`examples/cloudrun` is a runnable service — an HTTP server on `$PORT`, which is
all Cloud Run asks of your code.

**1. Build the image.** `FROM scratch`, so nothing is pulled:

```sh
cd examples/cloudrun

# For the daemon's architecture, not this machine's: they differ whenever
# Docker runs in a VM, and a mismatch is an exec format error inside the
# container rather than a build failure.
ARCH=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/;s/x86_64/amd64/')

CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH GOWORK=off go build -o server .
docker build -t cloudrun-demo:latest .
rm server && cd -
```

**2. Point gcloud at the emulator:**

```sh
export CLOUDSDK_CORE_PROJECT=cloudrig-local
. ./cloudrig-env.sh
```

Source it, do not pipe it — `. ./cloudrig-env.sh | tail` runs in a subshell and
silently loses every export.

Cloud Run is regional, and gcloud builds its endpoint by prefixing the region
onto the hostname: an override of `localhost` becomes `us-central1-localhost`,
which does not resolve. The script points it at `127.0.0.1.nip.io` instead,
whose wildcard DNS answers the prefixed name with `127.0.0.1`.

**3. Deploy, and call it:**

```sh
gcloud run deploy hello --image=cloudrun-demo:latest --region=us-central1 \
  --memory=256Mi --cpu=500m --quiet

curl "$(gcloud run services describe hello --region=us-central1 \
        --format='value(status.url)')/"
# hello from hello (hello-00001-cri)
```

That is a real container. The limits are real too:

```sh
docker inspect $(docker ps -q --filter label=cloudrig.service=hello) \
  --format 'memory={{.HostConfig.Memory}} nanocpus={{.HostConfig.NanoCpus}}'
# memory=268435456 nanocpus=500000000      # exactly 256Mi and half a core
```

A service that would be killed for exceeding its memory is killed here too.
`--concurrency` and `--max-instances` are accepted and **ignored**: one
container is one container, and faking local autoscaling would teach the wrong
thing.

**4. The rest of the lifecycle:**

```sh
gcloud run services list
gcloud run services update hello --region=us-central1 --update-env-vars=TIER=gold
gcloud run revisions list --region=us-central1     # hello-00002-cri
gcloud run services delete hello --region=us-central1 --quiet
```

`delete` removes the container.

---

## Run a service without Docker

The same example, deployed as a source directory instead of an image, runs as a
process — no build, no container:

```go
emu.CloudRun().Deploy(ctx, cloudrun.Service{
    Name:   "fast",
    Source: "./examples/cloudrun",
}, cloudrun.Options{})
```

Over HTTP, `source:` in the image field means the same thing:

```sh
curl -X POST localhost:4599/apis/serving.knative.dev/v1/namespaces/cloudrig-local/services \
  -H 'Content-Type: application/json' \
  -d "{\"metadata\":{\"name\":\"fast\"},\"spec\":{\"template\":{\"spec\":{\"containers\":
      [{\"image\":\"source:$PWD/examples/cloudrun\"}]}}}}"

curl localhost:4599/us-central1-cloudrig-local/fast/
# hello from fast (fast-00001-cri)
```

Same answer, no container used. It honours the contract your code sees, but
nothing about your image is exercised — use it while iterating on code, and an
image when the container is what you are testing.

Only one revision exists at a time: a deploy replaces what was running rather
than keeping the old container alive, so `revisions list` shows the current one
and there is nothing to roll back to.

Not supported: building an image from source (`gcloud run deploy --source`),
traffic splitting between revisions, concurrency and instance scaling, jobs, and
authenticated invocation — every IAM permission asked for is granted, because
there is no authentication here to enforce.

---

## Commands

```
cloudrig start [--port N] [--runner MODE] [--data-dir DIR]

cloudrig fn deploy <name> --source DIR [--runtime R] [--entry-point F]
                          [--project P] [--region L] [--watch]
                          [--trigger-bucket B] [--trigger-topic T]
                          [--trigger-event E]
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

**Pub/Sub** — topics, subscriptions, publish, streaming pull, ack and nack,
deadline expiry, and `--trigger-topic` to run a function on a message. gRPC for
the client libraries, REST for Terraform, one service behind both.

**Cloud Run** — deploy an image and it runs as a container through Docker,
driven by real `gcloud run`. A source directory runs as a process instead.

**Firestore** — documents, subcollections and queries over gRPC, found through
`FIRESTORE_EMULATOR_HOST`.

**Fault injection** — `emu.Faults()` fails chosen requests, so error paths and
retries can be tested.

**State forking** — `emu.Fork(t)` branches an emulator, copying metadata and
hardlinking payloads.

**Not yet** — the XML API, `gsutil`, ACLs, batch, Pub/Sub push subscriptions,
gen2 functions, and every other GCP service. IAM policies are stored but never
enforced. Cloud Run and anything else container-backed would need a container
runtime and is not emulated. See [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Troubleshooting

A stray `cloudrig start` will silently accept deploys meant for a new one:

```sh
pkill -f 'cloudrig start'
```

`cloudrig fn invoke` reporting "does not exist" usually means a project
mismatch — the error names where the function actually is.

---

## License

MIT. See [LICENSE](LICENSE).
