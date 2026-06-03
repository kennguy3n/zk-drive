# CloudFront distribution serving the SPA static assets (and preview
# thumbnails) from the private S3 bucket, while proxying /api/* and
# /healthz to the ALB so the browser sees a single origin (matching the
# same-origin posture the security headers / CSP expect).

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

  # ALB origin for the API. CloudFront -> ALB is HTTPS.
  origin {
    origin_id   = local.alb_origin_id
    domain_name = aws_lb.this.dns_name

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
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

  # SPA fallback: client-side routes (/drive, /login, ...) resolve to
  # index.html on a hard refresh.
  custom_error_response {
    error_code            = 403
    response_code         = 200
    response_page_path    = "/index.html"
    error_caching_min_ttl = 10
  }

  custom_error_response {
    error_code            = 404
    response_code         = 200
    response_page_path    = "/index.html"
    error_caching_min_ttl = 10
  }

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
