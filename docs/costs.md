# Run costs (design)

Status: **design for review. Build steps 1–4 (section 12) are built**, and
the sections they touch say so and note where the build differs. The rest
is not built. The build order at the end breaks it into small steps.

This doc covers what each Run costs: the hosts it ran on (built in) and
anything outside lux that it used, such as model tokens, video or image
generation (reported by **cost plugins**). Every mechanism below uses what
lux records today (see [Concepts](concepts.md) and
[Telemetry](telemetry.md)). Where lux lacks something the design needs,
the doc says so under **Missing today**.

## What lux has today

The parts this design builds on:

- **Run states** (`internal/server/lifecycle.go`): `submitted`,
  `provisioning`, `scheduled`, `starting`, `running`, `stopping`,
  `stopped`, `resuming`, `succeeded`, `failed`, `cancelled`, `lost`. Only
  `succeeded`, `failed` and `cancelled` are terminal (`terminal()`), and a
  `failed` Run can still be resumed (`resumableRunStates` =
  `stopped`, `lost`, `failed`). There is no "paused", "parked", "aborted"
  or "completed". Mapped onto lux's states, those words mean:

  | Requirement's word | lux transition |
  | --- | --- |
  | paused / parked | `running → stopping → stopped` (`stop`, or a `drain`/`preempt`/`migrate` move, which then goes `stopped → resuming`) |
  | stopped | same as above |
  | aborted / cancelled | `→ cancelled` (from `stopping`, or straight from `submitted`/`resuming`/`provisioning`/`stopped`/`lost` in `stopOrCancel`; or `lost` with `cancel_requested`) |
  | failed | `→ failed` (non-zero exit, `timeout`, `disk`, adapter failure) |
  | completed | `→ succeeded` |
  | (host died) | `→ lost` (`placementLost`) |

  Every Run state change goes through one function, `setRunState`. It
  writes `runs.state` and a `state` row in `run_events`, and the
  `run_events` insert trigger notifies `lux_events` (migration 012).

- **Placements** have the states `assigned`, `starting`, `running`,
  `stopping`, `exited` and `lost`. The live ones are `livePlacementStates`
  (`assigned`…`stopping`). A placement's **window** runs from
  `placements.created_at` (assigned: the scheduler reserves the capacity at
  that moment) to `placements.ended_at` (set when it becomes `exited` or
  `lost`). `started_at`, `workload_started_at`, `exited_at` and the other
  times sit inside that window.

- **Reservations**: `placements.resources` (jsonb), copied from the spec's
  `resources` in `assign` (`scheduler.go`). It holds `cpus` (a float) and
  `memory` (bytes). This is what `host_samples.alloc_cpus`/`alloc_mem`
  already sum.

- **Host capacity**: `hosts.capacity` (jsonb `{cpus, memory, disk, runs}`),
  as the runner advertises it on every hello. `lux-runner --cpus` may offer
  less than the machine has.

- **Host lifecycle**: `provision_requested_at`, `provisioned_at` (the first
  hello, set in `runner.go`), `registered_at`, `drain_requested_at`,
  `terminate_requested_at`, `terminated_at` (set when luxd marks the host
  terminated), `lost_at`. Host states: `provisioning`, `ready`, `draining`,
  `lost`, `terminated`. A lost host that comes back re-registers as the
  same row.

- **EC2**: `hosts.provider_id` (the instance id) and
  `hosts.launch_template` (the pool's template at launch: `region`,
  `instanceType`, `spot`, `subnets`).

- **Usage**: `placements.cpu_seconds` and `peak_memory_bytes` (totals and
  peaks), and `placement_samples` over time.

- **Sessions**: `runs.session_id` holds only the **latest** id. Adapters
  report a session (claude-code: `session_id` on its stream messages;
  acp/opencode: from `session/new` or `session/load`; codex: its thread
  id). `applyAdapterEvent` writes it to `runs.session_id` and adds a
  `session` event. `applySnapshotDone` also sets `runs.session_id` from the
  snapshot manifest's `sessionId`, and that path writes **no** `session`
  event.

- **Singletons across luxd instances**: the `leases` table (migration 006,
  used by the provisioner) and `pg_advisory_xact_lock` (`runnerbin.go`).
  Row-level security covers every tenant table. Platform tables are
  `system_only`.

**Missing today** (each one is a step in the build order):

- The instance's availability zone, and its instance type when the
  launch template picks it (**built**, section 3: `hosts.instance_type`,
  `zone`, `market`).
- The instance's real launch and termination times at the provider.
  lux has its own `provision_requested_at` and `terminated_at`, which are
  close but not exact.
- Any price from a provider, and the money tables beyond the lines and
  rates: `price_cache`, `cost_pending`, `cost_ticks` and `cost_hourly` do
  not exist, and nothing writes a cost line yet. (Built: `cost_lines.amount`
  and `cost_sources`, section 1; `host_rates` and static hosts' prices,
  sections 2 and 3; the per-run list of every session, `run_sessions`,
  section 6.)

## 1. The cost line

Everything is a cost line, compute included.

```sql
CREATE TABLE cost_lines (
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  source      text NOT NULL,              -- 'compute' or a plugin's configured name
  item        text NOT NULL DEFAULT '',   -- '' when the source gives none
  family      text NOT NULL,              -- free-form: compute, ai, video, image-gen, ...
  amount      numeric(24, 9) NOT NULL,    -- never float
  currency    text NOT NULL,              -- ISO 4217 as sent; never converted
  period_from timestamptz NOT NULL,       -- the time window this line covers
  period_to   timestamptz NOT NULL,
  final       boolean NOT NULL DEFAULT false,  -- false: an estimate
  details     jsonb NOT NULL DEFAULT '{}',
  reported_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, run_id, item)
);
CREATE INDEX cost_lines_tenant_period ON cost_lines (tenant_id, period_to);
CREATE INDEX cost_lines_run ON cost_lines (run_id);  -- the read API: the key leads with source
-- tenant_rows RLS policy, as for runs.
```

**Built**: migration `019_cost_lines.sql` (this table and `cost_sources`
from section 5), and `replaceCostLines` in `internal/server/costs.go`,
which no producer calls yet. It refuses a whole answer that names an item
twice, or an amount that is not a decimal string with at most 15 integer
and 9 fractional digits. Amounts come back with trailing zeros trimmed:
`"1.284310"` is returned as `"1.28431"`.

As JSON (API and plugin responses):

```json
{
  "runId": "run_01J…",
  "source": "model-gateway",
  "family": "ai",
  "item": "large-model-v3",
  "amount": "1.284310",
  "currency": "USD",
  "from": "2026-09-26T10:00:04Z",
  "to": "2026-09-26T10:41:52Z",
  "final": false,
  "details": {"inputTokens": 812345, "outputTokens": 40211, "requests": 57}
}
```

**Replace, never add.** A source's latest answer for a Run replaces all of
that source's lines for the Run, in one transaction:
`DELETE FROM cost_lines WHERE source = $1 AND run_id = $2`, then insert the
new set. The key is (source, run, item), so a source must not send the same
item twice for one Run. If it does, that Run's answer is refused and counted
as an error (see *Errors*), rather than summed silently. Reporting again
never double-counts, and an item that stops appearing is removed.

**Totals** are always per currency. There is no single "total cost" across
currencies:

- Run total: `SUM(amount) GROUP BY currency`.
- Per family: `GROUP BY family, currency`. Per item:
  `GROUP BY family, item, currency`.
- A Run's **status** comes from its sources (section 5): `complete` (every
  source has answered for its current windows), `incomplete` (some source
  has not answered or has failed, and the API names which), or `final`.

**Families** are open. lux stores and shows whatever strings arrive. A
plugin's describe endpoint may give a display name and a colour hint for
its families (section 4). An unknown family is shown by its raw name with a
neutral colour. `compute` is lux's own family.

## 2. Compute cost (built in)

Compute is on by default for `ec2` pools and can be turned off per
provider (`costs.compute.ec2`). A `static` pool's hosts registered
themselves, so no provider prices them: each carries a **flat hourly
price** of its own, stored in the database (not luxd's config, since pools
and hosts are created through the API), which fills its `host_rates`
periods with source `static`. Other providers plug in their own price
source: it fills the same `host_rates` rows (section 3), and nothing
downstream changes.

### Static prices

**Built** (migration `021_host_rates.sql`, `internal/server/prices.go`):

- **Per host:** `hosts.hourly_price numeric(24, 9)` and
  `hosts.price_currency text`, both NULL or both set (a CHECK). Set with
  `PUT /v1/hosts/{id}/price` (`{"hourlyPrice": "0.40", "currency":
  "USD"}`), cleared with `DELETE` on the same path, or `lux hosts price
  <host> --hourly-price 0.40 --currency USD` / `--clear`. Whoever may change
  the host's pool may do it: a tenant for its own hosts, an operator for
  any host (a platform host's price is the operators'). A tenant gets 404
  for a host it cannot see and 403 for a platform host it can. A host its
  pool's provider launched is refused (422): the provider prices it.
- **Pool default:** a `static` pool may carry `hourlyPrice` (a decimal
  string) and `currency` in the Pool API body (`lux pools set
  --hourly-price --currency`, `luxd admin create-pool` with the same
  flags). It is refused for `ec2` pools. A host registering into the pool
  for the first time copies it. **Changing the default does not reprice
  the pool's existing hosts**; set those one by one.
- **Periods:** on every hello and every price change, luxd compares the
  host's price and advertised capacity (`cpus`, `memory`) with its open
  `static` period. If either changed, the open period is closed and a new
  one opens at the same instant, with the current capacity and price.
  So a priced host's first registration opens its first period, a re-hello
  with the same capacity changes nothing, and clearing the price closes the
  open period and opens none. **No price, no period**: its Runs get no
  compute line (and, once lines are written, `details.missingRate` for
  that time).

### Formula

For a placement `p` of Run `r` on host `h`, at any instant `t`:

```
share(p)   = max(p.cpus / h.capacity.cpus, p.memory / h.capacity.memory)
S(h, t)    = Σ share(q) over placements q live on h at t
charged(p) = share(p) / max(1, S(h, t))
cost(p)    = ∫ rate(h, t) × charged(p) dt   over p's window
unalloc(h) = ∫ rate(h, t) × max(0, 1 − S(h, t)) dt   over h's billed window
```

- `p.cpus` and `p.memory` come from `placements.resources`: what the Run
  **reserved**, not what it used.
- `h.capacity` is what the runner **advertises**. If a runner offers only
  part of a machine, a full host still charges its whole price to the Runs
  on it, and the withheld part is not billed as unallocated. That is the
  point of `--cpus`.
- The window is `created_at` (assigned) to `ended_at`, or now while the
  placement is live. Image pulls, restores and the stop grace period are
  charged, because the capacity is reserved for the Run during all of them.
  A lost placement is charged up to `ended_at`, which is when luxd gave up
  on it (up to one lease after the last heartbeat).
- **Why `max(1, S)`:** the scheduler keeps Σ cpus ≤ capacity and
  Σ memory ≤ capacity separately, but the sum of each Run's *larger*
  share can go above 1 (one CPU-heavy Run plus one memory-heavy Run).
  Scaling by `1/S` when `S > 1` keeps allocated + unallocated exactly equal
  to the host's cost. When `S ≤ 1` (the usual case), each Run pays its
  share as is.
- The integral is exact, with no sampling. `S` only changes when a
  placement on the host starts or ends, and `rate` only changes at a price
  period boundary. So luxd splits the host's timeline at those instants and
  sums over the pieces. It needs only `placements` and `host_rates`, never
  the heartbeat samples.

**Built** (`internal/server/compute.go`): `computeCost`, a pure function
over one host's rate periods, billed window and placements (anything still
open ends at the `now` it is given), returns each placement's amount per
currency, the host's unallocated amount, and every piece with its `S`.
Money is exact rational arithmetic (`math/big.Rat`), never float64; an
amount is rounded to 9 fractional digits only when turned into a string.
A share uses the capacity of the **rate period** (`cap_cpus`,
`cap_memory`), so a capacity change is a period boundary like a price
change. A piece of the billed window with no period is returned as
**missing**, for the host and for each placement live in it, never priced
at zero. Overlapping periods are refused as an error. `loadHostCompute`
reads a host's `host_rates` and placements into it; the billed window is
from `provision_requested_at` (a self-registered host: `registered_at`) to
`terminated_at`, or still open. Nothing calls them yet: the drainer
(step 5) will.

**allocated + unallocated = host cost**, for every piece of the timeline:
`Σ charged + max(0, 1 − S)` is `S + (1 − S) = 1` when `S ≤ 1`, and
`S/S + 0 = 1` when `S > 1`. Unallocated covers the whole billed window,
including the time before any placement (provisioning, boot, registration),
idle time, draining, time lost before termination, and any empty part of a
busy host.

### Worked example

These prices are made up for illustration and are not real quotes. One
host with 8 CPUs and 32 GiB, on demand at **$0.40/h**, billed 10:00–11:00.

| Run | reserved | cpu share | mem share | share | placed |
| --- | --- | --- | --- | --- | --- |
| A | 2 CPU, 8 GiB | 0.25 | 0.25 | **0.25** | 10:00–10:30 |
| B | 1 CPU, 16 GiB | 0.125 | 0.5 | **0.5** | 10:15–11:00 |
| C | 4 CPU, 4 GiB | 0.5 | 0.125 | **0.5** | 10:30–10:45 |

Each 15-minute piece of the host costs $0.10:

| piece | live | S | A | B | C | unallocated |
| --- | --- | --- | --- | --- | --- | --- |
| 10:00–10:15 | A | 0.25 | 0.025 | | | 0.075 |
| 10:15–10:30 | A, B | 0.75 | 0.025 | 0.050 | | 0.025 |
| 10:30–10:45 | B, C | 1.00 | | 0.050 | 0.050 | 0 |
| 10:45–11:00 | B | 0.50 | | 0.050 | | 0.050 |
| **total** | | | **0.050** | **0.150** | **0.050** | **0.150** |

Allocated $0.25 plus unallocated $0.15 equals the host's $0.40.

Now add a Run D (2 CPU, 4 GiB, share 0.25) during 10:30–10:45. It fits,
since the reservations total 7 CPU and 24 GiB. Then `S = 1.25`, and B, C
and D pay `0.5/1.25`, `0.5/1.25` and `0.25/1.25` of $0.10: $0.04, $0.04
and $0.02. Unallocated for that piece is 0, and the piece still sums to
$0.10.

### Lines produced

Source `compute`, family `compute`, one line per Run per **instance type**
(`item`, for example `m7i.2xlarge`, with `:spot` appended for spot). A Run
placed twice on the same instance type gets one line. `details` lists the
placements:

```json
{"placements": [{"epoch": 1, "hostId": "host_…", "from": "…", "to": "…",
  "cpus": 2, "memory": 8589934592, "share": 0.25, "ratePerHour": "0.40",
  "market": "on-demand", "zone": "eu-west-1a", "amount": "0.05"}]}
```

Static hosts without a price have no period, so their Runs get no
compute line.

### Reserved vs used (efficiency)

Shown next to the cost and never charged. It comes from what lux already
records:

- CPU: `placements.cpu_seconds / (cpus × window seconds)`.
- Memory: `peak_memory_bytes / memory` (a peak, not an average), plus the
  average of `placement_samples.mem_bytes` over the window.

For A above, 1,080 CPU seconds against 2 CPU × 1,800 s is 30%, and a
3 GiB peak against 8 GiB is 38%. The API returns these as
`efficiency.cpu` and `efficiency.memoryPeak` / `memoryAvg`.

## 3. Prices

### Sources

- **EC2 on-demand**: the AWS Pricing API, `GetProducts` on service
  `AmazonEC2`, filtered by `instanceType`, `regionCode`,
  `operatingSystem=Linux`, `tenancy=Shared`, `preInstalledSw=NA` and
  `capacitystatus=Used`. The Pricing API is only served in a few regions,
  such as `us-east-1`, whatever region the host is in.
- **EC2 spot**: `DescribeSpotPriceHistory` for the host's availability
  zone and instance type, product `Linux/UNIX`, from the host's start to
  now. Each price change inside a host's window becomes one rate period.
- **List prices only.** Savings Plans, Reserved Instances, EDP or other
  discounts, credits, and tax appear only in the provider's bill. lux
  shows list-price costs and says so in the UI and the API
  (`"basis": "list"`). Storage and network (EBS, S3, data transfer) are not
  included either. A cost plugin could report them.

**IAM** luxd needs on top of what it has today (`ec2:RunInstances`,
`ec2:TerminateInstances`, `ec2:DescribeInstances`, `ec2:CreateTags`):

- `pricing:GetProducts`
- `ec2:DescribeSpotPriceHistory`

Both are read-only and cannot be scoped to a resource (`Resource: "*"`).
The endpoint overrides that already exist for tests (`LUX_EC2_ENDPOINT`)
need a twin for pricing (`LUX_PRICING_ENDPOINT`), so the fake EC2 in the
test suite can serve prices.

### What each host stores

New host columns, written at launch from the `RunInstances` reply
(**built**: migration `018_host_facts.sql`; `Provider.Launch` returns a
`server.Launched` with the id, the reply's `instanceType` and
`placement.availabilityZone`, and the market from the template's `spot`;
`launch()` in `provisioner.go` stores them; an empty value is stored as
NULL, as are all three on self-registered and pre-018 hosts; `GET
/v1/hosts` and `GET /v1/hosts/{id}` return them as `instanceType`, `zone`
and `market`, omitted when NULL, to whoever sees the host):

```sql
ALTER TABLE hosts ADD COLUMN instance_type text;   -- from the reply, not the template
ALTER TABLE hosts ADD COLUMN zone text;            -- Placement.AvailabilityZone
ALTER TABLE hosts ADD COLUMN market text CHECK (market IN ('on-demand', 'spot'));
```

Rate periods for each host. A new period starts when the price changes
(spot) or when the advertised capacity changes on a re-hello:

```sql
CREATE TABLE host_rates (
  host_id      text NOT NULL REFERENCES hosts(id),
  valid_from   timestamptz NOT NULL,
  valid_to     timestamptz,             -- NULL: still current
  per_hour     numeric(24, 9) NOT NULL,
  currency     text NOT NULL,
  cap_cpus     float8 NOT NULL,         -- hosts.capacity at the time
  cap_memory   bigint NOT NULL,
  source       text NOT NULL,           -- 'aws-pricing', 'aws-spot-history', 'static', ...
  PRIMARY KEY (host_id, valid_from)
);  -- system_only
```

**Built**: this table, in migration `021_host_rates.sql`, with a unique
index allowing one open period per host. Only static prices write it so
far (section 2).

**Past costs never change.** A period is closed (`valid_to` set) and never
updated again. A later price fetch opens a new period from that moment and
does not touch old ones. When spot history for a past interval arrives
late (the API can lag), luxd may **only fill a gap** that has no period
yet. It never overwrites an existing one. If no price is known for part of
a host's window, the Run's compute line is `incomplete` for that part
(`details.missingRate: true`), rather than priced at zero.

**Billed window** of an EC2 host: from `provision_requested_at` to
`terminated_at` (or now). AWS bills from when the instance enters
`running` until it stops, so this over-counts by the launch latency and
luxd's termination lag, typically seconds. Recording the instance's own
`LaunchTime` (already in `DescribeInstances`, which the provisioner calls
every `provider_check_every`) would tighten the start. See open question 5.

### Caching

- On-demand prices are cached per (region, instance type, OS) in a
  `price_cache` table (system_only, with `fetched_at`) and refreshed after
  `costs.prices.refresh` (default 24h). A new host copies the cached rate
  into its first `host_rates` row, so a launch never waits for the Pricing
  API. If the cache is empty, luxd fetches then and there. If the fetch
  fails, the host has no period and its compute is `incomplete` until one
  succeeds.
- Spot history is fetched on the cost tick for each (zone, instance type)
  with a live spot host, covering since the last period's start. That is
  one call per pair per tick, which is small.

## 4. Cost plugins

A cost plugin is an HTTP service named in luxd's config. lux never knows
what is behind it.

### Describe

`GET {url}/v1/describe`. luxd calls it at start and every
`costs.describe_every` (1h), and caches the answer in memory.

```json
{
  "protocol": [1],
  "name": "model-gateway",
  "families": {
    "ai":    {"displayName": "AI models", "color": "violet"},
    "video": {"displayName": "Video",     "color": "amber"}
  },
  "maxBatch": 500,
  "settle": ["15m", "2h"]
}
```

- `protocol` lists the versions the plugin speaks. luxd uses the highest
  one it shares, and marks the plugin unusable if they share none.
- `color` is a hint. The console maps it to the nearest token of its
  palette and never uses raw colour values.
- `maxBatch` and `settle` are optional. luxd uses the smaller of
  `maxBatch` and its own limit, and the plugin's `settle` if the config
  sets none for that plugin.

Families that several plugins describe differently: first plugin in the
config wins, logged once.

### Report

`POST {url}/v1/costs` with a batch of Runs:

```json
{
  "protocol": 1,
  "requestId": "cq_01J…",
  "sentAt": "2026-09-26T10:42:00Z",
  "runs": [
    {
      "runId": "run_01J…",
      "tenantId": "ten_01H…",
      "tenantName": "acme",
      "labels": {"team": "payments"},
      "adapter": "claude-code",
      "state": "running",
      "terminal": false,
      "window": {"from": "2026-09-26T10:00:00Z", "to": null},
      "placements": [
        {"epoch": 1, "from": "2026-09-26T10:00:00Z", "to": "2026-09-26T10:20:11Z"},
        {"epoch": 2, "from": "2026-09-26T10:21:30Z", "to": null}
      ],
      "sessions": [
        {"id": "5f1c…", "epoch": 1, "firstSeen": "2026-09-26T10:00:40Z", "lastSeen": "2026-09-26T10:20:05Z"},
        {"id": "5f1c…", "epoch": 2, "firstSeen": "2026-09-26T10:21:55Z", "lastSeen": "2026-09-26T10:41:58Z"}
      ]
    }
  ]
}
```

- `window.from` is the Run's `created_at`, and `to` is `finished_at` or
  null while it may still run. `placements` are the per-epoch windows from
  section 2. A plugin picks whichever it needs.
- `sessions` lists **every top-level session id** the Run has had, per
  epoch (section 6). Child or sub-agent sessions are the plugin's job: it
  resolves them from the parent session. lux sends only the top-level ids
  the adapters report.
- `terminal` is true once the Run is `succeeded`, `failed` or `cancelled`.
  A `failed` Run can still be resumed, and then it goes back to `false`.

Response, `200`:

```json
{
  "protocol": 1,
  "runs": [
    {
      "runId": "run_01J…",
      "status": "ok",
      "final": false,
      "lines": [
        {"family": "ai", "item": "large-model-v3", "amount": "1.284310", "currency": "USD",
         "from": "2026-09-26T10:00:40Z", "to": "2026-09-26T10:41:58Z",
         "details": {"inputTokens": 812345, "outputTokens": 40211}}
      ]
    },
    {"runId": "run_01K…", "status": "error", "error": "upstream ledger timeout", "retryAfter": "30s"}
  ]
}
```

- `status: "ok"` with `lines: []` means "nothing for this Run". It is a
  complete answer and clears any earlier lines from this source.
- `final: true` means "these lines will not change". It lets a Run become
  final before the settle retries run out (section 5).
- `amount` is a decimal **string**, up to 9 fractional digits. Negative
  amounts (refunds, credits) are allowed.
- Per-Run `status: "error"` marks only that Run incomplete for this
  source. Its previous lines stay, and it is retried after `retryAfter`
  or with backoff.
- A Run in the request but missing from the response counts as an error
  for that Run. A `runId` that was not in the request is ignored and
  logged.

### Transport rules

| | |
| --- | --- |
| Auth | Optional `Authorization: Bearer <token>`. The token comes from `token_file` or an env var named in the config, never inline in TOML. Plain `http://` is allowed only to loopback or private addresses unless `insecure = true`. |
| Timeout | `timeout` per request (default 30s). Connect timeout 5s. |
| Batch size | At most `max_batch` Runs per request (default 200, capped by describe's `maxBatch`). Larger batches are split into chunks, sent one after another per plugin. Plugins are called in parallel with each other. |
| Response size | 16 MiB maximum. Anything larger is an error for the whole chunk. |
| Idempotency | A request is a read. The same Runs may be asked about any number of times, and the answer replaces the previous one. `requestId` is for logs only. Plugins must be safe to call again at any time. |
| Transport errors | Connection failure, timeout, 5xx, 429, or a body that doesn't parse: the whole chunk is incomplete for this source, retried with exponential backoff (`backoff` 10s doubling to `backoff_max` 10m, with jitter, honouring `Retry-After`). The plugin's state (`ok`, `failing since …`, last error) is shown to operators. |
| 4xx other than 429 | A configuration fault (such as bad auth). Handled as above, but logged at error level and backed off at `backoff_max` straight away. |
| Versioning | The path carries the major version (`/v1/`). `protocol` in the body carries the version number. New optional fields may be added to either side without a bump. Unknown fields are ignored on both sides. |

A plugin that is down never blocks anything. Its Runs show
`incomplete: [model-gateway]` and keep their last known lines.

## 5. Cadence and lifecycle triggers

### Requirements

- Costs are computed every **2 minutes** for every active Run. Each plugin
  gets **one batch** (split into chunks) per tick.
- They are also computed **right away** whenever a Run leaves `running`.
- A state change never waits for a plugin.
- The queue survives a luxd restart.
- Several luxd instances never run the same tick twice.

### One durable queue, two producers

```sql
CREATE TABLE cost_pending (
  run_id      text PRIMARY KEY REFERENCES runs(id),
  due_at      timestamptz NOT NULL,
  reason      text NOT NULL,          -- 'tick' | 'state:<state>' | 'settle' | 'retry'
  claimed_by  text,                   -- instanceID of the luxd working it
  claimed_until timestamptz
);  -- system_only
CREATE INDEX cost_pending_due ON cost_pending (due_at);
```

The row is keyed by Run, so any number of triggers for one Run merge into
one evaluation (`ON CONFLICT (run_id) DO UPDATE SET due_at =
least(cost_pending.due_at, EXCLUDED.due_at)`).

**Lifecycle trigger.** `setRunState` adds one statement to the transaction
it already runs: when the new state is `stopping`, `stopped`, `lost`,
`succeeded`, `failed` or `cancelled`, it upserts `cost_pending` with
`due_at = now()`. The new row commits with the state change, or not at all
if the change rolls back. It is one index write, and no HTTP call ever
happens in that transaction. `stopping` is included so a stop's costs show
up right away. The following `stopped` or terminal state queues the Run
again, and that evaluation includes the placement's real `ended_at`.
Entering one of these states from somewhere other than `running` (for
example `submitted → cancelled`) queues the Run too. That is cheap, and it
lets a plugin report costs from before a placement.

**Tick.** Every `costs.every` (2m), each luxd tries to claim the tick:

```sql
INSERT INTO cost_ticks (tick_at) VALUES (to_timestamp(floor(extract(epoch FROM now()) / 120) * 120))
ON CONFLICT DO NOTHING RETURNING tick_at;
```

Only the instance whose insert returns a row runs that tick. It is exact
(one winner per 2-minute bucket), it survives restarts, it holds no
connection, and a crashed winner costs one tick at most. Old
`cost_ticks` rows are deleted after a day. The existing `leases` pattern
(as `provisionLease`) would also work. The tick-row approach is proposed
because it needs no renewal and cannot race around the lease's expiry.
Session-level `pg_advisory_lock` is not used, because it ties the singleton
to one pooled connection.

The winner doesn't call anything itself. It upserts `cost_pending`
(`reason = 'tick'`, `due_at = now()`) for every **active** Run:

- every Run in `scheduled`, `starting`, `running` or `stopping`
  (`live()`);
- every Run still settling (below);
- every Run whose sources are incomplete, once its backoff has passed.

That is one `INSERT … SELECT`.

**Drainer.** Every luxd runs one. It wakes every `costs.drain_every` (2s)
and also on the existing `lux_events` notification, because a state change
writes a `state` event. It claims due rows:

```sql
UPDATE cost_pending SET claimed_by = $me, claimed_until = now() + interval '2 minutes'
WHERE run_id IN (SELECT run_id FROM cost_pending
                 WHERE due_at <= now() AND (claimed_until IS NULL OR claimed_until < now())
                 ORDER BY due_at LIMIT $batch FOR UPDATE SKIP LOCKED)
RETURNING run_id;
```

The claiming transaction commits at once. No row lock is held across HTTP.
`SKIP LOCKED` and the claim expiry let several instances share the work
without taking the same Run twice. A luxd that dies mid-batch leaves claims
that expire and are picked up again.

For the claimed Runs, the drainer:

1. reads the Runs, their placements, sessions and host rates in one
   read-only transaction;
2. computes the `compute` lines in memory;
3. sends each plugin one request with every claimed Run (split into
   `max_batch` chunks), all plugins in parallel, each bounded by its
   `timeout`;
4. writes, for each (source, Run) that answered, the replaced lines and
   the source state (below). For each that didn't, only the source state.
   These are short transactions, one per chunk;
5. deletes the Runs' `cost_pending` rows, or moves `due_at` forward for
   retries and settle attempts.

Because several state changes in a few seconds merge into one row, and a
drain takes every due row, a burst of 300 Runs stopping (a drain, a scale
down) becomes a couple of chunked requests per plugin, not 300.

**Restart safety.** Everything is in the database: pending rows, claims
(which expire), ticks and source state. A restarted luxd simply keeps
draining. A state change made while every luxd was down doesn't exist,
since state changes are written by luxd.

### Per-source state and finality

```sql
CREATE TABLE cost_sources (
  run_id        text NOT NULL REFERENCES runs(id),
  tenant_id     text NOT NULL,
  source        text NOT NULL,
  status        text NOT NULL CHECK (status IN ('ok', 'incomplete', 'final')),
  answered_at   timestamptz,       -- last successful answer
  attempts      int NOT NULL DEFAULT 0,
  next_at       timestamptz,       -- next retry or settle attempt
  settles_left  int,               -- set when the Run turns terminal
  last_error    text NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, source)
);  -- tenant_rows RLS; last_error is shown only to operators
```

A **terminal** Run becomes final like this:

1. The state change queues it at once. Every source is asked with
   `terminal: true`.
2. After each source's first successful answer, `settles_left` is set to
   that source's `settle` list (default `["10m", "1h"]`), and the next
   attempt is set for `terminal_at + 10m`, then `terminal_at + 1h`.
3. A source is `final` when it answers `final: true`, or when it answers
   the last settle attempt. Failed attempts don't use up a settle slot:
   they back off and retry, up to `costs.settle_give_up` (7 days). After
   that, the source is `incomplete` for good and flagged to operators.
4. `compute` is final once every placement has `ended_at` and every piece
   of the host's billed window that overlaps them has a rate. For spot,
   luxd waits one settle round (spot history can lag).
5. The Run is **final** when every source is final. Lines are then marked
   `final = true`.

A `failed` Run that is **resumed** leaves the terminal state. Its sources
go back to `ok` (not final), and it is active again.

`stopped` and `lost` Runs are not terminal. They are evaluated when they
enter the state and go through the same settle attempts (so late plugin
answers still arrive), but they are never marked final. After that they
stay quiet (no ticks) until resumed.

## 6. Sessions: where the full list comes from

`runs.session_id` keeps only the latest id. Suppose a resume can't load
the old session and the adapter starts a new one. The old id is then gone
from that column, but its costs still belong to the Run.

**Decision: a new `run_sessions` table,** not a scan of `session` events.

```sql
CREATE TABLE run_sessions (
  tenant_id   text NOT NULL REFERENCES tenants(id),
  run_id      text NOT NULL REFERENCES runs(id),
  epoch       int  NOT NULL,
  session_id  text NOT NULL,
  first_seen  timestamptz NOT NULL DEFAULT now(),
  last_seen   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, epoch, session_id)
);
CREATE INDEX run_sessions_session ON run_sessions (tenant_id, session_id);
-- tenant_rows RLS.
```

Why a table and not the events:

- `session` events are incomplete. `applySnapshotDone` sets
  `runs.session_id` from the snapshot manifest and writes no event.
- Reading `run_events` (jsonb, append-only, every event type) for every
  active Run every 2 minutes is a scan that grows with how long each Run
  lives. `run_sessions` is a few rows per Run, indexed.
- Events have no "last seen" and cannot be updated (the app role has no
  `UPDATE` on `run_events`).

**Built** (migrations `017_run_sessions.sql` and
`020_run_sessions_backfill.sql`, `recordSession` in
`internal/server/lifecycle.go`). It is written in the same two places that
write `runs.session_id`: `applyAdapterEvent` (with the placement's epoch)
and `applySnapshotDone` (the manifest's `sessionId`, at the snapshot's
epoch, including a late snapshot of an older epoch, which no longer
changes `runs.session_id`). Each upserts `(run_id, epoch, session_id)` and
sets `last_seen = now()` on a repeat. Session events arrive when an
adapter learns an id, not with heartbeats, so this is not a per-heartbeat
write. Migration 020, a transaction of its own after the table exists,
**backfills** it from `run_events` (`type = 'session'`), from
`snapshots.manifest->>'sessionId'` (first/last seen: the earliest and
latest of those rows), and from `runs.session_id` at `current_epoch` when
neither of those has the id for the Run (last seen: `runs.updated_at`).

The index on `session_id` lets lux **notice** the same session id in two
Runs of one tenant. lux flags it (a warning on both Runs' cost cards) but
does not de-duplicate (open question 1). Within one Run, the same id in
several epochs is normal (a resumed session), and so is an old id coming
back after `resume --from-snapshot` restores an older snapshot. The plugin
receives each (epoch, id) pair and should count each session's usage
once.

## 7. Storage and retention

| Table | Rows | Scope | Kept |
| --- | --- | --- | --- |
| `cost_lines` | per (source, Run, item) | tenant (RLS) | as long as the Run row (Runs are not deleted today) |
| `cost_sources` | per (Run, source) | tenant (RLS) | as long as the Run |
| `cost_pending` | per Run with work due | system | deleted when done |
| `cost_ticks` | per tick | system | 1 day |
| `run_sessions` | per (Run, epoch, session) | tenant (RLS) | as long as the Run |
| `hosts` + 5 columns, `host_rates` | per host / price period | system | as long as the host row |
| `price_cache` | per (region, type, OS) | system | overwritten |
| `cost_hourly` | per hour × (tenant, Run, source, family, currency), plus host rows | tenant rows RLS, host rows system | `costs.hourly` (default 400d, like `history.hours`) |

**Relation to history.** Cost is money, not a sample, so it doesn't go in
`host_samples` or `placement_samples`. Those samples are expired after 48h
raw and 30d at minute resolution, while a Run's cost must last as long as
the Run. Compute cost is computed from `placements` and `host_rates`, which
are never expired, so it can always be recomputed exactly. Heartbeat
samples are used only for the average-memory efficiency figure, and only
while they exist.

**`cost_hourly`** feeds the charts over time. It is kept up to date for
the hours a changed Run's lines cover, in the same transaction that
replaces the lines. Compute rows are split exactly by hour (the pieces
from section 2). A plugin line is spread evenly over its `from`–`to` hours,
because the protocol gives one amount per item, not a time series. So
plugin charts are approximate in time and exact in total. Host rows hold
`allocated` and `unallocated` per host per hour, and are written by the
tick for every host with a billed window in the hour.

## 8. API and visibility

Visibility follows the existing rules (RLS and `principal`):

- A **tenant key** sees its own Runs' lines, totals, sources' status
  (without `last_error` text), and its own hosts' allocation.
- **Unallocated** compute, `host_rates`, platform hosts' cost, plugin
  health, and anything not tied to a Run are **operators only**. An
  operator's `?tenant=` sees what that tenant would see, as with
  `/v1/history`.

### `GET /v1/runs/{id}/cost`

**Built** (`runCost` in `internal/server/costs.go`, `read` scope, 404 for
another tenant's Run as on every Run endpoint; an operator reaches any
Run). What it returns today:

```json
{
  "runId": "run_01J…",
  "status": "incomplete",
  "final": false,
  "basis": "list",
  "totals": [{"currency": "USD", "amount": "1.4342", "final": "0.1499", "estimate": "1.2843"}],
  "byFamily": [
    {"family": "ai", "currency": "USD", "amount": "1.2843", "final": "0", "estimate": "1.2843"},
    {"family": "compute", "currency": "USD", "amount": "0.1499", "final": "0.1499", "estimate": "0"}
  ],
  "lines": [ /* cost lines as in section 1, plus reportedAt */ ],
  "sources": [
    {"source": "compute", "status": "ok", "answeredAt": "…"},
    {"source": "model-gateway", "status": "incomplete", "answeredAt": "…", "nextAt": "…"}
  ]
}
```

- Every total carries its `final` and `estimate` parts (they add up to
  `amount`), per currency, and per family and currency.
- `status` adds `pending`: no source has reported yet (for example, a
  Run that just started, before the first cost tick). It means no cost has
  been reported yet, not that the Run costs nothing. Lines with no
  `cost_sources` row count as `complete`, until a producer writes them.
- `last_error` is never returned here, to anyone. Operators get it with
  plugin health (step 7).

Still to come, in the steps that produce them: `displayName` and `color`
on `byFamily` (from a plugin's describe, step 7), `efficiency` (step 4)
and `warnings` about shared session ids. The planned full response:

```json
{
  "runId": "run_01J…",
  "status": "incomplete",
  "final": false,
  "basis": "list",
  "totals": [{"currency": "USD", "amount": "1.4342"}],
  "byFamily": [
    {"family": "ai", "displayName": "AI models", "color": "violet", "currency": "USD", "amount": "1.2843"},
    {"family": "compute", "displayName": "Compute", "currency": "USD", "amount": "0.1499"}
  ],
  "lines": [ /* cost lines as in section 1 */ ],
  "sources": [
    {"source": "compute", "status": "ok", "answeredAt": "…"},
    {"source": "model-gateway", "status": "incomplete", "answeredAt": "…", "nextAt": "…"}
  ],
  "efficiency": {"cpu": 0.30, "memoryPeak": 0.38, "memoryAvg": 0.22},
  "warnings": ["session 5f1c… also appears in run_01K…"]
}
```

### `GET /v1/costs`

This is the summary endpoint. It takes the range parameters that
`HistoryQuery` already has (`since`, `from`, `to`, plus `tenant` for
operators), and also:

- `group`: `tenant` (operators), `pool`, `host`, `family`, `run`, or
  `label:<key>`, repeatable for two levels;
- `family`: filter by family;
- `interval`: `hour` or `day`, to return a series instead of totals.

Amounts in a range come from `cost_hourly`. Grouping by `pool` or `host`
applies only to compute lines (the only ones tied to a host). Other
families appear under `"(none)"`.

For an operator without `tenant`, the response also has
`unallocated: [{currency, amount}]` and, grouped by `host`, each host's
allocated vs unallocated. Tenants never get these fields.

### `GET /v1/hosts/{id}/cost`

This returns a host's allocated and unallocated cost per hour over a range,
plus its rate periods. It follows the same rule as `hostHistory`: operators,
and the host's own tenant (for its own host). A platform host's cost is the
operators'.

### Runs list

`GET /v1/runs` gains `cost: {totals, status}` on each Run, read from
`cost_lines` by an aggregate over the page's Run ids. It is not stored on
`runs`.

### CLI

- `lux cost <run>`: the breakdown.
- `lux costs [--since 7d] [--by tenant|pool|host|family|label:K]`.
- `lux ls` gets a COST column, shown for one currency or marked `multi`.

## 9. Console

All of these use existing design-system components (`Card`, `KeyValue`,
`Table`, `Badge`, `StatTile`, `TimeSeriesChart`, `TimeRangePicker`,
`Tooltip`, `EmptyState`). Family colours come from the describe hint,
mapped to theme tokens.

- **Run page, Cost card** (next to `RunResources`):
  - the total per currency, with a Badge for `estimate`, `final` or
    `incomplete` (the tooltip names the sources);
  - one row per family;
  - a Table of lines by item;
  - reserved vs used for compute: CPU and memory efficiency.
- **Runs list:** a Cost column showing the total, or `—` while nothing has
  been reported.
- **Host page**: allocated vs unallocated over time, as a stacked
  `TimeSeriesChart` from `/v1/hosts/{id}/cost`, plus the rate periods
  (on-demand or spot, with the price) in a `KeyValue`. Only for those who
  can see the host's history.
- **Overview**:
  - cost over time, stacked by family, over the page's range;
  - Tables of top tenants (operators) and top values of a chosen label;
  - for an operator viewing all tenants, an **Unallocated** tile and a
    plugin health row (last answer, failing since).
- Every money figure is labelled "list price" once per page, in a
  Tooltip.

## 10. Config

This follows `docs/luxd.example.toml`: TOML keys, each with a `LUX_*`
environment variable.

```toml
[costs]
enabled = true                     # LUX_COSTS: off stops ticks, drains and price fetches
every = "2m"                       # LUX_COSTS_EVERY: the active-Run tick
drain_every = "2s"                 # LUX_COSTS_DRAIN_EVERY: pending-queue poll (also woken by lux_events)
batch = 1000                       # LUX_COSTS_BATCH: Runs one drain claims
settle = ["10m", "1h"]             # LUX_COSTS_SETTLE: re-asks after terminal before final (per plugin overridable)
settle_give_up = "168h"            # LUX_COSTS_SETTLE_GIVE_UP
backoff = "10s"                    # LUX_COSTS_BACKOFF
backoff_max = "10m"                # LUX_COSTS_BACKOFF_MAX
hourly = "9600h"                   # LUX_COSTS_HOURLY: cost_hourly retention

[costs.compute]
ec2 = true                         # LUX_COSTS_COMPUTE_EC2
prices_refresh = "24h"             # LUX_COSTS_PRICES_REFRESH: on-demand price cache
pricing_region = "us-east-1"       # LUX_COSTS_PRICING_REGION: where the Pricing API is called
pricing_endpoint = ""              # LUX_PRICING_ENDPOINT (tests)

[[costs.plugin]]                   # LUX_COSTS_PLUGINS: the same list as JSON
name = "model-gateway"             # its lines' source; must not be "compute"
url = "https://costs.internal:8443"
token_file = "/etc/lux/model-gateway.token"   # or token_env = "LUX_COSTS_MODEL_GATEWAY_TOKEN"
timeout = "30s"
max_batch = 200
settle = ["15m", "2h"]             # optional; else describe's, else costs.settle
insecure = false                   # allow plain http to a public address
```

Renaming a plugin starts a new source. The old name's lines stay, and
operators can delete them with `luxd admin costs forget-source <name>`
(which needs a new admin command).

## 11. Open questions

1. **One session in two Runs.** lux has no fork or clone of a Run today.
   A session moves between epochs of *one* Run, including
   `resume --from-snapshot`, which can bring back an older id. But a
   workload could pick up another Run's transcript (for example, from a
   shared state volume or a repo). Proposal: the plugin is the one that
   can see a session's usage, so it de-duplicates, and lux only warns when
   the same id shows up in two Runs of a tenant. Should lux refuse the
   second Run's lines for that session instead? And if a fork feature is
   added later, which Run should the shared prefix be charged to?
2. **How late may a plugin settle?** The proposed default is re-asking at
   +10m and +1h after terminal, plus `final: true` to end early, and giving
   up after 7 days. Is 1h enough for the upstreams you have in mind, or
   should the default be longer (such as +24h)?
3. **Key without family.** The key is (source, run, item), so one source
   can't report the same item under two families for a Run. Is that
   intended, or should `family` be part of the key?
4. **What `stopping` charges.** The compute window ends at placement
   `ended_at`, so the stop grace and snapshot time are charged to the Run.
   Should snapshot upload after exit (`uploadedAt`) also count? The host
   is still busy, but the reservation is released.
5. **Billed window of an EC2 host.** Is `provision_requested_at` to
   `terminated_at` good enough, or should luxd store the instance's own
   `LaunchTime` and its termination time from `DescribeInstances`?
6. **Quotas.** Costs are shown, not enforced. Should a tenant spending cap
   be a later step?
7. **Run deletion.** Runs are never deleted today. If they ever are,
   should their cost lines move to a per-tenant rollup, so totals survive?
8. **Multiple currencies from one plugin** are allowed. Should the Runs
   list column prefer a configured display currency (without
   converting), and just mark the others?
9. **Plugin list via environment.** The other settings map one-to-one to
   `LUX_*` variables. Is a JSON `LUX_COSTS_PLUGINS` acceptable for the
   array?

Decided:

- **`max(1, S)` scaling** is accepted. It only applies when the sum of
  each Run's max(cpu share, memory share) goes above 1, which the
  scheduler allows: on a 4 CPU / 16 GB host, Runs of 3 CPU / 4 GB and
  1 CPU / 12 GB both fit, and each has share 0.75.
- **Static pools:** each static host has a flat hourly price, stored in
  the database (`hosts.hourly_price`, `price_currency`), not luxd's config.
  It is set per host through the API (`PUT /v1/hosts/{id}/price`), or
  copied at first registration from the pool's default (`hourlyPrice`,
  `currency` on a static pool). Built in step 4 (section 2).
- **Host facts** (`instanceType`, `zone`, `market`) are in the host API
  now (section 3).

## 12. Build order

Each step can be reviewed and shipped on its own.

1. **`run_sessions`**: migration with backfill, and writes in
   `applyAdapterEvent` and `applySnapshotDone`. Test: a resume that starts
   a new session keeps both ids, and a snapshot-only id is recorded.
   Useful even without costs.
2. **Host facts at launch**: `Launch` returns the instance type and zone,
   stored in the new host columns, with `market` taken from the template.
   The fake EC2 returns them.
3. **Cost line storage and read API**: the `cost_lines` and `cost_sources`
   tables with RLS, `GET /v1/runs/{id}/cost`, and totals and families.
   Tested by inserting lines in fixtures and asserting on the totals
   returned, including one tenant not seeing another's lines.
4. **Compute cost, static rates first**: `host_rates`, the piecewise
   formula as a pure function, and unallocated. Table-driven tests with the
   worked example above (both variants), asserting allocated + unallocated
   = host cost. **Built**: migration `021_host_rates.sql`,
   `computeCost` and `loadHostCompute` (`internal/server/compute.go`),
   static prices per host and per pool with their periods
   (`internal/server/prices.go`, section 2). No cost line is written yet.
5. **The queue**: `cost_pending`, `cost_ticks`, the tick, the drainer, and
   the `setRunState` enqueue, with compute as the only source. Tests: a
   state change queues in the same transaction (rolled back means nothing
   queued), two luxd instances produce one tick per bucket, and an expired
   claim is retaken.
6. **EC2 prices**: the Pricing API and spot history (behind the fake
   endpoint), `price_cache`, and closed periods that are never rewritten.
   Docs: the IAM permissions in `operations.md`.
7. **Plugins**: config, describe, the report protocol, chunks, backoff and
   settle, and finality. Tests against an in-process fake plugin: down,
   slow, duplicate items, missing Runs, `final: true`.
8. **`cost_hourly` and `GET /v1/costs`, `GET /v1/hosts/{id}/cost`**, with
   the visibility rules. Tests: a tenant never gets unallocated, and an
   operator's `?tenant=` doesn't either.
9. **CLI**: `lux cost`, `lux costs`, and a COST column in `lux ls`.
10. **Console**: the Run page Cost card, then the Runs list column, the
    host page, and the Overview.
11. **Docs**: turn this design into `docs/costs.md` as it was actually
    built, and link it from the README.
