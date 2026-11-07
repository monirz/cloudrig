# Point gcloud back at real GCP. Source it: . ./cloudrig-env-off.sh
#
# Undoes cloudrig-env.sh, plus the CLOUDSDK_CONFIG and KUBECONFIG a demo shell
# sets. Run as a script it would only clear a child shell, so it refuses.
if [ -n "$ZSH_VERSION" ]; then
  case $ZSH_EVAL_CONTEXT in *:file*) ;; *) echo "source it: . ./cloudrig-env-off.sh" >&2; exit 1 ;; esac
elif [ -n "$BASH_VERSION" ] && [ "${BASH_SOURCE[0]}" = "$0" ]; then
  echo "source it: . ./cloudrig-env-off.sh" >&2; exit 1
fi

unset CLOUDRIG_ENDPOINT CLOUDSDK_CORE_PROJECT CLOUDSDK_AUTH_DISABLE_CREDENTIALS \
  PUBSUB_EMULATOR_HOST STORAGE_EMULATOR_HOST CLOUDSDK_CONFIG KUBECONFIG
for v in $(env | sed -n 's/^\(CLOUDSDK_API_ENDPOINT_OVERRIDES_[A-Z_]*\)=.*/\1/p'); do
  unset "$v"
done
unset v

# Overrides saved with `gcloud config set` outlive the shell; point them out.
if [ -n "$(gcloud config list --format='value(api_endpoint_overrides)' 2>/dev/null)" ]; then
  echo "warning: the gcloud config itself still overrides endpoints:" >&2
  gcloud config list --format='value(api_endpoint_overrides)' >&2
fi

echo "gcloud -> real GCP (account $(gcloud config get-value account 2>/dev/null), project $(gcloud config get-value project 2>/dev/null))"
