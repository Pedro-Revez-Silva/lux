variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "eu-north-1"
}

variable "name" {
  description = "Prefix for resource names."
  type        = string
  default     = "lux"
}

variable "config_repo_url" {
  description = "Git URL of this config repo, as the control host clones it (SSH with a deploy key, or HTTPS for a public repo)."
  type        = string
}

variable "config_repo_ref" {
  description = "Branch the control host follows."
  type        = string
  default     = "main"
}

variable "config_repo_path" {
  description = "Subdirectory of this repo that holds host/ (empty: the repo root)."
  type        = string
  default     = ""
}

variable "config_repo_deploy_key_parameter" {
  description = "SSM SecureString holding a read-only SSH deploy key for this repo; empty for a public repo."
  type        = string
  default     = ""
}

variable "public_url" {
  description = "The public URL clients and runners use (the Cloudflare Tunnel hostname)."
  type        = string
}

variable "amd64_runners_enabled" {
  description = "Also create an amd64 runner launch template (m7i.2xlarge) alongside the arm64 one."
  type        = bool
  default     = false
}

variable "cf_account_id" {
  description = "Cloudflare account id."
  type        = string
}

variable "cf_zone_id" {
  description = "Cloudflare zone id."
  type        = string
}

variable "cf_access_team" {
  description = "Cloudflare Access team domain (e.g. \"acme\" or \"acme.cloudflareaccess.com\"), from Zero Trust > Settings > Custom Pages in the dashboard."
  type        = string
}

variable "cf_allowed_emails" {
  description = "Individual emails Access lets in as operators."
  type        = list(string)
  default     = []
}

variable "cf_allowed_email_domains" {
  description = "Email domains Access lets in as operators."
  type        = list(string)
  default     = []
}
