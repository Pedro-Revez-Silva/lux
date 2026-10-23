# Operations

## luxd

`luxd` is stateless. Postgres holds state and S3 holds bytes, so you can
run several instances behind a load balancer. Each runner holds one
WebSocket to one instance.

```bash
luxd migrate      # once per upgrade, as the database owner
luxd serve        # as many as you like
```

### Building

`make build` builds the operator console (`console/`, with
[Bun](https://bun.sh)) and then the binaries in `bin/`; luxd embeds the
console. A luxd built with plain `go build` and no console build serves, at
`/`, a page saying how to build it.

### Postgres

Postgres 13 or later (tested on 18); no extensions. `luxd migrate` runs as
the database owner, who must be able to create roles: it creates `lux_app`
(no SUPERUSER, no BYPASSRLS: row-level security is the tenant boundary),
sets its password to `LUX_APP_PASSWORD` (default `lux_app`: set your own),
and grants it every table, new ones included, on each run. `luxd serve`
connects as `lux_app`; `luxd admin` works with either DSN.

Each luxd also holds one long-lived connection, outside its pool and made
with the same DSN, listening for Run events (`LISTEN lux_events`), which
is what makes the console and `lux logs -f` live. LISTEN does not work
through PgBouncer in transaction or statement mode: point luxd at Postgres
directly, or at a session-mode pool. If it cannot listen, updates still
arrive, a few seconds late.

### Upgrading

Upgrade runners for the `sandbox.cgroupDelegation: false` default to apply:
a new luxd with an older runner still delegates every nested Run.

1. Check the configuration with the new binary: `luxd validate`, run as
   the service user with the unit's environment. It fails (exit 1) on
   anything `luxd serve` would refuse before connecting, with the same
   message; the running luxd is untouched.
2. Run `luxd migrate` with the owner's DSN. It applies what is new and is
   safe to run again; running luxds keep working meanwhile, except while a
   migration builds an index, and except in a release whose note below
   says otherwise (056; 062–064; 079). 045 and 046 index the pool and host events
   tables in their migration's transaction: until each commits, anything
   luxd does that records an event (heartbeats, registration, placements,
   drains) waits — about 3–4 s per million events for the two together.
   A migration that fails on a deadlock with luxd can be run again.
   For a release with 056, steps 2–3 change: luxds do not keep working
   meanwhile, and are not rolled. 056 renames `runs.cancel_requested` to
   `terminate_requested` and rewrites `cancelled` to `terminated` in stored
   Runs, events and the cost queue. While 056 runs it blocks every access
   to `runs` (each luxd request, scheduler pass and runner report waits on
   it) for about 1–2 s per million `run_events` rows, plus the scan of
   `runs` itself; once it commits, a luxd older than it fails every
   request that reads or writes the column, until it is restarted on the
   new binary. With one luxd, migrate and then restart it at once (the
   host reconciler does exactly this): the old luxd's failures last only
   between the commit and the restart, and runners retry their reports.
   With several luxds sharing the database, stop every one before 056
   runs and start them again on the new binary (step 3) once it has
   committed, so that no request reaches an older luxd.
   For a release with 062–066 (block storage as its own cost family),
   grant the EC2 role `ec2:DescribeVolumes` **before** deploying it
   ([EC2 pools](#ec2-pools)): until a host's volumes are known, every Run
   on an EC2 host stays `incomplete` and is retried hourly; hosts that
   terminate before the grant need `luxd admin costs backfill-volumes`
   ([below](#block-storage-of-hosts-launched-before-luxd-recorded-volumes)).
   062–064 each take an ACCESS EXCLUSIVE lock in their transaction: 062 on
   `host_rates`, 063 on `cost_placement_snapshots`, 064 on `cost_hourly`,
   which revalidates its CHECK and rebuilds `cost_hourly_host` in about
   3.2 s per 4.4 million `cost_hourly` rows; 062 and 063 are sub-second at
   ordinary sizes. Until each commits, cost writes and cost reads
   (dashboards) wait. 066 builds `cost_hourly_host_pool_hour` the same
   way (about 3 s per 5 million rows), holding back cost writes only.
   Running luxds do **not** keep working through this release: once 062
   and 063 commit, an older luxd's cost writes (the cost drainer and the
   price refresh) fail with "no unique or exclusion constraint matching the
   ON CONFLICT specification" until it is restarted on the new binary.
   No data is lost: the cost queue and the price refresh retry them. So
   migrate and then restart (or roll) every luxd promptly.
   067–070 (peak CPU and pressure stalls, image platforms, market data)
   add nullable columns to `placements` and `placement_samples` and two new
   tables: no rewrite, and older luxds keep working. Grant the luxd role
   `ec2:GetSpotPlacementScores` and `ec2:DescribeSubnets` for complete
   [market data](telemetry.md#market-data); without them it is sampled
   without placement scores, or in the zones of the pools' hosts.
   078 (`runs.first_pool_id`) adds a column without a default: an ACCESS
   EXCLUSIVE lock on `runs` (and writes to `pools` held back) for a catalog
   change only, committed at once, after waiting up to `lock_timeout`
   (5 s) for transactions open on `runs`, during which new reads and
   writes of `runs` queue behind it; on timeout 078 fails and `luxd
   migrate` is run again. 079 then fills it in for Runs that had
   already ended (succeeded, failed, terminated), in a transaction of its
   own, about 41–51 s per million ended Runs on the test machine and more
   under load. It holds the row locks of those ended Runs until it
   commits. Reads, submits and a write to a single live Run go on; a write
   to an ended Run (a resume, a late report) waits. So does a **live**
   Run's runner report, for the rest of 079, behind the cost drainer of
   any luxd, old or new: the drainer locks a chunk of claimed Runs (up to
   100, live and ended mixed) in id order and waits at the first ended
   one 079 holds, keeping the live Runs before it locked
   (`writeComputeChunk` in `internal/server/costqueue.go`). Meanwhile luxd
   handles none of that host's heartbeats, which queue behind the report,
   and the host is lost once the wait exceeds the lease (`LUX_LEASE`,
   30 s by default). Count the ended Runs first:
   `SELECT count(*) FROM runs WHERE first_started_at IS NOT NULL AND state IN ('succeeded', 'failed', 'terminated')`.
   Above 300,000 (about 15 s at the measured rate, half the default lease;
   proportionally fewer with a shorter `LUX_LEASE`), steps 2–3 change:
   either stop every `luxd serve` before 079 runs and start them on the
   new binary once it has committed (time no luxd hears a host is not
   held against it), or restart every luxd with `[costs] enabled = false`
   (`LUX_COSTS=false`) before step 2 and with costs on again in step 3,
   once 079 has committed (Runs queued meanwhile are drained then). Live
   and resting Runs are left NULL and counted by the same rule when read.
   079 then builds `runs_first_pool_started`, holding back writes to
   `runs` (not reads) for the build, about 0.3 s per 200,000 Runs.
   Otherwise older luxds keep working.
3. Restart (or roll) every `luxd serve` onto the new binary; for a release
   with 056 and several luxds, or with 079 run with every luxd stopped
   (above), start every one of them, all stopped in step 2: do not roll.

Migrate first: a luxd newer than its schema does not check it, and fails
requests that touch what is missing (luxds older than the schema keep
working, except across the releases with 056 and with 062–064 above). Configuration through the environment alone keeps working: a
configuration file is optional.

Each Linux release tarball has a `FEATURES` file at its root, one feature
per line. A deployer reads it before running anything from the release,
since older releases lack both the file and the commands it names:

| Feature | Since the release says it, a deployer may |
| --- | --- |
| `validate` | run `luxd [--config FILE] validate` against the host's configuration before migrating or switching to the release. |
| `resume-policy` | let tenants submit a RunSpec with `resumePolicy` `restart`, `manual` or `never` ([resume policy](runspec.md#resume-policy)), once every luxd sharing the database runs such a release: an older luxd treats the Run as `auto`, and one that rewrites its spec drops the field for good. |
| `artifact-publish` | tell workloads to publish with `/.lux/bin/lux-shim publish` ([artifacts](runspec.md#artifacts)) once every luxd and then every runner runs such a release (luxd first: an older luxd refuses a runner's `artifact.published` report, and the runner retries it until luxd is upgraded). Such a runner no longer collects what a workload writes into the old `$LUX_ARTIFACTS` directory, nor sets the variable: a workload that still writes there loses those files. |
| `egress-wildcards` | let tenants submit a RunSpec with a wildcard host rule in `network.egress`, `*.<domain>` ([network egress](runspec.md#network-egress)), once every luxd and runner runs such a release: an older luxd refuses the spec, and an older runner allows no name under it. |
| `git-credentials` | send `credentials` on a push or a sync ([git](runspec.md#git)), once every luxd runs such a release: an older luxd refuses the field (422). A Run whose host runs an older lux-runner gets 409 `git_credentials_unsupported` until the host is upgraded. luxd holds the values in memory only until the runner has the request, so a credentialed push or sync must reach the luxd holding the runner's connection (a single luxd, or routing that sends it to the luxd the Run's host is connected to, as interactive streams need); any other luxd answers 503 `host_unreachable` and queues nothing. A queued request whose values are gone (luxd restarted, or the runner reconnected elsewhere first) ends failed, saying to send it again. |
| `pool-sizes` | give pools a `shape` and submit or resume with `resources.size` ([shaped pools](concepts.md#shaped-pools)), once every luxd runs such a release: an older luxd refuses both fields as unknown (422/400), and one sharing the database would submit to a shaped pool without rounding. Runners need nothing new for that; placing on a shaped pool needs `disk-shaping` too. |
| `disk-shaping` | shaped pools place Runs only on runners that advertise `disk-scale` and the shape's nominal capacity ([shaped pools](concepts.md#shaped-pools)), and a shape takes `diskHeadroom`: upgrade luxd and every runner of a shaped pool, and replace its hosts (luxd launches new ones with `LUX_RUNNER_CPUS`, `LUX_RUNNER_MEMORY`, `LUX_RUNNER_DISK` and `LUX_RUNNER_DISK_HEADROOM`), before shaping a pool that has Runs: an older runner's host is skipped for them, and an older luxd refuses `diskHeadroom` as unknown and places shaped Runs anywhere. Unshaped pools and runners started without `--disk-headroom` are unchanged. |
| `pool-placement` | set a pool's `placement` (`spread` or `pack`, [placement](concepts.md#placement-spread-or-pack)), once every luxd runs such a release: an older luxd refuses the field as unknown, and one sharing the database spreads every pool, shaped ones included. Upgrading also turns shaped pools that set none to `pack`. |
| `disk-grace` | set `[defaults] disk_grace` (`LUX_DEFAULT_DISK_GRACE`) and let tenants submit `resources.diskGrace` ([disk is measured](runspec.md#rules)), once every luxd and then every runner runs such a release: an older luxd refuses the key and the field, and an older runner ignores the grace and stops a Run on its first sample over its disk. |
| `disk-pressure` | rely on runners that scale disk (`--disk-headroom`) guarding their hosts' free space ([disk pressure](#disk-pressure)) instead of the 2× ceiling, once every luxd and then every such runner runs such a release. **luxd first**: an older luxd neither blocks admissions on a host's `diskPressure` nor stops a Run on `disk.pressure_stop`, and such a runner no longer has the ceiling, so a Run over its limit would run on until its grace ends. An older runner on a newer luxd is unchanged (it keeps the ceiling and sends no pressure, read as none). Runners without `--disk-headroom` keep the 2× ceiling in every release. |
| `managed-images` | configure `[images]` and let tenants submit `image.build.publish`, `image.build.contextFiles` and `image.build.contexts` ([published images](runspec.md#published-images), [build contexts](runspec.md#build-contexts)), once every luxd runs such a release: an older luxd refuses the fields as unknown, and one sharing the database places such Runs on any runner and sends no registry credential. Runs using them, or naming an image in the managed registry, are placed only on runners advertising `managed-images-v1`: upgrade runners for them to start. Other Runs are unchanged, except that build keys now cover the platform, so the first build of an existing build-cache entry after the upgrade misses once. |
| `server-inject` | give servers an `inject` ([servers](runspec.md#what-inject-touches)) in `workload.servers`, `POST`/`PATCH /v1/servers`, a Run's servers and `lux server --inject`, once every luxd runs such a release: an older luxd refuses the field as unknown, and one sharing the database proxies pages without the snippet. Runners need nothing new. |

A release with no `FEATURES` file, or no `validate` line, predates
`luxd validate`.

#### Configuration compatibility

A configuration that one release accepts, the next accepts too:

- A new setting always has a default or is optional: a file written for the
  previous release needs no edit.
- A removed setting stays accepted for one more release. luxd ignores it and
  warns `luxd: warning: retired: <key>; remove it` (the TOML key or the
  variable, never its value). The release after that refuses it as an
  unknown key. Retired settings are listed in `retiredKeys`
  (`cmd/luxd/config.go`).

### Configuration

luxd reads a TOML file: `--config FILE` (before or after the command),
else `LUX_CONFIG`, else `/etc/lux/luxd.toml` if it exists. Every setting
also has an environment variable, which overrides the file, so a secret can
stay out of it (`LUX_DATABASE_URL`, `LUX_S3_SECRET_KEY`). An unknown key or
a bad value stops luxd, naming it. luxd warns when others can read the
file: keep it `chmod 600`, or keep secrets in the environment.
[luxd.example.toml](luxd.example.toml) has every key with its default and
its variable; the table below lists them by variable.

`luxd [--config FILE] validate` checks a configuration the way `luxd serve`
does before it opens any connection, and connects to nothing, binds
nothing and writes nothing: the file and the environment are loaded
(unknown keys, types, values), `database.url` and `s3.bucket` must be set,
the database URL must parse, `s3.endpoint` and `s3.public_endpoint` (when
set) must be absolute http(s) URLs, `s3.region` must not be empty,
`s3.access_key` and `s3.secret_key` must be set together or not at all,
`listen` (and `preview.listen` when previews are on) must be `host:port`
with a numeric port 0–65535 or none (service names are refused; the host
is not resolved), and the console,
preview, cost-plugin, defaults, history and cost settings must be
consistent. It prints `ok: FILE` (or `ok: no file`) and exits 0; warnings
(a readable file, a retired key) go to stderr, naming the key, never a
value. On a refusal it prints what `serve` would and exits 1; extra
arguments exit 2. Not checked: whether the database and bucket are
reachable, and AWS shared config and credential sources, which the SDK
resolves when serve connects. It reads only local files: the
configuration, and any password, service or TLS files the database URL
names. A named pipe there would block it, so a caller should still run it
under a timeout. `luxd check-config` is an alias of `validate`.

| Variable | Default | |
| --- | --- | --- |
| `LUX_DATABASE_URL` | — | For `serve`, a DSN for the `lux_app` role; for `migrate`, the owner's. One file can serve both: set this variable per command. |
| `LUX_APP_PASSWORD` | `lux_app` | `migrate` sets `lux_app`'s password to it, each run. Set your own. |
| `LUX_LISTEN` | `127.0.0.1:7070` | Address to serve on. |
| `LUX_PUBLIC_URL` | — | The URL clients use. Runners too, unless `LUX_RUNNER_URL` is set. |
| `LUX_RUNNER_URL` | = public_url | The URL runners dial, if different from `LUX_PUBLIC_URL` (a private IP such as `http://10.0.1.10:7070`, unreachable from outside the VPC). |
| `LUX_RUNNER_BIN_DIR` | `/usr/local/lib/lux/runner` | Runner binaries luxd serves and hashes (sha256) at startup, for self-update: `<dir>/linux-{arm64,amd64}/{lux-runner,lux-shim}`. A missing arch or file is simply not offered. |
| `LUX_RUNNER_CONTROL_PLANE_PORTS` | — | TCP ports on the addresses the runner URL's host resolves to that Runs may reach, comma-separated (`runners.control_plane_ports`, a list, in the file): for a companion service on luxd's host, such as a build cache on `8090`. What a Run then reaches is in [Network egress](runspec.md#network-egress). Every runner gets the list on each (re)connect and applies it to its Runs at once, running ones included; an empty list keeps the control plane fully blocked, as does a runner connected to a luxd that predates the setting. Runners older than this release ignore the setting (the control plane stays blocked for their Runs) until they self-update. Integers 1-65535, at most 16; duplicates are collapsed. Refused at startup: luxd's own ports, i.e. the port of `LUX_LISTEN`, of the runner URL and of `LUX_PUBLIC_URL` (explicit, or 80/443 by scheme), and with previews on, `LUX_PREVIEW_LISTEN`'s and `LUX_PREVIEW_PUBLIC_PORT`. **Warning:** every Run on every host, untrusted ones included, can then reach that service: only expose one built for untrusted callers (authenticated, or holding nothing a Run must not read or write). |
| `LUX_S3_BUCKET` | — | Where snapshots, output and artifacts go. |
| `LUX_S3_ENDPOINT` | AWS | For MinIO and other S3-compatible stores (path-style). |
| `LUX_S3_PUBLIC_ENDPOINT` | = endpoint | The endpoint presigned URLs are signed for, if runners and clients reach S3 by another name. |
| `LUX_S3_REGION` | `us-east-1` | |
| `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY` | AWS chain | Only luxd holds S3 credentials. |
| `LUX_LEASE` | `30s` | A host that misses heartbeats this long is lost, along with its live placements. At least 3s. Runners heartbeat every third of it, and drop a connection that is silent for two of those. Time no luxd could hear them (every luxd stopped, frozen whole, or cut off from Postgres) is not held against them: nothing is lost for missed heartbeats during it, nor for a lease (plus 10s) after a luxd is back, so a running Run survives a deploy or an outage of any length, and a host that died meanwhile is lost soon after. Other timers (`LUX_LAUNCH_TIMEOUT`, `LUX_LOST_GRACE`) still count it. It is also how long a Run with secrets waits for a luxd that holds their values (after a restart, say) before it stops, to be resumed with them. |
| `LUX_TICK` | `1s` | Scheduler and reaper interval. |
| `LUX_DEFAULT_CPUS`, `LUX_DEFAULT_MEMORY`, `LUX_DEFAULT_DISK`, `LUX_DEFAULT_PIDS` | `2`, `8Gi`, `20Gi`, `4096` | Resources a Run gets when its spec sets none. |
| `LUX_DEFAULT_DISK_GRACE` | `0s` | `resources.diskGrace` of a Run whose spec sets none (`defaults.disk_grace`): how long it may stay over its disk before it is stopped, `0s` to `1h`. `0s` stops it on the first sample over. Twice its disk stops it at once whatever the grace, on a host without disk scaling; on one with it, the host's [disk pressure](#disk-pressure) guard stops it instead ([disk is measured](runspec.md#rules)). Written into the Run's spec at submit, so changing it leaves existing Runs as they are. |
| `LUX_PROVIDER_CHECK_EVERY` | `1m` | How often each EC2 pool's instances are listed (orphans terminated, vanished hosts written off). Mind the provider's API limits. |
| `LUX_LOST_GRACE` | `5m` | How long a lost provisioned host's instance is kept (a runner restart or a network blip is not a loss) before it is terminated. |
| `LUX_LISTING_LAG` | `1m` | How long after a launch EC2's listings may still miss an instance: until then a host missing from them is not written off. |
| `LUX_SCALE_DOWN_AFTER` | `10m` | How long a provisioned host stays idle before it is cordoned, then terminated once idle. |
| `LUX_LAUNCH_TIMEOUT` | `10m` | How long a launched host may take to register before it is terminated. |
| `LUX_OUTDATED_DRAIN_PERCENT` | `10` | Caps concurrent outdated-binaries drains per pool, as a percentage of its live hosts (at least 1 regardless). |
| `LUX_EC2_ENDPOINT` | AWS | Overrides the EC2 endpoint (tests). |
| `LUX_EC2_NO_CAPACITY_RETRY_AFTER` | `30s` | How long launches skip a candidate (region, instance type, launch template, subnet, market) after EC2 had no capacity for it (see **No capacity** below); `0` never skips. |
| `LUX_SAMPLE_EVERY` | `10s` | How often the system is sampled for history ([Operators](operators.md#history)). |
| `LUX_HISTORY_RAW` | `48h` | How long raw samples (hosts and placements: one per heartbeat) are kept. |
| `LUX_HISTORY_MINUTES` | `720h` | How long minute rollups are kept. |
| `LUX_HISTORY_HOURS` | `9600h` | How long hour rollups are kept. |
| `LUX_HISTORY_DISK_PATHS` | `/` | Directories on luxd's own machine whose filesystems (used, free, total) the operators' Overview charts, comma-separated (`history.disk_paths`, a list, in the file). A path that cannot be read is skipped and logged. An empty list (`[]`, or only commas such as `,`) tracks no filesystem. |
| `LUX_COSTS` | `true` | Run costs ([Costs](costs.md)): `false` stops the cost tick and the queue's drainer on this luxd. State changes still queue their Runs, for whichever luxd drains. |
| `LUX_COSTS_EVERY` | `2m` | The cost tick: every live Run's costs are evaluated once per interval, by one luxd. |
| `LUX_COSTS_DRAIN_EVERY` | `2s` | How often each luxd polls the cost queue, besides being woken by Run events. |
| `LUX_COSTS_BATCH` | `1000` | How many Runs one drain claims. |
| `LUX_COSTS_SETTLE` | `["10m","1h"]` | Increasing durations after a Run finishes, JSON array in the environment. Plugin-specific `settle` overrides this. |
| `LUX_COSTS_SETTLE_GIVE_UP` | `168h` | Stop retrying a failed source after seven days. |
| `LUX_COSTS_BACKOFF`, `LUX_COSTS_BACKOFF_MAX` | `10s`, `10m` | Retry delays for plugin failures. |
| `LUX_COSTS_DESCRIBE_EVERY` | `1h` | Refresh cost plugin descriptions. |
| `LUX_COSTS_PLUGINS` | `[]` | JSON array replacing the entire `[[costs.plugin]]` list from the file. |
| `LUX_COSTS_COMPUTE_EC2` | `true` | Price EC2 hosts using AWS on-demand and spot list prices. |
| `LUX_COSTS_PRICES_REFRESH` | `24h` | Refresh interval for the on-demand price cache. |
| `LUX_COSTS_PRICING_REGION` | `us-east-1` | AWS region for the Pricing API, independent of the host's region. |
| `LUX_PRICING_ENDPOINT` | AWS | Overrides the Pricing API endpoint (tests). |
| `LUX_MARKET_EVERY` | `1h` | How often each candidate instance type's spot and on-demand prices and spot placement score are sampled for every zone of each `ec2` pool ([Market data](telemetry.md#market-data)); `0s` turns it off. One luxd samples: the provisioner lease's holder. |
| `LUX_MARKET_CANDIDATES` | — | Instance types sampled for every `ec2` pool besides its template's `instanceType` and `fallbackInstanceTypes`; comma-separated (`market.candidates`, a list, in the file). Each (region, type) pair is one of EC2's daily spot placement score configurations, of which an account gets about 10 ([the quota](telemetry.md#market-data)). A name that is not `family.size` is refused at startup. |
| `LUX_DEBUG` | — | Debug logging. |
| `LUX_CONSOLE_AUTH` | `key` | How the console signs people in: `key` or `cloudflare-access` ([Operators](operators.md#signing-in)). |
| `LUX_CF_ACCESS_TEAM`, `LUX_CF_ACCESS_AUD` | — | For `cloudflare-access`: the Access team (`acme` or `acme.cloudflareaccess.com`) and the application's AUD tag. |
| `LUX_CF_ACCESS_OPERATORS` | — | Comma-separated explicit operator emails (TOML: `console.cloudflare_access.operators` list). Required in Access mode; matched case-insensitively against verified JWT email only. |
| `LUX_CF_ACCESS_DEFAULT_TENANT` | — | Existing tenant ID (preferred) or exact tenant name (e.g. `absmartly`) for non-operator Access users. Required in Access mode; missing tenants deny requests. |
| `LUX_IMAGES_REPOSITORY` | — | The managed image registry ([published images](runspec.md#published-images)): a repository with its registry and no tag, e.g. `ghcr.io/acme/lux-images`. A tenant publishes to `<repository>/<tenant id>`. Empty: off, and `image.build.publish` is refused. |
| `LUX_IMAGES_AUTH` | `static` | How luxd gets the managed registry's credential: `static` (the file or variable below) or `ecr` (an Amazon ECR token, with the AWS default credential chain: see below). |
| `LUX_IMAGES_CREDENTIAL_FILE`, `LUX_IMAGES_CREDENTIAL_ENV` | — | With `static`, where luxd reads the managed registry's credential (`user:token`, or a bare token), one of the two: an absolute file path, or the name of one of luxd's environment variables. Read for each assignment that needs it, so a rotated file takes effect without a restart. Refused with `ecr`. |
| `LUX_IMAGES_ECR_MIN_VALIDITY` | `4h` | With `ecr`, the least lifetime an ECR token has left when an assignment gets it: longer than any Run's image takes to ready (build and publication). 30m to 11h. |
| `LUX_IMAGES_SHARED` | — | Comma-separated repositories (`images.shared`, a list, in the file) every tenant may pull with the managed credential, never push to: a common base image. Not inside `LUX_IMAGES_REPOSITORY`. Each grants every tenant every image (any tag or digest) in that repository: list only repositories meant for all of them, never one that also holds private content. |
| `LUX_CONFIG` | `/etc/lux/luxd.toml` | The configuration file. |

luxd also serves the operator console at `/` ([Operators](operators.md#the-console)).

Cost plugins are configured under `[[costs.plugin]]` (see
[the example](luxd.example.toml)). Each needs a unique `name` other than
`compute` and an HTTP(S) base `url`. A bearer token may be read from
`token_file` or from the variable named by `token_env`, never inline in the
configuration. Optional `timeout` defaults to 30s, `max_batch` to 200;
`settle` overrides the global schedule. Plain HTTP is accepted for loopback
and private IPs; public HTTP requires `insecure = true`. Prefer HTTPS for
any plugin outside the private network. `LUX_COSTS_PLUGINS` takes a JSON
array of objects with the same keys (including duration strings), for example
`[{"name":"ledger","url":"https://ledger.example","token_env":"LEDGER_TOKEN"}]`.

The managed image registry (`[images]`) holds one credential that can push
to and pull from every tenant's repository under `repository` (and pull the
`shared` ones): for GHCR, a token with `write:packages` for the
organization owning it. It stays luxd's and the runners'. luxd reads it for
each assignment whose Run publishes, or names an image in the managed
registry (as `image.ref` or a `FROM`), and attaches it to that assignment
as it is sent, like a Run's secret values: never stored in the database, a
spec, an event or a log. The runner writes it to an auth file of its own
(0600, removed once the image is ready) keyed by the Run's tenant's
repository and the shared ones only, so Podman offers it for no other
repository; it never reaches the shim, a build or the container. A tenant
can neither name another tenant's repository (refused at submit, and by the
runner) nor choose where its image is pushed. lux never deletes published
images: keep them with the registry's own retention.

**Amazon ECR** (`auth = "ecr"`): `repository` is on a private registry,
`<account>.dkr.ecr.<region>.amazonaws.com/<root path>`, and luxd gets its
token itself, calling `ecr:GetAuthorizationToken` for that account in that
region with the AWS SDK's default credential chain (on the control host,
its instance role). Nothing calls AWS at startup or in `luxd validate`;
the first assignment that needs a token does. `shared` repositories must
be on the same registry, and no `credential_file` or `credential_env` is
given. An ECR token works until the `expiresAt` ECR returns with it, 12
hours after issue, and carries its caller's IAM permissions; the AWS
credentials that asked for it are the SDK's to refresh. luxd keeps one
token and gives an assignment a token only with at least
`ecr_min_validity` left (4h by default: a 3h build Run's image and its
publication, with room), fetching a new one when the one it holds has
less. A new token with less than that (a misconfigured role) is not given
out: the placement then fails saying luxd sent no credential, rather than
losing it mid-build. The assignment also carries the expiry, and a runner
whose image took longer refuses to start a pull or push with it, saying
so. One fetch runs at a time, bounded to 10s; after a failure luxd makes no
new call for 30s. Only the AWS error code is logged.

What the operator sets up, outside lux (lux neither creates nor checks it):

- luxd's IAM principal: `ecr:GetAuthorizationToken` (resource `*`); on
  `repository/<root path>/*`, `ecr:BatchCheckLayerAvailability`,
  `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`,
  `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`,
  `ecr:CompleteLayerUpload` and `ecr:PutImage`; the three read actions on
  each `shared` repository; and, to create repositories on push,
  `ecr:CreateRepository` (and `ecr:TagResource` if the template tags).
  The token carries all of these, for every repository they cover: lux
  limits its use (the runner offers it only for the Run's tenant's
  repository and the shared ones), not its scope.
- Repositories: a tenant's is `<root path>/<tenant id>`. Create them, or
  give the root path an ECR repository creation template with
  `CREATE_ON_PUSH`.
- Retention: a published image must stay while anything uses it: Runs
  resume on it and later Runs name it, and they fail (never rebuild) when
  it is gone. Use no lifecycle expiry on the root path, or only one
  coordinated with what consumers keep. Tags: `MUTABLE` is recommended
  (the build key's tag can be re-pushed when two hosts first build the
  same key at once; images are named by digest); with `IMMUTABLE`, that
  rare race's second publication fails.
- Hosts reach the ECR endpoint and its layer storage (S3) directly: the
  runner pulls and pushes outside Runs' egress rules.

### Tenants, keys and quotas

```bash
luxd admin create-tenant --name acme [--max-runs N] [--max-hosts N] [--retention-days 30] [--expire-after-days 90]
luxd admin create-key --tenant T --scopes read,run
luxd admin create-operator-key [--name N]       # every tenant: see docs/operators.md
luxd admin set-quota --tenant T [--max-runs N] [--max-hosts N] [--max-storage BYTES] [--retention-days N] [--expire-after-days N]
luxd admin costs backfill-volumes --pool P [--tenant T] --volume type=gp3,size=100,iops=3000,throughput=125 [--dry-run]   # see EC2 pools
```

- `--max-runs`: Runs that are not stopped or finished. Checked when a Run
  is submitted or resumed.
- `--max-hosts`: the tenant's registered hosts. Checked when a host
  registers.
- `--max-storage`: bytes in snapshots, output and artifacts not yet
  deleted. Checked when a Run is submitted or resumed.
- `--retention-days`: how long a terminated Run keeps its snapshots and
  output after it was terminated (default 30). The Run, its events and its
  artifacts stay. Every other Run can be resumed, so retention deletes
  nothing of it.
- `--expire-after-days`: how long a Run may rest `stopped`, `lost`,
  `failed` or `succeeded` before lux terminates it (default 90; 0: never).
  Its retention counts from then.

A request over quota gets HTTP 429, and the CLI exits with code 5.

## Where bytes live

Every placement ends with a **snapshot** of its state volumes, its
**output**, and its **artifacts**, all written to the host's disk. They
reach S3 in the background:

1. The runner uploads each blob straight to S3. It asks
   `POST /runner/v1/blobs/{id}/upload` with the blob's size, its sha256 and
   the sha256 of each 64 MiB part; luxd checks them against the blob's
   record and answers presigned URLs (valid for an hour): one `PutObject`,
   or a multipart upload with one `UploadPart` per part. Each URL has the
   body's SHA-256 signed in, so S3 refuses any other bytes. The runner then
   calls `POST /runner/v1/blobs/{id}/complete`; luxd completes the
   multipart upload, checks the object's length, and records the blob in
   S3. Runners never hold S3 credentials, but need network egress to S3
   (which downloads already use). luxd still accepts the older
   `PUT /runner/v1/blobs/{id}`, streaming the body into S3 and verifying
   its sha256 on the way, for runners that predate direct uploads. A part
   that fails on the network or with an S3 5xx is tried up to three
   times; a blob whose part still fails is retried from the start, and the
   runner first has luxd abort the multipart upload it gave up on. luxd also
   aborts one S3 refuses to complete; the bucket's lifecycle rule, after 7
   days, is the backstop for the rest. A multipart upload is checked part by
   part as it arrives; the whole blob's sha256 is checked again when a
   snapshot is restored. A presigned URL really lasts as long as the
   shorter of its hour and the credentials luxd signed it with (an instance
   role's session). A snapshot report is validated against its Run's blob
   records; a report that does not match is refused whole (a
   `snapshot.failed` event on the Run, which keeps its previous snapshot
   and is not resumed automatically, even when the same placement
   reported an accepted snapshot before). A redelivered report must match
   the recorded manifest, output and artifacts exactly. The Run's state
   reason says so; a resume starts it from that previous snapshot. A Run
   with no previous snapshot is not resumable (409 `no_snapshot`, and
   left out of `lux ls --resumable`) unless `--from-snapshot` names one,
   or unless its resumePolicy is restart and it has no session, which
   restores nothing.
   The runner deletes the refused snapshot's files instead of uploading
   them.
2. The host keeps its local copy (the Run's volumes; not its container,
   which the runner removes once the stop is reported), so a resume there
   moves nothing: it starts a new container on those volumes, as another
   host would on restored ones. It deletes the copy when:
   - luxd tells it the Run now runs elsewhere (from S3), or
   - the copy is older than the runner's `--host-ttl` (default 24h).

   A copy is never deleted before its upload finished.
3. A runner downloads a snapshot through a presigned S3 URL, valid for 15
   minutes: it asks `GET /runner/v1/blobs/{id}`, and luxd redirects it only
   for the volumes of the snapshot a Run placed on that host is restoring
   (recorded by the placement that took that snapshot).
   The assignment carries each volume's size and sha256 as luxd recorded
   them, and the runner checks what it restores against them. Artifacts are
   downloaded through luxd, which decompresses them (blobs are stored zstd)
   and sends the file with its length and sha256 (`X-Lux-SHA256`), so a
   download cut short is detected.
4. The reaper deletes what no Run can use any more
   ([Which snapshots are kept](concepts.md#which-snapshots-are-kept)):
   - the older snapshots of a resumable Run, once its current one is
     uploaded;
   - a terminated Run's snapshots and output, the tenant's
     `retention_days` after it was terminated;
   - a `stopped`, `lost`, `failed` or `succeeded` Run, which is resumable,
     is exempt from age-based retention until it expires
     (`expire_after_days`, default 90: terminated, then retention); its
     superseded snapshots are still removed, as above.

   Artifacts are deleted only by `DELETE /v1/runs/{id}/artifacts` (of a
   terminated Run). Each
   deletion marks the blobs deleted (and the snapshots unavailable) in the
   database first, under the Run's lock, then deletes the S3 objects; a
   delete S3 refuses is logged (`S3 delete failed; object orphaned`, with
   its key) and left in the bucket, never retried.

   **Upgrading** to migrations 053–054: the first reaper
   passes, 20 Runs per pass per kind, terminate every Run resting longer than
   90 days (`state_changed_at` is backfilled from each Run's last `state`
   event); delete the volumes of every snapshot but the current one of
   Runs that are not terminated whose current
   snapshot is uploaded; and stop deleting failed Runs' blobs and
   artifacts. Expired Runs lose their
   snapshots and output 30 days later (their retention starts at the
   expiry).

   **Upgrading** to migrations 056–057 (succeeded becomes resumable,
   cancelled becomes terminated): existing `succeeded` Runs stop being
   reaped by retention. One whose snapshot retention already deleted is
   still listed as resumable by its state (`resumable` and `?resumable=true`
   look at state, policy and refused reports, not at the snapshot's
   availability); `GET /v1/runs/{id}` shows the blocker "its snapshot is
   no longer available", and a resume of it ends `lost` with that reason
   when the scheduler finds nothing to restore. Each `succeeded` Run rests from its
   `state_changed_at`, so the first expiry passes terminate every one
   that succeeded more than the tenant's `expire_after_days` ago, 20 per
   pass; their retention then starts. A succeeded Run's older snapshots
   are now superseded and deleted once its current one is uploaded.
   Existing `cancelled` Runs become `terminated`, `terminated_at`
   taken from their `state_changed_at` (when they were cancelled).
   Runs with `resumePolicy: never` that already rest (`stopped`, `lost`,
   `failed`, `succeeded`) stay as they are until terminated or expired;
   resume refuses them, as before.
   Succeeded Runs now keep their snapshots and output until
   they expire (`expire_after_days`, default 90) plus retention, and those
   bytes count toward `max_storage_bytes`: clients should terminate Runs
   they will not resume.

   **Upgrading** to migration 058 (artifact versions): it numbers every
   existing artifact per Run and path, in one pass over `artifacts`. A
   luxd older than it cannot record artifacts afterwards (its insert has
   no version): with several luxds sharing the database, a snapshot
   report that lands on an older one is refused with an error and the
   runner retries it, so move them all to the new release promptly.
   Artifacts reads and writes block for the length of the migration on a
   large table; run `VACUUM artifacts` afterwards.

Keys are `tenants/<tenant>/runs/<run>/<blob>`. Encrypt the bucket at rest
(SSE-KMS on AWS).

## Hosts

Host requirements:

- Linux with cgroup v2. The runner makes cgroups of its own under
  `/sys/fs/cgroup/lux.slice` for image builds (their process limit, and
  ending everything a cancelled build started), and enables the `pids`
  controller for them.
- Podman ≥ 5 with netavark, run rootful.
- A `containers` range in `/etc/subuid` and `/etc/subgid` (for
  `--userns=auto`), for example `containers:2147483647:2147483648`.
- Podman storage on the native `overlay` driver, not fuse-overlayfs.
  Image builds run in their own user namespace, and under fuse-overlayfs
  their `RUN` steps can't write to the image's root directory.
- Tested with Podman 5.8. The runner turns idmapped overlay mounts off for
  builds (`_CONTAINERS_OVERLAY_DISABLE_IDMAP`, containers/storage's only
  switch for it). Without that, a build's files are owned by host ids, and
  its image id depends on the uid range it happened to get. The e2e test
  `test_rebuilds_match_whatever_uid_range_they_get` catches a Podman
  upgrade that drops the switch: run it before upgrading hosts.
- nftables (the runner owns the `inet lux` table; see
  [egress](runspec.md#network-egress)).
- For nested containers, `lux-runner --nested` (or `LUX_NESTED=true`): the
  host needs `/dev/fuse` and `/dev/net/tun`. The runner then labels the host
  `nested=true`; that label cannot be set with `--label` or a host token.
  `LUX_NESTED` accepts only `true` or `false`; anything else stops the
  runner.

```bash
LUX_URL=https://luxd.example LUX_HOST_TOKEN=luxh_… lux-runner --name host-a
```

A static host set up from a stock distro image can instead run luxd's
bootstrap script, which checks the host requirements above, installs
whatever is missing, and installs `lux-runner` as a systemd service
(`GET /runner/v1/bootstrap.sh`, no auth: it carries no secret). It is a bash
script (`missing=()` arrays, `set -o pipefail`), so it must run under
`bash`, not piped into a bare `sh`, which is `dash` on Debian and Ubuntu
and silently fails on it; `sudo env` is required to pass the environment
through:

```bash
curl -fsS https://luxd.example/runner/v1/bootstrap.sh | sudo env \
  LUX_URL=https://luxd.example LUX_HOST_TOKEN=luxh_… LUX_HOST_NAME=host-a bash
```

| Flag | Default | |
| --- | --- | --- |
| `--name` | hostname | Unique within the tenant. |
| `--data-dir` | `/var/lib/lux` | Run state and local snapshot copies. |
| `--shim` | `/usr/local/bin/lux-shim` | The shim binary to mount into containers. |
| `--label k=v` | | Host labels, matched by `placement.requires` and `prefers`. Also `LUX_LABELS=k=v,…`. |
| `--max-runs`, `--cpus` | 16, all | Capacity offered to the scheduler. `--cpus` also `LUX_RUNNER_CPUS`. |
| `--cpuset` | off | Pin each Run's container to `ceil(resources.cpus)` CPUs (`--cpuset-cpus`): disjoint sets for whole-CPU Runs while CPUs are free, the least-used CPUs after; fractional Runs share a CPU while their `cpus` sum to at most one (docs/runspec.md). Sets are chosen at placement and never moved. Never refuses a placement. The CPUs are the runner's own affinity mask, so keep CPUs for the host by confining the runner (`taskset`, systemd `CPUAffinity=`), not with `--cpus`, which stays the scheduler's capacity. The mask is read at runner start; already-running containers keep their sets. The set is in each Run's run state: a restarted runner keeps it. |
| `--memory` | MemTotal | The machine's gross memory, offered to the scheduler: Runs ask in these terms and get their share after headroom ([how](concepts.md#memory-the-hosts-terms-scaled-to-what-linux-sees)). An EC2 host luxd launches gets its instance type's memory. Also `LUX_RUNNER_MEMORY`. |
| `--memory-headroom` | 512 MiB | Bytes of MemTotal kept from Runs for the kernel and the host. Each Run's container gets `resources.memory × min(1, (MemTotal − headroom) / --memory)`. The runner refuses to start if it leaves nothing. Also `LUX_RUNNER_MEMORY_HEADROOM`. |
| `--disk` | not reserved | Bytes of disk the scheduler reserves Runs' `resources.disk` from. Without it, disk is not reserved (Runs still stop at their own limit). Set it to the space under `/var/lib/containers`, more to overcommit. With `--disk-headroom`, the host's nominal disk instead (below). Also `LUX_RUNNER_DISK`. |
| `--disk-headroom` | not given | Bytes of the `--data-dir` filesystem kept from Runs. Given with `--disk` (0 is allowed), `--disk` is nominal and each Run's disk limit is `resources.disk × min(1, (capacity − headroom) / --disk)`, in whole bytes rounded down; the grace counts from that limit, and there is no 2× ceiling: the [low-free-space guard](#disk-pressure) bounds the host instead. Capacity is the filesystem's used plus unprivileged-free blocks (statfs; blocks reserved for root count in neither), read once at start, never the free space. A grown volume takes effect only after a runner restart; adopted Runs also use the factor computed at that restart. The runner refuses to start if the headroom leaves nothing, and logs the factor. Only such a runner advertises `disk-scale`, which shaped pools require. Not given: limits are `resources.disk` as before, with the 2× ceiling. Also `LUX_RUNNER_DISK_HEADROOM`. |
| `--disk-low-free` | `5%` | With `--disk-headroom`: free space of the `--data-dir` filesystem under which the host is under [disk pressure](#disk-pressure). A percentage of the filesystem's capacity (`5%`) or a size (`8GiB`, `8Gi`, bytes). Also `LUX_RUNNER_DISK_LOW_FREE`. |
| `--disk-recover-free` | `10%` | With `--disk-headroom`: free space at which disk pressure ends. Must be above `--disk-low-free` and below the capacity. Also `LUX_RUNNER_DISK_RECOVER_FREE`. |
| `--usage-every` | `15s` | How often each Run's disk use is sampled, and its disk limit and `diskGrace` checked. The grace is counted in these samples, so a Run over its disk is stopped between `diskGrace` and `diskGrace` plus this interval from the first sample over. A sample of a large tree may take longer than this; such a round's next sample starts when it ends. |
| `--host-ttl` | `24h` | How long uploaded local copies are kept, and images lux pulled or built after their last use ([images on hosts](runspec.md#images-on-hosts)). |
| `--image-disk-high` | `80` | Percent use of the disk holding Podman's storage (its graph root, `/var/lib/containers`) over which lux's unused images are removed, least recently used first, until under it. `0` turns it off. Images the host had before lux are never removed. |
| `--provider-id` | | The cloud instance id, for provisioned hosts. Also `LUX_PROVIDER_ID`. |
| `--ec2-imds` | off | EC2 instance metadata URL (`http://169.254.169.254`) to watch for spot interruptions. Also `LUX_EC2_IMDS`. |
| `--poll` | off | Use HTTP polling instead of a WebSocket. There is no live output in this mode: output arrives after exit. |

Private registries ([`image.registryAuth`](runspec.md#private-registries))
should be HTTPS. For a plain-HTTP registry (a test or air-gapped one),
list it as insecure on every host:

```bash
cat > /etc/containers/registries.conf.d/50-internal.conf <<'EOF'
[[registry]]
location = "10.0.0.5:5000"
insecure = true
EOF
```

Host tokens come from `luxd admin create-host-token --tenant T [--pool P]
[--label k=v]`. Restarting `lux-runner` does not touch running containers
(Podman is daemonless). The new runner re-adopts them.

### Disk pressure

Disk limits are measured, not filesystem quotas, and a Run may stay over
its limit for its `resources.diskGrace`; image caches, stopped Runs' local
copies and snapshots waiting for upload also live on the host's disk
outside any Run's reservation. How the runner keeps that from filling the
filesystem depends on whether it scales disk:

- **A runner without `--disk-headroom`** (every unshaped pool's host,
  unless started otherwise) keeps the fixed ceiling: a Run at twice its
  disk limit is stopped at once, whatever its grace. It has no
  low-free-space guard, and `--disk-low-free` and `--disk-recover-free`
  are ignored (the runner says so at start).
- **A runner with `--disk` and `--disk-headroom`** (`disk-scale`: every
  shaped pool's host) has no ceiling. Its low-free-space guard watches the
  filesystem holding `--data-dir` instead.

The guard measures the free space of that filesystem with statfs (what an
unprivileged writer can still use: blocks reserved for root are not free)
every 2 seconds (`--usage-every` when shorter), apart from the disk
samples, and once at start, before the first heartbeat: a slow disk walk
never delays blocking admissions, and a runner restarted under pressure
never reports none. Reclaiming and stopping happen after each round of
disk samples (`--usage-every`, 15s by default). Each placement's sample is
cut off only after 4 times that interval or 2 minutes, whichever is
longer, so a slow walk of a large tree still concludes and only a hung
Podman is given up on. Keep
`--data-dir` and Podman's storage (`/var/lib/containers`) on the same
filesystem, as a stock host has them: the guard sees only the one holding
`--data-dir`.

Pressure starts when free space falls below `--disk-low-free` (default
5% of the capacity), and ends only once it is at or above
`--disk-recover-free` (default 10%). Between the two marks nothing
changes. Both defaults are conservative: no shaped host has measured them
yet. When pressure starts, and after every round of samples while it
lasts, the runner:

1. **Blocks admissions.** Its next heartbeat (sent at once) carries
   `diskPressure`. luxd places no new Run on the host, normal, spot or
   resumed: a waiting Run's reason says `1 host in its pool is under disk
   pressure (low free disk)` (a spot Run's: `1 host is under disk
   pressure`), and the capacity planner counts no room on the host, so a
   pool that may launch hosts launches one as it would for a full host.
   Runs already on the host keep running. A heartbeat without usage (the
   runner could not read its host's) leaves the state luxd has as it is.
2. **Reclaims caches.** It removes lux's unused images, least recently
   used first, toward the recovery mark, with the protections of
   `--image-disk-high`: never an image a container uses, one a local copy
   waiting for a resume needs, or one used in the last 10 minutes. Next
   it removes unmounted `kind: cache` volumes, least recently mounted or
   unmounted first, measuring free space after each until the recovery
   mark. A starting placement's cache and any volume still attached to a
   container are kept. Caches go before saved Run copies: they are
   disposable and cannot help a Run resume. Cache volumes have no per-cache
   size cap and count only against host free space, not a Run's disk limit.
   Then it discards the local copies of Runs no longer live on the host that
   S3 holds, oldest exit first, until the recovery mark: a copy goes only
   once every snapshot of its Run is reported and uploaded and no upload
   is pending (what `--host-ttl` waits for, without the wait; the rule
   luxd uses to discard a copy when a Run resumes elsewhere). Its state
   volumes and snapshot files go, and the next heartbeat tells luxd. A
   copy still uploading, snapshots not yet uploaded and git mirrors are
   never touched.
3. **Stops Runs over their disk limit.** Only placements whose latest
   sample is over their effective disk limit (within their grace)
   qualify, the largest excess first; a placement whose latest sample
   was inconclusive (a probe failed) does not. Before each, the runner
   counts what is already on its way back against what is missing to the
   recovery mark, and measures the filesystem again after each, so it
   stops no more than it needs. A stopped Run gets a `disk.pressure_stop`
   event and ends `failed`, its reason e.g. `stopped: host disk pressure,
   12.3 GiB over its 37.5 GiB disk limit`. **A Run at or under its limit
   is never stopped**, and neither is one with `placement.preemptible:
   false` ([Non-preemptible Runs](runspec.md#non-preemptible-runs)),
   however far over: with only such Runs over, the pass finds no relief and
   admissions stay blocked. Such a Run is still held to its own limit after
   its grace (`disk.exceeded`).

What a stop gives back, and when: the Run's container goes as soon as its
end is reported, which frees its writable layer. Its state volumes stay,
as for any stop, as the Run's local copy, and the stop first exports them
(zstd) for upload, which briefly uses more of the filesystem. So the
runner counts a stopping Run's writable layer as on its way back, then,
once it has ended, its snapshot's blob sizes (a lower bound of its
volumes, read from disk, so it holds across a runner restart) until their
upload finishes. The same goes for any ended Run's copy that is still
uploading. At the next round after that, step 2 discards the copy.
Recovery after a stop
therefore waits for that Run's upload, then the discard. The export is
kept under pressure, since skipping it would lose the Run's state. If it
fails for want of space (ENOSPC), the snapshot is reported failed
(`snapshot.failed`, its error) as for any failed export, the volumes stay
as the Run's only copy and are never discarded by the guard, and the Run
still ends `failed` with the disk-pressure reason.

If a pass finds nothing to reclaim, nothing on its way back and no Run
over its limit, the host stays under pressure, admissions blocked, and
says so (`noRelief`). It clears when space comes back: Runs ending, an
operator freeing space by hand, or a copy's upload finishing. Until an
image, a placement, a snapshot record or a sample crossing its limit
changes, or a minute passes, the runner does not look again: no podman
calls for images and no scan of local copies on every round.

An operator sees it:

- `lux hosts ls` and `lux hosts get <host>` show the state as `ready (disk
  pressure)`; `GET /v1/hosts/{id}` has `diskPressure` (`freeBytes`,
  `lowFreeBytes`, `recoverFreeBytes`, `lowFree`, `recoverFree`,
  `noRelief`) while it lasts (stored as `hosts.disk_pressure`, migration
  077).
- `lux hosts events <host>` records `host.disk_pressure` when it starts
  (`disk pressure: 4.1 GiB free, below 5%`), again when `noRelief`
  changes, and `host.disk_pressure_ended` when it ends.
- The capacity planner's `host.capacity_decision` names `disk pressure`.
- The runner logs `disk pressure: admissions blocked`, each image it
  removes, each local copy it discards (`disk pressure: discarded a local
  copy S3 holds`) and each Run it stops, and `disk pressure ended`.
  History keeps the filesystem's free bytes on every host sample.

Tuning: the marks are a percentage of the filesystem's capacity or a
size; use a size on a small filesystem where 5% is too little for an image
pull. The low mark should leave room for what can be written before the
watch sees it (every Run writing for 2 seconds) plus a stopped Run's
export, and the gap to the
recovery mark keeps a host from flapping in and out of pressure. The
runner refuses to start unless 0 < low < recover < the filesystem's
capacity, as resolved at start, and refuses a same-unit pair with low not
below recover before reading the filesystem. Set `--image-disk-high`
below `100% − --disk-recover-free` so ordinary image GC keeps the
filesystem out of pressure in the first place.

### Binary distribution and self-update

luxd serves the runner binaries a host needs, so no custom AMI or
config-management run has to carry them:

- `GET /runner/v1/bin/manifest` (host-token auth): `{"linux-arm64":
  {"lux-runner": "<sha256>", "lux-shim": "<sha256>"}, "linux-amd64": {...}}`
  for each arch `LUX_RUNNER_BIN_DIR` holds **both** binaries for; an arch
  missing either file is never listed, and never grounds for a drain (luxd
  never asks a host to update to something it cannot itself serve).
- `GET /runner/v1/bin/linux-{arch}/{lux-runner|lux-shim}` streams the binary,
  with `Content-Length` and its sha256 in `X-Lux-Sha256`.

luxd hashes what it finds under `LUX_RUNNER_BIN_DIR` once at startup. A
host downloads its arch's binaries before starting `lux-runner` (its
systemd unit's `ExecStartPre`), so an upgrade is: replace luxd's
`runner_bin_dir`, restart luxd, then restart or replace each host — never
patch a running binary in place. **Every luxd instance behind the same
`runner_url` must serve identical runner binaries before a rolling
deploy**: otherwise which luxd a host's next heartbeat happens to reach
decides whether it is "outdated", and `/runner/v1/bin` can serve a different
build to a host mid-update.

Every `Hello` and `Heartbeat` a runner sends carries the sha256 of its own
binary (`os.Executable()`) and of its `--shim`; older runners simply omit
them. When luxd holds both binaries for that host's arch and either sha
differs, and the host is not already draining, it cordons the host once,
recording an `outdated` cause (`hosts.drain_causes`; no Run is stopped,
and at most `LUX_OUTDATED_DRAIN_PERCENT`% of a pool's live hosts, at
least one, cordon at a time — enforced under a per-pool lock, so a burst
of Hellos after a luxd restart cannot overshoot it). `state_reason` is display text
only, shown as `outdated binaries` while that is the most recent cause set,
but never read back by luxd itself. A cordoned **provisioned** host stops
taking new Runs; once its existing ones finish and it goes idle it is
terminated same as any other drain, and the pool launches a fresh one with
the running luxd's binaries. A cordoned **static** host keeps its Runs;
once none are left and it has nothing left to upload, luxd asks it to
exit: its systemd unit's `Restart=always` brings it back, and its
`ExecStartPre` re-downloads the binaries first. If that exit goes
unacknowledged for 10 minutes and the host is still outdated and idle,
the reaper sends it again (logged each time). A `Hello` reporting matching
binaries removes only the `outdated` cause, in the same transaction that
acks any exit still queued for it; the host stays draining if another
cause remains (an operator's `lux hosts drain`, or `pools rm`) — an
operator's drain always outranks a release, so a host also carrying a
manual cause is left alone by the reaper too: it updates only once the
operator undrains it, or restarts it by hand. A runner ignores a
redelivered `exit` if its binaries already match what luxd's manifest
last showed, or if it holds live placements (logged either way).

A host installs the downloaded `lux-runner`, `lux-shim` and the fetch
script itself under `/usr/local/bin`, not under `/usr/local/lib` — Fedora
CoreOS enforces SELinux, and its default policy labels `/usr/local/bin`
`bin_t` (systemd's `ExecStart` can run it) but leaves `/usr/local/lib`
unlabeled, which fails with `203/EXEC`. This is unrelated to
`runner_bin_dir` on luxd's own host, which is unchanged.

## Pools and the default pool

A Run goes to the pool its spec names (`placement.pool`). One that names
none goes to its tenant's **default pool**, which a tenant (or an operator)
chooses by marking one of its pools:

```bash
lux pools set arm64 --default          # the tenant's default from now on
lux pools set arm64 --default=false    # no default: the platform's, else "default"
luxd admin create-pool --name shared-x86 --provider static --shared --default   # the platform's default
```

luxd picks the pool when the Run is submitted, in this order:

1. the tenant's default pool;
2. else the platform's default pool (a platform pool, `tenant_id` NULL,
   marked by an operator: `lux pools set <name> --default` without
   `--tenant`, or `luxd admin create-pool --default`);
3. else the pool named `default`, which is what every such Run got before
   default pools existed. A deployment with a pool named `default` keeps
   working without marking anything.

The pool is written into the Run's spec, so its resumes, retries and
migrations stay there even when the default changes later, unless a resume
names another pool (`placement.poolId`, `lux resume --pool`), which rebinds
the Run there from then on. The Run's
`submitted` event says which pool and why: `{"pool": "arm64", "poolFrom":
"tenant-default", "poolOwner": "tenant"}` (`platform-default`, `fallback`,
or `spec` when the spec named it). A Run that names a pool, `default`
included, is never redirected.

A tenant pool and a platform pool may share a name, so luxd records whose
pool it is too (`runs.pool_owner`; `poolOwner` in the event): the default's
owner, or for a name, the tenant's own pool of that name, else the
platform's. The Run then goes only to that owner's hosts, and only that
pool provisions for it. A name no pool has (static hosts that joined by
host token with a pool name no pool was created for) records no owner, and
such Runs, like those submitted before this was recorded, match hosts by
name as before. If the Run's pool is removed, it waits ("its pool burst
was removed") rather than taking another owner's pool of that name, or
that pool's static hosts, which `lux pools rm` leaves in service for Runs
without an owner; re-creating the pool serves it again.

A tenant has at most one default pool, and the platform one (Postgres
refuses a second). Marking another pool moves the mark in one statement.
`lux pools rm` clears the mark of the pool it removes; re-creating that
pool does not bring it back. Upgrading marks no pool.

### Shaped pools

A pool's optional `shape` ([concepts](concepts.md#shaped-pools)) is set
wherever pools are: `POST /v1/pools`, `lux pools set --shape` and
`luxd admin create-pool --shape`, all with the same validation:

```bash
luxd admin create-pool --name m --provider ec2 --template "$TEMPLATE" \
  --shape family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2
```

The shape is stored with the pool (`pools.shape`, migration 071) and
listed as `shape` and `sizes` in `GET /v1/pools`. For an ec2 pool, luxd
compares the template's `instanceType` and `fallbackInstanceTypes` with
the shape where the instance-type catalog (`instance_types`, filled by
the market sampler) has them: the arch must match, CPUs and memory must be
at least the shape's. A type the catalog lacks is not checked, so a pool
set before the sampler has run is checked only on its next set. Disk is
the shape's own: it comes from the launch template's volume, which no
instance type describes. Rolling back to a luxd
without shapes leaves the columns in place and ignores them; Runs keep
the resources they were given.

A pool's `placement` (`spread` or `pack`;
[concepts](concepts.md#placement-spread-or-pack)) is set the same three
ways (`--placement`) and stored as `pools.placement` (migration 075),
NULL for the default: `pack` for a shaped pool, `spread` otherwise. A
luxd without it ignores the column and spreads every pool.
A resume's raise for saved state (migration 076) is shown only while the
Run keeps the size it was raised to: after a rollback resizes a raised
Run, its raise label stays hidden until the next shaped resize.

For a static shaped pool, start each host's runner by hand with the shape's
nominal capacity. For the shape above:

```bash
lux-runner --cpus 8 --memory 34359738368 --disk 171798691840 --disk-headroom 10737418240
```

Memory, disk and disk headroom are plain byte values, not values with unit
suffixes. The advertised CPUs, memory and disk must equal the shape exactly
([concepts](concepts.md#shaped-pools)). Use the shape's
`diskHeadroom` (10 GiB by default), although equality of headroom is not
enforced on hand-started hosts: any explicit headroom that allows the runner
to start enables `disk-scale`.

## EC2 pools

Pool names follow the rule in [the CLI reference](cli.md#hosts-and-pools): 1-32
lowercase letters, digits and `-`. `luxd admin create-pool` and
`create-host-token --pool` apply it too.

A pool with `provider: ec2` is sized by luxd:

```bash
lux pools set burst --provider ec2 --min 0 --max 10 --warm 1 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner","instanceType":"m7i.2xlarge","subnets":["subnet-a","subnet-b"]}'
lux pools rm burst      # cordons its hosts; each is terminated once idle
lux pools rm burst --force-evict   # also stops its hosts' live Runs, so they resume elsewhere at once
```

- **Scale up:** when Runs for the pool wait in `provisioning`, luxd launches
  the hosts they need beyond what ready and starting hosts can hold, sized
  from the capacity its registered hosts reported
  ([capacity planning](telemetry.md#capacity-planning)), plus `--warm` idle
  ones kept ready. It keeps at
  least `--min` hosts and never more than `--max`. Launches alternate
  across the template's subnets.
- **No capacity:** when `RunInstances` fails with
  `InsufficientInstanceCapacity`, `InsufficientCapacity` or `Unsupported`
  (the type is not offered in that zone), the launch tries the next
  subnet, wrapping round, then each of `fallbackInstanceTypes` (optional,
  at most 4, distinct, not the `instanceType`, which must be set) the same
  way: types `[m8g, m7g]` and subnets `[a, b, c]` starting at `b` try m8g
  in b, c, a, then m7g in b, c, a. Each candidate without capacity logs at
  info (`ec2: no capacity`); a success on a later candidate warns once
  (`ec2: launched a later candidate`). These errors are not retried in
  place; any other error (a quota such as `VcpuLimitExceeded`, a
  permission, a bad parameter) ends the launch at once. A candidate without
  capacity is skipped for `LUX_EC2_NO_CAPACITY_RETRY_AFTER` (30s; `0` never
  skips) by every launch with the same region, instance type, launch
  template, subnet and market (spot or on-demand), so a shortage costs one
  sweep of `RunInstances` calls per interval. A sweep's marks all date from
  its start, so keep the interval well above one sweep's duration: a
  15-candidate sweep under EC2 throttling can take about 10s. When every
  candidate fails or is skipped, the launch fails with the code and the
  candidates and the pool backs off (below). Fallback types use the same
  launch template, so its AMI must suit them (same architecture);
  `lux hosts ls` shows the type a host got.
- **Scale down:** a host idle longer than the pool's `--scale-down-after`
  (default `LUX_SCALE_DOWN_AFTER`, 10m), above the minimum and warm count,
  is cordoned. It is terminated once
  it has no live placements and nothing left to upload. `lux pools rm`
  cordons the pool's hosts the same way, without `--force-evict`: a Run on
  one finishes where it is and the host is terminated once idle; with
  `--force-evict` it is stopped (snapshotted) and resumed elsewhere at once,
  never cut short (`resumePolicy: restart` starts it from scratch instead;
  `manual` or `never` ends it `failed`).
- **Scale to zero when idle:** `--warm-while-active` keeps the `--warm`
  hosts only while the pool is in use: a Run live on it, or placed or
  ended within its `--scale-down-after`. After that the pool drops to
  `--min`. With `--min 0 --warm 1 --warm-while-active --scale-down-after
  600s`, someone working keeps a host ready between Runs, and ten quiet
  minutes after the last one the pool has no hosts at all:

  ```bash
  lux pools set burst --provider ec2 --template '…' \
    --min 0 --max 10 --warm 1 --warm-while-active --scale-down-after 600s
  ```

  Without it, `--warm` hosts are kept at all times.
- **Checked when set:** before an `ec2` pool is stored (`lux pools set`,
  `POST /v1/pools`, `luxd admin create-pool`), luxd asks EC2 whether its
  template would launch: `RunInstances` with `DryRun`, once per subnet
  with `instanceType`, then once per `fallbackInstanceTypes` entry in the
  first subnet (no subnets: once per type). What a launch sends and the
  check sends the same: the launch template, instance type, subnet, spot
  market options, and every tag key with the pool's values
  (`lux:managed`, `lux:deployment`, `lux:pool-id`, `lux:pool`, and the
  template's `tags`); a pool the set creates is checked with the id it
  is then stored under. What is representative: the host's tag values
  (`Name`, `lux:host` carry a fresh host id, not the one a launch will
  use), and the user data, rendered in the template's format without the
  host's URL, token and name and without the instance type's memory.
  EC2 answers `DryRunOperation` when the call would have been allowed;
  any other answer refuses the pool, 422
  `invalid_pool` `template: ec2 cannot launch it: <code>: <message>` (a
  missing launch template, subnet or permission), and nothing is stored.
  A dry run does not test capacity. The whole check has 10s. It needs no
  IAM beyond launching: a dry-run `ec2:RunInstances` is authorised like
  the real call (same resources, same conditions), so luxd's launch
  policy covers it, and a policy that would refuse a launch refuses the
  check (`UnauthorizedOperation`), except for a condition on the host's
  own tag values or the user data. Setting a pool again with the same
  provider and an identical template is not checked, so a change to
  `--max` needs no EC2. A set that races another create of the same pool
  (or its replacement under a new id) fails with 409 `pool_changed` and
  stores nothing; set it again.
- **Failures:** provider launch errors, including capacity errors, delay
  the pool's next launch by 15s, then 30s, 60s … up to 5m after each
  consecutive failed attempt returns. Quota refusals, caller cancellation
  and persistence errors do not count; scale-down, terminations and alive
  checks continue. Provider success (even if persisting it fails) or a
  change to the pool's provisioner settings (`lux pools set`) resets the
  backoff. It is in memory: restart or any newly taken provisioner lease
  epoch (no previous row, an expired lease or a different holder) resets
  every pool; renewing this luxd's unexpired lease does not.
  Waiting passes record `pool.scale_blocked` with cause `launch_backoff`,
  counted per pass and folded best-effort into one row; a long backoff
  with differing errors can start another row. `lux pools events` shows
  `launch backing off after N failures; next attempt in …: <error>`. A host
  that never registers within `LUX_LAUNCH_TIMEOUT` (default 10m) is
  terminated. An instance EC2 no longer has is written off and replaced.
- **Orphans:** once a minute luxd lists the pool's instances by tag. One
  no host row claims (a launch whose reply was lost) is terminated; a host
  EC2 no longer lists (and whose runner is silent) is terminated and
  written off. A host lost for over 5 minutes is
  terminated.
- One luxd instance does all this at a time (a lease in Postgres).

### Spot instances

A new `ec2` pool is preemptible, so it launches one-time spot instances,
unless it is set with `--preemptible=false`, which launches on-demand ones
([Non-preemptible pools](#non-preemptible-pools)):

```bash
lux pools set burst --provider ec2 --max 10 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner"}'
```

The template's `"spot": true` is still accepted for compatibility, but it
must agree with the pool's `preemptible` (422 `invalid_pool` otherwise).

EC2 gives two minutes' notice before it takes a spot instance back. Each
instance's runner watches for it (user data sets `LUX_EC2_IMDS`, which the
runner reads as `--ec2-imds`; IMDSv2 must be reachable). On the notice:

1. The runner tells luxd, which drains the host: nothing new is placed on
   it, and each of its Runs is asked to stop with reason `preempt`.
2. Stops from then on get a grace of at most half the time left, so the
   Run has time to snapshot its state volumes and upload them.
3. Each preempted Run is resumed automatically from that snapshot, on
   another host. If the pool has none free it launches one. An agent picks
   its session back up, as after any resume. Its spec's `resumePolicy`
   can change that ([resume policy](runspec.md#resume-policy)): `restart`
   starts it again on another host from scratch, not from the snapshot;
   `manual` and `never` (one-shot work) end it `failed`, e.g.
   `preempt: not resumed (resumePolicy never)`.

A Run whose stop or upload cannot finish in time (a very large state
volume) is lost when the instance goes. It is resumable from its previous
snapshot. Keep state volumes small on spot pools, or use on-demand for
Runs that cannot afford it (`placement.pool`).

### Non-preemptible pools

A pool's `preemptible` decides an `ec2` pool's market: `true` (the default)
launches spot instances, `false` on-demand ones. luxd sets the template's
`spot` from it before every launch and template check, so it is the one
place the market is set; a template `spot` that contradicts it is refused
(422 `invalid_pool`). Omitted on create, the flag follows the template's
`spot` (else `true`); omitted on update, it is unchanged. Migration
`091_pool_preemptible` set every existing `ec2` pool without `"spot": true`
to `false`: they already launched on-demand. Static pools are `true` until
set otherwise.

For API and IaC callers this changes what a create means: a pool created
(`POST /v1/pools`, `lux pools set`, `luxd admin create-pool`) without
`preemptible` and without a template `spot` is now preemptible and
launches spot instances; it used to launch on-demand. Pass
`preemptible: false` to keep on-demand. Existing pools are backfilled as
they were and keep their market on later updates that omit `preemptible`.

```bash
lux pools set steady --provider ec2 --max 4 --preemptible=false \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner"}'
lux run --pool steady --preemptible=false -f release.yaml
```

Only a pool with `preemptible: false` takes Runs submitted with
`placement.preemptible: false`
([Non-preemptible Runs](runspec.md#non-preemptible-runs)). After setting a
pool's flag to `false`, its existing spot hosts keep taking ordinary Runs,
never non-preemptible ones. They do not necessarily scale down: the pool's
`min` and `warm` floors may keep them indefinitely, so `lux hosts drain`
them to have the pool launch on-demand replacements. Runners without the
`non-preemptible` capability take none of these Runs until they update.
Setting a pool back to `true` while it holds a non-preemptible Run that is queued, live or
resumable is refused (409 `pool_has_non_preemptible_runs`): terminate those
Runs or move them (`lux resume --pool` to another non-preemptible pool)
first. See the [Run guarantee](runspec.md#non-preemptible-runs) for what
still stops a non-preemptible Run, including forced operator actions.

What an instance needs:

- **No custom AMI.** A launch template with **no user data** (luxd's own
  replaces it on every `RunInstances`) and one of:
  - a stock **Fedora CoreOS** AMI (the default; see `userData` below) —
    nothing is installed at boot, so a host registers in under a minute;
  - a stock **Fedora Cloud, Ubuntu, Debian or AL2023** AMI with
    `"userData": "script"` — cloud-init runs a script that installs only
    what is missing (podman, nftables, git; `dnf` or `apt-get`);
  - any AMI already carrying `lux-runner`/`lux-shim` and its own boot
    script, with `"userData": "env"` (the pre-self-update behaviour: plain
    `KEY=value` lines).

  Whichever format, the instance downloads `lux-runner` and `lux-shim` from
  luxd itself (`GET /runner/v1/bin/...`, verified by sha256) rather than
  carrying them in the AMI, so a new release needs no new AMI.
- A launch template (id `lt-…` or name), a security group reaching
  `runner_url`, egress to the package mirrors (the `script` format) and to
  wherever images, git remotes and model APIs live, and an instance
  profile if the runner needs one (it doesn't hold S3 credentials).
- luxd needs EC2 permissions for `RunInstances` (with the launch template
  and `CreateTags`), `TerminateInstances`, `DescribeInstances` and
  `DescribeInstanceTypes` (a template's `instanceType`'s memory, passed to
  the runner as `LUX_RUNNER_MEMORY`; without it the launch still goes
  ahead, logged, and the host offers its MemTotal; a shaped pool's hosts
  get the shape's memory instead and need no lookup), plus
  `pricing:GetProducts` for on-demand, EBS, data-transfer and public IPv4 prices,
  `ec2:DescribeSpotPriceHistory` for spot prices and `ec2:DescribeVolumes`
  for each host's block-storage volumes (their size and type price its
  block storage; without it a host's block storage stays missing, never
  zero), and, for [market data](telemetry.md#market-data),
  `ec2:GetSpotPlacementScores` (without it the samples have no placement
  score, logged once per pass) and `ec2:DescribeSubnets` (the zones of a
  pool's subnets; without it, the zones its hosts were launched in), from
  its standard AWS configuration (environment or instance role). These
  actions are read-only and require `Resource: "*"`: none supports
  resource-level permissions. The Terraform module's luxd role
  (`deploy/terraform/aws/iam.tf`) grants them. The runner needs none. The
  Pricing API uses `LUX_COSTS_PRICING_REGION` regardless of the host's
  region. `LUX_EC2_ENDPOINT` and `LUX_PRICING_ENDPOINT` override their
  respective endpoints for tests.

`template.userData` picks the format (default `"ignition"`); a pool set
with an unrecognized value is refused, not left to fail at boot:

| Value | What it is | When to use it |
| --- | --- | --- |
| `ignition` (default) | An Ignition v3.4.0 config for Fedora CoreOS. | The default: no packages to install, fastest boot. |
| `script` | A `#!/bin/bash` script cloud-init runs. | A stock Fedora Cloud, Ubuntu, Debian or AL2023 AMI. |
| `env` | Plain `KEY=value` lines (`LUX_URL`, `LUX_HOST_TOKEN`, `LUX_HOST_NAME`, `LUX_EC2_IMDS`, `LUX_RUNNER_MEMORY` when luxd knows the instance type's memory, `LUX_NESTED` for a nested pool, and for a shaped pool `LUX_RUNNER_CPUS`, `LUX_RUNNER_MEMORY`, `LUX_RUNNER_DISK` and `LUX_RUNNER_DISK_HEADROOM`, its shape's nominal host and disk headroom). | A custom AMI with its own boot script, from before self-update. |

With `ignition` and `script`, luxd also caps the host's dirty page cache in `/etc/sysctl.d/90-lux-writeback.conf` (`vm.dirty_background_bytes` 64 MiB, `vm.dirty_bytes` 256 MiB). Every Run's container storage shares the host's one filesystem, and an overlay unmount waits until all of it is written; under the kernel default (20% of available memory) one Run's container teardown waited 10-20s behind another Run's writes. The file is written at a host's first boot, so hosts launched before a luxd upgrade keep the default until they are replaced. `env` (a custom AMI) and the bootstrap script for static hosts leave it alone.

### Block storage of hosts launched before luxd recorded volumes

luxd prices each EC2 host's own disk (the volumes deleted with the
instance) as the **Block storage** cost family, from `DescribeVolumes`
([costs](costs.md#block-storage-built-in)). Hosts launched before that have
no recorded volumes, so their block storage is missing. State the launch
template's disk once per pool to fill it in, history included:

```bash
luxd admin costs backfill-volumes --pool burst --dry-run \
  --volume type=gp3,size=100,iops=3000,throughput=125
luxd admin costs backfill-volumes --pool burst \
  --volume type=gp3,size=100,iops=3000,throughput=125
```

`--tenant` (id or name) picks a tenant's pool; without it, the platform's.
`--volume` repeats for several volumes (`size` in GiB, `throughput` in
MiB/s; `iops` and `throughput` optional). It records the volumes on the
pool's hosts whose volumes are unknown, marked `"assumed": true`, prices
them at today's list price, re-evaluates the Runs placed on them (finished
ones gain a block-storage line; their compute amounts do not change) and
rebuilds the hosts' hourly rows within `costs.hourly`. It prints the hosts,
hours and Runs it touched (or would, with `--dry-run`), and under `skipped`
the Runs it leaves alone: those whose compute went final before compute
snapshots existed (migration 027), which re-evaluating would reprice.
Running it again changes nothing. It needs `pricing:GetProducts` (or a
fresh cached price) and luxd's database settings, like the other admin
commands.

### Nested containers on an EC2 pool

`"nestedContainers": true` in an ec2 template starts every host the pool
launches from then on with `lux-runner --nested`: user data sets
`LUX_NESTED=true` in `/etc/lux/runner.env` (every format, including
`env`, whose own boot script must pass it on). It is off by default, a
JSON boolean (anything else is refused when the pool is set), and only for
ec2 pools: a static host offers nested containers with its own flag.

```bash
lux pools set burst --provider ec2 --max 10 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner","nestedContainers":true}'
```

- The AMI must provide `/dev/fuse` and `/dev/net/tun` (Fedora CoreOS does).
  A runner without them exits instead of registering, so a wrong AMI shows
  as launches that never become ready rather than nested Runs placed on
  hosts that cannot run them.
- Capacity planning counts a future host as nested-capable only when the
  current template opts in, never from the labels of hosts launched
  earlier. A Run with `sandbox.nestedContainers` on a pool without it waits
  with "host does not support nested containers" and launches nothing.
- Once the two newest hosts of the current template register without
  nested containers (or the only one so far does: an `env`-format AMI that
  does not pass `LUX_NESTED` on, say), nested Runs stop counting on its
  future hosts: they wait with "current template's hosts registered without
  nested containers", and only a Run that began waiting after the last
  registration launches one probe host. One host without nesting after a
  nested one does not stop nested launches. A nested host registering last
  (a slow launch from before the change) resumes them for at most one
  burst (plus one host if that burst's hosts register one at a time), and
  its first two hosts without nesting stop them again. Fix the AMI or
  launch template and edit the pool template to plan afresh.
- Hosts that already run keep what they registered with. Changing the
  template is a new template, so the pool scales on it as for any template
  edit; replace (drain) existing hosts to apply it to them.

Every format's token is single-use per host and revoked when the host is
terminated. A static host (outside any pool) uses the same script as
`userData: script`, unfilled: `curl <luxd>/runner/v1/bootstrap.sh | sudo env
LUX_HOST_TOKEN=luxh_… LUX_URL=https://luxd.example bash` (no auth on that
endpoint: it carries no secret, only how to reach luxd).

Instances are tagged `Name=<host>`, `lux:pool-id=<pool id>`,
`lux:pool=<pool name at launch>`, `lux:managed=true`,
`lux:deployment=<id>` (which lux database launched it: deployments sharing
an account never touch each other's instances) and `lux:host=<host id>`,
plus the template's `tags`. luxd lists a pool's instances by
`lux:pool-id`, `lux:managed` and `lux:deployment` (`DescribeInstances`
filtered by tag), never by name: renaming a pool (`lux pools rename`) is a
database change, and its instances keep running and stay its own.
`lux:pool` is informational and not updated by a rename. All tags are set
at launch; luxd tags nothing afterwards.

Reusable Terraform for running all of this on AWS — control host, S3,
runner launch templates, Cloudflare Tunnel — is under
[deploy/terraform/](../deploy/terraform/README.md).
