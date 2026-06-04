# Memorystore for Redis — the distributed rate limiter and session store
# (REDIS_URL). Standard tier gives a replica + automatic failover.
#
# In-transit encryption is intentionally DISABLED. Unlike the AWS module
# (which exposes a `redis_transit_encryption` toggle because ElastiCache
# presents an Amazon-CA-signed cert the stock go-redis client trusts via the
# system root store), Memorystore's SERVER_AUTHENTICATION mode terminates TLS
# with a Google-managed CA that is NOT in the system trust store, and the
# instance is dialed by private IP. The app connects via `redis.ParseURL`
# (cmd/server/main.go) with no custom RootCAs/ServerName, so `rediss://` would
# fail certificate verification. Enabling it cleanly therefore requires an
# app-side change (plumb the Memorystore server CA into the Redis client),
# which is out of scope for this IaC-only module. The instance is confined to
# the private authorized network reachable only by the Cloud Run app, so the
# transport stays inside Google's VPC. See deploy/terraform/README.md.
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
