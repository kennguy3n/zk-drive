# Application Load Balancer terminating TLS (ACM) and forwarding to the
# zk-drive-server ECS service. Health checks hit /healthz, matching the
# probe used by the Kubernetes manifests and docker-compose.prod.yml.

resource "aws_lb" "this" {
  name               = "${local.name}-alb"
  load_balancer_type = "application"
  internal           = false
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  drop_invalid_header_fields = true
  enable_http2               = true

  # Headroom over the app's ~54s WebSocket keepalive ping (see
  # var.alb_idle_timeout) so a delayed ping doesn't drop a live collab session
  # at the AWS default of 60s.
  idle_timeout = var.alb_idle_timeout

  tags = {
    Name = "${local.name}-alb"
  }
}

resource "aws_lb_target_group" "server" {
  name        = "${local.name}-server"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.this.id
  target_type = "ip"

  health_check {
    enabled             = true
    path                = "/healthz"
    port                = "traffic-port"
    protocol            = "HTTP"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30

  tags = {
    Name = "${local.name}-server"
  }
}

# ACM certificate for the public domain, used by the optional direct-HTTPS
# listener below. Only created when a custom domain is configured — with no
# domain the stack is still fully usable through CloudFront's default
# *.cloudfront.net domain (CloudFront reaches the ALB over HTTP/80). ACM
# rejects an empty domain_name, so guarding this keeps `terraform apply`
# working with all defaults. DNS validation records are surfaced in
# outputs.tf (this module does not assume Route 53 hosting).
resource "aws_acm_certificate" "this" {
  count             = local.has_domain ? 1 : 0
  domain_name       = var.domain_name
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }

  tags = {
    Name = "${local.name}-cert"
  }
}

# Port 80 forwards to the server target group rather than redirecting to
# HTTPS: this listener is the CloudFront origin (CloudFront connects over
# HTTP — see cloudfront.tf — and terminates viewer TLS itself). The ALB SG
# restricts ingress to CloudFront's origin-facing prefix list (see main.tf),
# so this hop is not reachable directly from the public internet.
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.server.arn
  }
}

# Optional direct-HTTPS path on the ALB, created only with a custom domain
# (it needs the ACM cert above). CloudFront itself talks to the origin over
# HTTP/80; this listener exists for operators who also want to reach the ALB
# directly over TLS on their domain.
resource "aws_lb_listener" "https" {
  count             = local.has_domain ? 1 : 0
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate.this[0].arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.server.arn
  }
}
