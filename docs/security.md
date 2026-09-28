# Security

What lux isolates, what it trusts, and what an operator must do. Details
are in the linked pages.

## Tenants

- **Row-level security.** Every tenant row (Runs, placements, events,
  snapshots, blobs, keys, samples) is guarded by Postgres row-level
  security. luxd connects as `lux_app`, a role that cannot bypass it
  (`migrate` makes sure: no SUPERUSER, no BYPASSRLS), and sets the tenant
  per transaction. A query without a tenant sees nothing.
- **Hosts** belong to a tenant or to the platform. A tenant sees its own
  hosts and platform hosts, and on a shared platform host only its own
  placements and what they hold ([Operators](operators.md#looking)).
- **Operator keys** belong to no tenant and see every tenant. They are made
  only by `luxd admin create-operator-key`, never through the API. Treat one
  like root.

## Keys and sign-in

- API keys are stored hashed. A key has scopes: `read`, `run`, `admin`
  (each implying the ones before), or `operator`.
- The console signs in with a key, kept in the browser tab's session
  storage only, or through Cloudflare Access (`console.auth =
  "cloudflare-access"`). There luxd verifies every Access token itself:
  signed by the team's keys, for the configured application (AUD), not
  expired. Only explicitly allowlisted verified JWT emails are operators;
  other users with a valid email are scoped to the configured default tenant
  (which must exist). Missing or invalid settings fail startup, and a missing
  tenant denies its users. Keep the Access application's policy restrictive:
  every admitted user can administer the default tenant. Service tokens (no
  email) are refused. The Access cookie authenticates reads, and writes
  only when the browser marks them same-origin (`Sec-Fetch-Site`), so another
  site cannot act with it ([Operators](operators.md#signing-in)).
- **Streams from a browser** (exec, attach, ports) authenticate with the
  Access cookie or a **stream ticket**: `POST /v1/runs/{id}/tickets`, made
  with the caller's own key, good once for 60 seconds, for one Run and one
  kind (exec or preview), stored hashed, and dead with the key that made
  it. Every stream upgrade from a page is refused (403 `bad_origin`)
  unless its `Origin` is luxd's `public_url`, the request's own host, or
  one in `console.allowed_origins`, so another site cannot open a shell
  with a visitor's cookie. Clients that send no `Origin` (the CLI) are not
  affected.
- **A terminal is the Run's secrets.** A shell in a Run sees the
  workload's environment, `env` secrets included: whoever may open one
  (`run` scope on the Run's tenant) may read them.

## Workloads

- **Containers** run under rootful Podman with `--userns=auto`: each has
  its own user namespace, and container root is not host root. No
  privileged containers; nested containers are opt-in and still
  unprivileged ([Concepts](concepts.md)).
- **Egress** is denied by default per Run, with nftables and a DNS stub;
  the spec allows hosts ([The RunSpec](runspec.md)).
- **Secrets** are never stored: luxd holds their values in memory from the
  submit or resume that supplied them, runners get them over their
  WebSocket, and output is redacted before it is written anywhere
  ([Concepts](concepts.md)). A workload can still write a secret into its
  state volumes; that is its choice, and is snapshotted like anything else.
- **Credentials a workload uses but must not hold** go through a service
  (`workload.services`): lux-shim adds them to each request it forwards
  from a socket in the container. The values are only in the shim's
  memory, and the shim is not dumpable, so even container root (without
  `CAP_SYS_PTRACE`, which no Run has) can't read them from `/proc`. The
  workload does choose the request path, so point a service only at an
  API with no endpoint that reflects request headers. Git credentials
  never enter the container at all: the runner clones and pushes. Git
  mirrors on a host are per tenant: one tenant never clones from another's.

## Previews

A Run's servers can be reached at `https://<server>-<run suffix>.<preview
domain>` ([Operators](operators.md#previews)). What protects that:

- **Always authenticated.** Cloudflare Access with the preview
  application's own AUD, where luxd also verifies the token and checks
  the user may read the Run (an operator, or its tenant under the Access
  mapping); or, in ticket mode, a `__Host-` cookie (host-only, `Secure`,
  `HttpOnly`, `SameSite=Lax`) that luxd signs (HMAC-SHA256 with a key it
  keeps in its database) and gives out only for a preview ticket of that
  very Run. A cookie made from an API key lasts 12 hours and stops working
  (within a minute) when the key is revoked; one made by a person signed
  in through Cloudflare Access lasts 1 hour, since luxd cannot see the
  Access policy change without a fresh token. The console hands a preview
  ticket only to a host under luxd's own preview domain
  (`<server>-<run>.<preview domain>`, https): a `/preview-auth` link to
  any other host is refused before a ticket is minted, so it cannot leak
  one.
- **A listener of its own** (`preview.listen`) that only proxies: it never
  serves `/v1`, `/runner` or the console, so preview content never shares
  an origin with them.
- **Header hygiene.** Toward the container, luxd removes
  `Cf-Access-Jwt-Assertion`, any `Authorization: Bearer lux…`, and the
  `CF_Authorization` and `__Host-lux_preview` cookies; it sets
  `X-Forwarded-Host`, `X-Forwarded-Proto: https` and `X-Lux-User` (the
  person's email or the key's name). From the container, every
  `Set-Cookie` loses its `Domain`, so a preview can set cookies for its
  own host only.
- **What is left:** preview hosts are subdomains of the preview domain, so
  they are same-site with anything else under its registrable domain, and
  browsers send them cookies other apps set with `Domain=` that parent.
  Serve previews from a domain of their own, or make sure the apps beside
  them use host-only cookies. Preview content is the Run's code: treat a
  preview like any page its author wrote.

## Deployment

- **Reach luxd privately**: its API and console carry every tenant's data.
  Put it behind a tunnel, a VPN or Cloudflare Access; lux has no public
  URLs of its own (port forwarding goes through the CLI), except previews,
  when configured, which are always authenticated (above).
- **Secrets in configuration** (the database password, the S3 secret key)
  are best set in the environment. A configuration file holding them
  should be readable by luxd only (`chmod 600`); luxd warns when others can
  read it ([Operations](operations.md#configuration)).
- **Only luxd holds S3 credentials.** Runners upload and download through
  luxd and presigned URLs.
- **Runners** authenticate with host tokens (`luxd admin
  create-host-token`), scoped to a tenant or the platform and a pool.
