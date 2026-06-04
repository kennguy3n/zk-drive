# Scheduled maintenance for GCP.
#
# The reconciler and orphan-GC run in-process inside the worker Cloud Run
# service (RECONCILE_INTERVAL_MINUTES / GC_INTERVAL_MINUTES, both > 0 by
# default), so they need no external scheduler. The audit-archiver is a
# SEPARATE binary (cmd/audit-archiver) that the worker does NOT run in-process,
# so without a dedicated trigger audit archival could never run on GCP — the
# AWS module already covers it with an EventBridge-scheduled ECS task. This
# file restores that parity with a Cloud Run Job invoked daily at 03:47 UTC by
# Cloud Scheduler, matching deploy/k8s/audit-archiver-cronjob.yaml ("47 3 * * *")
# and the AWS cron cadence.

locals {
  # Same secret env as the long-lived services, but DATABASE_URL points at the
  # direct (proxy-less) private-IP secret: the job is one-shot, so it can't run
  # a non-exiting Cloud SQL Proxy sidecar. All other secrets are reused as-is —
  # the archiver calls the same config.Load() as the worker.
  cron_secret_env = merge(local.app_secret_env, {
    DATABASE_URL = google_secret_manager_secret.database_url_direct.secret_id
  })
}

resource "google_cloud_run_v2_job" "audit_archiver" {
  name     = "${local.name}-audit-archiver"
  location = var.region

  labels = local.common_labels

  template {
    template {
      service_account = google_service_account.app.email

      # Daily cadence with a no-op-on-disabled binary: don't hammer retries on a
      # transient failure, just wait for tomorrow's run.
      max_retries = 1
      # Archiving a day of audit logs can outlast the 10-minute default.
      timeout = "3600s"

      vpc_access {
        connector = google_vpc_access_connector.this.id
        egress    = "PRIVATE_RANGES_ONLY"
      }

      containers {
        image   = "${var.app_image}:${var.app_version}"
        command = ["/app/audit-archiver"]

        # The archiver is opt-in: it exits zero as a no-op unless this is truthy
        # (cmd/audit-archiver/main.go). The shared app_env doesn't carry it, so
        # without this env the daily job would always no-op. Mirrors the K8s
        # CronJob, which sets it explicitly.
        env {
          name  = "AUDIT_LOG_ARCHIVE_ENABLED"
          value = tostring(var.audit_log_archive_enabled)
        }

        dynamic "env" {
          for_each = local.app_env
          content {
            name  = env.key
            value = env.value
          }
        }

        dynamic "env" {
          for_each = local.cron_secret_env
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
      }
    }
  }

  # Same secret-version + secretAccessor race as the Cloud Run services (see the
  # note in cloudrun.tf): the job reads secret_key_ref version="latest" and must
  # wait for the versions and the app SA's accessor grant before resolving them.
  depends_on = [
    google_project_service.this,
    google_secret_manager_secret_version.database_url_direct,
    google_secret_manager_secret_version.jwt,
    google_secret_manager_secret_version.credential_encryption_key,
    google_secret_manager_secret_version.s3_access_key,
    google_secret_manager_secret_version.s3_secret_key,
    google_secret_manager_secret_version.stripe_secret_key,
    google_secret_manager_secret_version.stripe_webhook_secret,
    google_secret_manager_secret_iam_member.app,
  ]
}

# Dedicated identity for Cloud Scheduler so the invoke permission is scoped to
# exactly this job (least privilege) rather than reusing the app SA.
resource "google_service_account" "scheduler" {
  account_id   = "${var.name_prefix}-${var.environment}-scheduler"
  display_name = "ZK Drive Cloud Scheduler invoker"
}

# run.invoker carries run.jobs.run, the permission Cloud Scheduler needs to
# execute the job. Scoped to this job only.
resource "google_cloud_run_v2_job_iam_member" "scheduler_invoke" {
  name     = google_cloud_run_v2_job.audit_archiver.name
  location = google_cloud_run_v2_job.audit_archiver.location
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}

resource "google_cloud_scheduler_job" "audit_archiver" {
  name      = "${local.name}-audit-archiver"
  region    = var.region
  schedule  = "47 3 * * *"
  time_zone = "Etc/UTC"

  # Hit the Cloud Run Admin API :run endpoint as the scheduler SA. This is the
  # documented "run a job on a schedule" pattern; the OAuth audience defaults to
  # the target URI, which is a *.googleapis.com host.
  http_target {
    http_method = "POST"
    uri         = "https://${var.region}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${var.project_id}/jobs/${google_cloud_run_v2_job.audit_archiver.name}:run"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }

  depends_on = [
    google_project_service.this,
    google_cloud_run_v2_job_iam_member.scheduler_invoke,
  ]
}
