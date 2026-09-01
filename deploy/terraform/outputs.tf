output "cluster_name" { value = google_container_cluster.main.name }
output "cluster_region" { value = google_container_cluster.main.location }
output "network_name" { value = google_compute_network.main.name }
output "sql_connection_name" { value = google_sql_database_instance.main.connection_name }
output "sql_private_ip" { value = google_sql_database_instance.main.private_ip_address }
output "control_plane_registry" { value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.control_plane.repository_id}" }
output "sandbox_registry" { value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.sandboxes.repository_id}" }
output "api_service_account" { value = google_service_account.api.email }
output "controller_service_account" { value = google_service_account.controller.email }
output "node_service_account" { value = google_service_account.nodes.email }
output "sql_dr_connection_name" {
  value       = try(google_sql_database_instance.dr[0].connection_name, null)
  description = "Cross-region DR replica connection name, or null when dr_region is unset."
}
