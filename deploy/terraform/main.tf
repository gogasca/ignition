resource "google_project_service" "api" {
  for_each           = var.enabled_services
  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_compute_network" "main" {
  name                    = var.network_name
  project                 = var.project_id
  auto_create_subnetworks = false
  depends_on              = [google_project_service.api]
}

resource "google_compute_subnetwork" "main" {
  name                     = var.subnet_name
  project                  = var.project_id
  region                   = var.region
  network                  = google_compute_network.main.id
  ip_cidr_range            = var.nodes_ipv4_cidr
  private_ip_google_access = true
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.pods_ipv4_cidr
  }
  secondary_ip_range {
    range_name    = "svcs"
    ip_cidr_range = var.services_ipv4_cidr
  }
}

resource "google_compute_subnetwork" "internet_sandbox" {
  name                     = var.internet_subnet_name
  project                  = var.project_id
  region                   = var.region
  network                  = google_compute_network.main.id
  ip_cidr_range            = var.internet_nodes_ipv4_cidr
  private_ip_google_access = true
  secondary_ip_range {
    range_name    = "internet-pods"
    ip_cidr_range = var.internet_pods_ipv4_cidr
  }
}

resource "google_compute_router" "main" {
  name    = "ignition-router"
  project = var.project_id
  region  = var.region
  network = google_compute_network.main.id
}
resource "google_compute_router_nat" "main" {
  name                               = "ignition-nat"
  project                            = var.project_id
  region                             = var.region
  router                             = google_compute_router.main.name
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  subnetwork {
    name                    = google_compute_subnetwork.internet_sandbox.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }
}

resource "google_compute_route" "private_google_apis" {
  name             = "ignition-private-google-apis"
  project          = var.project_id
  network          = google_compute_network.main.name
  dest_range       = var.private_google_access_vip_cidr
  next_hop_gateway = "default-internet-gateway"
  priority         = 1000
}

resource "google_compute_global_address" "private_services" {
  name          = "ignition-psa"
  project       = var.project_id
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 16
  network       = google_compute_network.main.id
}

locals {
  private_google_dns_zones = {
    googleapis = "googleapis.com."
    pkg_dev    = "pkg.dev."
    gcr_io     = "gcr.io."
  }
  private_google_access_addresses = [
    "199.36.153.8",
    "199.36.153.9",
    "199.36.153.10",
    "199.36.153.11",
  ]
  cloud_sql_private_cidr = "${google_compute_global_address.private_services.address}/${google_compute_global_address.private_services.prefix_length}"
}

resource "google_dns_managed_zone" "private_google" {
  for_each    = local.private_google_dns_zones
  name        = "ignition-${replace(trimsuffix(each.value, "."), ".", "-")}"
  project     = var.project_id
  dns_name    = each.value
  description = "Ignition: pin ${trimsuffix(each.value, ".")} to the Private Google Access VIP"
  visibility  = "private"

  private_visibility_config {
    networks {
      network_url = google_compute_network.main.id
    }
  }

  depends_on = [google_project_service.api]
}

resource "google_dns_record_set" "private_google_apex" {
  for_each     = local.private_google_dns_zones
  project      = var.project_id
  managed_zone = google_dns_managed_zone.private_google[each.key].name
  name         = each.value
  type         = "A"
  ttl          = 300
  rrdatas      = local.private_google_access_addresses
}

resource "google_dns_record_set" "private_google_wildcard" {
  for_each     = local.private_google_dns_zones
  project      = var.project_id
  managed_zone = google_dns_managed_zone.private_google[each.key].name
  name         = "*.${each.value}"
  type         = "CNAME"
  ttl          = 300
  rrdatas      = [each.value]
}

resource "google_compute_firewall" "node_egress_deny" {
  name               = "ignition-node-egress-deny"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 65500
  destination_ranges = ["0.0.0.0/0"]
  target_tags        = [var.node_network_tag]

  deny {
    protocol = "all"
  }
}

resource "google_compute_firewall" "node_egress_allow_cluster" {
  name               = "ignition-node-egress-allow-cluster"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = [var.nodes_ipv4_cidr, var.pods_ipv4_cidr, var.services_ipv4_cidr, var.internet_nodes_ipv4_cidr, var.internet_pods_ipv4_cidr]
  target_tags        = [var.node_network_tag]

  allow {
    protocol = "all"
  }
}

resource "google_compute_firewall" "node_egress_allow_control_plane" {
  name               = "ignition-node-egress-allow-control-plane"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = [var.master_ipv4_cidr]
  target_tags        = [var.node_network_tag]

  allow {
    protocol = "tcp"
    ports    = ["443", "8132", "10250"]
  }
}

resource "google_compute_firewall" "node_egress_allow_google_apis" {
  name               = "ignition-node-egress-allow-google-apis"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = [var.private_google_access_vip_cidr]
  target_tags        = [var.node_network_tag]

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

resource "google_compute_firewall" "node_egress_allow_cloud_sql" {
  name               = "ignition-node-egress-allow-cloudsql"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = [local.cloud_sql_private_cidr]
  target_tags        = [var.node_network_tag]

  allow {
    protocol = "tcp"
    ports    = ["3307"]
  }
}

# Internet-enabled sandbox nodes use a separate tag. Private destinations are
# denied before the broad public allow; Kubernetes NetworkPolicy provides the
# per-Pod selector and remains the primary tenant boundary.
resource "google_compute_firewall" "internet_node_egress_allow_cluster" {
  name               = "ignition-internet-node-egress-allow-cluster"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 800
  destination_ranges = [var.nodes_ipv4_cidr, var.pods_ipv4_cidr, var.services_ipv4_cidr, var.internet_nodes_ipv4_cidr, var.internet_pods_ipv4_cidr]
  target_tags        = [var.internet_node_network_tag]
  allow { protocol = "all" }
}
resource "google_compute_firewall" "internet_node_egress_allow_control_plane" {
  name               = "ignition-internet-node-egress-allow-control-plane"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 800
  destination_ranges = [var.master_ipv4_cidr, var.private_google_access_vip_cidr]
  target_tags        = [var.internet_node_network_tag]
  allow { protocol = "all" }
}
resource "google_compute_firewall" "internet_node_egress_deny_private" {
  name               = "ignition-internet-node-egress-deny-private"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 900
  destination_ranges = ["10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"]
  target_tags        = [var.internet_node_network_tag]
  deny { protocol = "all" }
}
resource "google_compute_firewall" "internet_node_egress_allow_public" {
  name               = "ignition-internet-node-egress-allow-public"
  project            = var.project_id
  network            = google_compute_network.main.name
  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = ["0.0.0.0/0"]
  target_tags        = [var.internet_node_network_tag]
  allow { protocol = "all" }
}
resource "google_service_networking_connection" "private_services" {
  network                 = google_compute_network.main.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_services.name]
  depends_on              = [google_project_service.api]
}

resource "google_container_cluster" "main" {
  name                     = var.cluster_name
  project                  = var.project_id
  location                 = var.region
  node_locations           = [var.zone]
  network                  = google_compute_network.main.id
  subnetwork               = google_compute_subnetwork.main.id
  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = var.deletion_protection
  datapath_provider        = "ADVANCED_DATAPATH"
  logging_service          = "logging.googleapis.com/kubernetes"
  monitoring_service       = "monitoring.googleapis.com/kubernetes"
  release_channel { channel = "REGULAR" }
  addons_config {
    gce_persistent_disk_csi_driver_config { enabled = true }
    horizontal_pod_autoscaling { disabled = false }
    http_load_balancing { disabled = false }
  }
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "svcs"

    additional_ip_ranges_config {
      subnetwork           = google_compute_subnetwork.internet_sandbox.id
      pod_ipv4_range_names = ["internet-pods"]
    }
  }
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = var.master_ipv4_cidr
  }
  master_authorized_networks_config {
    cidr_blocks {
      cidr_block   = var.operator_cidr
      display_name = "operator"
    }
  }
  workload_identity_config { workload_pool = "${var.project_id}.svc.id.goog" }
  node_config {
    machine_type    = var.system_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    tags            = [var.node_network_tag]
    gcfs_config { enabled = true }
  }
  depends_on = [
    google_project_iam_member.nodes,
    google_service_networking_connection.private_services,
  ]
}

# Nodes use a dedicated minimal identity. In particular, sandbox workloads do
# not inherit the Compute Engine default service account.
resource "google_service_account" "nodes" {
  project      = var.project_id
  account_id   = var.node_service_account_id
  display_name = "Ignition GKE nodes"
}

locals {
  node_roles = toset([
    "roles/container.defaultNodeServiceAccount",
  ])
}

resource "google_project_iam_member" "nodes" {
  for_each = local.node_roles
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_container_node_pool" "system" {
  name       = "cpu-system"
  project    = var.project_id
  location   = google_container_cluster.main.location
  cluster    = google_container_cluster.main.name
  node_count = 1

  autoscaling {
    total_min_node_count = 1
    total_max_node_count = var.system_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.system_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "cpu-system" }
    tags            = [var.node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 50
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  depends_on = [google_project_iam_member.nodes]
}

resource "google_container_node_pool" "cpu_sandbox" {
  name     = "cpu-sandbox"
  project  = var.project_id
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.name

  autoscaling {
    total_min_node_count = 0
    total_max_node_count = var.sandbox_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.cpu_sandbox_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "cpu-sandbox" }
    tags            = [var.node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 100
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    sandbox_config { type = "gvisor" }
    taint {
      key    = "ignition.io/sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  depends_on = [google_project_iam_member.nodes]
}

resource "google_container_node_pool" "gpu_sandbox" {
  name     = "gpu-sandbox-l4"
  project  = var.project_id
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.name

  autoscaling {
    total_min_node_count = 0
    total_max_node_count = var.gpu_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.gpu_sandbox_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "gpu-sandbox-l4" }
    tags            = [var.node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 100
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    sandbox_config { type = "gvisor" }
    guest_accelerator {
      type  = "nvidia-l4"
      count = 1
      gpu_driver_installation_config { gpu_driver_version = "DEFAULT" }
    }
    taint {
      key    = "ignition.io/gpu-sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  depends_on = [google_project_iam_member.nodes]
}

# Cache-epoch generation of the GPU sandbox pool: identical to gpu_sandbox
# except for its CONTAINER_IMAGE_CACHE secondary boot disk and node-pool
# label. Ignition's own scheduling does not yet select this pool (see the
# rollout note in docs/design/ignition-design-images-startup.md's secondary
# boot-disk section) — it exists so an operator can build and roll a cache
# epoch by creating a new instance of this resource (a new node-pool
# generation, blue/green, per the design) without hand-writing Kubernetes
# YAML. It creates nothing until gpu_cache_epoch_disk_image is set.
resource "google_container_node_pool" "gpu_sandbox_cache_epoch" {
  count    = var.gpu_cache_epoch_disk_image != "" ? 1 : 0
  name     = "gpu-sandbox-l4-cache-epoch"
  project  = var.project_id
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.name

  autoscaling {
    total_min_node_count = 0
    total_max_node_count = var.gpu_cache_epoch_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.gpu_sandbox_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "gpu-sandbox-l4-cache-epoch" }
    tags            = [var.node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 100
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    sandbox_config { type = "gvisor" }
    guest_accelerator {
      type  = "nvidia-l4"
      count = 1
      gpu_driver_installation_config { gpu_driver_version = "DEFAULT" }
    }
    secondary_boot_disks {
      disk_image = var.gpu_cache_epoch_disk_image
      mode       = "CONTAINER_IMAGE_CACHE"
    }
    taint {
      key    = "ignition.io/gpu-sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  depends_on = [google_project_iam_member.nodes]
}

resource "google_container_node_pool" "cpu_sandbox_internet" {
  name     = "cpu-sandbox-internet"
  project  = var.project_id
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.name
  autoscaling {
    total_min_node_count = 0
    total_max_node_count = var.sandbox_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.cpu_sandbox_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "cpu-sandbox-internet" }
    tags            = [var.internet_node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 100
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    sandbox_config { type = "gvisor" }
    taint {
      key    = "ignition.io/sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  network_config {
    enable_private_nodes = true
    subnetwork           = google_compute_subnetwork.internet_sandbox.id
    pod_range            = "internet-pods"
  }
  depends_on = [google_project_iam_member.nodes]
}

resource "google_container_node_pool" "gpu_sandbox_internet" {
  name     = "gpu-sandbox-l4-internet"
  project  = var.project_id
  location = google_container_cluster.main.location
  cluster  = google_container_cluster.main.name
  autoscaling {
    total_min_node_count = 0
    total_max_node_count = var.gpu_max_nodes
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
  node_config {
    machine_type    = var.gpu_sandbox_machine_type
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    labels          = { "ignition.io/node-pool" = "gpu-sandbox-l4-internet" }
    tags            = [var.internet_node_network_tag]
    disk_type       = "pd-balanced"
    disk_size_gb    = 100
    metadata        = { "disable-legacy-endpoints" = "true" }
    gcfs_config { enabled = true }
    workload_metadata_config { mode = "GKE_METADATA" }
    sandbox_config { type = "gvisor" }
    guest_accelerator {
      type  = "nvidia-l4"
      count = 1
      gpu_driver_installation_config { gpu_driver_version = "DEFAULT" }
    }
    taint {
      key    = "ignition.io/gpu-sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
  network_config {
    enable_private_nodes = true
    subnetwork           = google_compute_subnetwork.internet_sandbox.id
    pod_range            = "internet-pods"
  }
  depends_on = [google_project_iam_member.nodes]
}

resource "google_sql_database_instance" "main" {
  name                = var.sql_instance_name
  project             = var.project_id
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = var.sql_deletion_protection
  settings {
    tier                        = var.sql_tier
    availability_type           = "REGIONAL"
    disk_autoresize             = true
    deletion_protection_enabled = var.sql_deletion_protection
    retain_backups_on_delete    = true
    backup_configuration {
      enabled                        = true
      start_time                     = "03:00"
      point_in_time_recovery_enabled = true
      transaction_log_retention_days = 7
      backup_retention_settings {
        retained_backups = 14
        retention_unit   = "COUNT"
      }
    }
    ip_configuration {
      ipv4_enabled    = false
      private_network = google_compute_network.main.id
    }
  }
  depends_on = [google_service_networking_connection.private_services]
}
resource "google_sql_database" "main" {
  project  = var.project_id
  name     = var.sql_database_name
  instance = google_sql_database_instance.main.name
}
resource "google_sql_user" "ignition" {
  project  = var.project_id
  name     = "ignition"
  instance = google_sql_database_instance.main.name
  password = var.sql_password
}

resource "google_artifact_registry_repository" "control_plane" {
  project       = var.project_id
  location      = var.region
  repository_id = var.control_plane_repository_id
  format        = "DOCKER"
  description   = "Ignition control-plane images"
  depends_on    = [google_project_service.api]
}
resource "google_artifact_registry_repository" "sandboxes" {
  project       = var.project_id
  location      = var.region
  repository_id = var.sandbox_repository_id
  format        = "DOCKER"
  description   = "Ignition sandbox images"
  depends_on    = [google_project_service.api]
}

resource "google_artifact_registry_repository_iam_member" "nodes_control_plane_pull" {
  project    = var.project_id
  location   = google_artifact_registry_repository.control_plane.location
  repository = google_artifact_registry_repository.control_plane.repository_id
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_artifact_registry_repository_iam_member" "nodes_sandbox_pull" {
  project    = var.project_id
  location   = google_artifact_registry_repository.sandboxes.location
  repository = google_artifact_registry_repository.sandboxes.repository_id
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_service_account" "api" {
  project      = var.project_id
  account_id   = "ignition-api"
  display_name = "Ignition API"
}
resource "google_service_account" "controller" {
  project      = var.project_id
  account_id   = "ignition-controller"
  display_name = "Ignition controller"
}
resource "google_service_account_iam_member" "api_wi" {
  service_account_id = google_service_account.api.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[ignition-system/ignition-api]"
}
resource "google_service_account_iam_member" "controller_wi" {
  service_account_id = google_service_account.controller.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[ignition-system/ignition-controller]"
}
resource "google_project_iam_member" "api_sql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.api.email}"
}
resource "google_project_iam_member" "controller_sql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.controller.email}"
}
resource "google_project_iam_member" "controller_secret_manager" {
  project = var.project_id
  role    = "roles/secretmanager.secretAccessor"
  member  = "serviceAccount:${google_service_account.controller.email}"
}

# The CUJ prober authenticates to ignition-api with a Workload Identity ID
# token (see internal/probe/gcptoken.go). It needs no project IAM role - only
# the KSA -> GSA impersonation binding - and is authorized inside ignition-api
# by a row in role_bindings (db/rolebindings.sql).
resource "google_service_account" "prober" {
  project      = var.project_id
  account_id   = var.prober_service_account_id
  display_name = "Ignition CUJ prober"
}
resource "google_service_account_iam_member" "prober_wi" {
  service_account_id = google_service_account.prober.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[ignition-system/ignition-prober]"
}

# ---------------------------------------------------------------------------
# Cloud IAP for the ignition-api HTTPS load balancer (staging/prod).
#
# IAP runs with Google-managed OAuth (the legacy per-project OAuth brand/client
# admin APIs were shut down in March 2026), so there is no brand or client to
# create here: the deploy/k8s/components/iap BackendConfig enables IAP on the
# backend and Google supplies the consent screen. Terraform only enables the
# API and grants access. The grant is at the project compute-web scope because
# the GKE Ingress controller names the backend service dynamically.
# ---------------------------------------------------------------------------
resource "google_iap_web_type_compute_iam_member" "access" {
  for_each = var.iap_enabled ? toset(var.iap_members) : toset([])
  project  = var.project_id
  role     = "roles/iap.httpsResourceAccessor"
  member   = each.value
}

resource "google_sql_database_instance" "dr" {
  count                = var.dr_region == null ? 0 : 1
  name                 = "${var.sql_instance_name}-dr"
  project              = var.project_id
  region               = var.dr_region
  database_version     = "POSTGRES_16"
  master_instance_name = google_sql_database_instance.main.name
  deletion_protection  = var.sql_deletion_protection

  settings {
    tier                        = var.sql_tier
    availability_type           = "REGIONAL"
    disk_autoresize             = true
    deletion_protection_enabled = var.sql_deletion_protection
    ip_configuration {
      ipv4_enabled    = false
      private_network = google_compute_network.main.id
    }
  }

  depends_on = [google_service_networking_connection.private_services]
}
