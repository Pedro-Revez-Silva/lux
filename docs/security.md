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

## Deployment

- **Reach luxd privately**: its API and console carry every tenant's data.
  Put it behind a tunnel, a VPN or Cloudflare Access; lux has no public
  URLs of its own (port forwarding goes through the CLI).
- **Secrets in configuration** (the database password, the S3 secret key)
  are best set in the environment. A configuration file holding them
  should be readable by luxd only (`chmod 600`); luxd warns when others can
  read it ([Operations](operations.md#configuration)).
- **Only luxd holds S3 credentials.** Runners upload and download through
  luxd and presigned URLs.
- **Runners** authenticate with host tokens (`luxd admin
  create-host-token`), scoped to a tenant or the platform and a pool.
