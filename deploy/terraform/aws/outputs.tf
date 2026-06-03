output "alb_dns_name" {
  description = "Public DNS name of the Application Load Balancer. Point an ALIAS/CNAME at this if you front the ALB directly instead of via CloudFront."
  value       = aws_lb.this.dns_name
}

output "cloudfront_domain_name" {
  description = "CloudFront distribution domain name (the public entrypoint for the SPA + API)."
  value       = aws_cloudfront_distribution.this.domain_name
}

output "rds_primary_endpoint" {
  description = "RDS primary instance endpoint (host:port)."
  value       = aws_db_instance.primary.endpoint
}

output "rds_replica_endpoint" {
  description = "RDS read replica endpoint (host:port)."
  value       = aws_db_instance.replica.endpoint
}

output "redis_primary_endpoint" {
  description = "ElastiCache Redis primary endpoint."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "frontend_bucket" {
  description = "S3 bucket the built frontend (frontend/dist) should be synced to."
  value       = aws_s3_bucket.frontend.bucket
}

output "acm_certificate_validation_records" {
  description = "DNS records to create to validate the ACM certificate for the ALB. Add these to the DNS zone hosting var.domain_name."
  value = {
    for dvo in aws_acm_certificate.this.domain_validation_options :
    dvo.domain_name => {
      name  = dvo.resource_record_name
      type  = dvo.resource_record_type
      value = dvo.resource_record_value
    }
  }
}

output "alarms_sns_topic_arn" {
  description = "SNS topic alarms publish to. Subscribe an email/PagerDuty/Slack endpoint to receive notifications."
  value       = aws_sns_topic.alarms.arn
}
