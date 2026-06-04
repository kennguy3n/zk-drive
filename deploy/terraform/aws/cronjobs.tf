# Scheduled maintenance jobs run as one-shot ECS tasks via EventBridge
# Scheduler. Cadences mirror deploy/k8s/*-cronjob.yaml:
#   - reconciler      hourly at :17        (k8s "17 * * * *")
#   - orphan-gc       every 6h at :37      (k8s "37 */6 * * *")
#   - audit-archiver  daily at 03:47       (k8s "47 3 * * *")
#
# Each job uses the shared image with an entrypoint override and the
# direct DATABASE_URL (no PgBouncer sidecar, since an essential sidecar
# would keep a one-shot task from completing).

locals {
  cron_jobs = {
    reconciler = {
      entrypoint = "/app/reconciler"
      # EventBridge Scheduler cron is cron(min hour day-of-month month day-of-week year).
      schedule  = "cron(17 * * * ? *)"
      extra_env = []
    }
    orphan-gc = {
      entrypoint = "/app/orphan-gc"
      schedule   = "cron(37 0/6 * * ? *)"
      extra_env  = []
    }
    audit-archiver = {
      entrypoint = "/app/audit-archiver"
      schedule   = "cron(47 3 * * ? *)"
      # The audit-archiver binary is opt-in: it exits as a no-op unless
      # AUDIT_LOG_ARCHIVE_ENABLED is truthy (cmd/audit-archiver/main.go).
      # The shared app_environment doesn't carry it, so without this the
      # daily scheduled task would always no-op. Mirrors the K8s CronJob,
      # which sets it explicitly (deploy/k8s/audit-archiver-cronjob.yaml).
      extra_env = [
        { name = "AUDIT_LOG_ARCHIVE_ENABLED", value = tostring(var.audit_log_archive_enabled) },
      ]
    }
  }
}

resource "aws_ecs_task_definition" "cron" {
  for_each = local.cron_jobs

  family                   = "${local.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([
    {
      name        = each.key
      image       = "${var.app_image}:${var.app_version}"
      essential   = true
      entryPoint  = [each.value.entrypoint]
      environment = concat(local.app_environment, each.value.extra_env)
      secrets     = local.cron_secrets
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.cron.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = each.key
        }
      }
    },
  ])

  tags = {
    Name = "${local.name}-${each.key}"
  }
}

# Role EventBridge Scheduler assumes to launch the tasks.
data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "scheduler" {
  name               = "${local.name}-scheduler"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

data "aws_iam_policy_document" "scheduler" {
  statement {
    sid       = "RunCronTasks"
    actions   = ["ecs:RunTask"]
    resources = [for td in aws_ecs_task_definition.cron : "${td.arn_without_revision}:*"]

    condition {
      test     = "ArnLike"
      variable = "ecs:cluster"
      values   = [aws_ecs_cluster.this.arn]
    }
  }

  statement {
    sid       = "PassTaskRoles"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.task_execution.arn, aws_iam_role.task.arn]
  }
}

resource "aws_iam_role_policy" "scheduler" {
  name   = "${local.name}-scheduler"
  role   = aws_iam_role.scheduler.id
  policy = data.aws_iam_policy_document.scheduler.json
}

resource "aws_scheduler_schedule" "cron" {
  for_each = local.cron_jobs

  name                         = "${local.name}-${each.key}"
  schedule_expression          = each.value.schedule
  schedule_expression_timezone = "UTC"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_ecs_cluster.this.arn
    role_arn = aws_iam_role.scheduler.arn

    ecs_parameters {
      task_definition_arn = aws_ecs_task_definition.cron[each.key].arn_without_revision
      launch_type         = "FARGATE"
      task_count          = 1

      network_configuration {
        subnets          = aws_subnet.private[*].id
        security_groups  = [aws_security_group.app.id]
        assign_public_ip = false
      }
    }

    retry_policy {
      maximum_retry_attempts = 2
    }
  }
}
