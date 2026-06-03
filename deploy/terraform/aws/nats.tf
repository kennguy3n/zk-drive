# NATS JetStream on ECS Fargate with a managed EBS volume for durable
# stream storage. A single task owns the volume (JetStream file store);
# the app tasks reach it via service discovery at nats.<namespace>:4222.

# Infrastructure role ECS assumes to attach/manage the EBS volume for the
# Fargate task.
data "aws_iam_policy_document" "ecs_infra_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "ecs_infrastructure" {
  name               = "${local.name}-ecs-infra"
  assume_role_policy = data.aws_iam_policy_document.ecs_infra_assume.json
}

resource "aws_iam_role_policy_attachment" "ecs_infrastructure" {
  role       = aws_iam_role.ecs_infrastructure.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSInfrastructureRolePolicyForVolumes"
}

resource "aws_service_discovery_service" "nats" {
  name = "nats"

  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.internal.id

    dns_records {
      type = "A"
      ttl  = 10
    }

    routing_policy = "MULTIVALUE"
  }
}

resource "aws_ecs_task_definition" "nats" {
  family                   = "${local.name}-nats"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.nats_cpu
  memory                   = var.nats_memory
  execution_role_arn       = aws_iam_role.task_execution.arn

  volume {
    name                = "jetstream"
    configure_at_launch = true
  }

  container_definitions = jsonencode([
    {
      name      = "nats"
      image     = "nats:2.10-alpine"
      essential = true
      command   = ["-js", "-sd", "/data", "-m", "8222"]
      portMappings = [
        { containerPort = 4222, protocol = "tcp" },
        { containerPort = 8222, protocol = "tcp" },
      ]
      mountPoints = [
        { sourceVolume = "jetstream", containerPath = "/data", readOnly = false },
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "wget -qO- http://localhost:8222/healthz || exit 1"]
        interval    = 15
        timeout     = 5
        retries     = 5
        startPeriod = 20
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.nats.name
          "awslogs-region"        = data.aws_region.current.region
          "awslogs-stream-prefix" = "nats"
        }
      }
    },
  ])

  tags = {
    Name = "${local.name}-nats"
  }
}

resource "aws_ecs_service" "nats" {
  name            = "${local.name}-nats"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.nats.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = aws_subnet.private[*].id
    security_groups = [aws_security_group.internal.id]
  }

  service_registries {
    registry_arn = aws_service_discovery_service.nats.arn
  }

  # JetStream owns a single EBS volume; never run two tasks at once.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  volume_configuration {
    name = "jetstream"

    managed_ebs_volume {
      role_arn         = aws_iam_role.ecs_infrastructure.arn
      size_in_gb       = var.nats_storage_gib
      volume_type      = "gp3"
      encrypted        = true
      file_system_type = "xfs"
    }
  }

  tags = {
    Name = "${local.name}-nats"
  }
}
