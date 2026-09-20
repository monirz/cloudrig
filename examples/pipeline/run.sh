#!/usr/bin/env bash
# One CloudRig workflow, end to end: an object triggers a function that publishes
# to Pub/Sub, a worker enqueues a Cloud Task, and fast-forwarding the clock fires
# its retries — under an injected Pub/Sub fault, around a snapshot and restore.
# Run from the repo root with cloudrig on your PATH.
set -euo pipefail

# A sink for the Cloud Task. It returns 500 so you can watch the retries.
python3 - <<'PY' &
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self): print("  sink: task attempt", flush=True); self.send_response(500); self.end_headers()
    def log_message(self,*a): pass
http.server.HTTPServer(("127.0.0.1", 8080), H).serve_forever()
PY
SINK=$!

# Start CloudRig with a virtual clock. It points deployed functions at its own
# Pub/Sub and Cloud Tasks automatically; SINK_URL is this app's own setting.
SINK_URL=http://localhost:8080/ \
  cloudrig start --clock virtual &
CR=$!
trap 'kill $SINK $CR 2>/dev/null' EXIT
sleep 2

. ./cloudrig-env.sh                     # point gcloud at CloudRig too

echo "== provision =="
gcloud storage buckets create gs://intake
curl -sS -X PUT localhost:4599/v1/projects/cloudrig-local/topics/orders -d '{}' >/dev/null
gcloud tasks queues create pipeline --location=us-central1 --max-attempts=3 --min-backoff=1s

echo "== deploy =="
cloudrig fn deploy ingest --source ./examples/pipeline/ingest --entry-point Handler --trigger-bucket intake
cloudrig fn deploy worker --source ./examples/pipeline/worker --entry-point Handler --trigger-topic orders

echo "== snapshot the seeded world =="
cloudrig snapshot /tmp/cloudrig-seed.tar

echo "== arm a fault: Pub/Sub fails half its requests =="
cloudrig fault pubsub --error 503 --failure-rate 0.5

echo "== upload an object (drives the whole chain) =="
echo '{"id":42}' > /tmp/order-42.json
gcloud storage cp /tmp/order-42.json gs://intake/order-42.json
sleep 2

echo "== fast-forward 1h: the Cloud Task fires and retries =="
cloudrig clock advance 1h
sleep 1

echo "== clear the fault and roll back to the seed =="
cloudrig fault clear
cloudrig restore /tmp/cloudrig-seed.tar
