# Cloud Monitoring alerting policies mirroring the AWS module's alarms:
# Cloud Run CPU > 80%, Cloud SQL connections > 80% of max, and an HTTPS LB
# 5xx error rate > 1%. Attach notification channels via
# var.notification_channels.

# --- Cloud Run CPU > 80% (server + worker) ---------------------------------
resource "google_monitoring_alert_policy" "cloudrun_cpu" {
  display_name = "${local.name} Cloud Run CPU > 80%"
  combiner     = "OR"

  conditions {
    display_name = "Container CPU utilization > 80%"

    condition_threshold {
      filter          = "resource.type = \"cloud_run_revision\" AND metric.type = \"run.googleapis.com/container/cpu/utilizations\" AND (resource.label.\"service_name\" = \"${google_cloud_run_v2_service.server.name}\" OR resource.label.\"service_name\" = \"${google_cloud_run_v2_service.worker.name}\")"
      comparison      = "COMPARISON_GT"
      threshold_value = 0.8
      duration        = "300s"

      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_PERCENTILE_99"
        cross_series_reducer = "REDUCE_MEAN"
        group_by_fields      = ["resource.label.service_name"]
      }

      trigger {
        count = 1
      }
    }
  }

  notification_channels = var.notification_channels

  user_labels = local.common_labels
}

# --- Cloud SQL connections > 80% of max ------------------------------------
resource "google_monitoring_alert_policy" "cloudsql_connections" {
  display_name = "${local.name} Cloud SQL connections > 80%"
  combiner     = "OR"

  conditions {
    display_name = "Postgres backends > 80% of max_connections"

    condition_threshold {
      filter          = "resource.type = \"cloudsql_database\" AND metric.type = \"cloudsql.googleapis.com/database/postgresql/num_backends\" AND resource.label.\"database_id\" = \"${var.project_id}:${google_sql_database_instance.this.name}\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.cloudsql_max_connections * 0.8
      duration        = "300s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MEAN"
      }

      trigger {
        count = 1
      }
    }
  }

  notification_channels = var.notification_channels

  user_labels = local.common_labels
}

# --- HTTPS LB 5xx rate > 1% -------------------------------------------------
# Ratio of 5xx responses to all responses on the backend service. Both filters
# are scoped to this deployment's URL map (resource.label.url_map_name) so the
# alert never fires on unrelated load balancers in a shared project — parity
# with the AWS alarm, which scopes to aws_lb.this.arn_suffix.
resource "google_monitoring_alert_policy" "lb_5xx_rate" {
  display_name = "${local.name} HTTPS LB 5xx rate > 1%"
  combiner     = "OR"

  conditions {
    display_name = "5xx responses exceed 1% of requests"

    condition_threshold {
      filter             = "resource.type = \"https_lb_rule\" AND metric.type = \"loadbalancing.googleapis.com/https/request_count\" AND resource.label.\"url_map_name\" = \"${google_compute_url_map.this.name}\" AND metric.label.\"response_code_class\" = \"500\""
      denominator_filter = "resource.type = \"https_lb_rule\" AND metric.type = \"loadbalancing.googleapis.com/https/request_count\" AND resource.label.\"url_map_name\" = \"${google_compute_url_map.this.name}\""
      comparison         = "COMPARISON_GT"
      threshold_value    = 0.01
      duration           = "300s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_RATE"
      }

      denominator_aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_RATE"
      }

      trigger {
        count = 1
      }
    }
  }

  notification_channels = var.notification_channels

  user_labels = local.common_labels
}
