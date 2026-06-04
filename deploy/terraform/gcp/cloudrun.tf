# Cloud Run v2 services for the zk-drive-server and zk-drive-worker
# binaries. Each runs the application container alongside a Cloud SQL
# Proxy sidecar (the GCP analogue of the PgBouncer sidecar in the AWS
# module); the app reaches Postgres at 127.0.0.1:5432 via the DATABASE_URL
# secret.

resource "google_service_account" "app" {
  account_id   = "${var.name_prefix}-${var.environment}-app"
  display_name = "ZK Drive Cloud Run service account"
}

# The application connects to Cloud SQL through the proxy sidecar.
resource "google_project_iam_member" "app_cloudsql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.app.email}"
}

# Allow the app SA to write custom metrics (worker NATS-pending gauge).
resource "google_project_iam_member" "app_metric_writer" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.app.email}"
}

locals {
  redis_url = "redis://${google_redis_instance.this.host}:${google_redis_instance.this.port}"

  public_url = "https://${var.domain_name}"

  cloud_sql_proxy_image = "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.11.4"

  # Non-secret application config shared by server + worker. Names mirror
  # the env vars read by internal/config/config.go.
  app_env = {
    NATS_URL                 = var.nats_url
    CLAMAV_ADDRESS           = var.clamav_address
    REDIS_URL                = local.redis_url
    MIGRATIONS_DIR           = "migrations"
    RATE_LIMIT_PER_USER      = tostring(var.rate_limit_per_user)
    RATE_LIMIT_PER_WORKSPACE = tostring(var.rate_limit_per_workspace)
    S3_ENDPOINT              = var.fabric_endpoint
    S3_BUCKET                = var.fabric_bucket
    FABRIC_CONSOLE_URL       = var.fabric_console_url
    PUBLIC_URL               = local.public_url
    # Pin the credential-encryption mode explicitly rather than relying on
    # internal/crypto.LoadFromEnv auto-selecting "aesgcm" from the presence of
    # CREDENTIAL_ENCRYPTION_KEY (mirrors the AWS module).
    CREDENTIAL_ENCRYPTION = "aesgcm"
  }

  # Secret env vars: env var name -> Secret Manager secret id. Stripe entries
  # are added only when billing is configured (count-gated in secrets.tf), so
  # the env vars are simply absent otherwise and the app reports billing
  # disabled cleanly instead of reading a " " placeholder as enabled.
  app_secret_env = merge({
    DATABASE_URL              = google_secret_manager_secret.database_url.secret_id
    JWT_SECRET                = google_secret_manager_secret.jwt.secret_id
    CREDENTIAL_ENCRYPTION_KEY = google_secret_manager_secret.credential_encryption_key.secret_id
    S3_ACCESS_KEY             = google_secret_manager_secret.s3_access_key.secret_id
    S3_SECRET_KEY             = google_secret_manager_secret.s3_secret_key.secret_id
    },
    { for s in google_secret_manager_secret.stripe_secret_key : "STRIPE_SECRET_KEY" => s.secret_id },
    { for s in google_secret_manager_secret.stripe_webhook_secret : "STRIPE_WEBHOOK_SECRET" => s.secret_id },
  )
}

# ----------------------------------------------------------------------------
# Server
# ----------------------------------------------------------------------------

resource "google_cloud_run_v2_service" "server" {
  name     = "${local.name}-server"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"

  labels = local.common_labels

  template {
    service_account = google_service_account.app.email

    scaling {
      min_instance_count = var.server_min_instances
      max_instance_count = var.server_max_instances
    }

    max_instance_request_concurrency = var.server_concurrency

    vpc_access {
      connector = google_vpc_access_connector.this.id
      egress    = "PRIVATE_RANGES_ONLY"
    }

    containers {
      name    = "server"
      image   = "${var.app_image}:${var.app_version}"
      command = ["/app/server"]

      # Gate the app on the Cloud SQL Proxy sidecar being ready before it
      # starts (parity with the AWS task's dependsOn on the PgBouncer
      # sidecar). Combined with the proxy's startup_probe below, this removes
      # the brief window where the app could dial 127.0.0.1:5432 before the
      # proxy is listening and see connection refused.
      depends_on = ["cloud-sql-proxy"]

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = var.server_cpu
          memory = var.server_memory
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      env {
        name  = "LISTEN_ADDR"
        value = ":8080"
      }

      dynamic "env" {
        for_each = local.app_env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "env" {
        for_each = local.app_secret_env
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value
              version = "latest"
            }
          }
        }
      }

      startup_probe {
        http_get {
          path = "/healthz"
          port = 8080
        }
        initial_delay_seconds = 5
        period_seconds        = 5
        failure_threshold     = 12
      }

      liveness_probe {
        http_get {
          path = "/healthz"
          port = 8080
        }
        period_seconds = 30
      }
    }

    # Cloud SQL Proxy sidecar listening on 127.0.0.1:5432.
    containers {
      name  = "cloud-sql-proxy"
      image = local.cloud_sql_proxy_image
      args = [
        "--private-ip",
        "--port=5432",
        "--address=127.0.0.1",
        # Serve the proxy's HTTP health endpoints (/startup, /readiness) so the
        # app container's depends_on can wait until the proxy is actually
        # listening rather than just process-started.
        "--health-check",
        "--http-address=0.0.0.0",
        "--http-port=9090",
        google_sql_database_instance.this.connection_name,
      ]

      startup_probe {
        http_get {
          path = "/startup"
          port = 9090
        }
        period_seconds    = 2
        failure_threshold = 30
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
        cpu_idle = true
      }
    }
  }

  depends_on = [google_project_service.this]
}

# ----------------------------------------------------------------------------
# Worker
# ----------------------------------------------------------------------------

resource "google_cloud_run_v2_service" "worker" {
  name     = "${local.name}-worker"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_INTERNAL_ONLY"

  labels = local.common_labels

  template {
    service_account = google_service_account.app.email

    scaling {
      min_instance_count = var.worker_min_instances
      max_instance_count = var.worker_max_instances
    }

    vpc_access {
      connector = google_vpc_access_connector.this.id
      egress    = "PRIVATE_RANGES_ONLY"
    }

    containers {
      name    = "worker"
      image   = "${var.app_image}:${var.app_version}"
      command = ["/app/worker"]

      # Gate the worker on the Cloud SQL Proxy sidecar being ready (see the
      # server container for rationale; parity with the AWS PgBouncer dependsOn).
      depends_on = ["cloud-sql-proxy"]

      # The worker brings up a /metrics + /healthz surface on :9091
      # (WORKER_METRICS_ADDR). Cloud Run probes it as the service port.
      ports {
        container_port = 9091
      }

      resources {
        limits = {
          cpu    = var.worker_cpu
          memory = var.worker_memory
        }
        # Keep CPU allocated outside request handling so background NATS
        # consumers and the in-process reconcile/GC loops keep running.
        cpu_idle = false
      }

      env {
        name  = "WORKER_METRICS_ADDR"
        value = ":9091"
      }

      dynamic "env" {
        for_each = local.app_env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "env" {
        for_each = local.app_secret_env
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value
              version = "latest"
            }
          }
        }
      }

      startup_probe {
        http_get {
          path = "/healthz"
          port = 9091
        }
        initial_delay_seconds = 5
        period_seconds        = 5
        failure_threshold     = 12
      }

      # Restart the worker if its /healthz surface stops responding after
      # startup (e.g. a wedged NATS consumer). Mirrors the server's probe so
      # both services self-heal rather than silently going idle.
      liveness_probe {
        http_get {
          path = "/healthz"
          port = 9091
        }
        period_seconds = 30
      }
    }

    containers {
      name  = "cloud-sql-proxy"
      image = local.cloud_sql_proxy_image
      args = [
        "--private-ip",
        "--port=5432",
        "--address=127.0.0.1",
        # Serve the proxy's HTTP health endpoints (/startup, /readiness) so the
        # worker container's depends_on can wait until the proxy is actually
        # listening rather than just process-started.
        "--health-check",
        "--http-address=0.0.0.0",
        "--http-port=9090",
        google_sql_database_instance.this.connection_name,
      ]

      startup_probe {
        http_get {
          path = "/startup"
          port = 9090
        }
        period_seconds    = 2
        failure_threshold = 30
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
        cpu_idle = true
      }
    }
  }

  depends_on = [google_project_service.this]
}

# The external HTTPS load balancer (lb.tf) invokes the server as any
# end user, so the server must allow unauthenticated invocation. Access
# is still gated by the LB + the application's own auth.
resource "google_cloud_run_v2_service_iam_member" "server_public" {
  name     = google_cloud_run_v2_service.server.name
  location = google_cloud_run_v2_service.server.location
  role     = "roles/run.invoker"
  member   = "allUsers"
}
