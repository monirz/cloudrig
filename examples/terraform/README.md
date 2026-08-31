# Terraform against cloudrig

```sh
# terminal 1
make build && ./cloudrig start

# terminal 2
cd examples/terraform
terraform init
terraform apply -auto-approve
```

Two things make it work, both in the provider block:

- `access_token = "cloudrig-local"` — real credentials make the provider sign a
  JWT and exchange it at `oauth2.googleapis.com`. The emulator never looks at
  the token.
- `storage_custom_endpoint` — one `*_custom_endpoint` per service you use.

IAM policies are stored but never enforced, so `allUsers` grants nothing here.
It is routed because Terraform reads and writes it; without the endpoint the
provider retries a 404 as eventual consistency and the apply hangs.
