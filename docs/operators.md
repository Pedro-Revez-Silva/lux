# Operators and the console

An operator runs lux for everyone. An **operator key** belongs to no tenant:
it sees every tenant's Runs, hosts and pools, and can act on any of them.
Everything an operator does goes through the same API and CLI as tenants;
the console is a view of that API in a browser.

## Operator keys

```bash
luxd admin create-operator-key [--name alice]    # → {"apiKey": "lux_…"}
```

Only `luxd admin` makes them, never the API. The `operator` scope implies
`admin`, `run` and `read`. Tenant keys, even `admin` ones, never see another
tenant's rows: that is still enforced by Postgres row-level security, and an
operator's request on a Run runs in that Run's tenant's scope.

## One tenant, or all of them

With no tenant from the flag, the environment or the config file, an
operator sees every tenant, and lists show a TENANT column.
`--tenant <id or name>` (env `LUX_TENANT`, `tenant` in
`~/.config/lux/config.toml`, `?tenant=` in the API)
narrows every command to one tenant and shows what that tenant would see.
Tenant keys ignore it. With a default tenant in the config file,
`--tenant ""` (or `LUX_TENANT=`) reaches all tenants again.

A Run's own commands (`get`, `logs`, `stop`, `resume`…) need no `--tenant`:
luxd finds the Run's tenant. Commands that create something for a tenant
(`lux run`, `lux pools set`) need one.

## Looking

```bash
lux status                          # Runs by state, busy/idle, queue, time to start, hosts, capacity
lux ls [--resumable] [--host H] [--state …] [--limit N]
lux get <run>                       # with what a resume would take, when stopped, lost or failed
lux hosts ls [--pool P] [--state S] [--all]
lux hosts get <host>                # lifecycle, capacity, what its live placements hold
lux tenants ls                      # quotas and what each tenant uses
lux history [--since 24h]           # the system over time (sparklines; -o json for the samples)
lux history <run>                   # a Run's CPU, memory, disk, pids, network, across placements
lux history --host <host>
lux events --all [--after ID] [--follow=false]   # every Run's events, as they happen
```

On a platform host (shared by tenants), a tenant sees its own placements and
what they hold, never another tenant's; the host's history is the
operators'.

A host is named by id or name. Two tenants may each have a host of the
same name; an operator then names it by id, or with `--tenant`.

## Acting

```bash
lux hosts drain <host> [--force-evict]          # no new Runs; --force-evict also moves its Runs elsewhere
lux stop <run> / lux cancel <run>
lux migrate <run> [--to HOST] [--input TEXT] [--wait]
lux resume <run> [--to HOST] [--from-snapshot S] [--input TEXT]
```

**Migrate** moves a running Run: it is stopped (its state volumes
snapshotted, as on any stop), then resumed at once on `--to`, or anywhere
but the host it was on (if no other host can take it, it goes back there
rather than wait). It is the path a force-evict drain takes, for one Run. An
agent resumes its session, so it has its whole conversation; if it was in
the middle of a turn, that turn was interrupted. Nothing is said to it
unless `--input` is given: that text is delivered once it runs again ("go
on where you left off", say). A generic workload restarts its command with
its state volumes restored.

A Run already being stopped (by its tenant, a force-evict drain, a cancel)
cannot also be migrated. A Run on a host that is merely cordoned (a plain
drain, scale-down, `pools rm`) is not being stopped by that alone, so
migrate still applies to it. A tenant's `stop` during a migration wins: the
Run stays stopped. A chosen host that stops taking Runs (drained, lost)
before the Run gets there no longer holds it: it goes wherever it may.

**Resume** by an operator can choose the host (`--to`). A Run's secrets are
never stored: luxd holds their values in memory from the submit or resume
that supplied them. While it still does (it has not restarted since), an
operator resumes the Run without them; otherwise only the tenant can, by
supplying them again. `lux get` says which (`secrets: … held by luxd`).

A chosen host (`--to`) must be ready, not draining, and one the Run's tenant
may use; the Run still needs room there. It may be outside the Run's pool
or labels: the operator's choice wins over the spec's placement.

## History

luxd keeps samples of:

- every host, on each heartbeat: CPU in use, memory in use, disk used on
  the runner's data filesystem, and its live placements and what they asked
  for; and the runner process's own CPU, memory (RSS, its peak since the
  last heartbeat, Go heap) and goroutines, with when it started, so a
  restart shows;
- every live placement, on each heartbeat: CPU, memory and pids now, disk,
  and network counters;
- the system, every `LUX_SAMPLE_EVERY`, once in total and once per tenant:
  Runs by state, busy and idle, the queue, Runs started and finished, time
  to start (p50, p95), hosts by state, capacity and what is allocated.
- the control host, the machine luxd itself runs on, with the system: CPU
  and memory used against what it has, each tracked filesystem's used and
  free space (`history.disk_paths`, `LUX_HISTORY_DISK_PATHS`; default `/`;
  a path that cannot be read is skipped and logged), its Postgres
  database's size and connections, and the luxd process's own use (as a
  runner's, above). Only operators viewing all tenants see
  it. The charts draw a line per machine (by hostname) and one for
  Postgres, and a line per luxd process, named by its machine and the tail
  of its id: several luxd instances, or a restart, are separate lines.

Raw samples are rolled up into minutes and hours; each resolution is kept
for its own period (see [Operations](operations.md)). A read picks the
finest resolution that still covers its range. Counters (CPU seconds,
network bytes) are served as rates.

## The console

luxd serves a web console at `/`, the same origin as the API (every path
outside `/v1/` and `/runner/`; old `/console/...` links redirect). It
shows what its user can see: an operator the whole system, with a tenant
filter at the top; a tenant key, that tenant.

- **Overview**: status now, history charts over the chosen range, and a live
  feed of every Run's events. An operator viewing all tenants also gets a
  **Control host** row: luxd's machine (CPU, memory, one disk card per
  tracked path), its Postgres (size, connections), and luxd itself (CPU,
  memory, goroutines), a line per luxd process. A host page charts its
  runner the same way, a line per runner process.
- **Runs**: filterable, with a Run page per Run: output (live), placement
  timeline, resource charts, events, snapshots and artifacts, and the
  actions above.
- **Hosts**: capacity and allocation, and a host page with its history
  (the runner process's too), lifecycle and the Runs on it.
- **Pools** and **Tenants**.

The console is static files built with Bun and embedded in luxd (see
[Development](development.md)). Reach it like the API: through the tunnel
or load balancer you already use for luxd.

### Signing in

`console.auth` (`LUX_CONSOLE_AUTH`) says how:

- **`key`** (the default): the console asks for an API key, kept for the
  browser tab's session only.
- **`cloudflare-access`**: luxd sits behind a Cloudflare Access
  application. Configure `console.cloudflare_access.team`, `.aud`,
  `.operators` (an explicit list of operator email addresses), and
  `.default_tenant` (an existing tenant ID, preferred, or exact name such as
  `absmartly`). Missing or invalid settings prevent startup. Only an email
  in the verified Access JWT can match the operator allowlist; matching is
  case-insensitive. Every other Access user with a valid email is an `admin`
  principal confined to the default tenant, including when `?tenant=` names
  another tenant. An absent default tenant denies their requests; it never
  promotes them to operator. Create the tenant and verify its ID before
  enabling this mode. Keep Access policies restrictive as the default tenant
  grants Run and admin operations to every admitted user. Never put personal
  addresses in a public config example; keep the actual allowlist in private
  deployment configuration. luxd verifies tokens (header or cookie): team
  signature, issuer, audience and expiry. API keys still work alongside.
  The Access cookie authenticates reads and same-origin writes only
  (`Sec-Fetch-Site`). Service tokens without email are refused. Actions are
  recorded as the JWT email. The foot of the sidebar shows who is signed
  in, with a button to sign out of Access (`/cdn-cgi/access/logout`). Their
  photo comes from the identity provider's `picture` claim: in Zero Trust,
  add `picture` under the identity provider's OIDC claims (and make sure it
  requests the `profile` scope); luxd reads it from Access's identity
  endpoint (`oidc_fields.picture`, https URLs only). Without one, the
  sidebar shows initials. In key mode, "Sign out" forgets the key.

  With a tunnel (`cloudflared`), `originRequest.access.required` can also
  refuse unauthenticated requests before they reach luxd.

Other ways to sign in can be added as further `console.auth` values.

### What only operators can do

`lux tenants ls`, `lux migrate`, choosing a host (`resume --to`), and
resuming without supplying secrets. Tenants and keys are made with `luxd
admin` only, never through the API, by operators or anyone.
