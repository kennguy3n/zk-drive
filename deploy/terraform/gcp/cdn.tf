# Cloud CDN for the frontend static assets, backed by a GCS bucket attached
# to the external HTTPS load balancer (lb.tf). This bucket holds ONLY the
# built frontend (frontend/dist) — public web artifacts (JS/CSS/HTML). It is
# NOT the object-storage bucket for user files, and it does NOT hold preview
# thumbnails: both live on the zk-object-fabric gateway (S3_BUCKET), served
# through the app behind auth (internal/preview/preview.go).

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

# Grant public read on the bucket objects. This is the standard pattern for a
# GCS bucket fronted by Cloud CDN (a backend bucket requires either public
# objects or signed URLs/cookies). It is safe here because the bucket holds
# only the public frontend build (JS/CSS/HTML) — the same bytes any visitor
# downloads to run the SPA. No user data or preview thumbnails are ever stored
# here (those are in zk-object-fabric and served through the authenticated
# app), so there is nothing sensitive to expose. This differs from the AWS
# module's CloudFront-OAC + private-bucket setup only because GCS+Cloud CDN
# has no OAC equivalent; the effective exposure (public static assets) is the
# same on both clouds.
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
