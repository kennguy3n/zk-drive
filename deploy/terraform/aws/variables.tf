# Input variables for the ZK Drive AWS deployment module.
#
# Sensible defaults are provided for everything that is not
# environment-specific so a clean `terraform apply` only requires the
# operator to set `domain_name` (and, in production, override the
# Secrets Manager seed values). Sizing defaults mirror
# deploy/docker-compose.prod.yml and deploy/k8s/.

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Deployment environment name. Applied as the `environment` tag on every resource and used as a name prefix."
  type        = string
  default     = "production"
}

variable "name_prefix" {
  description = "Prefix for resource names. Combined with `environment` to keep names unique across stacks in one account."
  type        = string
  default     = "zk-drive"
}

variable "domain_name" {
  description = "Public domain the platform is served on (e.g. drive.example.com). Used for the ACM certificate and CloudFront aliases. Leave empty to skip custom-domain wiring (CloudFront uses its default *.cloudfront.net domain and the ALB uses its AWS DNS name)."
  type        = string
  default     = ""
}

# ----------------------------------------------------------------------------
# Networking
# ----------------------------------------------------------------------------

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.20.0.0/16"
}

variable "az_count" {
  description = "Number of availability zones to spread subnets across. Multi-AZ RDS and the ALB both require at least two."
  type        = number
  default     = 2
}

# ----------------------------------------------------------------------------
# Container image
# ----------------------------------------------------------------------------

variable "app_image" {
  description = "Container image repository for all ZK Drive binaries. The same image ships every entrypoint (/app/server, /app/worker, /app/reconciler, ...)."
  type        = string
  default     = "ghcr.io/kennguy3n/zk-drive"
}

variable "app_version" {
  description = "Tag of the ZK Drive image to deploy."
  type        = string
  default     = "0.1.0"
}

# ----------------------------------------------------------------------------
# RDS Postgres
# ----------------------------------------------------------------------------

variable "rds_instance_class" {
  description = "Instance class for the primary RDS Postgres instance."
  type        = string
  default     = "db.t4g.medium"
}

variable "rds_replica_instance_class" {
  description = "Instance class for the RDS read replica."
  type        = string
  default     = "db.t4g.medium"
}

variable "rds_max_connections" {
  description = "Postgres max_connections for the primary instance, used to derive the 80% DatabaseConnections alarm threshold. Default ~410 matches db.t4g.medium's memory-derived default; set it to match the chosen rds_instance_class."
  type        = number
  default     = 410
}

variable "rds_allocated_storage" {
  description = "Allocated storage (GiB) for the primary RDS instance."
  type        = number
  default     = 50
}

variable "rds_max_allocated_storage" {
  description = "Upper bound (GiB) for RDS storage autoscaling."
  type        = number
  default     = 200
}

variable "rds_backup_retention_days" {
  description = "Number of days to retain automated RDS backups."
  type        = number
  default     = 14
}

variable "db_name" {
  description = "Initial Postgres database name."
  type        = string
  default     = "zkdrive"
}

variable "db_username" {
  description = "Master username for the Postgres instance."
  type        = string
  default     = "zkdrive"
}

# ----------------------------------------------------------------------------
# ElastiCache (Redis 7 / Valkey)
# ----------------------------------------------------------------------------

variable "redis_node_type" {
  description = "Node type for the ElastiCache Redis replication group."
  type        = string
  default     = "cache.t4g.small"
}

variable "redis_engine_version" {
  description = "Redis engine version (7.x is Valkey-compatible)."
  type        = string
  default     = "7.1"
}

variable "redis_transit_encryption" {
  description = <<-EOT
    Enable in-transit (TLS) encryption for ElastiCache Redis. Defaults to false:
    the cluster lives on private subnets reachable only by the app tasks, and
    disabling TLS avoids the per-op handshake/latency cost. Set to true for
    compliance regimes (SOC2/HIPAA) that mandate encryption in transit even
    inside the VPC; the app then connects over `rediss://` (ElastiCache presents
    an Amazon-CA-signed cert that the standard go-redis client trusts via the
    system root store, so no app change is required).
  EOT
  type        = bool
  default     = false
}

# ----------------------------------------------------------------------------
# ECS service sizing. CPU is in vCPU-units (1024 = 1 vCPU), memory in MiB.
# CPU tiers mirror deploy/docker-compose.prod.yml (server/worker 1 vCPU, NATS
# 0.5 vCPU). Memory is set to the Fargate-valid *minimum* for each CPU tier
# rather than the compose memory limit: Fargate only accepts a fixed set of
# CPU/memory pairings (1 vCPU -> 2048-8192 MiB, 0.5 vCPU -> 1024-4096 MiB),
# so compose's 512 MiB limit is not a legal Fargate task size and would make
# `terraform apply` fail at RegisterTaskDefinition. The app's actual working
# set is well under these minimums; the extra headroom is unavoidable, not a
# deliberate reservation.
# ----------------------------------------------------------------------------

variable "server_cpu" {
  description = "Fargate CPU units for the server task (1024 = 1 vCPU)."
  type        = number
  default     = 1024
}

variable "server_memory" {
  description = "Fargate memory (MiB) for the server task. Minimum valid value for a 1 vCPU (1024) task is 2048."
  type        = number
  default     = 2048
}

variable "server_desired_count" {
  description = "Baseline number of server tasks before autoscaling."
  type        = number
  default     = 2
}

variable "server_min_count" {
  description = "Minimum number of server tasks."
  type        = number
  default     = 2
}

variable "server_max_count" {
  description = "Maximum number of server tasks under autoscaling."
  type        = number
  default     = 10
}

variable "server_target_requests_per_instance" {
  description = "Target ALB request count per server task per minute used by the request-count autoscaling policy. 200 req/s * 60s = 12000 req/min."
  type        = number
  default     = 12000
}

variable "worker_cpu" {
  description = "Fargate CPU units for the worker task (1024 = 1 vCPU)."
  type        = number
  default     = 1024
}

variable "worker_memory" {
  description = "Fargate memory (MiB) for the worker task. Minimum valid value for a 1 vCPU (1024) task is 2048."
  type        = number
  default     = 2048
}

variable "worker_desired_count" {
  description = "Baseline number of worker tasks before autoscaling."
  type        = number
  default     = 1
}

variable "worker_min_count" {
  description = "Minimum number of worker tasks."
  type        = number
  default     = 1
}

variable "worker_max_count" {
  description = "Maximum number of worker tasks under autoscaling."
  type        = number
  default     = 6
}

variable "worker_target_nats_pending" {
  description = "Target NATS JetStream pending message count per worker task. The worker autoscaling policy tracks a CloudWatch metric `zk-drive/NATSPendingMessages` (published by the worker) against this target."
  type        = number
  default     = 1000
}

variable "clamav_cpu" {
  description = "Fargate CPU units for the ClamAV task (1024 = 1 vCPU)."
  type        = number
  default     = 1024
}

variable "clamav_memory" {
  description = "Fargate memory (MiB) for the ClamAV task."
  type        = number
  default     = 2048
}

variable "nats_cpu" {
  description = "Fargate CPU units for the NATS task (1024 = 1 vCPU)."
  type        = number
  default     = 512
}

variable "nats_memory" {
  description = "Fargate memory (MiB) for the NATS task. Minimum valid value for a 0.5 vCPU (512) task is 1024."
  type        = number
  default     = 1024
}

variable "nats_storage_gib" {
  description = "Size (GiB) of the EBS volume attached to the NATS task for JetStream persistence."
  type        = number
  default     = 20
}

# ----------------------------------------------------------------------------
# Application configuration (non-secret). Secrets live in secrets.tf.
# Names mirror the env vars read by internal/config/config.go.
# ----------------------------------------------------------------------------

variable "fabric_console_url" {
  description = "Base URL of the zk-object-fabric console API (FABRIC_CONSOLE_URL). When empty, signup falls back to the static S3_* settings."
  type        = string
  default     = ""
}

variable "fabric_endpoint" {
  description = "S3 endpoint of the zk-object-fabric storage gateway (S3_ENDPOINT)."
  type        = string
  default     = ""
}

variable "fabric_bucket" {
  description = "S3 bucket used for object storage on the fabric gateway (S3_BUCKET). This is NOT the frontend-assets bucket created by this module."
  type        = string
  default     = ""
}

variable "fabric_access_key" {
  description = "Access key for the zk-object-fabric storage gateway (S3_ACCESS_KEY). Required by the app whenever fabric_endpoint is set."
  type        = string
  default     = ""
  sensitive   = true
}

variable "fabric_secret_key" {
  description = "Secret key for the zk-object-fabric storage gateway (S3_SECRET_KEY). Required by the app whenever fabric_endpoint is set."
  type        = string
  default     = ""
  sensitive   = true
}

variable "rate_limit_per_user" {
  description = "RATE_LIMIT_PER_USER applied by the API rate limiter."
  type        = number
  default     = 100
}

variable "rate_limit_per_workspace" {
  description = "RATE_LIMIT_PER_WORKSPACE applied by the API rate limiter."
  type        = number
  default     = 500
}

variable "log_retention_days" {
  description = "Retention period (days) for CloudWatch log groups."
  type        = number
  default     = 30
}

variable "audit_log_archive_enabled" {
  description = "Sets AUDIT_LOG_ARCHIVE_ENABLED on the daily audit-archiver cron task. The audit-archiver binary is opt-in (defaults to false) and exits as a no-op until this is true, so the scheduled task does nothing until enabled. Leave false until zk-object-fabric storage (fabric_endpoint/bucket) is configured and the archive prefix is confirmed writable; the K8s deployment hardcodes this true (deploy/k8s/audit-archiver-cronjob.yaml)."
  type        = bool
  default     = false
}

# ----------------------------------------------------------------------------
# Secret seed values. Override these (or rotate the generated secrets out of
# band) for real deployments. STRIPE_* default to empty because billing is
# optional.
# ----------------------------------------------------------------------------

variable "stripe_secret_key" {
  description = "STRIPE_SECRET_KEY value to seed into Secrets Manager. Optional."
  type        = string
  default     = ""
  sensitive   = true
}

variable "stripe_webhook_secret" {
  description = "STRIPE_WEBHOOK_SECRET value to seed into Secrets Manager. Optional."
  type        = string
  default     = ""
  sensitive   = true
}

variable "tags" {
  description = "Additional tags merged onto every resource."
  type        = map(string)
  default     = {}
}
