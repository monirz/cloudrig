# Cloud Functions + Cloud Storage

Two functions, run from the repo root with `cloudrig` on your PATH.

- **hello/** is an HTTP function that logs every call.
- **process/** runs on each upload to `gs://intake`, reads the object with the
  official storage client, and writes a line and word count to `gs://processed`.

## Run it

```sh
cloudrig start &
. ./cloudrig-env.sh

# HTTP
cloudrig fn deploy hello --source ./examples/functions-gcs/hello
curl 'localhost:4599/us-central1-cloudrig-local/hello?name=team'
cloudrig fn logs hello

# Storage trigger
gcloud storage buckets create gs://intake
gcloud storage buckets create gs://processed
gcloud functions deploy process --source ./examples/functions-gcs/process \
  --entry-point Process --runtime go122 --trigger-bucket intake \
  --region us-central1 --no-gen2
gcloud storage cp README.md gs://intake/
gcloud storage cat gs://processed/README.md.report.json
gcloud functions logs read process --region us-central1
```

The storage client finds CloudRig through `STORAGE_EMULATOR_HOST`, which
deployed functions inherit without any setup.
