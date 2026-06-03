# ElastiCache Redis 7 (Valkey-compatible) used by the API for the
# distributed rate limiter and session store (REDIS_URL). A small
# two-node replication group gives automatic failover without
# over-provisioning for an SME footprint.

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
  transit_encryption_enabled = false

  snapshot_retention_limit = 5
  snapshot_window          = "02:00-03:00"
  maintenance_window       = "sun:05:30-sun:06:30"

  tags = {
    Name = "${local.name}-redis"
  }
}
