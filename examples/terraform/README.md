# Terraform against cloudrig

Two runnable examples, each in its own directory (Terraform reads every `.tf`
in a directory together, so they are kept apart):

- [`services/`](services/) — Storage and Pub/Sub. Lightweight; applies in
  seconds. Start here.
- [`gke/`](gke/) — a `google_container_cluster` that provisions a **real**
  local Kubernetes cluster (k3d/kind). Slow (~1 min) and needs a container
  runtime, so it lives on its own.

Both point the provider at `http://localhost:4599` and need the emulator
running:

```sh
make build && ./cloudrig start
```

Then `cd` into either directory and run `terraform init && terraform apply`.
See each directory's README for details.
