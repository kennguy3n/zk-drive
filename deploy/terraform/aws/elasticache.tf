# ElastiCache Redis 7 (Valkey-compatible) used by the API for the
# distributed rate limiter and session store (REDIS_URL). A small
# two-node replication group gives automatic failover without
# over-provisioning for an SME footprint.

# Optional Redis AUTH token (var.redis_auth_token_enabled). Alphanumeric only
# (special=false): ElastiCache AUTH tokens must be 16-128 printable chars and
# reject /, ", @, and spaces, and the value also lands in a rediss:// URL where
# an alphanumeric token needs no percent-encoding.
resource "random_password" "redis_auth" {
  count   = var.redis_auth_token_enabled ? 1 : 0
  length  = 64
  special = false
}

resource "aws_elasticache_subnet_group" "this" {
  name       = "${local.name}-redis"
  subnet_ids = aws_subnet.private[*].id

  tags = {
    Name = "${local.name}-redis"
  }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${local.name}-redis"
  description          = "ZK Drive sessions + rate limiting"

  engine         = "redis"
  engine_version = var.redis_engine_version
  node_type      = var.redis_node_type
  port           = 6379

  num_cache_clusters         = 2
  automatic_failover_enabled = true
  multi_az_enabled           = true

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = [aws_security_group.redis.id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = var.redis_transit_encryption

  # Redis AUTH (optional). AWS only permits an auth_token when transit
  # encryption is on, so the precondition fails fast at plan time with a clear
  # message instead of a cryptic apply error if the operator enables AUTH
  # without TLS.
  auth_token = var.redis_auth_token_enabled ? random_password.redis_auth[0].result : null

  snapshot_retention_limit = 5
  snapshot_window          = "02:00-03:00"
  maintenance_window       = "sun:05:30-sun:06:30"

  lifecycle {
    precondition {
      condition     = !var.redis_auth_token_enabled || var.redis_transit_encryption
      error_message = "redis_auth_token_enabled requires redis_transit_encryption = true (ElastiCache only allows an AUTH token when in-transit encryption is enabled)."
    }
  }

  tags = {
    Name = "${local.name}-redis"
  }
}
