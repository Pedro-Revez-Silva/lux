# The control host's user_data must stay under EC2's 16 KiB limit (raw
# bytes, before base64). Mocked providers: no credentials, no API calls.
# The rendered cloud-config depends only on this module's templates and
# variables, so the mocked ids stand in for real ones at the same length.
# The control host must also carry no lux:* tag, and var.tags must refuse
# one: luxd's terminate permission keys on lux:* tags (iam.tf), so such a
# tag would let luxd terminate its own host.

mock_provider "aws" {
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
  # The backup script in user_data embeds the bucket name.
  mock_resource "aws_s3_bucket" {
    defaults = {
      id = "lux-pg-backups-123456789012-eu-north-1"
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
  lux_version                    = "v0.0.0"
  public_url                     = "https://lux.example.com"
  cf_access_team                 = "example"
  cf_access_aud                  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  manage_cloudflare_tunnel_token = true
  cloudflare_tunnel_token        = "token"
}

run "control_user_data_fits_ec2_limit" {
  command = apply

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

run "lux_tags_in_var_tags_are_refused" {
  command = plan

  variables {
    tags = { "lux:host" = "x" }
  }

  expect_failures = [var.tags]
}
