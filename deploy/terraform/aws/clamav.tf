# ClamAV on ECS Fargate with a shared EFS volume for the signature
# database, so freshclam updates survive task restarts and are shared
# across tasks (avoiding the per-pod cold-start download the K8s README
# warns about). Reached by the worker at clamav.<namespace>:3310.

resource "aws_efs_file_system" "clamav" {
  creation_token = "${local.name}-clamav"
  encrypted      = true

  lifecycle_policy {
    transition_to_ia = "AFTER_30_DAYS"
  }

  tags = {
    Name = "${local.name}-clamav"
  }
}

resource "aws_efs_mount_target" "clamav" {
  count           = var.az_count
  file_system_id  = aws_efs_file_system.clamav.id
  subnet_id       = aws_subnet.private[count.index].id
  security_groups = [aws_security_group.efs.id]
}

resource "aws_efs_access_point" "clamav" {
  file_system_id = aws_efs_file_system.clamav.id

  posix_user {
    uid = 100
    gid = 101
  }

  root_directory {
    path = "/clamav"
    creation_info {
      owner_uid   = 100
      owner_gid   = 101
      permissions = "0755"
    }
  }

  tags = {
    Name = "${local.name}-clamav"
  }
}

resource "aws_service_discovery_service" "clamav" {
  name = "clamav"

  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.internal.id

    dns_records {
      type = "A"
      ttl  = 10
    }

    routing_policy = "MULTIVALUE"
  }
}

resource "aws_ecs_task_definition" "clamav" {
  family                   = "${local.name}-clamav"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.clamav_cpu
  memory                   = var.clamav_memory
  execution_role_arn       = aws_iam_role.task_execution.arn

  volume {
    name = "signatures"

    efs_volume_configuration {
      file_system_id     = aws_efs_file_system.clamav.id
      transit_encryption = "ENABLED"

      authorization_config {
        access_point_id = aws_efs_access_point.clamav.id
        iam             = "DISABLED"
      }
    }
  }

  container_definitions = jsonencode([
    {
      name      = "clamav"
      image     = "clamav/clamav:stable"
      essential = true
      portMappings = [
        { containerPort = 3310, protocol = "tcp" },
      ]
      mountPoints = [
        { sourceVolume = "signatures", containerPath = "/var/lib/clamav", readOnly = false },
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "echo PING | clamdscan --version >/dev/null 2>&1 || exit 1"]
        interval    = 30
        timeout     = 10
        retries     = 3
        startPeriod = 120
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.clamav.name
          "awslogs-region"        = data.aws_region.current.region
          "awslogs-stream-prefix" = "clamav"
        }
      }
    },
  ])

  tags = {
    Name = "${local.name}-clamav"
  }
}

resource "aws_ecs_service" "clamav" {
  name            = "${local.name}-clamav"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.clamav.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.internal.id]
  }

  service_registries {
    registry_arn = aws_service_discovery_service.clamav.arn
  }

  depends_on = [aws_efs_mount_target.clamav]

  tags = {
    Name = "${local.name}-clamav"
  }
}
