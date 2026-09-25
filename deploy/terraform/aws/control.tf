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

variable "lux_repo" {
  description = "GitHub \"owner/repo\" that publishes lux releases."
  type        = string
  default     = "marcioapm/lux"
}

variable "lux_app_db_name" {
  description = "Postgres database name luxd uses."
  type        = string
  default     = "lux"
}

# --- values that can change after first boot --------------------------
# aws_instance.control ignores changes to ami and user_data_base64 (see its
# lifecycle block below), so cloud-init's write_files/runcmd only ever
# run once, at first boot. Anything that legitimately changes later
# (public_url, the Access team/AUD, bucket names) goes through SSM
# instead: lux-render-config.sh reads these parameters and rewrites
# /etc/lux/luxd.toml on every lux-deploy.service run (every 5 minutes),
# so a `terraform apply` that only changes one of these values reaches
# the box without replacing the instance or rerunning cloud-init.
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

# The control host's SSM Session Manager attachment lives in iam.tf
# (control_ssm), alongside its other IAM statements.

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
  cloud_init = templatefile("${path.module}/templates/control-cloud-init.yaml.tpl", {
    hostname                 = var.name
    luxd_port                = var.luxd_port
    runner_bin_dir           = "/usr/local/lib/lux/runner"
    region                   = var.region
    lux_repo                 = var.lux_repo
    version_parameter        = aws_ssm_parameter.lux_version.name
    public_url_parameter     = aws_ssm_parameter.public_url.name
    cf_access_team_parameter = aws_ssm_parameter.cf_access_team.name
    cf_access_aud_parameter  = aws_ssm_parameter.cf_access_aud.name
    blob_bucket_parameter    = aws_ssm_parameter.blob_bucket.name
    tunnel_token_parameter   = local.cloudflare_tunnel_token_parameter
    db_name                  = var.lux_app_db_name
    volume_id_nodash         = replace(aws_ebs_volume.pg_data.id, "-", "")
    deploy_script            = file("${path.module}/templates/scripts/deploy-lux.py")
    backup_script = templatefile("${path.module}/templates/scripts/pg-backup.sh.tpl", {
      backup_bucket = aws_s3_bucket.pg_backups.id
      db_name       = var.lux_app_db_name
      region        = var.region
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

  # gzip: the rendered cloud-config (with deploy-lux.py inlined) is over
  # EC2's 16 KiB user_data limit as plain text. cloud-init detects and
  # decompresses gzipped user data by itself.
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

  tags = merge(var.tags, { Name = "${var.name}-control", "lux:managed" = "true" })

  lifecycle {
    # ami: most_recent on data.aws_ami.debian means a newer Debian image
    # would otherwise make every `plan` propose replacing this instance
    # (downtime, a full reinstall, new Postgres/luxd passwords). Replace
    # it only deliberately (terraform taint / apply -replace).
    #
    # user_data: cloud-init runs write_files/runcmd once, at first boot;
    # changing user_data here wouldn't rerun it, only recreate the
    # instance (the AWS provider requires a stop/start or replace to
    # apply a new user_data value). Values that legitimately change
    # after first boot go through SSM instead (see the parameters
    # above and ssm.tf's module comment), which lux-render-config.sh
    # re-reads on every deploy run without touching this resource.
    ignore_changes = [ami, user_data_base64]
  }
}

output "control_instance_id" {
  value = aws_instance.control.id
}

output "control_private_ip" {
  value = aws_instance.control.private_ip
}
