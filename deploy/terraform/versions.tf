terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0, < 8.0"
    }
  }

  # Partial config: bucket/prefix are supplied at `terraform init` time via
  # -backend-config, one bucket+prefix per install, so state for a customer
  # deployment lives in GCS instead of on whichever machine ran `apply`.
  # deploy/scripts/onboard.sh creates the bucket and passes both flags; see
  # docs/guides/customer-onboarding.md. Omit -backend-config entirely (and
  # this block, via `terraform init -backend=false`) only for a disposable
  # local/dev run where losing state on laptop wipe is acceptable.
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
  zone    = var.zone
}
