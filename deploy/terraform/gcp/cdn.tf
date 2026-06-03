# Cloud CDN for the frontend static assets (and preview thumbnails),
# backed by a GCS bucket attached to the external HTTPS load balancer
# (lb.tf). This bucket holds the built frontend (frontend/dist) — it is
# NOT the object-storage bucket for user files (that lives on the
# zk-object-fabric gateway, S3_BUCKET).

resource "google_storage_bucket" "frontend" {
  name                        = "${local.name}-frontend-${var.project_id}"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = false

  labels = local.common_labels

  website {
    main_page_suffix = "index.html"
    # SPA fallback: unknown client-side routes resolve to index.html.
    not_found_page = "index.html"
  }

  versioning {
    enabled = true
  }
}

# Serve objects to the public CDN edge (the LB), not directly.
resource "google_storage_bucket_iam_member" "public_read" {
  bucket = google_storage_bucket.frontend.name
  role   = "roles/storage.objectViewer"
  member = "allUsers"
}

resource "google_compute_backend_bucket" "frontend" {
  name        = "${local.name}-frontend"
  bucket_name = google_storage_bucket.frontend.name
  enable_cdn  = true

  cdn_policy {
    cache_mode        = "CACHE_ALL_STATIC"
    client_ttl        = 3600
    default_ttl       = 3600
    max_ttl           = 86400
    negative_caching  = true
    serve_while_stale = 86400
  }
}
