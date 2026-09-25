# The control host: one instance running luxd and Postgres locally (no
# replicas). Debian 13 (trixie), arm64: a stable, minimal, well-documented
# base with official Debian Cloud Images on AWS (owner 136693071363,
# verified against `aws ec2 describe-images` for eu-north-1), apt-based
# and easy to layer the PGDG repo onto for Postgres 18 (trixie ships 17;
# docs/operations.md says tested on 18). Ubuntu 24.04 would work as well;
# Debian was picked for the smaller base image and longer LTS runway
# relative to this deployment's expected lifetime. Amazon Linux 2023 was
# ruled out: no PGDG arm64 repo, and its 5-year support clock starts from
# release rather than from when a fleet adopts it.

variable "control_instance_type" {
  description = "Instance type for the control host."
  type        = string
  default     = "t4g.medium"
}

variable "control_root_volume_size" {
  description = "Control host root EBS volume size in GB (OS and luxd binaries; Postgres data is a separate volume)."
  type        = number
  default     = 20
}

variable "control_pg_data_volume_size" {
  description = "Size in GB of the separate gp3 volume holding Postgres data."
  type        = number
  default     = 50
}

variable "debian_ami_owner" {
  description = "AWS account id that publishes official Debian Cloud Images."
  type        = string
  default     = "136693071363"
}

variable "public_url" {
  description = "The public URL clients and runners use (the Cloudflare Tunnel hostname), e.g. \"https://lux.example.com\". Written to luxd.toml as public_url."
  type        = string
}

variable "cf_access_team" {
  description = "Cloudflare Access team domain (e.g. \"acme\" or \"acme.cloudflareaccess.com\"). Empty leaves console_auth at its \"key\" default."
  type        = string
  default     = ""
}

variable "cf_access_aud" {
  description = "Cloudflare Access application AUD tag. Required if cf_access_team is set."
  type        = string
  default     = ""
}

variable "config_repo_url" {
  description = "Git URL (SSH or HTTPS) of the operator's config repo: the control host clones it and runs <config_repo_path>/host/reconcile.py from it every 5 minutes."
  type        = string

  validation {
    condition     = length(trimspace(var.config_repo_url)) > 0
    error_message = "config_repo_url must be set."
  }
}

variable "config_repo_ref" {
  description = "Branch or tag of the config repo the control host follows."
  type        = string
  default     = "main"
}

variable "config_repo_path" {
  description = "Subdirectory of the config repo that holds host/ (empty: the repo root)."
  type        = string
  default     = ""
}

variable "config_repo_deploy_key_parameter" {
  description = "Name of an existing SSM SecureString holding a read-only SSH deploy key for the config repo. Empty: the repo is public (fetched without a key)."
  type        = string
  default     = ""
}

variable "lux_app_db_name" {
  description = "Postgres database name luxd uses."
  type        = string
  default     = "lux"
}

# --- values that can change after first boot --------------------------
# aws_instance.control ignores changes to ami and user_data_base64 (see its
# lifecycle block below), so cloud-init only ever runs once, at first
# boot. Everything the reconciler needs from the infrastructure goes
# through SSM instead and is re-read on every run (every 5 minutes).
resource "aws_ssm_parameter" "public_url" {
  name  = "${local.ssm_prefix}/public_url"
  type  = "String"
  value = var.public_url

  tags = var.tags
}

resource "aws_ssm_parameter" "cf_access_team" {
  name  = "${local.ssm_prefix}/cf_access_team"
  type  = "String"
  value = var.cf_access_team

  tags = var.tags
}

resource "aws_ssm_parameter" "cf_access_aud" {
  name  = "${local.ssm_prefix}/cf_access_aud"
  type  = "String"
  value = var.cf_access_aud

  tags = var.tags
}

resource "aws_ssm_parameter" "blob_bucket" {
  name  = "${local.ssm_prefix}/blob_bucket"
  type  = "String"
  value = aws_s3_bucket.blobs.id

  tags = var.tags
}

resource "aws_ssm_parameter" "backup_bucket" {
  name  = "${local.ssm_prefix}/backup_bucket"
  type  = "String"
  value = aws_s3_bucket.pg_backups.id

  tags = var.tags
}

resource "aws_ssm_parameter" "db_name" {
  name  = "${local.ssm_prefix}/db_name"
  type  = "String"
  value = var.lux_app_db_name

  tags = var.tags
}

resource "aws_ssm_parameter" "luxd_port" {
  name  = "${local.ssm_prefix}/luxd_port"
  type  = "String"
  value = tostring(var.luxd_port)

  tags = var.tags
}

resource "aws_ssm_parameter" "pg_data_volume_id" {
  name  = "${local.ssm_prefix}/pg_data_volume_id"
  type  = "String"
  value = aws_ebs_volume.pg_data.id

  tags = var.tags
}

resource "aws_ssm_parameter" "config_repo_url" {
  name  = "${local.ssm_prefix}/config_repo_url"
  type  = "String"
  value = var.config_repo_url

  tags = var.tags
}

resource "aws_ssm_parameter" "config_repo_ref" {
  name  = "${local.ssm_prefix}/config_repo_ref"
  type  = "String"
  value = var.config_repo_ref

  tags = var.tags
}

# SSM refuses an empty String value: absent means a public repo.
resource "aws_ssm_parameter" "config_repo_deploy_key_parameter" {
  count = var.config_repo_deploy_key_parameter != "" ? 1 : 0

  name  = "${local.ssm_prefix}/config_repo_deploy_key_parameter"
  type  = "String"
  value = var.config_repo_deploy_key_parameter

  tags = var.tags
}

data "aws_ami" "debian" {
  most_recent = true
  owners      = [var.debian_ami_owner]

  filter {
    name   = "name"
    values = ["debian-13-arm64-*"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
  filter {
    name   = "root-device-type"
    values = ["ebs"]
  }
}

# The control host's SSM agent policy lives in iam.tf
# (control_ssm_agent), alongside its other IAM statements.

# Survives instance replacement: not attached to the instance's lifecycle,
# and protected from accidental destroy. To replace the control host,
# terraform taint/replace the instance, not this volume; to actually
# retire the volume, remove prevent_destroy first.
resource "aws_ebs_volume" "pg_data" {
  availability_zone = local.azs[0]
  size              = var.control_pg_data_volume_size
  type              = "gp3"
  encrypted         = true

  tags = merge(var.tags, { Name = "${var.name}-pg-data" })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_volume_attachment" "pg_data" {
  device_name = "/dev/xvdf"
  volume_id   = aws_ebs_volume.pg_data.id
  instance_id = aws_instance.control.id
  # The volume outlives instance replacement; don't let detaching it at
  # destroy time race the instance's own termination.
  stop_instance_before_detaching = true
}

locals {
  config_checkout = "/var/lib/lux/config"
  config_host_dir = join("/", compact([local.config_checkout, trim(var.config_repo_path, "/"), "host"]))
  cloud_init = templatefile("${path.module}/templates/control-cloud-init.yaml.tpl", {
    hostname  = var.name
    region    = var.region
    checkout  = local.config_checkout
    host_dir  = local.config_host_dir
    repo_url  = var.config_repo_url
    repo_ref  = var.config_repo_ref
    key_param = var.config_repo_deploy_key_parameter
    host_json = jsonencode({
      region           = var.region
      ssm_prefix       = local.ssm_prefix
      checkout         = local.config_checkout
      config_repo_path = trim(var.config_repo_path, "/")
    })
  })
}

resource "aws_instance" "control" {
  ami                    = data.aws_ami.debian.id
  instance_type          = var.control_instance_type
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.control.id]
  iam_instance_profile   = aws_iam_instance_profile.control.name
  # A public IP, because there is no NAT gateway: outbound-only internet
  # access (apt/PGDG, GitHub releases, SSM, the Cloudflare Tunnel) needs
  # one-to-one NAT through the internet gateway either way. "No inbound
  # from the internet" is enforced by the security group (no ingress
  # rule from 0.0.0.0/0 — see network.tf), not by withholding the address.
  associate_public_ip_address = true

  # gzip: keeps the bootstrap well under EC2's 16 KiB user_data limit.
  # cloud-init detects and decompresses gzipped user data by itself.
  user_data_base64 = base64gzip(local.cloud_init)

  # t4g's default (unlimited) bills sustained load above the 20%
  # baseline as surplus credits at the instance's hourly rate — cheap
  # per burst, but unbounded if luxd or Postgres runs hot for a while.
  # standard caps CPU at the baseline instead: predictable cost, at the
  # price of throttling under sustained load (see the README's cost
  # table).
  credit_specification {
    cpu_credits = "standard"
  }

  root_block_device {
    volume_size           = var.control_root_volume_size
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }

  metadata_options {
    http_tokens   = "required"
    http_endpoint = "enabled"
  }

  # No lux:* tags: those mark instances luxd launched, and iam.tf lets
  # luxd terminate an instance carrying lux:managed and lux:host.
  tags = merge(var.tags, { Name = "${var.name}-control" })

  lifecycle {
    # ami: most_recent on data.aws_ami.debian means a newer Debian image
    # would otherwise make every `plan` propose replacing this instance
    # (downtime, a full reinstall, new Postgres/luxd passwords). Replace
    # it only deliberately (terraform taint / apply -replace).
    #
    # user_data: cloud-init runs once, at first boot, and only bootstraps
    # the config repo checkout and its reconcile timer. Everything after
    # that is the reconciler's job: host code and the desired state come
    # from the config repo, infrastructure values from the SSM parameters
    # above, so neither needs a new user_data.
    ignore_changes = [ami, user_data_base64]

    # tags_all includes the provider's default_tags, which this module
    # cannot strip: refuse to create a control host luxd could terminate.
    postcondition {
      condition     = !contains(keys(self.tags_all), "lux:host") && lookup(self.tags_all, "lux:managed", "") != "true"
      error_message = "The control host must not carry lux:host or lux:managed=true (from var.tags or the provider's default_tags): luxd's role may terminate instances with those tags."
    }
  }
}

output "control_instance_id" {
  value = aws_instance.control.id
}

output "control_private_ip" {
  value = aws_instance.control.private_ip
}
