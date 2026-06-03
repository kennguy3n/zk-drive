# External HTTPS load balancer fronting the platform. Static assets are
# served from a CDN-backed GCS bucket (cdn.tf); /api/* and /healthz are
# routed to the zk-drive-server Cloud Run service via a serverless NEG.
# A Google-managed SSL certificate terminates TLS. `domain_name` must be
# set for `terraform apply` (managed certs require a domain).

resource "google_compute_region_network_endpoint_group" "server" {
  name                  = "${local.name}-server-neg"
  region                = var.region
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = google_cloud_run_v2_service.server.name
  }
}

resource "google_compute_backend_service" "app" {
  name                  = "${local.name}-app"
  protocol              = "HTTP"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  port_name             = "http"
  timeout_sec           = 60

  backend {
    group = google_compute_region_network_endpoint_group.server.id
  }

  log_config {
    enable      = true
    sample_rate = 1.0
  }
}

resource "google_compute_url_map" "this" {
  name = "${local.name}-urlmap"

  # Static assets from the CDN bucket by default.
  default_service = google_compute_backend_bucket.frontend.id

  host_rule {
    hosts        = local.has_domain ? [var.domain_name] : ["*"]
    path_matcher = "main"
  }

  path_matcher {
    name            = "main"
    default_service = google_compute_backend_bucket.frontend.id

    path_rule {
      paths   = ["/api", "/api/*"]
      service = google_compute_backend_service.app.id
    }

    path_rule {
      paths   = ["/healthz"]
      service = google_compute_backend_service.app.id
    }
  }
}

resource "google_compute_managed_ssl_certificate" "this" {
  name = "${local.name}-cert"

  managed {
    domains = [var.domain_name]
  }
}

resource "google_compute_target_https_proxy" "this" {
  name             = "${local.name}-https"
  url_map          = google_compute_url_map.this.id
  ssl_certificates = [google_compute_managed_ssl_certificate.this.id]
}

resource "google_compute_global_address" "this" {
  name = "${local.name}-ip"
}

resource "google_compute_global_forwarding_rule" "https" {
  name                  = "${local.name}-https"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.this.id
  port_range            = "443"
  target                = google_compute_target_https_proxy.this.id
}

# HTTP -> HTTPS redirect.
resource "google_compute_url_map" "redirect" {
  name = "${local.name}-redirect"

  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_compute_target_http_proxy" "redirect" {
  name    = "${local.name}-http"
  url_map = google_compute_url_map.redirect.id
}

resource "google_compute_global_forwarding_rule" "http" {
  name                  = "${local.name}-http"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_address            = google_compute_global_address.this.id
  port_range            = "80"
  target                = google_compute_target_http_proxy.redirect.id
}
