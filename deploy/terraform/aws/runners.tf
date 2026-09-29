# Runner EC2 launch templates. luxd itself calls RunInstances against
# these (docs/operations.md "EC2 pools"): Terraform only creates the
# templates and looks up AMIs; it sets no user data; luxd passes it per
# instance at launch time (LUX_URL, LUX_HOST_TOKEN, … as an Ignition
# config, a cloud-init script, or plain env lines, per userData below).

variable "runner_pools" {
  description = <<-EOT
    EC2 launch templates for runner pools, one per map key (used in
    resource names and the `lux pools set` output). `image = "fcos"`
    (the default) boots the latest Fedora CoreOS stable AMI, looked up
    by architecture; podman, netavark and nftables are already on it, so
    luxd's Ignition user data installs nothing. `image = "custom"` needs
    `ami_id`: a stock Fedora/Ubuntu/AL2023 AMI (luxd's "script" user data
    installs podman if missing) or a prebuilt image (luxd's "env" user
    data, plain KEY=value lines). `user_data_format` must stay
    "ignition" for `image = "fcos"`.
  EOT
  type = map(object({
    instance_type    = string
    arch             = string                       # "arm64" or "amd64": the AMI architecture and what to put in the pool-set instanceType
    image            = optional(string, "fcos")     # "fcos" | "custom"
    ami_id           = optional(string, "")         # required when image = "custom"
    user_data_format = optional(string, "ignition") # "ignition" | "script" | "env"
    spot             = optional(bool, true)
  }))
  default = {
    arm64 = {
      instance_type = "m8g.2xlarge"
      arch          = "arm64"
    }
  }

  validation {
    condition     = alltrue([for k, v in var.runner_pools : contains(["arm64", "amd64"], v.arch)])
    error_message = "runner_pools[*].arch must be \"arm64\" or \"amd64\"."
  }
  validation {
    condition     = alltrue([for k, v in var.runner_pools : contains(["fcos", "custom"], v.image)])
    error_message = "runner_pools[*].image must be \"fcos\" or \"custom\"."
  }
  validation {
    condition     = alltrue([for k, v in var.runner_pools : v.image != "custom" || v.ami_id != ""])
    error_message = "runner_pools[*].ami_id is required when image = \"custom\"."
  }
  validation {
    condition     = alltrue([for k, v in var.runner_pools : v.image != "fcos" || v.user_data_format == "ignition"])
    error_message = "runner_pools[*].user_data_format must be \"ignition\" when image = \"fcos\": Fedora CoreOS only takes Ignition."
  }
}

variable "runner_root_volume_size" {
  description = "Runner root EBS volume size in GB. FCOS keeps container storage on /var, on this same volume."
  type        = number
  default     = 100
}

variable "runner_root_volume_iops" {
  description = "gp3 IOPS for the runner root volume. 3000 is the gp3 baseline: free, no provisioned-IOPS surcharge."
  type        = number
  default     = 3000
}

variable "runner_root_volume_throughput" {
  description = "gp3 throughput (MiB/s) for the runner root volume. 125 is the gp3 baseline: free, no surcharge."
  type        = number
  default     = 125
}

locals {
  runner_fcos_archs = distinct([for k, v in var.runner_pools : v.arch if v.image == "fcos"])
}

# Fedora CoreOS publishes one AMI per stream/arch/release, owned by the
# Fedora project's AWS account (125523088429; verified against the
# published stream metadata at
# https://builds.coreos.fedoraproject.org/streams/stable.json, which lists
# this account's AMI ids per region). Named "fedora-coreos-<version>-<arch>".
data "aws_ami" "fcos" {
  for_each = toset(local.runner_fcos_archs)

  most_recent = true
  owners      = ["125523088429"]

  filter {
    name   = "architecture"
    values = [each.key == "arm64" ? "arm64" : "x86_64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
  filter {
    name   = "description"
    values = ["Fedora CoreOS stable *"]
  }
}

# A custom AMI's root device name varies by distro (Fedora Cloud: /dev/sda1;
# Debian/Ubuntu: /dev/sda1 or /dev/xvda; AL2023: /dev/xvda), so it is looked
# up rather than assumed.
data "aws_ami" "custom" {
  for_each = { for k, v in var.runner_pools : k => v.ami_id if v.image == "custom" }

  filter {
    name   = "image-id"
    values = [each.value]
  }
}

locals {
  runner_ami_id = {
    for k, v in var.runner_pools :
    k => v.image == "fcos" ? data.aws_ami.fcos[v.arch].id : data.aws_ami.custom[k].id
  }
  runner_root_device_name = {
    for k, v in var.runner_pools :
    k => v.image == "fcos" ? data.aws_ami.fcos[v.arch].root_device_name : data.aws_ami.custom[k].root_device_name
  }
}

resource "aws_launch_template" "runner" {
  for_each = var.runner_pools

  name          = "${var.name}-runner-${each.key}"
  image_id      = local.runner_ami_id[each.key]
  instance_type = each.value.instance_type

  # luxd's RunInstances call launches "$Default" (internal/ec2/ec2.go), so
  # each new version this template gets (a new FCOS AMI from most_recent,
  # a volume size or IOPS change) must become the default too, or runners
  # keep booting whatever AMI was current when the template was first
  # created.
  update_default_version = true

  vpc_security_group_ids = [aws_security_group.runner.id]

  # No iam_instance_profile: Fedora CoreOS has no SSM agent (shell access
  # is through lux itself — `lux exec`/attach, or `get-console-output` to
  # debug boot — not SSM), and the runner never holds S3 credentials
  # (docs/operations.md). With no instance profile, luxd's role needs no
  # iam:PassRole (see iam.tf). A "custom" image needing its own agent role
  # can add one back on this launch template.

  block_device_mappings {
    device_name = local.runner_root_device_name[each.key]

    ebs {
      volume_size           = var.runner_root_volume_size
      volume_type           = "gp3"
      iops                  = var.runner_root_volume_iops
      throughput            = var.runner_root_volume_throughput
      encrypted             = true
      delete_on_termination = true
    }
  }

  metadata_options {
    # IMDSv2, reachable: the runner watches it for spot interruption
    # notices (LUX_EC2_IMDS, set by luxd in the user data it passes to
    # RunInstances, not by this template).
    http_tokens   = "required"
    http_endpoint = "enabled"
  }

  # No user_data: luxd passes it per instance in RunInstances (see the
  # module comment above).

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${var.name}-runner-${each.key}" })
  }

  tags = merge(var.tags, { Name = "${var.name}-runner-${each.key}" })
}

output "runner_launch_template_ids" {
  value = { for k, v in aws_launch_template.runner : k => v.id }
}

output "runner_pools_set_commands" {
  description = "Ready-to-paste `lux pools set` commands, one per runner pool."
  value = {
    for k, v in var.runner_pools : k => join(" ", [
      "lux pools set ${k}",
      "--provider ec2",
      "--min 0 --max 10 --warm 0",
      "--template '${jsonencode({
        region         = var.region
        launchTemplate = aws_launch_template.runner[k].name
        instanceType   = v.instance_type
        subnets        = aws_subnet.public[*].id
        spot           = v.spot
        userData       = v.user_data_format
      })}'",
    ])
  }
}
