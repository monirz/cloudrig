# Authentication

CloudRig has no identity model. It never checks credentials, so a client points
at it with authentication turned off.

**Client libraries** connect without credentials:

```go
c, _ := storage.NewClient(ctx,
    option.WithEndpoint(emu.BaseURL()+"/storage/v1/"),
    option.WithoutAuthentication(),
)
```

For gRPC services, dial with insecure transport credentials (the `EMULATOR_HOST`
env vars set this up for you).

**gcloud** uses `cloudrig-env.sh`, which sets
`CLOUDSDK_AUTH_DISABLE_CREDENTIALS=true` alongside the endpoint overrides.

**Terraform** passes a dummy `access_token = "cloudrig-local"`, which stops the
provider from signing a real JWT and exchanging it at `oauth2.googleapis.com`.
The emulator never looks at the token.

**IAM is stored, never enforced.** `getIamPolicy`/`setIamPolicy` and
`testIamPermissions` round-trip so Terraform can manage bindings, but every
permission asked for is granted: there is no identity here to evaluate a policy
against. See [UNSUPPORTED.md](../UNSUPPORTED.md).
