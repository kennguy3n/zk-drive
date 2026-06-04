# Input variables for the ZK Drive GCP deployment module.
#
# Only `project_id` is strictly required; sizing defaults mirror
# deploy/docker-compose.prod.yml and the AWS module. Set `domain_name`
# to wire the managed SSL certificate and the external HTTPS load
# balancer to a custom domain.

variable "project_id" {
  description = "GCP project ID to deploy into."
  type        = string
}

variable "region" {
  description = "GCP region for regional resources (Cloud Run, Cloud SQL, Memorystore)."
  type        = string
  default     = "us-central1"
}

variable "environment" {
  description = "Deployment environment name. Applied as the `environment` label and used as a name prefix."
  type        = string
  default     = "production"
}

variable "name_prefix" {
  description = "Prefix for resource names."
  type        = string
  default     = "zk-drive"
}

variable "domain_name" {
  description = "Public domain the platform is served on (e.g. drive.example.com). Used for the Google-managed SSL certificate and is REQUIRED for `terraform apply`: the external HTTPS load balancer's managed cert cannot be created with an empty domain."
  type        = string
  default     = ""

  validation {
    # The whole external HTTPS LB stack (managed cert, HTTPS proxy,
    # forwarding rule) needs a real domain, and Cloud Run is configured for
    # internal-LB-only ingress, so there is no domain-less serving path on
    # GCP. Fail at plan time with a clear message instead of letting the
    # Google API reject an empty managed-cert domain partway through apply.
    condition     = var.domain_name != ""
    error_message = "domain_name is required on GCP: the external HTTPS load balancer's Google-managed certificate cannot be created without a domain. Pass -var 'domain_name=drive.example.com'."
  }
}

# ----------------------------------------------------------------------------
# Networking
# ----------------------------------------------------------------------------

variable "subnet_cidr" {
  description = "Primary CIDR range for the VPC subnet."
  type        = string
  default     = "10.30.0.0/20"
}

variable "serverless_connector_cidr" {
  description = "Dedicated /28 CIDR for the Serverless VPC Access connector that links Cloud Run to the VPC (Cloud SQL private IP, Memorystore)."
  type        = string
  default     = "10.30.16.0/28"
}

variable "private_service_access_address" {
  description = "Start address of the /16 block reserved for Private Service Access (Cloud SQL private IP, Memorystore). Set explicitly so the allocation is deterministic rather than auto-selected by GCP. Must not overlap subnet_cidr or serverless_connector_cidr; the default 10.40.0.0/16 is well clear of the default 10.30.0.0/20 subnet and 10.30.16.0/28 connector ranges."
  type        = string
  default     = "10.40.0.0"
}

# ----------------------------------------------------------------------------
# Container image
# ----------------------------------------------------------------------------

variable "app_image" {
  description = "Container image repository for all ZK Drive binaries."
  type        = string
  default     = "ghcr.io/kennguy3n/zk-drive"
}

variable "app_version" {
  description = "Tag of the ZK Drive image to deploy."
  type        = string
  default     = "0.1.0"
}

# ----------------------------------------------------------------------------
# Cloud SQL (Postgres 16)
# ----------------------------------------------------------------------------

variable "cloudsql_tier" {
  description = "Machine tier for the Cloud SQL Postgres instance."
  type        = string
  default     = "db-custom-2-8192"
}

variable "cloudsql_disk_size_gb" {
  description = "Initial Cloud SQL disk size (GiB). Storage autoresizes upward."
  type        = number
  default     = 50
}

variable "db_name" {
  description = "Initial Postgres database name."
  type        = string
  default     = "zkdrive"
}

variable "db_username" {
  description = "Postgres user the application connects as."
  type        = string
  default     = "zkdrive"
}

# ----------------------------------------------------------------------------
# Memorystore (Redis)
# ----------------------------------------------------------------------------

variable "redis_memory_size_gb" {
  description = "Memorystore Redis capacity (GiB)."
  type        = number
  default     = 1
}

variable "redis_version" {
  description = "Memorystore Redis engine version."
  type        = string
  default     = "REDIS_7_0"
}

# ----------------------------------------------------------------------------
# Cloud Run sizing. Limits mirror deploy/docker-compose.prod.yml.
# ----------------------------------------------------------------------------

variable "server_cpu" {
  description = "Cloud Run CPU for the server service."
  type        = string
  default     = "1"
}

variable "server_memory" {
  description = "Cloud Run memory for the server service."
  type        = string
  default     = "512Mi"
}

variable "server_min_instances" {
  description = "Minimum server instances (kept warm)."
  type        = number
  default     = 1
}

variable "server_max_instances" {
  description = "Maximum server instances under autoscaling."
  type        = number
  default     = 10
}

variable "server_concurrency" {
  description = "Max concurrent requests per server instance. Cloud Run scales out when this is exceeded; 80 ~= 200 req/s at 400ms p50."
  type        = number
  default     = 80
}

variable "worker_cpu" {
  description = "Cloud Run CPU for the worker service."
  type        = string
  default     = "1"
}

variable "worker_memory" {
  description = "Cloud Run memory for the worker service."
  type        = string
  default     = "512Mi"
}

variable "worker_min_instances" {
  description = "Minimum worker instances (kept warm for NATS consumers)."
  type        = number
  default     = 1
}

variable "worker_max_instances" {
  description = "Maximum worker instances under autoscaling."
  type        = number
  default     = 6
}

# ----------------------------------------------------------------------------
# Application configuration (non-secret). Names mirror the env vars read
# by internal/config/config.go.
# ----------------------------------------------------------------------------

variable "fabric_console_url" {
  description = "Base URL of the zk-object-fabric console API (FABRIC_CONSOLE_URL)."
  type        = string
  default     = ""
}

variable "fabric_endpoint" {
  description = "S3 endpoint of the zk-object-fabric storage gateway (S3_ENDPOINT)."
  type        = string
  default     = ""
}

variable "fabric_bucket" {
  description = "Object-storage bucket on the fabric gateway (S3_BUCKET). NOT the frontend-assets bucket."
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

variable "nats_url" {
  description = "NATS JetStream URL (NATS_URL) reachable from the VPC. On GCP, NATS is expected to run on GKE/Compute or a managed offering; provide its in-VPC address here. REQUIRED: the worker has no NATS-less mode and falls back to nats://localhost:4222, which does not exist in a Cloud Run instance, so an empty value crash-loops the worker."
  type        = string
  default     = ""

  validation {
    # The worker (cmd/worker/main.go) unconditionally dials NATS and falls back
    # to nats://localhost:4222 when NATS_URL is empty; there is no localhost NATS
    # in a Cloud Run instance, so the worker would fail to connect and Cloud Run
    # would restart it in a loop. Fail at plan time with a clear message instead.
    # (The server treats NATS as optional, but this module always deploys the
    # worker, so nats_url is effectively required.) clamav_address is NOT guarded
    # here because ClamAV is genuinely optional (cmd/worker/main.go scan service
    # tolerates an empty address).
    condition     = var.nats_url != ""
    error_message = "nats_url is required: the worker has no NATS-less mode and would crash-loop against the localhost fallback in Cloud Run. Pass -var 'nats_url=nats://<in-vpc-host>:4222'."
  }
}

variable "clamav_address" {
  description = "ClamAV clamd address host:port (CLAMAV_ADDRESS) reachable from the VPC. Optional: leave empty to disable virus scanning (the worker's scan service tolerates an empty address)."
  type        = string
  default     = ""
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

# ----------------------------------------------------------------------------
# Secret seed values.
# ----------------------------------------------------------------------------

variable "stripe_secret_key" {
  description = "STRIPE_SECRET_KEY value to seed into Secret Manager. Optional."
  type        = string
  default     = ""
  sensitive   = true
}

variable "stripe_webhook_secret" {
  description = "STRIPE_WEBHOOK_SECRET value to seed into Secret Manager. Optional."
  type        = string
  default     = ""
  sensitive   = true
}

variable "notification_channels" {
  description = "Cloud Monitoring notification channel IDs the alert policies notify (e.g. email/PagerDuty/Slack channels created out of band). Empty means policies fire without notifications."
  type        = list(string)
  default     = []
}

variable "cloudsql_max_connections" {
  description = "Configured Postgres max_connections, used to derive the 80% alert threshold."
  type        = number
  default     = 200
}

variable "labels" {
  description = "Additional labels merged onto every resource."
  type        = map(string)
  default     = {}
}
