# SSM Parameter Store: the infrastructure values the control host's
# reconciler (host/reconcile.py in the config repo) reads on every run —
# bucket names, public URL, Access team/AUD, the Postgres data volume, the
# config repo's URL/ref and deploy-key parameter name — and the Cloudflare
# Tunnel token (a SecureString, created only if
# manage_cloudflare_tunnel_token is true; otherwise it must already exist).
# aws_instance.control ignores user_data changes, so changing one of these
# is a plain `terraform apply` that reaches the host within 5 minutes. The
# lux version and luxd operator settings are not here: they live in the
# config repo's host/lux-host.toml. Postgres passwords are generated on
# the control host and never touch Terraform state or SSM.

variable "ssm_prefix" {
  description = "SSM Parameter Store path prefix for this deployment's parameters."
  type        = string
  default     = "/lux"
}

variable "cloudflare_tunnel_token_parameter" {
  description = "SSM parameter name for the cloudflared tunnel token. Defaults under ssm_prefix."
  type        = string
  default     = ""
}

variable "manage_cloudflare_tunnel_token" {
  description = "Whether Terraform creates the SecureString parameter for the Cloudflare Tunnel token. Must be a plain bool, known at plan time — unlike checking cloudflare_tunnel_token != \"\", which is unknown until apply when the token comes from the cloudflare module's output (an \"Invalid count argument\" error at plan time)."
  type        = bool
  default     = false
}

variable "cloudflare_tunnel_token" {
  description = "Cloudflare Tunnel token. Used only if manage_cloudflare_tunnel_token is true; otherwise the parameter is expected to already exist (e.g. created by deploy/terraform/cloudflare or by hand) and Terraform does not manage it."
  type        = string
  default     = ""
  sensitive   = true
}

locals {
  ssm_prefix                        = var.ssm_prefix
  cloudflare_tunnel_token_parameter = var.cloudflare_tunnel_token_parameter != "" ? var.cloudflare_tunnel_token_parameter : "${local.ssm_prefix}/cloudflare-tunnel-token"
}

resource "aws_ssm_parameter" "tunnel_token_parameter" {
  name  = "${local.ssm_prefix}/tunnel_token_parameter"
  type  = "String"
  value = local.cloudflare_tunnel_token_parameter

  tags = var.tags
}

resource "aws_ssm_parameter" "cloudflare_tunnel_token" {
  count = var.manage_cloudflare_tunnel_token ? 1 : 0

  name  = local.cloudflare_tunnel_token_parameter
  type  = "SecureString"
  value = var.cloudflare_tunnel_token

  tags = var.tags
}

output "cloudflare_tunnel_token_parameter" {
  value = local.cloudflare_tunnel_token_parameter
}