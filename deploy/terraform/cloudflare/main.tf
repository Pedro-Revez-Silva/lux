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
    ingress = [
      {
        hostname = var.hostname
        service  = "http://localhost:${var.origin_port}"
      },
      {
        service = "http_status:404"
      },
    ]
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
