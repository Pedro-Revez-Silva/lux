# IAM for the control host: the luxd instance role. Runners get no
# instance profile (see runners.tf): Fedora CoreOS has no SSM agent, is
# reached through lux itself, and never holds S3 credentials, so luxd's
# role needs no iam:PassRole either — RunInstances launches the runner
# template as-is, with no IAM role to pass.

variable "create_spot_service_linked_role" {
  description = <<-EOT
    Whether to create the AWSServiceRoleForEC2Spot service-linked role
    (aws_service_name = spot.amazonaws.com). The first spot RunInstances
    call in an account needs this role to exist; luxd's own role has no
    iam:CreateServiceLinkedRole, so without it every spot launch fails
    with AuthFailure.ServiceLinkedRoleCreationNotPermitted. Set to false
    on an account that has already used EC2 Spot elsewhere: this
    resource errors if the role already exists. To adopt Terraform
    management of an existing role instead of setting this to false, run
    `terraform import aws_iam_service_linked_role.spot
    arn:aws:iam::<account-id>:role/aws-service-role/spot.amazonaws.com/AWSServiceRoleForEC2Spot`
    before applying with this left at its default.
  EOT
  type        = bool
  default     = true
}

resource "aws_iam_service_linked_role" "spot" {
  count = var.create_spot_service_linked_role ? 1 : 0

  aws_service_name = "spot.amazonaws.com"
}

data "aws_partition" "current" {}

locals {
  arn_prefix = "arn:${data.aws_partition.current.partition}"
  ec2_arn    = "${local.arn_prefix}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}"
  # By pool name.
  runner_launch_template_arns = { for k, lt in aws_launch_template.runner : k => "${local.ec2_arn}:launch-template/${lt.id}" }
  ssm_parameter_arn           = "${local.arn_prefix}:ssm:${var.region}:${data.aws_caller_identity.current.account_id}:parameter"
}

# --- control host ---------------------------------------------------------

resource "aws_iam_role" "control" {
  name = "${var.name}-control"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_instance_profile" "control" {
  name = "${var.name}-control"
  role = aws_iam_role.control.name

  tags = var.tags
}

# Shell access to the control host is SSM Session Manager only (no SSH, no
# key pairs). This is AmazonSSMManagedInstanceCore (v2) minus its
# parameter reads: that managed policy grants ssm:GetParameter(s) on "*",
# which would let the host read any parameter in the account, including
# unrelated SecureStrings, and defeat ReadOwnParameters' scoping below.
# ssm:GetManifest is left out too: only SSM Distributor packages
# (AWS-ConfigureAWSPackage) use it, and Session Manager and Run Command
# do not. The agent's per-instance calls are scoped to this instance;
# the rest act on documents, associations or nothing at all, so "*".
resource "aws_iam_role_policy" "control_ssm_agent" {
  name = "${var.name}-ssm-agent"
  role = aws_iam_role.control.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "SsmAgentThisInstance"
        Effect = "Allow"
        Action = [
          "ssm:UpdateInstanceInformation",
          "ssm:ListInstanceAssociations",
          "ssm:PutComplianceItems",
        ]
        Resource = "${local.ec2_arn}:instance/${aws_instance.control.id}"
      },
      {
        Sid    = "SsmAgent"
        Effect = "Allow"
        Action = [
          "ssm:DescribeAssociation",
          "ssm:GetDeployablePatchSnapshotForInstance",
          "ssm:GetDocument",
          "ssm:DescribeDocument",
          "ssm:ListAssociations",
          "ssm:PutInventory",
          "ssm:PutConfigurePackageResult",
          "ssm:UpdateAssociationStatus",
          "ssm:UpdateInstanceAssociationStatus",
        ]
        Resource = "*"
      },
      {
        Sid    = "SessionManagerChannels"
        Effect = "Allow"
        Action = [
          "ssmmessages:CreateControlChannel",
          "ssmmessages:CreateDataChannel",
          "ssmmessages:OpenControlChannel",
          "ssmmessages:OpenDataChannel",
        ]
        Resource = "*" # ssmmessages has no resource types.
      },
      {
        Sid    = "RunCommandMessages"
        Effect = "Allow"
        Action = [
          "ec2messages:AcknowledgeMessage",
          "ec2messages:DeleteMessage",
          "ec2messages:FailMessage",
          "ec2messages:GetEndpoint",
          "ec2messages:GetMessages",
          "ec2messages:SendReply",
        ]
        Resource = "*" # ec2messages has no resource types.
      },
    ]
  })
}

resource "aws_iam_role_policy" "control_luxd" {
  name = "${var.name}-luxd"
  role = aws_iam_role.control.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "RunRunnerInstances"
        Effect = "Allow"
        Action = "ec2:RunInstances"
        # RunInstances is authorized per resource type touched by the
        # call; scoping each type to the runner pools' own launch
        # templates, subnets and security group stops luxd's role (if a
        # credential leaked) from launching arbitrary instances. The
        # instance itself is in RunInstancesFromRunnerTemplates below.
        Resource = concat(
          values(local.runner_launch_template_arns),
          [for s in aws_subnet.public : "${local.ec2_arn}:subnet/${s.id}"],
          [
            "${local.ec2_arn}:network-interface/*",
            "${local.ec2_arn}:volume/*",
            "${local.arn_prefix}:ec2:${var.region}::image/*",
            "${local.ec2_arn}:security-group/${aws_security_group.runner.id}",
            # Spot launches (internal/ec2/ec2.go sets InstanceMarketOptions):
            # RunInstances is authorized against this resource type too,
            # in addition to instance/network-interface/volume/image above.
            "${local.ec2_arn}:spot-instances-request/*",
          ]
        )
      },
      {
        # A RunInstances call that names no launch template is not
        # constrained by the launch-template resource above: it touches
        # none. Requiring ec2:LaunchTemplate on the instance makes every
        # launch come from a runner template. ec2:IsLaunchTemplateResource
        # is not required: luxd overrides the subnet, instance type, market
        # options, user data and tags per launch (internal/ec2/ec2.go), so
        # those resources are not the template's own.
        Sid      = "RunInstancesFromRunnerTemplates"
        Effect   = "Allow"
        Action   = "ec2:RunInstances"
        Resource = "${local.ec2_arn}:instance/*"
        Condition = {
          ArnEquals = { "ec2:LaunchTemplate" = values(local.runner_launch_template_arns) }
        }
      },
      {
        Sid      = "TagOnCreate"
        Effect   = "Allow"
        Action   = "ec2:CreateTags"
        Resource = "${local.ec2_arn}:instance/*"
        Condition = {
          StringEquals = { "ec2:CreateAction" = "RunInstances" }
        }
      },
      {
        # lux:host is set only by luxd, at launch (internal/server/
        # provisioner.go), and TagOnCreate allows no tagging after launch,
        # so only instances luxd itself launched carry it. The control
        # host is refused it (the postcondition on aws_instance.control),
        # so luxd cannot terminate its own host.
        Sid      = "TerminateManagedInstances"
        Effect   = "Allow"
        Action   = "ec2:TerminateInstances"
        Resource = "${local.ec2_arn}:instance/*"
        Condition = {
          StringEquals = { "ec2:ResourceTag/lux:managed" = "true" }
          Null         = { "ec2:ResourceTag/lux:host" = "false" }
        }
      },
      {
        Sid      = "DescribeInstances"
        Effect   = "Allow"
        Action   = "ec2:DescribeInstances"
        Resource = "*" # DescribeInstances does not support resource-level permissions.
      },
      {
        Sid    = "Blobs"
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:DeleteObject",
          "s3:ListBucket",
        ]
        Resource = [
          aws_s3_bucket.blobs.arn,
          "${aws_s3_bucket.blobs.arn}/*",
        ]
      },
      {
        Sid    = "PgBackups"
        Effect = "Allow"
        Action = [
          "s3:PutObject",
          "s3:GetObject",
          "s3:ListBucket",
        ]
        Resource = [
          aws_s3_bucket.pg_backups.arn,
          "${aws_s3_bucket.pg_backups.arn}/*",
        ]
      },
      {
        Sid    = "ReadOwnParameters"
        Effect = "Allow"
        Action = ["ssm:GetParameter", "ssm:GetParameters"]
        # The tunnel token's parameter may be named outside the prefix
        # (cloudflare_tunnel_token_parameter), and nothing else grants
        # parameter reads.
        Resource = [
          "${local.ssm_parameter_arn}${local.ssm_prefix}/*",
          "${local.ssm_parameter_arn}${startswith(local.cloudflare_tunnel_token_parameter, "/") ? "" : "/"}${local.cloudflare_tunnel_token_parameter}",
        ]
      },
    ]
  })
}
