terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 5.0" }
  }
}

# access_token is what makes this work without credentials: real ones make the
# provider sign a JWT and exchange it at oauth2.googleapis.com. The emulator
# never looks at the token.
provider "google" {
  project      = "my-project"
  region       = "us-central1"
  access_token = "cloudrig-local"

  storage_custom_endpoint = "http://localhost:4599/storage/v1/"
  pubsub_custom_endpoint  = "http://localhost:4599/v1/"
}

resource "google_storage_bucket" "demo" {
  name                        = "tf-bucket"
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = false
}

resource "google_storage_bucket_object" "hello" {
  name    = "hello.txt"
  bucket  = google_storage_bucket.demo.name
  content = "from terraform"
}

resource "google_storage_bucket_iam_member" "public_read" {
  bucket = google_storage_bucket.demo.name
  role   = "roles/storage.objectViewer"
  member = "allUsers"
}

resource "google_pubsub_topic" "orders" {
  name   = "tf-orders"
  labels = { env = "local" }
}

resource "google_pubsub_subscription" "worker" {
  name                 = "tf-worker"
  topic                = google_pubsub_topic.orders.id
  ack_deadline_seconds = 30
}

output "object_url" {
  value = "http://localhost:4599/storage/v1/b/${google_storage_bucket.demo.name}/o/${google_storage_bucket_object.hello.name}?alt=media"
}
