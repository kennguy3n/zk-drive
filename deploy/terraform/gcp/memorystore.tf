# Memorystore for Redis — the distributed rate limiter and session store
# (REDIS_URL). Standard tier gives a replica + automatic failover.

resource "google_redis_instance" "this" {
  name           = "${local.name}-redis"
  region         = var.region
  tier           = "STANDARD_HA"
  memory_size_gb = var.redis_memory_size_gb
  redis_version  = var.redis_version

  authorized_network      = google_compute_network.this.id
  connect_mode            = "PRIVATE_SERVICE_ACCESS"
  transit_encryption_mode = "DISABLED"

  labels = local.common_labels

  depends_on = [
    google_project_service.this,
    google_service_networking_connection.private_vpc,
  ]
}
