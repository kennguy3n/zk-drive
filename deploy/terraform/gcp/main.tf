# Provider, project APIs, and the network substrate (VPC, subnet,
# firewall rules, private service access for Cloud SQL, and the
# Serverless VPC Access connector that links Cloud Run to the VPC).

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

locals {
  name = "${var.name_prefix}-${var.environment}"

  common_labels = merge(
    {
      project     = "zk-drive"
      environment = var.environment
      managed-by  = "terraform"
    },
    var.labels,
  )

  # Note: unlike the AWS module there is no `has_domain` toggle here. On GCP a
  # domain is mandatory (var.domain_name has a non-empty validation), because
  # the external HTTPS LB's Google-managed cert and Cloud Run's internal-only
  # ingress leave no domain-less serving path. So var.domain_name is used
  # directly rather than behind an always-true conditional.

  # APIs the module needs enabled on the project.
  services = [
    "compute.googleapis.com",
    "run.googleapis.com",
    "sqladmin.googleapis.com",
    "redis.googleapis.com",
    "secretmanager.googleapis.com",
    "servicenetworking.googleapis.com",
    "vpcaccess.googleapis.com",
    "monitoring.googleapis.com",
    # Cloud Scheduler triggers the audit-archiver Cloud Run Job (cronjobs.tf).
    "cloudscheduler.googleapis.com",
  ]
}

resource "google_project_service" "this" {
  for_each = toset(local.services)

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ----------------------------------------------------------------------------
# VPC + subnet
# ----------------------------------------------------------------------------

resource "google_compute_network" "this" {
  name                    = "${local.name}-vpc"
  auto_create_subnetworks = false

  depends_on = [google_project_service.this]
}

resource "google_compute_subnetwork" "this" {
  name          = "${local.name}-subnet"
  ip_cidr_range = var.subnet_cidr
  region        = var.region
  network       = google_compute_network.this.id

  private_ip_google_access = true
}

# ----------------------------------------------------------------------------
# Firewall
# ----------------------------------------------------------------------------

# Allow internal traffic within the VPC (app <-> Cloud SQL proxy,
# Memorystore, NATS/ClamAV running in-VPC).
resource "google_compute_firewall" "internal" {
  name      = "${local.name}-allow-internal"
  network   = google_compute_network.this.id
  direction = "INGRESS"

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }

  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }

  allow {
    protocol = "icmp"
  }

  source_ranges = [var.subnet_cidr, var.serverless_connector_cidr]
}

# Allow Google Cloud health checkers to reach backends behind the LB.
resource "google_compute_firewall" "health_checks" {
  name      = "${local.name}-allow-health-checks"
  network   = google_compute_network.this.id
  direction = "INGRESS"

  allow {
    protocol = "tcp"
  }

  # Documented Google health-check / LB source ranges.
  source_ranges = ["130.211.0.0/22", "35.191.0.0/16"]
}

# ----------------------------------------------------------------------------
# Private Service Access for Cloud SQL private IP
# ----------------------------------------------------------------------------

# PSA range for Cloud SQL/Memorystore private IPs. The start address is pinned
# (var.private_service_access_address) so the allocation is deterministic and
# can't drift onto a range that a future subnet expansion might want — rather
# than letting GCP auto-pick from the VPC's free space. Default 10.40.0.0/16 is
# well clear of the subnet (10.30.0.0/20) and connector (10.30.16.0/28).
resource "google_compute_global_address" "private_service_range" {
  name          = "${local.name}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  address       = var.private_service_access_address
  prefix_length = 16
  network       = google_compute_network.this.id
}

resource "google_service_networking_connection" "private_vpc" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_service_range.name]

  depends_on = [google_project_service.this]
}

# ----------------------------------------------------------------------------
# Serverless VPC Access connector (Cloud Run -> VPC)
# ----------------------------------------------------------------------------

# Serverless VPC Access connector names are capped at 25 characters by the
# API. "${local.name}-conn" is "<name_prefix>-<environment>-conn" (24 chars at
# the defaults), so a longer name_prefix/environment would otherwise fail with
# an opaque error at apply. The precondition surfaces it at plan time instead.
resource "google_vpc_access_connector" "this" {
  name          = "${local.name}-conn"
  region        = var.region
  network       = google_compute_network.this.name
  ip_cidr_range = var.serverless_connector_cidr

  min_instances = 2
  max_instances = 3

  depends_on = [google_project_service.this]

  lifecycle {
    precondition {
      condition     = length("${local.name}-conn") <= 25
      error_message = "VPC Access connector name \"${local.name}-conn\" is ${length("${local.name}-conn")} chars; the limit is 25. Shorten var.name_prefix or var.environment."
    }
  }
}
