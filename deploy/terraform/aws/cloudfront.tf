# CloudFront distribution serving the SPA static assets from the private S3
# bucket, while proxying /api/* and /healthz to the ALB so the browser sees a
# single origin (matching the same-origin posture the security headers / CSP
# expect). Preview thumbnails are NOT served here — they live in
# zk-object-fabric and are fetched through the authenticated /api/* path.

resource "aws_cloudfront_origin_access_control" "frontend" {
  name                              = "${local.name}-frontend"
  description                       = "OAC for ZK Drive frontend bucket"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

locals {
  s3_origin_id  = "frontend-s3"
  alb_origin_id = "api-alb"
}

# SPA fallback, scoped to the S3 origin only. A viewer-request function
# rewrites extensionless paths (client-side routes like /drive, /login) to
# /index.html so a hard refresh resolves to the SPA shell. This is attached
# ONLY to the default (S3) cache behavior, so it never touches the /api/* or
# /healthz behaviors. We deliberately do NOT use a distribution-level
# custom_error_response: that applies to every origin, so an honest 403/404
# from the ALB API origin would be rewritten to 200 + index.html, silently
# breaking the JSON error contract the frontend relies on.
resource "aws_cloudfront_function" "spa_rewrite" {
  name    = "${local.name}-spa-rewrite"
  runtime = "cloudfront-js-2.0"
  comment = "Rewrite extensionless SPA routes to /index.html (S3 origin only)"
  publish = true
  code    = <<-EOT
    function handler(event) {
      var request = event.request;
      var uri = request.uri;
      var lastSegment = uri.slice(uri.lastIndexOf('/') + 1);
      // Requests for a concrete file (anything with an extension, e.g.
      // /assets/app.123.js) pass through to S3 unchanged; everything else is
      // treated as a client-side route and served the SPA shell.
      if (lastSegment.indexOf('.') === -1) {
        request.uri = '/index.html';
      }
      return request;
    }
  EOT
}

resource "aws_cloudfront_distribution" "this" {
  enabled             = true
  comment             = "${local.name} frontend + API"
  default_root_object = "index.html"
  price_class         = "PriceClass_100"
  aliases             = local.has_domain ? [var.domain_name] : []

  # S3 origin for static assets.
  origin {
    origin_id                = local.s3_origin_id
    domain_name              = aws_s3_bucket.frontend.bucket_regional_domain_name
    origin_access_control_id = aws_cloudfront_origin_access_control.frontend.id
  }

  # ALB origin for the API. CloudFront connects to the ALB over plain HTTP on
  # port 80: CloudFront already terminates viewer TLS, and the hop to the ALB
  # stays inside AWS. Using HTTPS here would fail the origin TLS handshake
  # because the ACM cert covers var.domain_name, not the ALB's generated DNS
  # name (aws_lb.this.dns_name) that CloudFront dials.
  origin {
    origin_id   = local.alb_origin_id
    domain_name = aws_lb.this.dns_name

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "http-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # Default: static assets from S3, cached aggressively.
  default_cache_behavior {
    target_origin_id       = local.s3_origin_id
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true

    forwarded_values {
      query_string = false
      cookies {
        forward = "none"
      }
    }

    min_ttl     = 0
    default_ttl = 3600
    max_ttl     = 86400

    function_association {
      event_type   = "viewer-request"
      function_arn = aws_cloudfront_function.spa_rewrite.arn
    }
  }

  # API: forward everything to the ALB, no caching.
  ordered_cache_behavior {
    path_pattern           = "/api/*"
    target_origin_id       = local.alb_origin_id
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true

    forwarded_values {
      query_string = true
      headers      = ["*"]
      cookies {
        forward = "all"
      }
    }

    min_ttl     = 0
    default_ttl = 0
    max_ttl     = 0
  }

  # Health check passthrough.
  ordered_cache_behavior {
    path_pattern           = "/healthz"
    target_origin_id       = local.alb_origin_id
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]

    forwarded_values {
      query_string = false
      cookies {
        forward = "none"
      }
    }

    min_ttl     = 0
    default_ttl = 0
    max_ttl     = 0
  }

  # SPA fallback is handled by the viewer-request function on the default
  # (S3) behavior above — see aws_cloudfront_function.spa_rewrite. No
  # distribution-level custom_error_response, so API 403/404 from the ALB
  # origin pass through to the client untouched.

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  # Use the ACM cert (us-east-1, required by CloudFront) when a custom
  # domain is configured; otherwise fall back to the default
  # *.cloudfront.net certificate.
  dynamic "viewer_certificate" {
    for_each = local.has_domain ? [1] : []
    content {
      acm_certificate_arn      = aws_acm_certificate.cloudfront[0].arn
      ssl_support_method       = "sni-only"
      minimum_protocol_version = "TLSv1.2_2021"
    }
  }

  dynamic "viewer_certificate" {
    for_each = local.has_domain ? [] : [1]
    content {
      cloudfront_default_certificate = true
    }
  }

  tags = {
    Name = "${local.name}-cdn"
  }
}

# CloudFront requires its ACM certificate in us-east-1. Created via the
# aliased provider so the distribution can use a custom domain.
resource "aws_acm_certificate" "cloudfront" {
  count    = local.has_domain ? 1 : 0
  provider = aws.us_east_1

  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = {
    Name = "${local.name}-cloudfront-cert"
  }
}
