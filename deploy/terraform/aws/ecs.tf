# ECS Fargate cluster, IAM, task definitions, services, and autoscaling
# for the zk-drive-server and zk-drive-worker binaries. Each task pairs the
# application container with a PgBouncer sidecar that pools connections to
# the RDS primary (the app reaches Postgres at 127.0.0.1:6432).

data "aws_caller_identity" "current" {}

resource "aws_ecs_cluster" "this" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = {
    Name = local.name
  }
}

resource "aws_ecs_cluster_capacity_providers" "this" {
  cluster_name       = aws_ecs_cluster.this.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = 1
  }
}

# Private DNS namespace so the app tasks can resolve the NATS and ClamAV
# services by name (nats.<ns>, clamav.<ns>). The discovery services
# themselves are declared in nats.tf / clamav.tf. Named with the
# environment-qualified local.name (like every other resource) so two stacks
# sharing one imported VPC don't collide on the namespace name.
resource "aws_service_discovery_private_dns_namespace" "internal" {
  name        = "${local.name}.internal"
  description = "ZK Drive internal service discovery"
  vpc         = aws_vpc.this.id
}

locals {
  namespace = aws_service_discovery_private_dns_namespace.internal.name

  nats_url       = "nats://nats.${local.namespace}:4222"
  clamav_address = "clamav.${local.namespace}:3310"
  redis_scheme   = var.redis_transit_encryption ? "rediss" : "redis"
  redis_url      = "${local.redis_scheme}://${aws_elasticache_replication_group.this.primary_endpoint_address}:6379"

  # PUBLIC_URL is the externally-reachable base URL the email service uses to
  # build invite-accept links; config.go forcibly disables transactional email
  # when it's empty. With a custom domain we use it; otherwise fall back to the
  # CloudFront distribution domain, which is the URL users actually reach the
  # SPA on when no domain is wired (CloudFront serves the SPA for non-/api
  # paths, so invite links resolve). This keeps email functional out of the box
  # for domainless deployments instead of silently disabling it. No dependency
  # cycle: CloudFront depends only on the S3 bucket + ALB, not on ECS.
  public_url = local.has_domain ? "https://${var.domain_name}" : "https://${aws_cloudfront_distribution.this.domain_name}"

  # Non-secret application config shared by server + worker, in the ECS
  # `environment` shape ({ name, value }). Names mirror the env vars read
  # by internal/config/config.go.
  # REDIS_URL is a plaintext env var in the common case, but when Redis AUTH is
  # enabled the URL embeds the token, so it's injected via the task `secrets`
  # block instead (secrets.tf local.redis_url_secrets) and omitted here to keep
  # the credential out of the ECS task-definition environment.
  app_environment = concat([
    { name = "NATS_URL", value = local.nats_url },
    { name = "CLAMAV_ADDRESS", value = local.clamav_address },
    { name = "LISTEN_ADDR", value = ":8080" },
    { name = "MIGRATIONS_DIR", value = "migrations" },
    { name = "RATE_LIMIT_PER_USER", value = tostring(var.rate_limit_per_user) },
    { name = "RATE_LIMIT_PER_WORKSPACE", value = tostring(var.rate_limit_per_workspace) },
    { name = "S3_ENDPOINT", value = var.fabric_endpoint },
    { name = "S3_BUCKET", value = var.fabric_bucket },
    { name = "FABRIC_CONSOLE_URL", value = var.fabric_console_url },
    { name = "PUBLIC_URL", value = local.public_url },
    # Worker /metrics listen address; matches config.go's :9091 default and the
    # GCP module. Harmless on the server binary, which ignores it.
    { name = "WORKER_METRICS_ADDR", value = ":9091" },
    # Pin the credential-encryption mode explicitly. internal/crypto.LoadFromEnv
    # would auto-select "aesgcm" from the presence of CREDENTIAL_ENCRYPTION_KEY,
    # but setting it removes the dependence on that implicit fallback (a future
    # change to the default must not silently downgrade us to "none").
    { name = "CREDENTIAL_ENCRYPTION", value = "aesgcm" },
    ],
    var.redis_auth_token_enabled ? [] : [{ name = "REDIS_URL", value = local.redis_url }],
  )

  # PgBouncer sidecar definition shared by server + worker tasks. Pools
  # connections to the RDS primary; the app connects at 127.0.0.1:6432 via
  # the DATABASE_URL secret.
  pgbouncer_container = {
    name      = "pgbouncer"
    image     = "bitnami/pgbouncer:1.22.1"
    essential = true
    environment = [
      { name = "POSTGRESQL_HOST", value = aws_db_instance.primary.address },
      { name = "POSTGRESQL_PORT", value = "5432" },
      { name = "POSTGRESQL_USERNAME", value = var.db_username },
      { name = "PGBOUNCER_DATABASE", value = var.db_name },
      { name = "PGBOUNCER_PORT", value = "6432" },
      { name = "PGBOUNCER_AUTH_TYPE", value = "scram-sha-256" },
      { name = "PGBOUNCER_POOL_MODE", value = "transaction" },
      { name = "PGBOUNCER_MAX_CLIENT_CONN", value = "500" },
      { name = "PGBOUNCER_DEFAULT_POOL_SIZE", value = "25" },
    ]
    secrets = [
      { name = "POSTGRESQL_PASSWORD", valueFrom = aws_secretsmanager_secret.db_password.arn },
    ]
    portMappings = [
      { containerPort = 6432, protocol = "tcp" },
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.app.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "pgbouncer"
      }
    }
  }
}

# ----------------------------------------------------------------------------
# IAM
# ----------------------------------------------------------------------------

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# Execution role: pulls images, writes logs, reads the injected secrets.
resource "aws_iam_role" "task_execution" {
  name               = "${local.name}-task-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "task_execution_managed" {
  role       = aws_iam_role.task_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "secrets_read" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = local.app_secret_arns
  }
}

resource "aws_iam_role_policy" "task_execution_secrets" {
  name   = "${local.name}-secrets-read"
  role   = aws_iam_role.task_execution.id
  policy = data.aws_iam_policy_document.secrets_read.json
}

# Lean execution role for infrastructure tasks (NATS, ClamAV) that inject no
# application secrets. It carries only the managed ECR-pull + CloudWatch-logs
# permissions and deliberately omits the secrets-read inline policy above, so a
# compromised NATS/ClamAV container can't call secretsmanager:GetSecretValue on
# JWT_SECRET, DATABASE_URL, CREDENTIAL_ENCRYPTION_KEY, etc. (least privilege —
# those tasks never reference any secret).
resource "aws_iam_role" "task_execution_infra" {
  name               = "${local.name}-task-execution-infra"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "task_execution_infra_managed" {
  role       = aws_iam_role.task_execution_infra.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Dedicated execution role for the cron tasks (reconciler, orphan-gc,
# audit-archiver). Identical managed permissions (ECR pull + CloudWatch logs)
# as task_execution, but its secrets-read policy is scoped to local.cron_secret_arns
# — the exact set the cron tasks inject — instead of the full app_secret_arns.
# So even though cron_secrets already omits Stripe/Redis/PgBouncer DATABASE_URL
# from the task definition, the cron execution role also cannot read them,
# matching least-privilege at the IAM layer (parity with the lean infra role).
resource "aws_iam_role" "cron_execution" {
  name               = "${local.name}-task-execution-cron"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "cron_execution_managed" {
  role       = aws_iam_role.cron_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "cron_secrets_read" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = local.cron_secret_arns
  }
}

resource "aws_iam_role_policy" "cron_execution_secrets" {
  name   = "${local.name}-cron-secrets-read"
  role   = aws_iam_role.cron_execution.id
  policy = data.aws_iam_policy_document.cron_secrets_read.json
}

# Task role: the application's own AWS identity. Minimal today (CloudWatch
# metric publishing for the worker's NATS-pending gauge); extend as the app
# integrates more AWS services.
resource "aws_iam_role" "task" {
  name               = "${local.name}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

data "aws_iam_policy_document" "task" {
  statement {
    actions   = ["cloudwatch:PutMetricData"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values   = ["zk-drive"]
    }
  }
}

resource "aws_iam_role_policy" "task" {
  name   = "${local.name}-task"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}

# ----------------------------------------------------------------------------
# Task definitions
# ----------------------------------------------------------------------------

resource "aws_ecs_task_definition" "server" {
  family                   = "${local.name}-server"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.server_cpu
  memory                   = var.server_memory
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([
    {
      name        = "server"
      image       = "${var.app_image}:${var.app_version}"
      essential   = true
      entryPoint  = ["/app/server"]
      environment = local.app_environment
      secrets     = local.app_secrets
      portMappings = [
        { containerPort = 8080, protocol = "tcp" },
      ]
      dependsOn = [
        { containerName = "pgbouncer", condition = "START" },
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "wget -qO- http://localhost:8080/healthz || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "server"
        }
      }
    },
    local.pgbouncer_container,
  ])

  tags = {
    Name = "${local.name}-server"
  }
}

resource "aws_ecs_task_definition" "worker" {
  family                   = "${local.name}-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.worker_cpu
  memory                   = var.worker_memory
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([
    {
      name        = "worker"
      image       = "${var.app_image}:${var.app_version}"
      essential   = true
      entryPoint  = ["/app/worker"]
      environment = local.app_environment
      secrets     = local.app_secrets
      portMappings = [
        { containerPort = 9091, protocol = "tcp" },
      ]
      dependsOn = [
        { containerName = "pgbouncer", condition = "START" },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "worker"
        }
      }
    },
    local.pgbouncer_container,
  ])

  tags = {
    Name = "${local.name}-worker"
  }
}

# ----------------------------------------------------------------------------
# Services
# ----------------------------------------------------------------------------

resource "aws_ecs_service" "server" {
  name            = "${local.name}-server"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.server.arn
  desired_count   = var.server_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.app.id]
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.server.arn
    container_name   = "server"
    container_port   = 8080
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  # The target group must be attached to a live listener before ECS will
  # register tasks. The HTTP/80 listener (the CloudFront origin) is always
  # present; the HTTPS/443 listener is optional (custom-domain only).
  depends_on = [aws_lb_listener.http]

  lifecycle {
    ignore_changes = [desired_count]
  }

  tags = {
    Name = "${local.name}-server"
  }
}

resource "aws_ecs_service" "worker" {
  name            = "${local.name}-worker"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.worker.arn
  desired_count   = var.worker_desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.app.id]
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  lifecycle {
    ignore_changes = [desired_count]
  }

  tags = {
    Name = "${local.name}-worker"
  }
}

# ----------------------------------------------------------------------------
# Autoscaling
# ----------------------------------------------------------------------------

# Server: scale on ALB request count per target (200 req/s per instance,
# expressed per-minute) with a CPU guardrail.
resource "aws_appautoscaling_target" "server" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.server.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.server_min_count
  max_capacity       = var.server_max_count
}

resource "aws_appautoscaling_policy" "server_requests" {
  name               = "${local.name}-server-requests"
  service_namespace  = aws_appautoscaling_target.server.service_namespace
  resource_id        = aws_appautoscaling_target.server.resource_id
  scalable_dimension = aws_appautoscaling_target.server.scalable_dimension
  policy_type        = "TargetTrackingScaling"

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ALBRequestCountPerTarget"
      resource_label         = "${aws_lb.this.arn_suffix}/${aws_lb_target_group.server.arn_suffix}"
    }
    target_value       = var.server_target_requests_per_instance
    scale_in_cooldown  = 120
    scale_out_cooldown = 60
  }
}

resource "aws_appautoscaling_policy" "server_cpu" {
  name               = "${local.name}-server-cpu"
  service_namespace  = aws_appautoscaling_target.server.service_namespace
  resource_id        = aws_appautoscaling_target.server.resource_id
  scalable_dimension = aws_appautoscaling_target.server.scalable_dimension
  policy_type        = "TargetTrackingScaling"

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = 70
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}

# Worker: scale on NATS JetStream pending message count (a custom metric
# the worker publishes to CloudWatch) with a CPU guardrail. Workers scale
# independently of the server.
resource "aws_appautoscaling_target" "worker" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.worker.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.worker_min_count
  max_capacity       = var.worker_max_count
}

resource "aws_appautoscaling_policy" "worker_nats_pending" {
  name               = "${local.name}-worker-nats-pending"
  service_namespace  = aws_appautoscaling_target.worker.service_namespace
  resource_id        = aws_appautoscaling_target.worker.resource_id
  scalable_dimension = aws_appautoscaling_target.worker.scalable_dimension
  policy_type        = "TargetTrackingScaling"

  target_tracking_scaling_policy_configuration {
    customized_metric_specification {
      metric_name = "NATSPendingMessages"
      namespace   = "zk-drive"
      statistic   = "Average"

      dimensions {
        name  = "Service"
        value = "${local.name}-worker"
      }
    }
    target_value       = var.worker_target_nats_pending
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}

resource "aws_appautoscaling_policy" "worker_cpu" {
  name               = "${local.name}-worker-cpu"
  service_namespace  = aws_appautoscaling_target.worker.service_namespace
  resource_id        = aws_appautoscaling_target.worker.resource_id
  scalable_dimension = aws_appautoscaling_target.worker.scalable_dimension
  policy_type        = "TargetTrackingScaling"

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = 75
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}
