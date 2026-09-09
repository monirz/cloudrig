# GKE against cloudrig: this provisions a real local Kubernetes cluster (k3s via
# k3d, or kind) through Terraform. `terraform apply` spins an actual cluster, so
# it takes a minute; `terraform destroy` tears the real cluster down.
#
# Needs a container runtime (Docker/colima) and k3d or kind installed.

terraform {
  required_providers {
    google = { source = "hashicorp/google" }
  }
}

# access_token is a dummy: it stops the provider signing a real JWT. The GKE
# admin API is at the container endpoint; the emulator ignores the token.
provider "google" {
  project                   = "cloudrig-local"
  access_token              = "cloudrig-local"
  container_custom_endpoint = "http://localhost:4599/v1/"
}

resource "google_container_cluster" "demo" {
  name               = "tf-demo"
  location           = "us-central1"
  initial_node_count = 1

  # cloudrig does not model deletion protection; turn it off so destroy works.
  deletion_protection = false
}

output "cluster_endpoint" {
  value = google_container_cluster.demo.endpoint
}

# The cluster is real, so kubectl runs workloads on it. gcloud's get-credentials
# writes a kubeconfig that cannot authenticate (GKE auth vs the local cluster's
# client certs), so reach it with the backend's own kubeconfig instead:
#
#   export KUBECONFIG=$(k3d kubeconfig write cloudrig-tf-demo)
#   kubectl get nodes
#   kubectl create deployment web --image=nginx
