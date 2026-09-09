# GKE against cloudrig

`terraform apply` here provisions a **real** local Kubernetes cluster through
cloudrig's GKE admin API — not a stub. Needs a container runtime (Docker/colima)
and k3d or kind (`brew install k3d`).

```sh
# terminal 1
make build && ./cloudrig start

# terminal 2
cd examples/gke
terraform init
terraform apply -auto-approve      # spins a real cluster, ~1 min
```

`terraform destroy` removes the real cluster.

## Using the cluster

gcloud and Terraform manage the cluster; `kubectl` runs workloads on it. Reach
it with the backend's own kubeconfig — the credentials gcloud writes use a GCP
auth plugin the local cluster cannot satisfy:

```sh
export KUBECONFIG=$(k3d kubeconfig write cloudrig-tf-demo)
kubectl get nodes
kubectl create deployment web --image=nginx
kubectl get pods
```

`cloudrig-` is the prefix cloudrig gives a cluster, so it does not clash with
one you run yourself. A default k3d/kind cluster has no ingress controller;
install one (e.g. ingress-nginx) or use `kubectl port-forward` for local access.
