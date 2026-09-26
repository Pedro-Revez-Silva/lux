# A VPC with public subnets only: runners need internet reachability (package
# mirrors, git, model APIs) for their public IPs, and the control host needs
# it too (SSM, PGDG/APT mirrors, Cloudflare Tunnel), so a NAT gateway (~$33/mo
# plus data processing) would buy nothing here. Both security groups below
# have no ingress from the internet; the S3 gateway endpoint keeps blob and
# backup traffic off the public internet and off any (nonexistent) NAT.

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)
  # One /20 per AZ out of the VPC's /16: room for thousands of hosts per AZ.
  public_subnet_cidrs = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
}

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(var.tags, { Name = var.name })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = var.name })
}

resource "aws_subnet" "public" {
  count = var.az_count

  vpc_id                  = aws_vpc.this.id
  availability_zone       = local.azs[count.index]
  cidr_block              = local.public_subnet_cidrs[count.index]
  map_public_ip_on_launch = true

  tags = merge(var.tags, { Name = "${var.name}-public-${local.azs[count.index]}" })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-public" })
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count = var.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# Free: no hourly or data charge, unlike interface endpoints. Used for S3
# blob and backup traffic, and by SSM's dependency on S3 for some transfers.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.public.id]

  tags = merge(var.tags, { Name = "${var.name}-s3" })
}

# Runners: no inbound at all. lux-runner dials out to luxd; nothing needs to
# reach a runner.
resource "aws_security_group" "runner" {
  name        = "${var.name}-runner"
  description = "lux runners: no inbound, all outbound"
  vpc_id      = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-runner" })
}

resource "aws_vpc_security_group_egress_rule" "runner_all" {
  security_group_id = aws_security_group.runner.id
  description       = "all outbound"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# Control host: no inbound from the internet. The only ingress is runners
# reaching luxd's port; everything else (SSM, Cloudflare Tunnel) is
# outbound-initiated from the host.
resource "aws_security_group" "control" {
  name        = "${var.name}-control"
  description = "lux control host: luxd reachable from runners only, no internet inbound"
  vpc_id      = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name}-control" })
}

resource "aws_vpc_security_group_ingress_rule" "control_from_runners" {
  security_group_id            = aws_security_group.control.id
  description                  = "runners to luxd"
  referenced_security_group_id = aws_security_group.runner.id
  from_port                    = var.luxd_port
  to_port                      = var.luxd_port
  ip_protocol                  = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "control_all" {
  security_group_id = aws_security_group.control.id
  description       = "all outbound"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

output "vpc_id" {
  value = aws_vpc.this.id
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "availability_zones" {
  value = local.azs
}

output "control_security_group_id" {
  value = aws_security_group.control.id
}

output "runner_security_group_id" {
  value = aws_security_group.runner.id
}
