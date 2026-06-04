# Provider, shared locals, and the network substrate (VPC, subnets,
# gateways, route tables, and security groups) for the ZK Drive AWS
# deployment.

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.common_tags
  }
}

# CloudFront's ACM certificate and a handful of global resources must live
# in us-east-1 regardless of the primary region. This aliased provider is
# used only for those.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  default_tags {
    tags = local.common_tags
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  name = "${var.name_prefix}-${var.environment}"

  common_tags = merge(
    {
      project     = "zk-drive"
      environment = var.environment
      managed-by  = "terraform"
    },
    var.tags,
  )

  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)

  # /20 public + /20 private subnets carved out of the VPC CIDR, one pair
  # per AZ.
  public_subnet_cidrs  = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  private_subnet_cidrs = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 8)]

  has_domain = var.domain_name != ""
}

# ----------------------------------------------------------------------------
# VPC + subnets
# ----------------------------------------------------------------------------

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = "${local.name}-vpc"
  }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = {
    Name = "${local.name}-igw"
  }
}

resource "aws_subnet" "public" {
  count                   = var.az_count
  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_subnet_cidrs[count.index]
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true

  tags = {
    Name = "${local.name}-public-${local.azs[count.index]}"
    tier = "public"
  }
}

resource "aws_subnet" "private" {
  count             = var.az_count
  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_subnet_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = {
    Name = "${local.name}-private-${local.azs[count.index]}"
    tier = "private"
  }
}

# One NAT gateway per AZ so private subnets retain egress (image pulls,
# Stripe/SMTP/fabric calls) without a single-AZ failure domain.
resource "aws_eip" "nat" {
  count  = var.az_count
  domain = "vpc"

  tags = {
    Name = "${local.name}-nat-${local.azs[count.index]}"
  }
}

resource "aws_nat_gateway" "this" {
  count         = var.az_count
  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = {
    Name = "${local.name}-nat-${local.azs[count.index]}"
  }

  depends_on = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = {
    Name = "${local.name}-public"
  }
}

resource "aws_route_table_association" "public" {
  count          = var.az_count
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  count  = var.az_count
  vpc_id = aws_vpc.this.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this[count.index].id
  }

  tags = {
    Name = "${local.name}-private-${local.azs[count.index]}"
  }
}

resource "aws_route_table_association" "private" {
  count          = var.az_count
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# ----------------------------------------------------------------------------
# Security groups
# ----------------------------------------------------------------------------

# AWS-managed prefix list of CloudFront's origin-facing IP ranges. Used to
# lock the ALB down so it only accepts traffic forwarded by CloudFront,
# preventing clients from bypassing the CDN (and its caching/headers) to
# reach the origin directly.
data "aws_ec2_managed_prefix_list" "cloudfront" {
  name = "com.amazonaws.global.cloudfront.origin-facing"
}

# ALB ingress restricted to CloudFront's origin-facing ranges: the ALB is an
# origin behind CloudFront, not a public endpoint. CloudFront reaches it over
# HTTP/80 (see cloudfront.tf); 443 stays open to the same ranges for an
# optional direct-HTTPS path when a domain is configured.
resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "ALB ingress from CloudFront origin-facing ranges"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "HTTP from CloudFront"
    from_port       = 80
    to_port         = 80
    protocol        = "tcp"
    prefix_list_ids = [data.aws_ec2_managed_prefix_list.cloudfront.id]
  }

  ingress {
    description     = "HTTPS from CloudFront"
    from_port       = 443
    to_port         = 443
    protocol        = "tcp"
    prefix_list_ids = [data.aws_ec2_managed_prefix_list.cloudfront.id]
  }

  egress {
    description = "All egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${local.name}-alb"
  }
}

# Application tasks (server + worker). Server accepts traffic from the ALB
# on 8080; both need full egress for image pulls and outbound API calls.
resource "aws_security_group" "app" {
  name        = "${local.name}-app"
  description = "ECS server/worker tasks"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "HTTP from ALB"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  egress {
    description = "All egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${local.name}-app"
  }
}

# Internal services (NATS, ClamAV) reachable only from the app tasks.
resource "aws_security_group" "internal" {
  name        = "${local.name}-internal"
  description = "NATS and ClamAV ECS tasks"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "NATS client"
    from_port       = 4222
    to_port         = 4222
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  ingress {
    description     = "NATS monitoring"
    from_port       = 8222
    to_port         = 8222
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  ingress {
    description     = "ClamAV clamd"
    from_port       = 3310
    to_port         = 3310
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  # NATS and ClamAV tasks must also reach each other's ports? No — keep
  # the surface minimal: allow intra-group traffic so a future clustered
  # NATS can gossip without another SG.
  ingress {
    description = "Intra-group"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    self        = true
  }

  egress {
    description = "All egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "${local.name}-internal"
  }
}

# RDS: Postgres only from app tasks (the PgBouncer sidecar connects here).
resource "aws_security_group" "rds" {
  name        = "${local.name}-rds"
  description = "RDS Postgres"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "Postgres from app tasks"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  tags = {
    Name = "${local.name}-rds"
  }
}

# ElastiCache Redis from app tasks.
resource "aws_security_group" "redis" {
  name        = "${local.name}-redis"
  description = "ElastiCache Redis"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "Redis from app tasks"
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  tags = {
    Name = "${local.name}-redis"
  }
}

# EFS mount targets for the ClamAV signature DB, reachable from the
# internal services SG (ClamAV runs there).
resource "aws_security_group" "efs" {
  name        = "${local.name}-efs"
  description = "EFS for ClamAV signature database"
  vpc_id      = aws_vpc.this.id

  ingress {
    description     = "NFS from ClamAV tasks"
    from_port       = 2049
    to_port         = 2049
    protocol        = "tcp"
    security_groups = [aws_security_group.internal.id]
  }

  tags = {
    Name = "${local.name}-efs"
  }
}
