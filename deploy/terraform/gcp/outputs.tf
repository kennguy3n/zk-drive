output "load_balancer_ip" {
  description = "Global anycast IP of the external HTTPS load balancer. Point an A record for var.domain_name at this address."
  value       = google_compute_global_address.this.address
}

output "server_service_url" {
  description = "Default *.run.app URL of the server Cloud Run service (internal; public traffic flows through the load balancer)."
  value       = google_cloud_run_v2_service.server.uri
}

output "worker_service_name" {
  description = "Name of the worker Cloud Run service."
  value       = google_cloud_run_v2_service.worker.name
}

output "cloudsql_connection_name" {
  description = "Cloud SQL instance connection name (project:region:instance), used by the Cloud SQL Proxy sidecar."
  value       = google_sql_database_instance.this.connection_name
}

output "cloudsql_private_ip" {
  description = "Private IP of the Cloud SQL primary instance."
  value       = google_sql_database_instance.this.private_ip_address
}

output "cloudsql_replica_connection_name" {
  description = "Cloud SQL read replica connection name."
  value       = google_sql_database_instance.replica.connection_name
}

output "redis_host" {
  description = "Memorystore Redis host."
  value       = google_redis_instance.this.host
}

output "frontend_bucket" {
  description = "GCS bucket the built frontend (frontend/dist) should be synced to."
  value       = google_storage_bucket.frontend.name
}

output "managed_ssl_certificate" {
  description = "Name of the Google-managed SSL certificate. Provisioning completes once the A record for var.domain_name points at load_balancer_ip."
  value       = google_compute_managed_ssl_certificate.this.name
}
