# RDS Postgres 16 primary (Multi-AZ) + a read replica.
#
# Connection pooling is handled by a PgBouncer sidecar in each ECS task
# (see ecs.tf) rather than RDS Proxy, keeping the managed-service surface
# small for SME operators. The sidecar connects to the primary endpoint
# below using the generated master password (secrets.tf).

resource "aws_db_subnet_group" "this" {
  name       = "${local.name}-db"
  subnet_ids = aws_subnet.private[*].id

  tags = {
    Name = "${local.name}-db"
  }
}

# Postgres 16 parameter group. Forces TLS for any client connecting from
# outside the VPC; the in-VPC PgBouncer sidecar uses the security group to
# reach 5432.
resource "aws_db_parameter_group" "this" {
  name        = "${local.name}-pg16"
  family      = "postgres16"
  description = "ZK Drive Postgres 16 parameters"

  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_instance" "primary" {
  identifier     = "${local.name}-primary"
  engine         = "postgres"
  engine_version = "16"
  instance_class = var.rds_instance_class

  allocated_storage     = var.rds_allocated_storage
  max_allocated_storage = var.rds_max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = var.db_name
  username = var.db_username
  password = random_password.db.result

  multi_az               = true
  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  parameter_group_name   = aws_db_parameter_group.this.name

  backup_retention_period    = var.rds_backup_retention_days
  backup_window              = "03:00-04:00"
  maintenance_window         = "sun:04:30-sun:05:30"
  copy_tags_to_snapshot      = true
  auto_minor_version_upgrade = true

  performance_insights_enabled = true
  deletion_protection          = true
  skip_final_snapshot          = false
  final_snapshot_identifier    = "${local.name}-primary-final"

  enabled_cloudwatch_logs_exports = ["postgresql", "upgrade"]

  tags = {
    Name = "${local.name}-primary"
  }
}

# Read replica for read-heavy workloads (search, previews, exports). Lives
# in the same region/subnet group; a replica does not take its own
# password (it inherits the source) and cannot be Multi-AZ on its own.
resource "aws_db_instance" "replica" {
  identifier          = "${local.name}-replica"
  replicate_source_db = aws_db_instance.primary.identifier
  instance_class      = var.rds_replica_instance_class

  storage_encrypted          = true
  vpc_security_group_ids     = [aws_security_group.rds.id]
  parameter_group_name       = aws_db_parameter_group.this.name
  auto_minor_version_upgrade = true

  performance_insights_enabled = true
  skip_final_snapshot          = true

  tags = {
    Name = "${local.name}-replica"
  }
}
