variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}

variable "cluster_name" {
  type    = string
  default = "ignition"
}

variable "network_name" {
  type    = string
  default = "ignition-vpc"
}

variable "subnet_name" {
  type    = string
  default = "ignition-subnet"
}

variable "internet_subnet_name" {
  type        = string
  default     = "ignition-internet-subnet"
  description = "Subnet for internet-enabled sandbox node pools in the same GKE cluster."
}

variable "operator_cidr" {
  type        = string
  description = "CIDR allowed to reach the GKE control plane."
}

variable "master_ipv4_cidr" {
  type    = string
  default = "172.16.0.0/28"
}

variable "nodes_ipv4_cidr" {
  type    = string
  default = "10.10.0.0/20"
}

variable "pods_ipv4_cidr" {
  type    = string
  default = "10.20.0.0/16"
}

variable "services_ipv4_cidr" {
  type    = string
  default = "10.30.0.0/20"
}

variable "internet_nodes_ipv4_cidr" {
  type        = string
  default     = "10.40.0.0/20"
  description = "Primary node range for internet-enabled sandbox pools."
}

variable "internet_pods_ipv4_cidr" {
  type        = string
  default     = "10.50.0.0/16"
  description = "Secondary Pod range for internet-enabled sandbox pools."
}

variable "sql_instance_name" {
  type    = string
  default = "ignition-sql"
}

variable "sql_database_name" {
  type    = string
  default = "ignition"
}

variable "sql_tier" {
  type    = string
  default = "db-custom-1-3840"
}

variable "sql_password" {
  type      = string
  sensitive = true
}

variable "gpu_max_nodes" {
  type    = number
  default = 2
}

variable "system_max_nodes" {
  type    = number
  default = 3
}

variable "system_machine_type" {
  type    = string
  default = "e2-standard-4"
}

variable "cpu_sandbox_machine_type" {
  type    = string
  default = "n2-standard-8"
}

variable "gpu_sandbox_machine_type" {
  type    = string
  default = "g2-standard-8"
}

variable "sandbox_max_nodes" {
  type    = number
  default = 3
}

variable "deletion_protection" {
  type    = bool
  default = false
}

variable "sql_deletion_protection" {
  type        = bool
  default     = true
  description = "Protect Cloud SQL from deletion in both Terraform and the Cloud SQL API. Disable explicitly before an intentional destroy."
}

variable "dr_region" {
  type        = string
  default     = null
  nullable    = true
  description = "Optional region for a regional-HA cross-region Cloud SQL read replica. Null disables the additional billable DR instance."

  validation {
    condition     = var.dr_region == null || var.dr_region != var.region
    error_message = "dr_region must differ from region."
  }
}

variable "node_service_account_id" {
  type    = string
  default = "ignition-nodes"
}

variable "prober_service_account_id" {
  type        = string
  default     = "ignition-prober"
  description = "Service account the CUJ prober runs as. It authenticates to ignition-api with a Workload Identity ID token; it holds no project IAM roles and is bound in the database's role_bindings table."
}

variable "iap_enabled" {
  type        = bool
  default     = false
  description = "Grant Cloud IAP access to the project's HTTPS load balancers. IAP itself is turned on per-backend by the deploy/k8s/components/iap overlay and uses Google-managed OAuth. Leave false for dev; enable for staging/prod."
}

variable "iap_members" {
  type        = list(string)
  default     = []
  description = "Identities granted IAP access to every HTTPS backend in the project, e.g. [\"group:eng@ignition.dev\", \"user:alice@ignition.dev\"]. Applied only when iap_enabled is true."
}

variable "node_network_tag" {
  type        = string
  default     = "ignition-node"
  description = "Network tag selecting the default-deny node egress firewall policy."
}

variable "internet_node_network_tag" {
  type        = string
  default     = "ignition-sandbox-internet"
  description = "Network tag for sandbox pools that may egress to the public internet through Cloud NAT."
}

variable "private_google_access_vip_cidr" {
  type        = string
  default     = "199.36.153.8/30"
  description = "private.googleapis.com VIP used for Google API and Artifact Registry access without general internet egress."
}

variable "control_plane_repository_id" {
  type    = string
  default = "ignition"
}

variable "sandbox_repository_id" {
  type    = string
  default = "sandboxes"
}

variable "enabled_services" {
  type    = set(string)
  default = ["artifactregistry.googleapis.com", "cloudbuild.googleapis.com", "cloudresourcemanager.googleapis.com", "compute.googleapis.com", "container.googleapis.com", "containerfilesystem.googleapis.com", "dns.googleapis.com", "iamcredentials.googleapis.com", "iap.googleapis.com", "secretmanager.googleapis.com", "servicenetworking.googleapis.com", "sqladmin.googleapis.com"]
}
