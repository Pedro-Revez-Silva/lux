# Run costs (design)

Status: **compute, prices, plugins, hourly views, the CLI and the console
are built**, with the per-placement estimate rules described below. These
are list-price estimates, not invoice-accurate charges.

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
- Any price from a provider, and the money tables beyond the lines, rates
  and queue: `price_cache` and `cost_hourly` do not exist. (Built:
  `cost_lines.amount` and `cost_sources`, section 1; `host_rates` and
  static hosts' prices, sections 2 and 3; `cost_pending`, `cost_ticks` and
  the compute lines, section 5; the per-run list of every session,
  `run_sessions`, section 6.)

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
which the drainer calls for compute (section 5). It refuses a whole
answer that names an item twice, or an amount that is not a decimal
string with at most 15 integer and 9 fractional digits. Amounts come back
with trailing zeros trimmed: `"1.284310"` is returned as `"1.28431"`.

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
  "USD"}`; it answers the price as stored, trailing zeros trimmed as the
  pool API shows a default: `"0.4"`), cleared with `DELETE` on the same
  path, or `lux hosts price <host> --hourly-price 0.40 --currency USD` /
  `--clear`. Whoever may change the host's pool may do it: a tenant for its
  own hosts, an operator for any host (a platform host's price is the
  operators'). A tenant gets 404 for a host it cannot see and 403 for a
  platform host it can. A host its pool's provider launched is refused
  (422): the provider prices it.
- **Pool default:** a `static` pool may carry `hourlyPrice` (a decimal
  string) and `currency` in the Pool API body (`lux pools set
  --hourly-price --currency`, `luxd admin create-pool` with the same
  flags). Like every other field, it is replaced on each `pools set`: one
  without `--hourly-price` removes the default. It is refused for `ec2`
  pools. A host registering into the pool for the first time copies it,
  and only from its own tenant's pool of that name (a platform host: the
  platform's): a tenant host joining a pool name only the platform has
  gets no default price. **Changing the default does not reprice the
  pool's existing hosts**; set those one by one.
- **Periods:** on every hello and every price change, luxd compares the
  host's price and advertised capacity (`cpus`, `memory`) with its open
  `static` period. If either changed, the open period is closed and a new
  one opens at the same instant, with the current capacity and price.
  A priced host's first registration opens its first period; clearing the
  price closes the open period and opens none. **No price, no period:**
    time without a static price is missing for a placement starting then.
  Clearing the price later does not invalidate a rate selected at that
  placement's start. An unpriced placement is not zero-cost and remains
  incomplete until a matching rate can be resolved; setting a price later
 does not create a historical
  static period. A host advertising neither cpus nor memory cannot supply
  a usable rate either. Provider hosts also need a usable rate (section 3).

### Formula

For a placement `p` of Run `r` on host `h`, at any instant `t`, the
host-hour allocation uses:

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
  Scaling by `1/S` when `S > 1` keeps host-hour allocated + unallocated
  equal to the host's cost. A placement estimate uses the same occupancy
  share at its evaluation, but freezes when that placement ends.
- The host-hour integral uses rate-period boundaries and placement windows,
  without heartbeat sampling. Run placement snapshots instead use one
  selected rate for the entire placement (see below).

**Built** (`internal/server/compute.go`): `computeCost`, a pure function
over one host's rate periods, billed window and placements (anything still
open ends at the `now` it is given), returns each placement's amount per
currency, the host's unallocated amount, and every piece with its `S`.
Money is exact rational arithmetic (`math/big.Rat`), never float64; an
amount is rounded to 9 fractional digits only when turned into a string. A
share uses the capacity of the **rate period** (`cap_cpus`, `cap_memory`),
so a capacity change is a period boundary like a price change. A piece of
the billed window with no period, or with a period with neither cpus nor
memory, is returned as **missing**, for the host and for each placement
live in it, never priced at zero. Overlapping periods are refused as an
error. It is one sweep over the cut instants, so its cost grows with the
number of placements and periods, not with their square. `loadHostCompute`
reads a host's `host_rates` and placements overlapping a window
`[from, to)` (a billing hour) into it, never the host's whole history (an
index on `placements (host_id, ended_at)` finds them). It supplies host-hour
reporting; Run compute lines use placement snapshots instead.

For Run compute lines, the drainer uses a **per-placement snapshot**
(`cost_placement_snapshots`, migration `027_cost_placement_snapshots.sql`).
It selects a usable rate covering the placement's `created_at`, preferring
its own host, or the latest matching known rate if none covers that start.
Provider matches require the same provider, instance type and market, plus
zone for spot or region for on-demand; on-demand may also use a matching
cached price. A borrowed rate uses the placement host's capacity, not the
other host's. Static placements use only their own host's static rates.
That selected hourly rate and capacity price the **entire placement window**,
including after rate changes; an open placement is re-estimated on each
cost evaluation. Once it ends with a usable rate, its amount and rate are
frozen independently of the Run and later placements or prices do not
reprice it. A placement without a usable rate has no amount, not a zero
amount: compute stays `incomplete` and terminal Runs retry until a matching
rate is available. A priced placement can freeze even when another placement
on the Run is missing a rate. The Run's compute line becomes final only when
it is terminal and all placements are ended and priced; resuming resets the
Run line's final flag, not its frozen placement amounts. This is an
estimate contract, not reconciliation with provider billing.

**Host-hour reporting is separate:** `computeCost` still integrates host
rate periods and occupancy for allocated and unallocated host-hour rows.
Run compute lines and Run hourly rows instead use placement snapshots; the
latter spread each snapshot amount across its placement hours. Thus Run
amounts need not equal the host-hour allocated totals, especially when a
placement's frozen or borrowed rate differs from the host's later rates.
Within each host-hour piece, **allocated + unallocated = host cost**:

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

The table illustrates the host-hour allocation when the rate is constant.
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

**Built** (`internal/server/costqueue.go`, step 5). A line's `from`–`to`
spans its priced placements (to now while one is live); `details.placements`
holds, per priced placement, `epoch`, `hostId`, `from`, `to` (null while live),
`cpus`, `memory`, `amount`, `share`, `ratePerHour` and `finalized`; `market`
and `zone` appear when the host has them. Missing placements have no amount
and need not appear in a line. Other choices:

- **A static host has no instance type**: its time is the item `static`
  (all of a Run's static hosts in one line). A provider's host whose type
  is not known is `unknown`.
- **Missing rate**: any placement, static or provider, without a usable
  matching rate has no amount; compute is `incomplete` even if no line can
  yet be emitted. Priced placements remain on the line with
  `details.missingRate: true`; gaps are recorded in `last_error` (operators
  only). Missing is never priced at zero.
- **Two currencies for one item** (hosts priced in different currencies):
  one line per currency, the item suffixed `:<currency>`, since the key is
  (source, run, item).

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
  zone and instance type, product `Linux/UNIX`. Refresh takes the latest
  valid observation in the recent lookback for live hosts; a new observation
  changes the current host rate, not historical periods. Run placements
  use the rate selected for their start, not a reconstructed spot history.
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
  PRIMARY KEY (host_id, valid_from),
  CHECK (valid_to IS NULL OR valid_to > valid_from)  -- closed after it opened
);  -- system_only
```

**Built**: this table, in migration `021_host_rates.sql`, with a unique
index allowing one open period per host and a CHECK that a period closes
after it opens. Static and EC2 provider prices write periods.

A provider's first period takes its capacity from the instance type, or
opens at the host's first hello: before then `hosts.capacity` is empty,
and a period with neither cpus nor memory prices nothing (its time is
missing).

**Past host rate periods are not repriced.** A changed current rate closes
its period and opens another. The exception is a terminated provider host
with no rate periods and a pending unpriced placement: recovery creates one
estimated period for its registered-to-terminated window, without claiming
historical spot accuracy. A placement ending with a usable selected
rate freezes its own estimate; a placement with no matching rate remains
incomplete, including on a static host with no price. A later matching
observation may price an incomplete placement, but does not change frozen
amounts or fill historical host-rate gaps.

**Billed window** of an EC2 host: from `provision_requested_at` to
`terminated_at` (or now). AWS bills from when the instance enters
`running` until it stops, so this over-counts by the launch latency and
luxd's termination lag, typically seconds. Recording the instance's own
`LaunchTime` (already in `DescribeInstances`, which the provisioner calls
every `provider_check_every`) would tighten the start. See open question 5.

### Caching

- On-demand prices are cached per (region, instance type, OS) in a
  `price_cache` table (system_only, with `fetched_at`) and refreshed after
  `costs.prices.refresh` (default 24h). Launch does not wait for Pricing.
  A cached price known before the first hello opens a rate at registration;
  otherwise a successful later fetch starts a rate when it becomes known.
  A failure leaves the earlier time incomplete.
- Spot prices are requested for live hosts sharing a (provider, zone,
  instance type) key over a recent 24-hour lookback. Only the latest valid
  observation is applied to each live host. A terminated host with no rate
  periods is retried only while one of its placements has an unpriced snapshot
  (or no snapshot) and its Run's compute source is incomplete. Recovery writes one
  estimated rate from registration (or provision request) to termination,
  using the latest valid observation, **not** a reconstructed history of
  spot transitions. On-demand recovery uses the same single-rate window
  with the current cached or fetched list price. Host-hour rows therefore
  show an estimate for that window, not provider billing; already priced
  periods and frozen placement snapshots are never rewritten.

**Built:** price refresh runs separately from provisioning and the cost
queue, with a 30-second pass deadline. On-demand lookups share a cache key
within each pass; a first host rate cannot start before the cached price
was fetched. Spot refresh uses the latest recent observation without
historical gap repair. A provider's current rate splits on a runner hello
that changes capacity.

**Deferred:** multiple luxd processes can still fetch the same expired
on-demand cache key concurrently. Terminated hosts with existing rate periods
are not historically gap-filled; their incomplete Run estimates can use a
matching rate from another host or the on-demand cache.

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
- Each line's `from`–`to` span must be at most 90 days. The sum of hourly
  buckets touched by all lines in one Run's answer must be at most 2161
  (counting each line separately, including both endpoint hours). A
  report over either limit is rejected for that Run: previous lines and
  hourly rows stay, the source is incomplete and retried, and `final: true`
  does not make it final.
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

1. reads the Runs, their placements and sessions;
2. refreshes unfrozen placement snapshots and rebuilds compute lines and
   Run hourly rows from them;
3. sends each due plugin a request with its claimed Runs (split into
   `max_batch` chunks), bounded by its `timeout`;
4. writes each answered plugin's replaced lines, hours and source state,
   or only its source state on failure;
5. deletes the Runs' `cost_pending` rows. Source retries and plugin settle
   attempts are queued independently.

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
2. Each plugin schedules its own `settle` list after a terminal answer
   (default `["10m", "1h"]`); compute does not use these slots.
3. A plugin source is `final` when it answers `final: true`, or when it
   answers the last settle attempt. Failed attempts don't use up a settle
 slot:
   they back off and retry, up to `costs.settle_give_up` (7 days). After
   that, the source is `incomplete` for good and flagged to operators.
4. `compute` is final when the Run is terminal, every placement has ended
   and each has a frozen, priced snapshot. There is no spot-specific
   24-hour settlement wait. Plugin settlement is independent of compute.
5. The Run is **final** when every source is final. Lines are then marked
   `final = true`.


A `failed` Run that is **resumed** leaves the terminal state. Its sources
go back to `ok` (not final), and it is active again.

`stopped` and `lost` Runs are not terminal. They are evaluated when they
enter the state; plugins may settle late answers, but compute has no
settlement timer. Neither source is marked final until the Run becomes
terminal. After that these Runs stay quiet until resumed.

**Built** (migrations `022_cost_queue.sql` and
`027_cost_placement_snapshots.sql`, `internal/server/costqueue.go`):

- `setRunState` queues through `lux_cost_enqueue(run, reason)`, a
  `SECURITY DEFINER` function: an API stop or cancel runs in the tenant's
  scope, where the system-only `cost_pending` is out of reach. It queues
  only a Run that scope can see. `PUBLIC` may not execute it: only
  `lux_app` (granted with its other privileges when luxd migrates), and
  `reason` is checked against the known triggers. The merge also sets
  `reason` to the latest trigger and **frees any claim**: a drainer's
  result read before the change is then not written (it writes only under
  its own claim), and the Run is evaluated again.
- The tick's bucket is `costs.every` (not a fixed 120 s), and its
  "settling or backing off" Runs are those whose `compute` source is not
  final and whose `next_at` has passed. The drainer runs the tick itself,
  before draining, at the start of each bucket (up to a second after it),
  and again at its next pass if the tick failed. The tick locks those Runs
  first (`FOR KEY SHARE SKIP LOCKED`, in id order), then inserts their
  queue rows in the same order; a Run another transaction holds is
  skipped, as it is changing state, which queues it.
- The drainer wakes on any `lux_events` notification (any Run event, not
  only a state change), then waits 1 s so a burst becomes one claim: at
  most about one claim a second per luxd while events flow. Run metadata
  is read for claimed Runs; compute snapshots and derived lines are written
  under the Run and queue-row locks, in transactions of up to 100 Runs.
  A read that fails frees the claims and moves `due_at` one tick on; so
  does a write that fails, for its own 100 Runs, and the next ones are
  still written. Each write locks
  its Runs' `runs` rows, then their `cost_pending` rows, in id order: the
  order a state change takes them in (it holds the Run when it queues it),
  so the two wait for each other, never deadlock.
- Open placements are re-estimated at each evaluation; ended placements
  with a usable rate freeze their snapshots. Host-hour reporting is
  independently refreshed from host rates and occupancy (section 7).
- Compute's source row is `ok` after a priced answer, `incomplete` if any
  placement (including static) has no usable matching rate, and `final`
  when the Run is terminal and every placement is frozen. Missing placements
  are listed in `last_error`; they are not counted as zero. An incomplete
  terminal Run gets a retry at `costs.every`, doubling up to 1h. A stopped
  or lost Run gets no compute retry until resumed.
- A resume (`requestResume`) sets final sources back to `ok`, clears their
  `next_at` and attempts, and marks the Run's lines estimates again, in
  the resume's transaction. It also queues the Run (`state:resuming`),
  which frees any claim: a drainer's result, read while the Run was still
  finished, is not written.
- Spot compute has no separate settlement clock: a terminal Run becomes
  compute-final as soon as every ended placement has a priced snapshot.
  Later spot observations do not revise frozen placement amounts.

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
| `price_cache` | per (provider, region, type, OS) | system | overwritten |
| `cost_placement_snapshots` | per placement, frozen when ended and priced | tenant (RLS) | as long as the placement |
| `cost_host_refresh`, `cost_host_turn` | host-hour cursor and work priority | system | as long as needed for host-hour refresh |
| `cost_hourly` | per hour × (tenant, Run, source, family, currency), plus host rows | tenant rows RLS, host rows system | `costs.hourly` (default 400d, like `history.hours`) |

**Relation to history.** Cost is money, not a sample, so it doesn't go in
`host_samples` or `placement_samples`. Those samples are expired after 48h
raw and 30d at minute resolution, while a Run's cost must last as long as
the Run. Run compute amounts are retained as per-placement snapshots:
priced ended placements do not change even if host rates or other
placements change. Host-hour allocations can be recomputed from
 `placements` and `host_rates`
but are not a ledger of those frozen Run amounts. Heartbeat samples are
used only for the average-memory efficiency figure, and only while they
exist.

**`cost_hourly`** feeds the charts over time; it is not a permanent ledger.
The tick deletes hours older than `costs.hourly` (default 400d), while
`cost_lines` and final source state remain. A changed Run's hourly rows are
replaced in the same transaction as its lines, but only hours inside that
retention window are written. Run compute rows spread each placement's
frozen or current estimate over its placement hours; host allocated and
unallocated rows are calculated separately from the host's rate periods
and occupancy, so their amounts can differ. A plugin line is spread evenly
over its `from`–`to` hours, because the protocol gives one amount per item,
not a time series.
Plugin charts are approximate in time and exact in total only while all
hours are retained. Host rows hold `allocated` and `unallocated` per host
per hour. A bounded refresh advances through each host's billed window,
including idle hours, and revisits the current open hour; old hours outside
retention are not rebuilt. Host refresh uses a per-host cursor and retries
open hours; when `costs.batch` is one, a durable turn alternates priority
between current-hour and older work so neither starves. Hourly rows for a
Run are produced when that Run is evaluated, including final evaluations;
previously finalized sources are not reconstructed from stored lines.

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
  `cost_sources` row count as `complete` (the drainer writes both).
- `last_error` is never returned here, to anyone. Operators get it with
  plugin health (step 7).

Still to come, in the steps that produce them: `displayName` and `color`
on `byFamily` (from a plugin's describe, step 7), `efficiency` (reserved
vs used, section 2; step 4 did not build it) and `warnings` about shared session ids. The planned full response:

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

Both cost range endpoints use `[from, to)` over whole UTC hour buckets.
An omitted `to` is now; an omitted `from` is one hour before `to` (or
`since` before `to`). A supplied partial-hour `from` rounds down and a
partial-hour `to` rounds up; exact-hour bounds stay exact. The response's
`from` and `to` are these effective bounds, not the raw query parameters.
`from` must precede `to` before rounding, and the effective range must not
exceed 90 days (400 response). The combined number of rows in a summary's
`totals`, `series`, `unallocated` and `hosts`, or a host cost response's
`hours` and `rates`, is limited to 10,000 (413 response).
Summary totals, series, unallocated and host allocations are read from one
repeatable-read snapshot with the request's tenant scope; only an operator
without a tenant scope receives system-wide host allocations.

Amounts in a range come from `cost_hourly`. Grouping by `pool` or `host`
applies only to compute lines (the only ones tied to a host). Other
families appear under `"(none)"`.

For an operator without `tenant`, the response also has
`unallocated: [{currency, amount}]` and, grouped by `host`, each host's
allocated vs unallocated. Tenants never get these fields.

Grouped by `family`, the response has `families: [{family, displayName,
color}]`, resolved as a Run's `byFamily` is (the first usable plugin's
describe; `Compute` for compute), so a family reads the same everywhere.
Grouped by `run`, it has `runs: [{id, name}]` for the Runs in `totals`,
read in the same snapshot, so a list of top Runs needs no call per Run.

### `GET /v1/hosts/{id}/cost`

This returns a host's allocated and unallocated cost per hour over a range,
plus its rate periods. It follows the same rule as `hostHistory`: operators,
and the host's own tenant (for its own host). A platform host's cost is the
operators'.

### Runs list

**Built** (`listRunCosts` in `internal/server/costs.go`). `GET /v1/runs`
carries `cost: {status, totals}` on each Run: `totals` per currency with
their `final` and `estimate` parts, and `status` as `GET /v1/runs/{id}/cost`
derives it (`pending` with no totals when nothing has reported). It is read
by one aggregate over the page's Run ids from `cost_lines` and one read of
`cost_sources`, in the list's own transaction and RLS scope, so a Run's
cost is visible exactly when the Run is. It is not stored on `runs`.
`GET /v1/runs/{id}` does not carry it.

### CLI

**Built** (`internal/cli/costs.go`, `money.go`; [CLI](cli.md#costs)):

- `lux cost <run>`: the status (naming the sources not answered when
  `incomplete`), the total per currency split into final and estimate, one
  row per family (with `displayName` when there is one), the lines grouped
  by family (item, source, amount, currency, from–to, final or estimate),
  and each source's status and `answeredAt`.
- `lux costs [--since 7d | --from T [--to T]] [--by G]... [--family F]
  [--interval hour|day]`: `--by` is `tenant` (operators), `pool`, `host`,
  `family`, `run` or `label:K`, at most twice. An operator without
  `--tenant` also gets the unallocated total and, by `host`, each host's
  allocated and unallocated. luxd's 400 and 413 messages are printed as
  they are.
- `lux ls` has a COST column: the total for one currency, `multi` for
  several, and `—` while `pending`. A leading `~` marks a total that may
  still change: part of it is an estimate, or the status is `incomplete`.

Amounts are shown rounded half-even to 4 decimals (`math/big`, never a
float), trailing zeros trimmed, with the currency code; a non-zero amount
under that is shown as `<0.0001`, and a missing one as `—`, never `0`.
The console rounds the same way (`formatMoney`), with the currency's
symbol (`$0.0421`, `<$0.0001`) and cents kept (`$0.15`).
Each command says "list price" once. `-o json` prints luxd's response
unchanged, with exact amounts. The range and series of `lux costs` are
shown in UTC, like its buckets.

## 9. Console

**Built** (step 10). Every piece is a design-system component
(`packages/design-system`, gallery section "costs"): `Card`, `KeyValue`,
`Table`, `Badge`/`CostStatusBadge`, `StatTile`, a stacked
`TimeSeriesChart`, `Tooltip`, `EmptyState`, `MoneyList`, `ListPriceNote`,
and `familyDisplay` for each family's label and colour (describe
`displayName`, and its `color` hint mapped to a `--chart-N` token; compute
is fixed to slot 1).

- **Run page, Cost card** (the "Resources & cost" tab, above
  `RunResources`), from `GET /v1/runs/{id}/cost`, polled every 15s while
  the Run is active and every minute after:
  - the total per currency, with a Badge for `estimate`, `final` or
    `incomplete` (the tooltip names the sources not yet answered);
  - one row per family, with its estimate part when only part is final;
  - a Table of lines (family, item, source, window, status, amount);
  - pending: an empty state, "No cost reported yet", never `$0.00`.
  - Not built: reserved vs used efficiency (the API has no such field).
- **Runs list:** a Cost column, written against the planned
  `cost: {totals, status}` on `GET /v1/runs`; `—` until luxd sends it.
- **Host page**: allocated vs unallocated per hour, stacked, from
  `/v1/hosts/{id}/cost` (a tenant sees its allocated part only), and, for
  operators, the rate periods in a `KeyValue` (price per hour, and the
  source once: `static`, `on-demand` or `spot`). Shown to those who can see
  the host's history.
- **Overview**, over the page's range (1h reads 6h: costs are hourly):
  - cost per hour (per day at 30d), stacked by family, one chart per
    currency, labelled and coloured as on the Run page;
  - top tenants (operators across tenants) and top Runs, each ranked per
    currency; a Run is shown by name with its id beside it (the summary's
    `runs`);
  - a Cost tile and, for an operator viewing all tenants, an
    **Unallocated** tile.
  - Not built: plugin health (last answer, failing since). No endpoint
    exposes plugin state.
- Every page with money says "list price" once, explained in a Tooltip.
  Amounts are never added across currencies. An amount rounded for display
  shows its exact value in a Tooltip (`Money`).

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
describe_every = "1h"             # LUX_COSTS_DESCRIBE_EVERY
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

**Built** (step 5): `enabled`, `every`, `drain_every` and `batch`.
**Built** (step 7): `settle`, `settle_give_up`, `backoff`,
`backoff_max`, `describe_every`, and `[[costs.plugin]]`, including the JSON
environment override `LUX_COSTS_PLUGINS`. The drainer calls configured plugins
and tracks their retries and settlement. With
`enabled = false` a luxd neither ticks nor drains; state changes still queue
their Runs. `hourly` and the range endpoints are built (step 8).

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
- **A static host without a price** has no usable rate for an unpriced
  placement. It remains missing and compute stays incomplete, not zero.
  A later static price opens a new period rather than backdating one;
  matching rates at the placement start (or the latest known rate when
  none covers it) determine its estimate (section 2).

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
   claim is retaken. **Built**: migration `022_cost_queue.sql`,
   `internal/server/costqueue.go` and the `[costs]` config (sections 2, 5
   and 10); `GET /v1/runs/{id}/cost` shows the compute lines and source.
6. **EC2 prices (built)**: the Pricing API and recent spot lookback
   (behind the fake endpoint), `price_cache`, and current rate periods.
   Historical gap backfill and spot settlement are not performed.
   Docs: the IAM permissions in `operations.md`.
7. **Plugins (built)**: config, describe, the report protocol, chunks,
   backoff and plugin settlement, independent of compute finality.
8. **`cost_hourly` and `GET /v1/costs`, `GET /v1/hosts/{id}/cost`
   (built)**: host-hour refresh is separate from frozen Run placement
   estimates; previously finalized sources are not backfilled from lines.
9. **CLI (built)**: `lux cost`, `lux costs`, and a COST column in
   `lux ls`, with `cost` on `GET /v1/runs`.
10. **Console (built)**: the Run page Cost card, the host page and the
    Overview (section 9), and the Runs list column against the planned
    `GET /v1/runs` field. `GET /v1/costs` gains `families` and `runs` when
    grouped by them. Tested in `tests/suites/test_console.py` against a real
    luxd, with a static host price and a fake cost plugin.
11. **Docs**: turn this design into `docs/costs.md` as it was actually
    built, and link it from the README.
