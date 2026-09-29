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

1. Run `luxd migrate` with the owner's DSN. It applies what is new and is
   safe to run again; running luxds keep working meanwhile.
2. Restart (or roll) every `luxd serve` onto the new binary.

Migrate first: a luxd newer than its schema does not check it, and fails
requests that touch what is missing (luxds older than the schema keep
working). Configuration through the environment alone keeps working: a
configuration file is optional.

### Configuration

luxd reads a TOML file: `--config FILE` (before or after the command),
else `LUX_CONFIG`, else `/etc/lux/luxd.toml` if it exists. Every setting
also has an environment variable, which overrides the file, so a secret can
stay out of it (`LUX_DATABASE_URL`, `LUX_S3_SECRET_KEY`). An unknown key or
a bad value stops luxd, naming it. luxd warns when others can read the
file: keep it `chmod 600`, or keep secrets in the environment.
[luxd.example.toml](luxd.example.toml) has every key with its default and
its variable; the table below lists them by variable.

| Variable | Default | |
| --- | --- | --- |
| `LUX_DATABASE_URL` | — | For `serve`, a DSN for the `lux_app` role; for `migrate`, the owner's. One file can serve both: set this variable per command. |
| `LUX_APP_PASSWORD` | `lux_app` | `migrate` sets `lux_app`'s password to it, each run. Set your own. |
| `LUX_LISTEN` | `127.0.0.1:7070` | Address to serve on. |
| `LUX_PUBLIC_URL` | — | The URL clients use. Runners too, unless `LUX_RUNNER_URL` is set. |
| `LUX_RUNNER_URL` | = public_url | The URL runners dial, if different from `LUX_PUBLIC_URL` (a private IP such as `http://10.0.1.10:7070`, unreachable from outside the VPC). |
| `LUX_RUNNER_BIN_DIR` | `/usr/local/lib/lux/runner` | Runner binaries luxd serves and hashes (sha256) at startup, for self-update: `<dir>/linux-{arm64,amd64}/{lux-runner,lux-shim}`. A missing arch or file is simply not offered. |
| `LUX_S3_BUCKET` | — | Where snapshots, output and artifacts go. |
| `LUX_S3_ENDPOINT` | AWS | For MinIO and other S3-compatible stores (path-style). |
| `LUX_S3_PUBLIC_ENDPOINT` | = endpoint | The endpoint presigned URLs are signed for, if runners and clients reach S3 by another name. |
| `LUX_S3_REGION` | `us-east-1` | |
| `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY` | AWS chain | Only luxd holds S3 credentials. |
| `LUX_LEASE` | `30s` | A host that misses heartbeats this long is lost, along with its live placements. Runners heartbeat every third of it. It is also how long a Run with secrets waits for a luxd that holds their values (after a restart, say) before it stops, to be resumed with them. |
| `LUX_TICK` | `1s` | Scheduler and reaper interval. |
| `LUX_DEFAULT_CPUS`, `LUX_DEFAULT_MEMORY`, `LUX_DEFAULT_DISK`, `LUX_DEFAULT_PIDS` | `2`, `8Gi`, `20Gi`, `1024` | Resources a Run gets when its spec sets none. |
| `LUX_PROVIDER_CHECK_EVERY` | `1m` | How often each EC2 pool's instances are listed (orphans terminated, vanished hosts written off). Mind the provider's API limits. |
| `LUX_LOST_GRACE` | `5m` | How long a lost provisioned host's instance is kept (a runner restart or a network blip is not a loss) before it is terminated. |
| `LUX_LISTING_LAG` | `1m` | How long after a launch EC2's listings may still miss an instance: until then a host missing from them is not looked up. After it, one is looked up by id, and written off only if EC2 says it is gone. |
| `LUX_SCALE_DOWN_AFTER` | `10m` | How long a provisioned host stays idle before it is cordoned, then terminated once idle. |
| `LUX_LAUNCH_TIMEOUT` | `10m` | How long a launched host may take to register before it is terminated. |
| `LUX_OUTDATED_DRAIN_PERCENT` | `10` | Caps concurrent outdated-binaries drains per pool, as a percentage of its live hosts (at least 1 regardless). |
| `LUX_EC2_ENDPOINT` | AWS | Overrides the EC2 endpoint (tests). |
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
| `LUX_DEBUG` | — | Debug logging. |
| `LUX_CONSOLE_AUTH` | `key` | How the console signs people in: `key` or `cloudflare-access` ([Operators](operators.md#signing-in)). |
| `LUX_CF_ACCESS_TEAM`, `LUX_CF_ACCESS_AUD` | — | For `cloudflare-access`: the Access team (`acme` or `acme.cloudflareaccess.com`) and the application's AUD tag. |
| `LUX_CF_ACCESS_OPERATORS` | — | Comma-separated explicit operator emails (TOML: `console.cloudflare_access.operators` list). Required in Access mode; matched case-insensitively against verified JWT email only. |
| `LUX_CF_ACCESS_DEFAULT_TENANT` | — | Existing tenant ID (preferred) or exact tenant name (e.g. `absmartly`) for non-operator Access users. Required in Access mode; missing tenants deny requests. |
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

### Tenants, keys and quotas

```bash
luxd admin create-tenant --name acme [--max-runs N] [--max-hosts N] [--retention-days 30]
luxd admin create-key --tenant T --scopes read,run
luxd admin create-operator-key [--name N]       # every tenant: see docs/operators.md
luxd admin set-quota --tenant T [--max-runs N] [--max-hosts N] [--max-storage BYTES] [--retention-days N]
```

- `--max-runs`: Runs that are not stopped or finished. Checked when a Run
  is submitted or resumed.
- `--max-hosts`: the tenant's registered hosts. Checked when a host
  registers.
- `--max-storage`: bytes in snapshots, output and artifacts not yet
  deleted by retention. Checked when a Run is submitted or resumed.
- `--retention-days`: how long a finished Run's blobs are kept (default
  30). The Run and its events stay. Resuming a finished Run clears its
  finish time, so a live Run's snapshots are never deleted.

A request over quota gets HTTP 429, and the CLI exits with code 5.

## Where bytes live

Every placement ends with a **snapshot** of its state volumes, its
**output**, and its **artifacts**, all written to the host's disk. They
reach S3 in the background:

1. The runner uploads each blob with `PUT /runner/v1/blobs/{id}`. luxd streams
   it into S3 and verifies its sha256 on the way. Runners never hold S3
   credentials.
2. The host keeps its local copy, so a resume there moves nothing. It
   deletes the copy when:
   - luxd tells it the Run now runs elsewhere (from S3), or
   - the copy is older than the runner's `--host-ttl` (default 24h).

   A copy is never deleted before its upload finished.
3. A runner downloads a snapshot through a presigned S3 URL, valid for 15
   minutes: it asks `GET /runner/v1/blobs/{id}`, and luxd redirects it only
   for blobs of a Run placed on that host. Artifacts are downloaded through
   luxd, which decompresses them (blobs are stored zstd) and sends the file
   with its length and sha256 (`X-Lux-SHA256`), so a download cut short is
   detected.
4. Retention deletes a finished Run's blobs from S3 after the tenant's
   `retention_days`.

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
- For nested containers, `lux-runner --nested`: the host needs `/dev/fuse`
  and `/dev/net/tun`. The runner then labels the host `nested=true`; that
  label cannot be set with `--label` or a host token.

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
| `--max-runs`, `--cpus`, `--memory` | 16, all, all | Capacity offered to the scheduler. |
| `--disk` | not reserved | Bytes of disk the scheduler reserves Runs' `resources.disk` from. Without it, disk is not reserved (Runs still stop at their own limit). Set it to the space under `/var/lib/containers`, more to overcommit. |
| `--usage-every` | `15s` | How often each Run's disk use is sampled, and its disk limit checked. |
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
  enough hosts for them, plus `--warm` idle ones kept ready. It keeps at
  least `--min` hosts and never more than `--max`. Launches alternate
  across the template's subnets.
- **Scale down:** a host idle longer than the pool's `--scale-down-after`
  (default `LUX_SCALE_DOWN_AFTER`, 10m), above the minimum and warm count,
  is cordoned. It is terminated once
  it has no live placements and nothing left to upload. `lux pools rm`
  cordons the pool's hosts the same way, without `--force-evict`: a Run on
  one finishes where it is and the host is terminated once idle; with
  `--force-evict` it is stopped (snapshotted) and resumed elsewhere at once,
  never cut short.
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
- **Failures:** a launch that fails is retried on the next pass. A host
  that never registers within `LUX_LAUNCH_TIMEOUT` (default 10m) is
  terminated. An instance EC2 no longer has is written off and replaced.
- **Orphans:** once a minute luxd lists the pool's instances by tag. One
  no host row claims (a launch whose reply was lost) is terminated, after
  checking the host rows once more, under any pool name of the owner, right
  before. A host missing from the listing is looked up by instance id
  (`DescribeInstances` with `InstanceIds`, which tag changes do not delay):
  written off only if EC2 says it is terminated or does not know it (and
  its runner is silent); still running, it is kept, logged at WARN, and
  re-tagged if its `lux:pool` is not its pool's. A tag listing alone never
  terminates or writes off anything. A host lost for over 5 minutes is
  terminated.
- One luxd instance does all this at a time (a lease in Postgres). The
  lease carries a fencing token, new whenever another luxd takes it; each
  terminate, re-tag and write-off first checks that the pass still holds
  the lease it began with, and EC2 calls are cancelled when it expires.

### Renaming a pool

`lux pools rename burst burst-eu` (`POST /v1/pools/{name}/rename`)
renames a pool with its hosts running: nothing is drained or terminated.
In one transaction the pool, its hosts (terminated ones too, which cost
lookups join by name), its host tokens, their `cost_hourly` rows and every
Run not yet final that names it move to the new name; finished Runs keep
the spec they ran with. A Run waiting for the pool schedules under the new
name at once. The new name follows the [pool-name rule](cli.md#hosts-and-pools)
(422 `invalid_pool` otherwise; a name kept from before the rule is never
given anew). Refused with 409:

- `pool_exists`: the name is taken by a live or removed pool of the
  tenant, or by hosts or host tokens; for a platform pool, also when a
  tenant whose Runs would follow owns a pool by the new name (its Runs
  would then go to its own pool).
- `rename_in_progress`: the pool's previous rename is unfinished.
- `pool_name_reserved`: the new name is a live alias of another pool of
  the owner (see below).
- `too_many_aliases`: an `ec2` pool already has 8 live aliases.
- `rename_cooldown`: an `ec2` pool finished a rename less than the
  provisioner lease (30s, or ten scheduler ticks) plus `LUX_LISTING_LAG`
  ago. A `CreateTags` sent by a luxd that has since lost its lease may still
  land in that time, and would put the previous name back.
- `rename_unsupported_by_deployment`: an `ec2` pool, while some luxd that
  cannot follow a rename is running (see below).
- `confirm_required`: the pool has hosts and the request's `confirm` is
  not its current name. `lux pools rename` sends it (the name typed on the
  command line); the console asks for it to be typed.

`cost_hourly` is
rewritten rather than kept under the old name because it is recomputed
from `hosts.pool`: history kept under the old name would split one host's
hours between both names as they are recomputed.

An `ec2` pool's instances carry its name in the `lux:pool` tag, which is
how luxd lists them, and an instance listed with no host row to claim it
is terminated as an orphan. So the tags are changed after the database,
and the pool keeps its previous names as **aliases**
(`pool_tag_aliases`), listed alongside its name:

1. The transaction above adds the old name to the pool's aliases. From its
   commit, the provisioner lists the pool under its name and every live
   alias: every instance, whichever tag it carries, is claimed by its host
   row, or, with none, terminated as an orphan. That includes an instance
   whose `RunInstances` was sent with the old tag before the rename and
   answered long after it (its reply lost, its host row timed out): it
   turns up under the alias and is handled like any launch whose reply
   was lost, never left running untracked. A live alias stays reserved:
   no other pool of the owner can take it and see these instances as its
   orphans (`pool_name_reserved`). Nothing is tagged before this commit,
   so no instance ever carries a name no pool answers to.
2. On each provider check (`LUX_PROVIDER_CHECK_EVERY`) the provisioner
   re-tags the instances still carrying an alias (`ec2:CreateTags`,
   `lux:pool` only). EC2's tag filters lag behind
   tags, so for `LUX_LISTING_LAG` after a re-tag a host missing from every
   listing is not even looked up; after it, it is looked up by id like any
   unlisted host, and kept while it runs. The rename finishes (`pools ls`
   no longer shows RENAMED FROM; the console drops its "renaming" badge)
   on a check at least `LUX_LISTING_LAG` after the rename and the last
   re-tag that lists nothing live under any alias, every host under the
   new name, and has no launch in flight. Each check also moves to the new
   name any `cost_hourly` row a cost pass wrote under an old one from a
   host row it read before the rename.
3. A finished rename's alias is still listed. It is retired (no longer
   listed nor reserved) once every check has found nothing under it for
   twice the longer of `LUX_LAUNCH_TIMEOUT` and `LUX_LISTING_LAG`; a check
   that finds an instance under it starts that wait again. `pools ls -o
   json` shows a pool's live `aliases`; a pool has at most 8, and a rename
   that would add a ninth is refused (`too_many_aliases`). Renaming a pool
   back to one of its own aliases is allowed: that alias is its name again.

A luxd that stops before the commit leaves nothing renamed; after it,
whichever luxd holds the provisioner lease carries on with step 2, which
is idempotent. A refused `CreateTags` (the IAM statement below missing) is
logged as a warning and retried on every check; the pool keeps working
under its new name and nothing is terminated, but it stays listed under
its aliases, `lux pools ls` shows RENAMED FROM, and it cannot be renamed
again until the tags are fixed. A static pool has no tags: the
transaction is the whole rename, and it gets no alias. A pool's provider
cannot change while it has a live alias (`lux pools set` and
`luxd admin create-pool`: 409 `rename_in_progress`).

**Every luxd must be of this version or later before an `ec2` pool is
renamed.** An older luxd lists a pool under its current name only: holding
the provisioner lease during a rename, it would terminate every instance
still tagged with the old name. Each luxd records its version and what it
can do in `luxd_instances` before its first attempt at the provisioner
lease, and again every `LUX_PROVIDER_CHECK_EVERY` (at most every lease
duration). Two guards follow from it:

- The database refuses the provisioner lease to a luxd that has not
  checked in with the `pool-rename` capability while any pool has a live
  alias: a trigger on `leases` raises an error, which an older luxd logs
  and retries, so it never becomes the provisioner while an alias is
  live. With no live alias, any luxd may hold it: a mixed fleet keeps
  working as long as nobody renames.
- A rename of an `ec2` pool locks the provisioner lease row (creating it,
  expired and held by no one, if there is none) and is refused while its
  current holder, or any luxd that wrote a control sample within the last
  two lease durations, has not checked in with that capability
  (`rename_unsupported_by_deployment`, naming them). The rename and a
  lease acquisition both take that row's lock, so one waits for the
  other: either the rename sees the new holder, or the acquisition sees
  the alias and is refused.

Static pools can always be renamed. Do not roll luxd back below this
version while any pool has a live alias (`lux pools ls -o json` shows
`aliases`): the database would refuse the older luxd the provisioner
lease until the aliases retire. Wait for them to, or roll forward.

The re-tag needs this IAM statement on luxd's role (the Terraform module
has it as `RetagManagedInstancePool`): `ec2:CreateTags` on instances
tagged `lux:managed=true` and carrying `lux:host`, for the `lux:pool` key
only:

```json
{
  "Effect": "Allow",
  "Action": "ec2:CreateTags",
  "Resource": "arn:aws:ec2:<region>:<account>:instance/*",
  "Condition": {
    "StringEquals": { "ec2:ResourceTag/lux:managed": "true" },
    "Null": { "ec2:ResourceTag/lux:host": "false", "aws:TagKeys": "false" },
    "ForAllValues:StringEquals": { "aws:TagKeys": ["lux:pool"] }
  }
}
```

A rename is logged at INFO (`pool renamed`, with the old and new names and
how many hosts, Runs and instances followed) and again when its instances
all carry the new name (`pool rename finished`).

### Spot instances

Add `"spot": true` to the template to launch one-time spot instances:

```bash
lux pools set burst --provider ec2 --max 10 \
  --template '{"region":"eu-west-1","launchTemplate":"lux-runner","spot":true}'
```

EC2 gives two minutes' notice before it takes a spot instance back. Each
instance's runner watches for it (user data sets `LUX_EC2_IMDS`, which the
runner reads as `--ec2-imds`; IMDSv2 must be reachable). On the notice:

1. The runner tells luxd, which drains the host: nothing new is placed on
   it, and each of its Runs is asked to stop with reason `preempt`.
2. Stops from then on get a grace of at most half the time left, so the
   Run has time to snapshot its state volumes and upload them.
3. Each preempted Run is resumed automatically from that snapshot, on
   another host. If the pool has none free it launches one. An agent picks
   its session back up, as after any resume.

A Run whose stop or upload cannot finish in time (a very large state
volume) is lost when the instance goes. It is resumable from its previous
snapshot. Keep state volumes small on spot pools, or use on-demand for
Runs that cannot afford it (`placement.pool`).

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
  and `CreateTags`), `TerminateInstances` and `DescribeInstances`,
  `CreateTags` of `lux:pool` on its own instances to rename a pool
  ([above](#renaming-a-pool)), plus
  `pricing:GetProducts` for on-demand prices and
  `ec2:DescribeSpotPriceHistory` for spot prices, from its standard AWS
  configuration (environment or instance role). Both pricing actions are
  read-only and require `Resource: "*"`; the runner needs neither. The
  Pricing API uses `LUX_COSTS_PRICING_REGION` regardless of the host's
  region. `LUX_EC2_ENDPOINT` and `LUX_PRICING_ENDPOINT` override their
  respective endpoints for tests.

`template.userData` picks the format (default `"ignition"`); a pool set
with an unrecognized value is refused, not left to fail at boot:

| Value | What it is | When to use it |
| --- | --- | --- |
| `ignition` (default) | An Ignition v3.4.0 config for Fedora CoreOS. | The default: no packages to install, fastest boot. |
| `script` | A `#!/bin/bash` script cloud-init runs. | A stock Fedora Cloud, Ubuntu, Debian or AL2023 AMI. |
| `env` | Plain `KEY=value` lines (`LUX_URL`, `LUX_HOST_TOKEN`, `LUX_HOST_NAME`, `LUX_EC2_IMDS`). | A custom AMI with its own boot script, from before self-update. |

Every format's token is single-use per host and revoked when the host is
terminated. A static host (outside any pool) uses the same script as
`userData: script`, unfilled: `curl <luxd>/runner/v1/bootstrap.sh | sudo env
LUX_HOST_TOKEN=luxh_… LUX_URL=https://luxd.example bash` (no auth on that
endpoint: it carries no secret, only how to reach luxd).

Instances are tagged `Name=<host>`, `lux:pool=<pool>` (`<tenant>/<pool>`
for a tenant's pool), `lux:managed=true`, `lux:deployment=<id>` (which lux
database launched it: deployments sharing an account never touch each
other's instances) and `lux:host=<host id>`, plus the
template's `tags`. luxd also needs `DescribeInstances`, filtered by tag and by instance id.

Reusable Terraform for running all of this on AWS — control host, S3,
runner launch templates, Cloudflare Tunnel — is under
[deploy/terraform/](../deploy/terraform/README.md).
