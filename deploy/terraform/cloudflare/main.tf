# Cloudflare Tunnel + Access in front of luxd: nothing exposed directly.
# The tunnel's only ingress is http://localhost:<origin_port> (luxd,
# reached over loopback since cloudflared runs on the control host
# itself); DNS for the chosen hostname points at the tunnel.
#
# Access sits at Cloudflare's edge, in front of the tunnel, and makes its
# allow/deny decision purely on request domain/path against an
# application's policies — it never looks at the request body or headers
# (an `Authorization: Bearer <key>` does not make Access let a request
# through). So two Access applications, not one:
#
#   - "console", on the bare hostname: an allow policy for the given
#     emails/domains. This is what gates a person opening the console in
#     a browser.
#   - "api-bypass", on the /v1/* and /runner/* destinations under the
#     same hostname: a `bypass` decision, matching everyone. Runners
#     authenticate with a per-host runner token and API/CLI clients with
#     lux API keys (docs/operators.md "Signing in"), both checked by
#     luxd itself, not Access — internal/server/access.go's consoleUser
#     only ever runs for a request with *no* API key. Without this
#     second application, every /v1 and /runner request would need an
#     Access session before it even reached luxd, which breaks runners
#     and the CLI outright (neither can obtain one).
#
# Access resolves overlapping applications by specificity (more specific
# path wins — see the provider's docs on application paths), so
# api-bypass's narrower match on /v1/* and /runner/* takes precedence
# over console's bare-hostname match for those paths, and console's
# allow policy still governs everything else. The console's own browser
# calls to /v1 (e.g. streaming output) go through api-bypass at the edge
# and are unauthenticated *there*, but still carry the CF_Authorization
# cookie Access set when the person signed in; luxd's own consoleUser
# check reads that cookie directly, so the console keeps working without
# Access gating /v1 a second time.
#
# Previews (optional; nothing below exists unless preview_domain is set):
# luxd serves a Run's preview ports on a listener of its own, under
# <name>.<preview_domain>. Setting preview_domain adds a proxied wildcard
# CNAME *.<preview_domain> to the same tunnel, and an ingress rule
# sending *.<preview_domain> to http://localhost:<preview_origin_port>
# (after the luxd hostname rule, before the catch-all 404). luxd's side:
#
#   [preview]
#   domain = "<preview_domain>"
#   listen = "127.0.0.1:<preview_origin_port>"
#   auth   = "cloudflare-access"
#
# with LUX_PREVIEW_CF_ACCESS_AUD set to this module's
# preview_access_application_aud output. That comes from a third Access
# application, on *.<preview_domain>, created when preview_access_emails
# or preview_access_email_domains is non-empty, with its own allow policy
# (who may open previews need not match who may open the console).
#
# Cloudflare's Universal SSL covers the zone apex and one level of
# wildcard (*.example.com), not a second-level wildcard such as
# *.preview.example.com, so previews get a certificate error unless
# something else covers them: preview_certificate_pack = true orders an
# Advanced certificate pack for *.<preview_domain> and <preview_domain>
# (needs Advanced Certificate Manager on the zone); otherwise upload a
# custom certificate outside this module.

variable "account_id" {
  description = "Cloudflare account id."
  type        = string
}

variable "zone_id" {
  description = "Cloudflare zone id for the DNS record and Access applications."
  type        = string
}

variable "name" {
  description = "Prefix for the tunnel's name."
  type        = string
  default     = "lux"
}

variable "hostname" {
  description = "Public hostname for luxd, e.g. \"lux.example.com\"."
  type        = string
}

variable "origin_port" {
  description = "Port cloudflared reaches luxd on (matches the aws module's luxd_port)."
  type        = number
  default     = 7070
}

variable "allowed_emails" {
  description = "Individual emails Access lets in as operators."
  type        = list(string)
  default     = []
}

variable "allowed_email_domains" {
  description = "Email domains Access lets in as operators (e.g. [\"example.com\"])."
  type        = list(string)
  default     = []
}

variable "access_session_duration" {
  description = "How often an Access session must re-authenticate."
  type        = string
  default     = "24h"
}

variable "enable_api_bypass" {
  description = "Whether to create the api-bypass Access application that exempts /v1/* and /runner/* (see the module comment) from needing an Access session. On by default: without it, runners and API/CLI clients — which authenticate with lux keys, not Access — cannot reach luxd through the tunnel at all."
  type        = bool
  default     = true
}

variable "preview_domain" {
  description = "Domain luxd serves Run previews under (luxd's [preview] domain), e.g. \"preview.example.com\" — the domain itself, not \"*.\"-prefixed. Empty (default): no preview DNS, ingress, Access or certificate."
  type        = string
  default     = ""

  validation {
    condition     = !startswith(var.preview_domain, "*") && !startswith(var.preview_domain, ".")
    error_message = "preview_domain is the domain itself (e.g. \"preview.example.com\"); the module adds the \"*.\"."
  }
}

variable "preview_origin_port" {
  description = "Port cloudflared reaches luxd's preview listener on (luxd's [preview] listen = \"127.0.0.1:<port>\")."
  type        = number
  default     = 7071
}

variable "preview_access_emails" {
  description = "Individual emails Access lets in to previews. This or preview_access_email_domains non-empty creates the preview Access application; requires preview_domain."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.preview_access_emails) == 0 || var.preview_domain != ""
    error_message = "preview_access_emails requires preview_domain."
  }
}

variable "preview_access_email_domains" {
  description = "Email domains Access lets in to previews (e.g. [\"example.com\"]); requires preview_domain."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.preview_access_email_domains) == 0 || var.preview_domain != ""
    error_message = "preview_access_email_domains requires preview_domain."
  }
}

variable "preview_certificate_pack" {
  description = "Order an Advanced certificate pack (Google CA, TXT validation, 90 days) for *.<preview_domain> and <preview_domain>, since Universal SSL does not cover second-level wildcards. Needs Advanced Certificate Manager on the zone; requires preview_domain."
  type        = bool
  default     = false

  validation {
    condition     = !var.preview_certificate_pack || var.preview_domain != ""
    error_message = "preview_certificate_pack requires preview_domain."
  }
}

locals {
  previews         = var.preview_domain != ""
  preview_wildcard = "*.${var.preview_domain}"
  preview_access   = local.previews && length(var.preview_access_emails) + length(var.preview_access_email_domains) > 0
}

resource "random_id" "tunnel_secret" {
  byte_length = 32
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "lux" {
  account_id    = var.account_id
  name          = "${var.name}-tunnel"
  tunnel_secret = random_id.tunnel_secret.b64_std
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "lux" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lux.id
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "lux" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lux.id

  config = {
    ingress = concat(
      [{
        hostname = var.hostname
        service  = "http://localhost:${var.origin_port}"
      }],
      local.previews ? [{
        hostname = local.preview_wildcard
        service  = "http://localhost:${var.preview_origin_port}"
      }] : [],
      [{
        service = "http_status:404"
      }],
    )
  }
}

resource "cloudflare_dns_record" "lux" {
  zone_id = var.zone_id
  name    = var.hostname
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.lux.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1 # automatic (required by the API when proxied = true)
}

resource "cloudflare_zero_trust_access_application" "lux" {
  zone_id              = var.zone_id
  name                 = "${var.name} console"
  domain               = var.hostname
  type                 = "self_hosted"
  session_duration     = var.access_session_duration
  app_launcher_visible = false

  # Provider v5: a list of {id, precedence} objects, not bare ids.
  policies = [{
    id         = cloudflare_zero_trust_access_policy.lux.id
    precedence = 1
  }]
}

resource "cloudflare_zero_trust_access_policy" "lux" {
  account_id = var.account_id
  name       = "${var.name} operators"
  decision   = "allow"

  include = concat(
    [for e in var.allowed_emails : { email = { email = e } }],
    [for d in var.allowed_email_domains : { email_domain = { domain = d } }],
  )
}

# Narrower than "lux" above, so it takes precedence on these two path
# prefixes (see the module comment): luxd's own key/token checks are the
# only gate here, not an Access session.
resource "cloudflare_zero_trust_access_application" "api_bypass" {
  count = var.enable_api_bypass ? 1 : 0

  zone_id              = var.zone_id
  name                 = "${var.name} api bypass"
  type                 = "self_hosted"
  session_duration     = var.access_session_duration
  app_launcher_visible = false

  destinations = [
    { type = "public", uri = "${var.hostname}/v1/*" },
    { type = "public", uri = "${var.hostname}/runner/*" },
  ]

  policies = [{
    id         = cloudflare_zero_trust_access_policy.api_bypass[0].id
    precedence = 1
  }]
}

resource "cloudflare_zero_trust_access_policy" "api_bypass" {
  count = var.enable_api_bypass ? 1 : 0

  account_id = var.account_id
  name       = "${var.name} api bypass"
  decision   = "bypass"
  include    = [{ everyone = {} }]
}

resource "cloudflare_dns_record" "preview" {
  count = local.previews ? 1 : 0

  zone_id = var.zone_id
  name    = local.preview_wildcard
  type    = "CNAME"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.lux.id}.cfargotunnel.com"
  proxied = true
  ttl     = 1
}

resource "cloudflare_zero_trust_access_application" "preview" {
  count = local.preview_access ? 1 : 0

  zone_id              = var.zone_id
  name                 = "${var.name} previews"
  domain               = local.preview_wildcard
  type                 = "self_hosted"
  session_duration     = var.access_session_duration
  app_launcher_visible = false

  policies = [{
    id         = cloudflare_zero_trust_access_policy.preview[0].id
    precedence = 1
  }]
}

resource "cloudflare_zero_trust_access_policy" "preview" {
  count = local.preview_access ? 1 : 0

  account_id = var.account_id
  name       = "${var.name} preview viewers"
  decision   = "allow"

  include = concat(
    [for e in var.preview_access_emails : { email = { email = e } }],
    [for d in var.preview_access_email_domains : { email_domain = { domain = d } }],
  )
}

# Universal SSL stops at one wildcard level, so *.<preview_domain> needs
# its own certificate (see the module comment).
resource "cloudflare_certificate_pack" "preview" {
  count = local.previews && var.preview_certificate_pack ? 1 : 0

  zone_id               = var.zone_id
  type                  = "advanced"
  hosts                 = [local.preview_wildcard, var.preview_domain]
  validation_method     = "txt"
  validity_days         = 90
  certificate_authority = "google"
  cloudflare_branding   = false
}

output "tunnel_id" {
  value = cloudflare_zero_trust_tunnel_cloudflared.lux.id
}

output "tunnel_token" {
  description = "The tunnel token cloudflared needs (aws module's cloudflare_tunnel_token variable)."
  value       = data.cloudflare_zero_trust_tunnel_cloudflared_token.lux.token
  sensitive   = true
}

output "access_application_aud" {
  value = cloudflare_zero_trust_access_application.lux.aud
}

output "preview_access_application_aud" {
  description = "AUD tag of the preview Access application (luxd's LUX_PREVIEW_CF_ACCESS_AUD); null when it is not created."
  value       = one(cloudflare_zero_trust_access_application.preview[*].aud)
}
