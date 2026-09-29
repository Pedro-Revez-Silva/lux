# Previews are off unless preview_domain is set, and then add a wildcard
# record and an ingress rule on the same tunnel, plus (on request) an
# Access application and a certificate pack. Mocked providers: no
# credentials, no API calls.

mock_provider "cloudflare" {}
mock_provider "random" {}

variables {
  account_id     = "0123456789abcdef0123456789abcdef"
  zone_id        = "fedcba9876543210fedcba9876543210"
  hostname       = "lux.example.com"
  allowed_emails = ["ops@example.com"]
}

run "defaults_create_no_previews" {
  command = plan

  assert {
    condition     = length(cloudflare_dns_record.preview) == 0
    error_message = "no preview_domain must mean no wildcard record"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_application.preview) == 0 && length(cloudflare_zero_trust_access_policy.preview) == 0
    error_message = "no preview_domain must mean no preview Access application"
  }
  assert {
    condition     = length(cloudflare_certificate_pack.preview) == 0
    error_message = "no preview_domain must mean no certificate pack"
  }
  assert {
    condition     = length(cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress) == 2
    error_message = "ingress must be just the luxd hostname and the catch-all"
  }
  assert {
    condition     = output.preview_access_application_aud == null
    error_message = "preview_access_application_aud must be null without a preview Access application"
  }
}

run "preview_domain_with_access_and_certificate" {
  command = plan

  variables {
    preview_domain           = "preview.example.com"
    preview_access_emails    = ["viewer@example.com"]
    preview_certificate_pack = true
  }

  assert {
    condition     = length(cloudflare_dns_record.preview) == 1 && cloudflare_dns_record.preview[0].name == "*.preview.example.com"
    error_message = "expected one wildcard record *.preview.example.com"
  }
  assert {
    condition     = cloudflare_dns_record.preview[0].proxied && cloudflare_dns_record.preview[0].type == "CNAME"
    error_message = "the wildcard record must be a proxied CNAME"
  }
  assert {
    condition     = length(cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress) == 3
    error_message = "ingress must be luxd, previews, catch-all"
  }
  assert {
    condition     = cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress[0].hostname == "lux.example.com"
    error_message = "the luxd hostname rule must stay first"
  }
  assert {
    condition = (
      cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress[1].hostname == "*.preview.example.com" &&
      cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress[1].service == "http://localhost:7071"
    )
    error_message = "the preview rule must send *.preview.example.com to the preview port"
  }
  assert {
    condition     = cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress[2].service == "http_status:404"
    error_message = "the catch-all must stay last"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_application.preview) == 1 && cloudflare_zero_trust_access_application.preview[0].domain == "*.preview.example.com"
    error_message = "expected one preview Access application on *.preview.example.com"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_policy.preview) == 1 && length(cloudflare_zero_trust_access_policy.preview[0].include) == 1
    error_message = "expected one preview allow policy with the one email"
  }
  assert {
    condition     = length(cloudflare_certificate_pack.preview) == 1 && toset(cloudflare_certificate_pack.preview[0].hosts) == toset(["*.preview.example.com", "preview.example.com"])
    error_message = "the certificate pack must cover *.preview.example.com and preview.example.com"
  }
  assert {
    condition     = cloudflare_certificate_pack.preview[0].type == "advanced" && cloudflare_certificate_pack.preview[0].validation_method == "txt"
    error_message = "the certificate pack must be advanced with TXT validation"
  }
}

run "preview_domain_alone_creates_no_access_or_certificate" {
  command = plan

  variables {
    preview_domain      = "preview.example.com"
    preview_origin_port = 8081
  }

  assert {
    condition     = cloudflare_zero_trust_tunnel_cloudflared_config.lux.config.ingress[1].service == "http://localhost:8081"
    error_message = "the preview rule must follow preview_origin_port"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_application.preview) == 0 && length(cloudflare_certificate_pack.preview) == 0
    error_message = "no access lists and no certificate pack requested must create neither"
  }
}

run "certificate_pack_requires_preview_domain" {
  command = plan

  variables {
    preview_certificate_pack = true
  }

  expect_failures = [var.preview_certificate_pack]
}

run "access_lists_require_preview_domain" {
  command = plan

  variables {
    preview_access_email_domains = ["example.com"]
  }

  expect_failures = [var.preview_access_email_domains]
}
