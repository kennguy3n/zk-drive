# Cloud SQL for PostgreSQL 16, regional (HA) with a private IP on the VPC.
#
# Cloud Run reaches the instance through the Cloud SQL Proxy sidecar (see
# cloudrun.tf), which terminates at 127.0.0.1:5432 inside each instance —
# the GCP analogue of the PgBouncer sidecar in the AWS module.

resource "google_sql_database_instance" "this" {
  name             = "${local.name}-pg"
  database_version = "POSTGRES_16"
  region           = var.region

  deletion_protection = true

  depends_on = [google_service_networking_connection.private_vpc]

  settings {
    tier              = var.cloudsql_tier
    availability_type = "REGIONAL"
    disk_type         = "PD_SSD"
    disk_size         = var.cloudsql_disk_size_gb
    disk_autoresize   = true

    user_labels = local.common_labels

    ip_configuration {
      ipv4_enabled    = false
      private_network = google_compute_network.this.id
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "03:00"
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = 14
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day  = 7
      hour = 4
    }

    insights_config {
      query_insights_enabled = true
    }
  }
}

# Read replica for read-heavy traffic (search, previews, exports).
resource "google_sql_database_instance" "replica" {
  name                 = "${local.name}-pg-replica"
  database_version     = "POSTGRES_16"
  region               = var.region
  master_instance_name = google_sql_database_instance.this.name

  deletion_protection = false

  replica_configuration {
    failover_target = false
  }

  settings {
    tier              = var.cloudsql_tier
    availability_type = "ZONAL"
    disk_type         = "PD_SSD"
    disk_autoresize   = true

    user_labels = local.common_labels

    ip_configuration {
      ipv4_enabled    = false
      private_network = google_compute_network.this.id
    }
  }
}

resource "google_sql_database" "this" {
  name     = var.db_name
  instance = google_sql_database_instance.this.name
}

resource "google_sql_user" "this" {
  name     = var.db_username
  instance = google_sql_database_instance.this.name
  password = random_password.db.result
}
