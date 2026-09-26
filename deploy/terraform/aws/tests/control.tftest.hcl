# The control host's user_data must stay under EC2's 16 KiB limit (raw
# bytes, before base64). Mocked providers: no credentials, no API calls.
# The rendered cloud-config depends only on this module's templates and
# variables (long config repo values below stand in for real ones).
# The control role's parameter reads are exactly the module's prefix, the
# tunnel token and, when set, the one config repo deploy key.
# The control host must also carry no lux:* tag, and var.tags must refuse
# one: luxd's terminate permission keys on lux:* tags (iam.tf), so such a
# tag would let luxd terminate its own host.

# Plan only: an apply run leaves aws_ebs_volume.pg_data in the test state,
# and its prevent_destroy makes teardown fail after every run has passed.
# override_during = plan feeds the mock ids and ARNs below into the plan.
mock_provider "aws" {
  override_during = plan

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["eu-north-1a", "eu-north-1b", "eu-north-1c"]
    }
  }
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }
  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }
  mock_data "aws_ami" {
    defaults = {
      id = "ami-0123456789abcdef0"
    }
  }
  mock_resource "aws_ebs_volume" {
    defaults = {
      id = "vol-0123456789abcdef0"
    }
  }
  mock_resource "aws_instance" {
    defaults = {
      id = "i-0123456789abcdef0"
    }
  }
  mock_resource "aws_s3_bucket" {
    defaults = {
      id  = "lux-pg-backups-123456789012-eu-north-1"
      arn = "arn:aws:s3:::lux-pg-backups-123456789012-eu-north-1"
    }
  }
  mock_resource "aws_subnet" {
    defaults = {
      id = "subnet-0123456789abcdef0"
    }
  }
  mock_resource "aws_security_group" {
    defaults = {
      id = "sg-0123456789abcdef0"
    }
  }
  mock_resource "aws_launch_template" {
    defaults = {
      id = "lt-0123456789abcdef0"
    }
  }
}

variables {
  region                         = "eu-north-1"
  config_repo_url                = "git@github.example.com:acme-infrastructure/lux-control-host-configuration.git"
  config_repo_ref                = "release/production-eu-north-1"
  config_repo_path               = "environments/production/eu-north-1"
  public_url                     = "https://lux.example.com"
  cf_access_team                 = "example"
  cf_access_aud                  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  manage_cloudflare_tunnel_token = true
  cloudflare_tunnel_token        = "token"
}

run "control_user_data_fits_ec2_limit" {
  command = plan

  # Raw size of the base64 payload: 3 bytes per 4 characters, minus padding.
  assert {
    condition     = length(aws_instance.control.user_data_base64) / 4 * 3 - length(regexall("=", aws_instance.control.user_data_base64)) <= 16384
    error_message = "The control host's user_data is over EC2's 16384-byte limit (raw, before base64)."
  }

  assert {
    condition     = length([for k in keys(aws_instance.control.tags) : k if startswith(k, "lux:")]) == 0
    error_message = "The control host carries a lux:* tag; luxd's role may terminate instances tagged lux:managed and lux:host."
  }
}

run "public_config_repo_grants_no_extra_parameter_read" {
  command = plan

  assert {
    condition = toset(one([for st in jsondecode(aws_iam_role_policy.control_luxd.policy).Statement : st.Resource if st.Sid == "ReadOwnParameters"])) == toset([
      "arn:aws:ssm:eu-north-1:123456789012:parameter/lux/*",
      "arn:aws:ssm:eu-north-1:123456789012:parameter/lux/cloudflare-tunnel-token",
    ])
    error_message = "With no deploy key, the control role must read only its own prefix and the tunnel token."
  }

  assert {
    condition     = length(aws_ssm_parameter.config_repo_deploy_key_parameter) == 0
    error_message = "No deploy key parameter name should be published for a public config repo."
  }
}

run "deploy_key_parameter_is_readable_and_nothing_more" {
  command = plan

  variables {
    config_repo_deploy_key_parameter = "/acme/lux/config-repo-deploy-key"
  }

  assert {
    condition = toset(one([for st in jsondecode(aws_iam_role_policy.control_luxd.policy).Statement : st.Resource if st.Sid == "ReadOwnParameters"])) == toset([
      "arn:aws:ssm:eu-north-1:123456789012:parameter/lux/*",
      "arn:aws:ssm:eu-north-1:123456789012:parameter/lux/cloudflare-tunnel-token",
      "arn:aws:ssm:eu-north-1:123456789012:parameter/acme/lux/config-repo-deploy-key",
    ])
    error_message = "The control role must read exactly its prefix, the tunnel token and the one deploy key parameter."
  }

  assert {
    condition     = aws_ssm_parameter.config_repo_deploy_key_parameter[0].value == "/acme/lux/config-repo-deploy-key"
    error_message = "The reconciler finds the deploy key through <ssm_prefix>/config_repo_deploy_key_parameter."
  }
}

run "config_repo_url_is_required" {
  command = plan

  variables {
    config_repo_url = " "
  }

  expect_failures = [var.config_repo_url]
}

run "lux_tags_in_var_tags_are_refused" {
  command = plan

  variables {
    tags = { "lux:host" = "x" }
  }

  expect_failures = [var.tags]
}
