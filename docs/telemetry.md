# Telemetry

lux records when things happened and how much they used, in Postgres, for
every Run, placement and host. The API returns all of it, and
`lux get <run> -o json` shows a Run's.

## Runs

| Field | When |
| --- | --- |
| `createdAt` | Requested (`POST /v1/runs`). |
| `firstScheduledAt` | First assigned to a host. |
| `firstStartedAt` | First reached `running`. |
| `finishedAt` | Reached a terminal state. |
| `usage` | Rolled up over placements: peak memory, disk and PIDs (maxima), CPU seconds and network bytes (sums), placement count, and `queueSeconds` (requested → first workload start). |

## Placements

Each stint on a host, in `placements[]`:

| Field | When |
| --- | --- |
| `assignedAt` | luxd chose the host and queued the assignment. |
| `acceptedAt` | The runner acknowledged it. |
| `imageReadyAt` | The image was pulled or built. |
| `volumesRestoredAt` | State volumes were restored (or found locally). |
| `containerStartedAt` | The container started. |
| `workloadStartedAt` | The init script finished and the workload was started. |
| `stopRequestedAt` | A stop, cancel, drain or timeout was requested (`stopReason` says which). |
| `exitedAt` | The container exited. |
| `snapshotDoneAt` | Its state was saved on the host. |
| `uploadedAt` | All its blobs reached S3. |

Resource figures are read from the container's cgroup (v2):

| Field | Source |
| --- | --- |
| `peakMemoryBytes` | `memory.peak`, which the kernel keeps, so short spikes between samples are not missed. |
| `peakPids` | `pids.peak`. |
| `cpuSeconds` | `cpu.stat` `usage_usec`. |
| `peakDiskBytes` | The largest sampled size of the state volumes plus the writable layer. |
| `netRxBytes`, `netTxBytes` | Network I/O of the container's network namespace. |
| `snapshotBytes` | Compressed size of its final snapshot. |

The runner sends these with **every heartbeat** (about every third of the
lease period, 10 s by default) and once more at exit. So a placement whose
host dies still has its last-known values. Values only ever grow: a late or
reordered report cannot lower a peak.

## Hosts

`lux hosts ls -o json`, under `times`:

| Field | When |
| --- | --- |
| `provisionRequested` | luxd asked a provider (EC2) for this host. |
| `provisioned` | The provider reported it running (or, for static hosts, first contact). |
| `registered` | Its runner first said hello. |
| `firstPlacement` | The first container started on it. |
| `lastPlacementEnded` | The most recent placement ended. It is idle since then, and scale-down uses this. |
| `drainRequested` | Draining was requested. |
| `terminateRequested` | luxd asked the provider to terminate it. |
| `terminated` | The provider confirmed. |
| `lost` | It missed heartbeats for a whole lease period (not counting time no luxd could hear it: see `LUX_LEASE` in [operations](operations.md)). |

Plus `lastHeartbeat`, which is updated with every heartbeat.

### Operational state and launch outcome

A host's `state` is operational: what the scheduler, the provisioner, the
reapers, token checks and cost work act on. `terminated` means the host is
gone from all of them, whatever ended it. How luxd's *launch* of a
provisioned host went is recorded apart, in `launch`:

| `launch.outcome` | Meaning |
| --- | --- |
| `requested` | luxd asked the provider; no answer recorded yet. |
| `launched` | The provider started an instance (`finishedAt`: when it answered). The host may since have ended the usual ways; its `stateReason` says which (never registered, lost, gone at the provider, drained). |
| `failed` | The provider refused (`error`: its message). No instance ever existed: the host is `terminated` operationally (its one-use token revoked, as before), but reports no `terminateRequested` or `terminated` time and has no uptime or host cost. The console shows it as "Launch failed", and `GET /v1/hosts?state=launch_failed` lists these (`state=terminated` then lists the others). |
| `abandoned` | No answer was ever recorded (luxd stopped mid-launch) and the row was written off; an instance found later by its tags is an orphan and is terminated. |

Hosts that registered themselves have no `launch`. Migration 040 filled in
history only where it is unambiguous: a terminated provisioned host whose
reason starts `launch failed:` and that never had an instance id or a
registration is `failed` (its reason text is kept); one with an instance id
is `launched`; everything else was left without an outcome.

## Pools over time

Each system sample also writes one row per pool into `pool_samples`
(migration 041), keyed by the pool's id, so a rename keeps a pool's history.
A pool removed (`DELETE /v1/pools/{name}`) and then set again under the same
name by the same owner is the same pool: `POST /v1/pools` revives the
retired row, with its id, so its samples and cost history continue. The pool's own row
(tenant `''`) holds its hosts by state, the capacity of its ready and
draining hosts, what live placements on its hosts hold, its Runs running
and queued, Runs first started and finished in the sample's window, and the
launches it requested and the provider refused. A row per tenant holds
that tenant's part (its Runs, allocation, starts and finishes) and nothing
of the pool's hosts. They roll up and expire with the other samples. The
first rows are written when this ships: an older range is a gap, never an
invented zero (`historyFrom` says where a pool's history starts).

Sizing: one row per key per sample, where the keys are the pools sampled
(live ones, and retired ones with hosts or Runs) plus each (tenant, pool)
pair with Runs or placements on it. At the default resolutions (raw every
10 s, minutes and hours) a pool's history takes about
`keys × (8640 × raw_days + 1440 × minute_days + 24 × hour_days) × 210 B`,
indexes included: 143 keys at 2 days raw, 30 days of minutes and 400 days of
hours is about 10 M rows and 2.1 GB. A pool's own row is written every
sample, idle or not. Each minute's rollup reads only the rows since the
newest bucket already rolled up, not the whole retention.

`GET /v1/pools/{name}/metrics` reads them (with the pool's figures now),
`GET /v1/pools/{name}/cost` reads `cost_hourly` by the pool's id (its Runs'
cost by family per hour, or per day, top Runs, and for operators its host
time, allocated and idle, which is never added to the Runs' cost), and
`GET /v1/pools/stats` is every pool's figures in one read for the Pools
list. A tenant looking at a shared platform pool sees its hosts and
capacity, as `GET /v1/hosts` shows them, and only its own Runs, allocation
and cost: its sample rows, and cost rows under its RLS scope. Money is per
currency and never summed across currencies.

## Lists: sort and pages

`GET /v1/hosts`, `GET /v1/runs`, `GET /v1/pools/{name}/events` and
`GET /v1/hosts/{id}/events` page when asked to (`sort`, `dir`, `limit`, or a cursor): keyset pages over (the sort
column's value, id), in any sortable column's order, with missing values
last in either direction. A response carries `next`, `prev` and `page`
cursors, sent back as `?next=`, `?prev=` and `?at=` (`at` re-reads the page
from its first row, so a refresh never moves a reader to another page).
Values that grow with time (uptime, runtime, placement time) are sorted at
the clock the first page was read at, which the cursors carry. Hosts also
take `offset` for numbered pages and return `total` and `offset`. Without
any of these, each list answers as it always has (hosts: every one, by
name; Runs: newest first with `before` and `limit`; events: by id with
`before`/`after`). For hosts, `limit` or `offset` alone pages too (newest
first); for Runs and events `limit` alone does not, and keeps its unpaged
meaning (Runs: 1 to 1000, out of range ignored; paged: 1 to 200, out of
range a 400). The unpaged-only parameters (`before`, events' `after`), and
`offset` with a cursor, are 400s in a paged request. Events sort by `time`,
`id` or `type`, each from an index, so a page costs the same however many
events there are; there is no sort by an event's data.

**Host totals.** `GET /v1/hosts/summary` is the unfiltered host list's
totals in one read: the live hosts, and the capacity of the ready and
draining ones against what their live placements hold (a tenant: its own),
as a sum over `GET /v1/hosts`' rows gives them.

A Run carries its **placement time**: for each placement, from when the Run
needed a host (it was created, or its previous placement ended, or it was
resumed) until that placement's workload started, summed over its
placements (`placementSeconds` = `placementWaitSeconds`, waiting for a host,
+ `placementStartSeconds`, starting on it). A placement's start ends when
its workload starts, else when luxd saw it running (a runner that does not
report the workload's start), else when it ended. While a Run waits or
starts it counts up (`placing`).

What happened to a host, and to its pool, is also an event log of its own
(`lux hosts events <host>`, `lux pools events <pool>`; see the
[CLI](cli.md#hosts-and-pools)): each event is written in the transaction
of the change it records. Like Run events they are kept as long as their
host or pool.

## Capacity planning

On each provisioner pass an `ec2` pool simulates placing its waiting Runs
(state `provisioning`, oldest first) to decide how many hosts to launch.
The simulation reserves capacity only for sizing; the scheduler places
Runs independently.

**Where new-host capacity comes from.** Only hosts luxd launched for the
same pool and the same tenant, that have registered, and whose launch
template is exactly the pool's current one count as observations, and of
those only the latest 8 by registration time, terminated ones included.
The expected capacity of a new host is, per resource (`cpus`, `memory`,
`disk`, `runs`), the smallest value those 8 reported, and its labels
are those every one of them shares with the same value. A reported `0`
means that resource is unlimited on that host; the expectation is
unlimited only when every observation reported `0`, otherwise the
smallest non-zero value wins. With no observation (a new pool, or a
changed template) the capacity is unknown, which is never taken as
unlimited: luxd launches one host to learn it, and none while a host of
the current template is already starting.

**Probe hosts.** The pool's template names an EC2 launch template, and
the instance type behind its `$Default` version can change without the
pool's template changing. The expectation then still describes the old
instance type until 8 newer hosts register. When a Run fits no expected
new host (a resource or a label), no host of the current template is
starting, and none has registered since the Run entered `provisioning`,
the pool launches one *probe* host (`pool.scale_up` with `probe: true`),
as it does for unknown capacity. Once the probe registers, the Runs that
were waiting are older than it, so it is at most one probe per group of
Runs that began waiting together; if the probe is no larger, those Runs
stay unmet and `pool.scale_blocked` says so.

**Planning order.** Each Run is tried against:

1. ready hosts the scheduler would consider, by id;
2. hosts already starting from the current template (not draining, within
   `LUX_LAUNCH_TIMEOUT`), sized by the expected capacity;
3. new hosts of the expected capacity, added one at a time as needed.

A Run whose prerequisites are unmet (its secrets are not in this luxd's
memory; its snapshot is missing, unavailable, not yet uploaded, or
references blobs it cannot read), or that waits for a chosen host, is
*blocked* and never causes a launch. A Run that fits nowhere, including a
new host, is *unmet*. The pool launches the new hosts of step 3, plus
warm hosts not already covered by unreserved idle or starting hosts,
at least enough for `--min` and never beyond `--max`. Idle ready hosts
the simulation reserved are not drained by scale-down.

**`pool.scale_up`** keeps its earlier fields (`hosts`, `reason`,
`waiting`, `warm`, `min`, `max`, `total`, `idle`, `provisioning`) and
adds the plan; rows written before planning have none of these:

| Field | Meaning |
| --- | --- |
| `ready` | Runs the simulation fitted on ready hosts. |
| `starting` | Runs fitted on hosts already starting. |
| `planned` | Runs fitted on new hosts (the launches' reason). |
| `unmet` | Capacity-eligible Runs no ready, starting or new host fits. |
| `blocked` | Runs excluded before simulation (prerequisites, chosen host). |
| `expected` | New-host `capacity` (`cpus`, `memory` and `disk` in bytes, `runs`; `0` unlimited) and how many `observations` it is from; `null` when unknown. |
| `unknown` | Why `expected` is `null`. |
| `deficits` | Per Run: prerequisite (`stage` `prerequisite`) or new-host (`new_host`) blockers. |
| `exhausted` | Per actual host (`host`, `stage` `ready` or `starting`): the first Run it could not fit and why. |
| `ineligible` | Ready hosts the scheduler skips: `draining`, `no heartbeat`, `heartbeat stale`. |
| `omitted` | Entries left out of `deficits` and `exhausted`, each capped at 8; `ineligible` is capped at 8 too. |
| `probe` | `true` when the one host launched is a probe (above). |

**`pool.scale_blocked`** is written on a pass that launches nothing while
hosts are wanted or Runs stay unmet. `cause` says why: `max` (`--max`
stops the hosts the plan, warm or `--min` wanted), `quota` (the tenant's
host quota is reached) or `no_fit` (unmet Runs that no new host fits). It
is not written while unmet Runs wait for a probe or bootstrap host of the
current template that is still starting: the pass is waiting, not blocked.
It carries `waiting`, `total`, `max`, the same plan fields as
`pool.scale_up` (`ready` through `omitted`) and, for `max` and `quota`,
`wanted`: how many hosts the pool asked for.

It records a state, not each pass. For `max` and `quota`, only `cause` and
`max` distinguish states; changes to queue size, fit or expected capacity
do not write another row. For `no_fit`, identity also includes `unmet`,
`blocked`, `planned`, `expected`, `unknown`, and the deficits' Runs and
blockers. A scale-up ends the state. Evidence values are those of the pass
that wrote the row, not live usage. A state is recorded again whenever its
last record falls outside the latest 32 pool events; more than 32 events
per pass can make it repeat each pass. Clearing demand without a scale-up
does not reset this bounded event lookup, and a folded scale-up retains
its original position in the stream.

A blocker is either a resource, with `resource` (`cpus`, `memory`, `disk`
or `runs`), `requested`, `used`, `capacity` and `available`
(`capacity - used`) in that resource's unit (CPUs, bytes, placements), or
a constraint `reason` (another tenant's host, a removed pool, no nested
containers; a label mismatch is `required labels do not match`, without
the label values).

**`host.capacity_decision`** is the planner's verdict on one actual host,
written only when it differs from that host's previous one: `pool`;
`stage` (`ready` or `starting`; for `idle`, the host's state then:
`ready`, `starting`, `draining` or `lost`); `decision`
`reserved` (it holds simulated Runs and every Run tried on it fit),
`exhausted` (it holds some, and at least one other Run did not fit),
`blocked` (it holds none), `idle` (no waiting Run considered it this
pass, after an earlier decision that was not `idle`: the provisioner keeps
the hosts it decided on in memory, and reads a previous provisioner's
decisions once per pool when it takes over) or `ineligible` (with
`reason`, as above); and `blockers` for `exhausted` and `blocked`: those of
the first Run that did not fit, in queue order. A pass records at most 32
hosts per pool, in host id order starting after the last host the previous
pass recorded and wrapping around, so on a larger pool each host's
decision lands within a few passes (after a luxd restart the rotation
starts again from the first id).

`pool.placement` and `host.placement_assigned` carry the Run's requested
`resources` (`cpus`, `memory` and `disk` in bytes, `pids`).

`lux pools events`, `lux hosts events` and the console render all of these
as one line each, with memory and disk in binary units.

## Events

Every lifecycle change is also an event on its Run (`lux events <run>`).
This includes state changes, inputs and their delivery acknowledgements,
snapshots, image pulls, and volume restores, each with a timestamp and
epoch. Events are append-only: the application's database role cannot
update or delete them. `lux events --all` follows every Run's events at
once (`GET /v1/events`, SSE).

Followers are woken, not polling: each insert into `run_events` notifies
the `lux_events` channel, and every luxd holds one connection listening
to it. The feed and output streams then re-read under their own tenant's
scope (a notification carries nothing), so an event reaches a browser or
`lux logs -f` within milliseconds, whichever luxd wrote it.

## History

The columns above are lifecycle times, peaks and totals. Use over time is
kept as samples, in these tables, at three resolutions (`res`: 0 raw, 60,
3600 seconds):

| Table | Written | What |
| --- | --- | --- |
| `host_samples` | each heartbeat | the host's CPU seconds (counter), memory and disk in use; its live placements and the CPU and memory they asked for; the runner process's own use (below) |
| `placement_samples` | each heartbeat, per live placement | CPU seconds (counter), memory and pids now, disk, network counters |
| `system_samples` | every `LUX_SAMPLE_EVERY` | for the system (tenant `''`) and each tenant with anything live: Runs by state, busy, idle, queued, started and finished since the last sample, time to start p50/p95, hosts by state, capacity, allocated |
| `control_samples`, `control_disk_samples` | with the whole system's sample, at its instant | the control host, the machine luxd runs on: CPU seconds (counter) and cores, memory used and total; its Postgres database's size and connections (read with SQL, so a remote database works too); for each of `LUX_HISTORY_DISK_PATHS` its filesystem's used, free (writable without root) and total bytes; and the luxd process's own use (below) |

lux's own processes, luxd on its control samples and each host's runner on
its host samples, are recorded as `proc_started` (when the process
started), `proc_cpu_seconds` (user and system seconds since then),
`proc_rss`, `proc_peak_rss` (the highest RSS since the previous stored
sample: the kernel's high-water mark, reset at each reading with the peak
kept until a sample is stored, so a spike between samples still shows, and
one in a sample that was lost carries to the next; absent where the process
cannot reset it, e.g. a non-dumpable one, rather than a lifetime peak), `proc_heap` (Go heap objects, live or not yet swept) and
`goroutines`. The runner's are the runner process only, not the podman
and conmon processes it starts; a runner that predates them sends none.
A restart starts the CPU counter again, so a CPU rate is only taken between
two samples of the same start, and a minute or hour bucket keeps its last
reading's counter and start (not the maximum, which may be the previous
process's); RSS, heap and goroutines are averaged, the peak is the
bucket's maximum. The API has the runner's as `runner` on a host's
samples, with the CPU as cores.

Control samples are keyed by the luxd process that took them (`instance`:
a `luxd_...` id new at each start, so no two processes share one, on one
machine or several), with its machine's `hostname` beside it (rows from
before process ids have the hostname as both). `/v1/history` serves them
as `control`, by what each figure is of: `machines`, a series per
hostname (its CPU rate spans luxd restarts; a counter that drops, a
reboot, gives no rate for that point); `postgres`, one series; and
`luxd`, a series per luxd process, so a restart is a new series.

Every minute, luxd rolls complete buckets up into the next resolution
(levels averaged, counters and peaks their maximum, starts and finishes
summed, states the bucket's last) and deletes what is older than that
resolution's retention (`LUX_HISTORY_RAW`, `_MINUTES`, `_HOURS`). The API
(`/v1/history`, `/v1/hosts/{id}/history`, `/v1/runs/{id}/history`) serves
counters as rates. `/v1/history` carries the control host (`control`)
only for an operator key reading the whole system: never for
a tenant key, nor for an operator's `?tenant=`. See
[Operators](operators.md#history).
