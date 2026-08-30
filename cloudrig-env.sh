# Point gcloud at a local cloudrig. Source it: . ./cloudrig-env.sh
#
# Every service gcloud consults during a functions deploy needs its own
# override. Without them gcloud reaches the real googleapis.com hosts — which
# fails today on auth, and with live credentials would touch a real project.
: "${CLOUDRIG_ENDPOINT:=http://localhost:4599}"

export CLOUDSDK_CORE_PROJECT="${CLOUDSDK_CORE_PROJECT:-cloudrig-local}"
export CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true
export CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDFUNCTIONS="$CLOUDRIG_ENDPOINT/"
export CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDBUILD="$CLOUDRIG_ENDPOINT/"
export CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE="$CLOUDRIG_ENDPOINT/"
export CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER="$CLOUDRIG_ENDPOINT/"
export CLOUDRIG_ENDPOINT

echo "gcloud -> $CLOUDRIG_ENDPOINT (project $CLOUDSDK_CORE_PROJECT)"
