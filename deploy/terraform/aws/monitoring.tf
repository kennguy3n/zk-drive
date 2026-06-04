# CloudWatch log groups and alarms.
#
# An SNS topic is created so operators can subscribe (email / PagerDuty /
# Slack via Chatbot) out of band; the alarms publish to it.

resource "aws_cloudwatch_log_group" "app" {
  name              = "/zk-drive/${var.environment}/app"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "nats" {
  name              = "/zk-drive/${var.environment}/nats"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "clamav" {
  name              = "/zk-drive/${var.environment}/clamav"
  retention_in_days = var.log_retention_days
}

resource "aws_cloudwatch_log_group" "cron" {
  name              = "/zk-drive/${var.environment}/cron"
  retention_in_days = var.log_retention_days
}

resource "aws_sns_topic" "alarms" {
  name = "${local.name}-alarms"
}

# --- ECS CPU > 80% (server + worker) ---------------------------------------

resource "aws_cloudwatch_metric_alarm" "server_cpu_high" {
  alarm_name          = "${local.name}-server-cpu-high"
  alarm_description   = "zk-drive-server ECS service CPU utilization > 80%"
  namespace           = "AWS/ECS"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 5
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    ClusterName = aws_ecs_cluster.this.name
    ServiceName = aws_ecs_service.server.name
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]
}

resource "aws_cloudwatch_metric_alarm" "worker_cpu_high" {
  alarm_name          = "${local.name}-worker-cpu-high"
  alarm_description   = "zk-drive-worker ECS service CPU utilization > 80%"
  namespace           = "AWS/ECS"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 5
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    ClusterName = aws_ecs_cluster.this.name
    ServiceName = aws_ecs_service.worker.name
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]
}

# --- RDS connections > 80% of max -------------------------------------------
# Threshold is derived from var.rds_max_connections (default ~410 for
# db.t4g.medium) so it tracks the instance class instead of being a magic
# constant. Mirrors the GCP module's var.cloudsql_max_connections approach.

resource "aws_cloudwatch_metric_alarm" "rds_connections_high" {
  alarm_name          = "${local.name}-rds-connections-high"
  alarm_description   = "RDS DatabaseConnections above 80% of the instance maximum"
  namespace           = "AWS/RDS"
  metric_name         = "DatabaseConnections"
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 5
  threshold           = floor(var.rds_max_connections * 0.8)
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    DBInstanceIdentifier = aws_db_instance.primary.identifier
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]
}

# --- ALB 5xx rate > 1% ------------------------------------------------------
# Expressed as a metric math expression: 5xx / total requests.

resource "aws_cloudwatch_metric_alarm" "alb_5xx_rate_high" {
  alarm_name          = "${local.name}-alb-5xx-rate-high"
  alarm_description   = "ALB HTTP 5xx responses exceed 1% of requests"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 5
  threshold           = 1
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "rate"
    expression  = "100 * (errors / MAX([requests, 1]))"
    label       = "5xx error rate (%)"
    return_data = true
  }

  metric_query {
    id = "errors"
    metric {
      namespace   = "AWS/ApplicationELB"
      metric_name = "HTTPCode_ELB_5XX_Count"
      stat        = "Sum"
      period      = 60
      dimensions = {
        LoadBalancer = aws_lb.this.arn_suffix
      }
    }
  }

  metric_query {
    id = "requests"
    metric {
      namespace   = "AWS/ApplicationELB"
      metric_name = "RequestCount"
      stat        = "Sum"
      period      = 60
      dimensions = {
        LoadBalancer = aws_lb.this.arn_suffix
      }
    }
  }

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]
}
