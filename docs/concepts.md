# Concepts

## Run

A **Run** is one unit of work, from submission until it is terminated. It is
described by an immutable [RunSpec](runspec.md). A Run can survive being
stopped and moved between hosts any number of times.

## Placement and epoch

A **placement** is one stint of a Run on one host. Each placement gets the
next **epoch** (1, 2, 3…). Everything a runner reports about a Run
(status, output, snapshots, uploads) carries its epoch, and luxd rejects
anything from an epoch that is not the Run's current one. This is
**fencing**. It is what makes it safe for a host to come back after luxd has
given up on it: the host is told its placement is stale and stops it, and
nothing it says can overwrite the newer placement's state.

A Run's `epoch` is therefore also how many times it has been placed on a
host (first start, retries, resumes, migrations): the Runs list shows it
as **Placements**. Its **runtime** (`runtimeSeconds` on `GET /v1/runs` and
`GET /v1/runs/{id}`) is the sum over its placements of the time each
spent running, from reaching running (`started_at`) to exiting or being
lost (`ended_at`). A placement still running counts up to the time of the
response, and `runtimeSince` then says when it started, so a client can
keep counting; a placement that never reached running adds nothing.
Queueing, pulling images and restoring volumes before that are not
runtime.

## States

```
submitted → scheduled → starting → running ─┬─▶ succeeded ─┐
    ▲                                        ├─▶ failed ────┤ (resume)
    │                                        │              ▼
    │                        stopping ◀──────┘ (stop, drain, timeout, terminate)
    │                            │
    └── resuming ◀── stopped ◀───┘
                        │
         (host lost while live) ──▶ lost ──(resume)──▶ resuming

any state ──(terminate, or expiry of a resting Run)──▶ terminated (final)
```

| State | Meaning |
| --- | --- |
| `submitted` | Accepted, waiting for a host. |
| `provisioning` | Waiting for a provider to launch a host (EC2 pools). |
| `scheduled` | A host has been chosen and sent the assignment. |
| `starting` | Image ready, volumes restored, container starting, init running. |
| `running` | The workload is running. `activity` says whether an agent is `busy` or `idle` (waiting for input). |
| `stopping` | Asked to wind down. The adapter stops the workload gracefully, then it is killed after the grace period. |
| `stopped` | Exited on request with its state saved. **Resumable.** An idle timeout stops with reason `idle`; resume it with `lux resume [--input]`. A Run stopped by a move (force-evicting drain, spot preemption, migrate) is then resumed by lux at once, unless its spec's `resumePolicy` says otherwise: `restart` starts it again from scratch, `manual` ends it `failed` and `never` ends it `terminated` ([resume policy](runspec.md#resume-policy)). |
| `resuming` | Waiting for a host for its next placement. |
| `succeeded` / `failed` | Ended: its workload exited. **Resumable**, like `stopped` (the adapter's resume path, with its session). A Run whose `resumePolicy` is `never` never rests here: it ends `terminated`, its outcome in `stateReason`. |
| `terminated` | Ended for good: the only terminal state. Never resumed, and its snapshots and output go after the tenant's retention. A Run becomes terminated by `lux terminate` (`POST /v1/runs/{id}/terminate`), or by lux when it expires. |
| `lost` | Its host stopped heartbeating while it was live. Resumable from the last snapshot taken *before* the lost placement. Work since then is gone. Never resumed automatically. A Run whose `resumePolicy` is `never` ends `terminated` instead. |

Every Run the API returns carries `resumable`, always present: whether
`POST /v1/runs/{id}/resume` without `fromSnapshot` would accept it now
(its state is `stopped`, `lost`, `failed` or `succeeded`, its
`resumePolicy` is not `never`, and it is not left with only a refused
snapshot report). It is the same test as `GET /v1/runs?resumable=true`
(`lux ls --resumable`).

A Run that stays `stopped`, `lost`, `failed` or `succeeded` (resting:
resumable, not running) longer than its tenant's `expireAfterDays`
(default 90; 0: never) is **terminated by lux**, with `stateReason`
`expired: stopped for 90 days` (or `lost`, `failed`, `succeeded`), and a
`state` event like any terminate. The clock is the time in that state: a
resume restarts it at the next end. Its storage then follows the
terminated Run's ([below](#which-snapshots-are-kept)): 90 days resting plus
the tenant's retention. Terminate a Run sooner yourself to free it sooner.

`stateReason` explains the current state, for example `exit code 3`,
`waiting for capacity: 2 hosts in its pool lack cpus (requested 4)`, or
`lease expired: host stopped heartbeating`. A Run waiting for a host counts
only hosts of its own pool and tenant (or its chosen host) that the luxd
writing the reason is connected to, per missing resource or constraint,
without host names or their usage; the reason is at most 512 bytes.

### Stages

A Run's **stage** (`stage`, `stageSince` and, while stopping,
`stageReason` on `GET /v1/runs` and `GET /v1/runs/{id}`) says where it
is on its way to running, and since when, finer than its state while it
starts. luxd derives it from the Run and its current placement:

| `stage` | While | `stageSince` | Clock |
| --- | --- | --- | --- |
| `waiting` | No live placement (`submitted`, `provisioning`, `resuming`), or one assigned whose runner has not accepted it. | When the Run last needed a host: since a resume or a move's end, else its previous placement's end, else its creation (where placement time starts counting). An assignment its host refused at delivery, placed again, keeps the wait's start. | luxd |
| `image` | Accepted; pulling or building the image. | `acceptedAt` | luxd |
| `volumes` | Restoring the state volumes. | `imageReadyAt` | runner |
| `repositories` | Cloning repositories and fetching a resume's sync. A spec without repositories passes through it at once. | `volumesRestoredAt` | runner |
| `container` | Creating and starting the container. | `reposReadyAt` | runner |
| `running` | The container started (init, then the workload, run in it: `state` is `starting` until luxd sees the shim up). | `containerStartedAt`; luxd's `started_at` for a runner that reports no container start. | runner (luxd on fallback) |
| `stopping` | A stop was asked of its live placement: the workload winds down, then the state volumes are snapshotted and reported. `stageReason` is the stop's: `stop`, `terminate`, `drain`, `preempt`, `migrate`, `timeout`, `idle`, `disk`. | `stopRequestedAt` | luxd |
| `stopped`, `lost`, `succeeded`, `failed`, `terminated` | Its state, resting. | When it entered that state: its placement's end, or a later change (a terminate, an expiry). | luxd |

`running` starts at `containerStartedAt`, where `container` ends, so the
start stages follow one another with no gap on one clock. A move (`drain`,
`preempt`, `migrate`) is `stopping`, then `waiting` (since its placement
ended) and the start stages on the next host: its placement's end and its
resume are one change, so it is never `stopped`. The runner reports each
start mark as it reaches it, and luxd keeps the first time reported for
each.

Each change of stage is a **`stage`** event on the Run (and the
tenant's feed): `{stage, since, epoch, reason?}`, once per change and in
order, so a client following events need not poll. One change of the Run
announces only the stage it leaves the Run in, and a redelivered or late
report that leaves the stage as it was emits nothing.

## Volumes and snapshots

A spec declares **volumes**, which are named directories in the container:

- `state` volumes are **snapshotted every time the container exits**,
  however it exits, except in a Run whose `resumePolicy` is `never`: its
  snapshot lists no volume ([resume policy](runspec.md#resume-policy)).
  These are the only thing guaranteed to survive a move.
  Put the git checkout and the agent's session transcript here.
- `ephemeral` volumes start empty on every placement.
- `cache` volumes are host-local, shared by the tenant's Runs using the same
  name and workload UID and GID on that host. They are never snapshotted,
  uploaded or restored. A resume on the same host reuses them; a move uses
  the destination host's cache, possibly empty. Contents are best-effort
  and may vanish anytime. Keep no secrets or irreplaceable files here.
  Several Runs may mount one cache at once; lux does not lock it.
- File secrets live on a tmpfs, which is never part of a snapshot.

A **snapshot** is the state volumes as of one exit, plus a manifest. The
runner exports each volume (`podman volume export`, zstd) and keeps the
snapshot locally. It then uploads it to S3 in the background, straight from
the host, with presigned URLs luxd signs for it. Runners never hold S3
credentials.

### Which snapshots are kept

Every snapshot is a full copy of the state volumes, so lux keeps only the
ones a Run can still use:

- **A Run that can resume** (`stopped`, `lost`, `failed`, `succeeded`, or
  on its way back) keeps its **current** snapshot (the one a resume starts
  from), however long it rests, until it is terminated.
  Older snapshots are deleted once the current one has finished uploading;
  until then the older one is the only copy that would survive losing the
  host. `resume --from-snapshot` an older snapshot works while it still
  exists; a deleted one is 409 `snapshot_unavailable`. `lux snapshots`
  keeps listing every snapshot, deleted ones with `available` false.
- **A terminated Run** (an expired Run too) keeps its snapshots and output
  for the tenant's retention (default 30 days) counted from when it was
  terminated (not from when its last placement ended), then they are
  deleted.
- **Artifacts** are never deleted by time: they stay until their owner
  deletes them (`lux artifacts <run> --delete`, a terminated
  Run's only).

**What does not survive a stop:** running processes, memory, open
connections, background servers, and anything outside the state volumes,
including the container's writable layer (`/tmp`, `/run`): every resume,
on the same host or another, starts in a new container. Anything a workload installs at
runtime belongs in the image or in the init script, and the init script runs
on every start, so it must be idempotent.

## Resume

- **Same host, local snapshot still there:** nothing moves: the state
  volumes are already there. The runner starts a new container on them,
  exactly as another host would. The scheduler strongly prefers this host.
- **Another host:** the new runner downloads the snapshot from S3 through a
  short-lived presigned URL, imports the volumes and starts a new
  container. If the snapshot has not finished uploading from its host, the
  scheduler waits for the upload.
- The adapter's **resume** path runs with the stored session id. An agent
  reloads its conversation from its transcript on the restored volume.
- A resume must supply the Run's **secrets** again, because lux never stores
  secret values. This also means a resume can rotate credentials, add
  secrets and remove them ([Secrets](#secrets)).
- A resume can **sync** repositories (`sync: [{repo, ref}]`, `lux resume
  --sync app=main`): their restored checkouts move to the ref's commit
  before init ([the RunSpec](runspec.md#syncing-checkouts)). A running Run
  syncs with `POST /v1/runs/{id}/sync` (`lux sync`).
- Attached servers start again ([Servers](#servers)).
- A resume may **move the Run to another pool**
  ([below](#moving-to-another-pool)). Its state follows, as on any host
  change: nothing in a snapshot is tied to a pool.

### Resizing on resume

A resume can change what a stopped, lost, failed or succeeded Run gets from then on
(`resources: {cpus, memory, disk, diskGrace}` in the request; `lux resume --cpus
--memory --disk --disk-grace`). A running Run's limits never change: stop it first.
What a resume applies is written into the Run's spec, so `GET
/v1/runs/{id}` shows it, the scheduler reserves it, and every later
placement gets it.

- **cpus and memory** apply, larger or smaller. They must be greater than
  0 (422 `invalid_spec` otherwise, as at submit). They shape only the
  container's limits and the reservation; the resume's new container gets
  them, on the same state volumes.
- **disk** larger applies. Smaller applies only if it is at least the
  Run's saved state plus headroom: the peak disk use (writable layer plus
  state volumes) of the placement that took the snapshot it resumes from,
  plus a quarter of that and at least 1 GiB. That peak counts only once
  the placement has reported its exit, whose final sample is taken after
  the snapshot: a placement lost before then (its Run lost) has no final
  measurement. Less than that floor, or with no final measurement, the
  Run keeps its disk and resumes anyway, cpus and memory still applied: a
  smaller limit it is already over would only stop it again
  ([disk is measured](runspec.md#rules)), after its `diskGrace` or at once
  at twice the limit.
- **diskGrace** (how long the Run may stay over its disk) applies as asked,
  `0` to `1h`; outside that is 422 `invalid_spec`. Absent leaves it as it
  is. Whatever it is, the resumed placement starts its grace from zero: time
  over the limit before the stop does not carry over.
- The answer's `resize` and the `resume.requested` event's `resources`
  say what was asked (`requested`), what the Run has now (`applied`),
  and, when a disk was kept, `disk: {requested, kept, reason,
  measuredBytes, neededBytes}`.
- In a [shaped pool](#shaped-pools) a resume resizes to a whole size:
  `resources.size` (`lux resume --size`), or cpus, memory and disk
  rounded up to a size as at submit; `resize.size` names it. Its disk is
  never kept apart from its cpus and memory. A size with less disk than
  the Run has must hold the saved state with the headroom above; when it
  does not, the Run gets the smallest larger size that does, never less
  than asked in any dimension: `resize.raisedFrom` is the size asked for,
  `resize.raisedFor` is `saved state`, with `measuredBytes` and
  `neededBytes`, and the Run's `sizing` keeps `raisedFrom` and
  `raisedFor` until a later resume sizes it. When no size holds the saved
  state, or the placement that took it reported no final measurement (as
  for the unshaped disk rule, nothing then says how large it is), the
  resume is refused with 422 `size_below_saved_state`, naming the largest
  size, nothing changed. `validateOnly` answers the same. `size` on an
  unshaped pool is 422 `size_unsupported`.
- A new size is placed like a submit of that size. The snapshot's host is
  only preferred: if the new size does not fit there, the Run goes to
  another host of its pool that has room, or, in a provisioned pool, a new
  host is asked for; with no host that could fit it, it waits for
  capacity, saying which resource is short.
- A resume of a Run already resuming is a retry of the resume it waits
  on, and its `resources` are compared with what that resume asked for
  (its `requested`, not what was applied):
  - the same `cpus`, `memory`, `disk` and `diskGrace` (each present or absent alike):
    202, with that first resume's `resize`, a disk it kept included;
  - no `resources`, or all of them absent or `disk` 0: 202 without
    `resize`, whatever the first resume asked;
  - anything else, including a request where the first had none: 409
    `not_resumable`. Invalid values (cpus or memory 0 or less, a
    negative disk, a diskGrace outside 0 to 1h) are 422 `invalid_spec` first, as on any resume.
  - A Run lux resumed itself, after a `drain`, `preempt` or `migrate`
    move, counts as a resume that asked for no resources, even when an
    earlier resume of yours resized it. While it is resuming, a resume
    with `resources` gets 409; one without gets 202. With `resumePolicy:
    restart` it is restarted from scratch instead (no snapshot restored);
    with `manual` or `never` it is not placed again: it ends `failed`
    ([resume policy](runspec.md#resume-policy)).

### Moving to another pool

A resume can name another pool: `placement: {poolId}` in the request
(`lux resume --pool <name|id>`), the same field and rule as a submit's
`placement.poolId`. Together with `resources`, it gives a Run a size that
lives in another pool without losing its checkout or its conversation:
snapshots belong to the tenant, not to a pool, and a host in the new pool
downloads them through a presigned URL as any other host would.

The Run is rebound before it is placed: its `pool`/`poolId` (in `GET
/v1/runs/{id}`) and its spec's `placement.pool` and `placement.poolId` are
the new pool's, so its later resumes, retries and migrations stay there.
It is placed only on that pool's hosts (with a `pool.placement` event
there, as for any placement). Its costs follow its placements: each is
priced at its host's rate and counted in its host's pool.

lux checks, before anything changes, with the resources this resume sets.
A refusal is 422 and changes nothing (pool, spec, state, secrets):

- `unknown_pool`: no pool with that id is the tenant's or the platform's,
  or it was removed.
- `platform_mismatch`: the image's platforms, as the Run's last placement
  reported them, share none with the pool's: its hosts' (each runner
  reports its arch) and, for an ec2 pool, its template's instance types
  as the instance-type catalog describes them. An `image.build` is rebuilt
  on each host, so its platform is not known. When lux knows either side
  not at all, it moves the Run and says so: `placement.platformUnchecked:
  true` in the answer and in the event.
- `pool_unfit`: no host the pool has, or launches, could ever take the
  Run: one with `sandbox.nestedContainers` in a pool whose hosts lack
  `lux-runner --nested` (an ec2 template without `nestedContainers`, or
  one whose latest hosts registered without it), one whose
  `placement.requires` labels none of its hosts has, or cpus, memory or
  disk beyond every host it has and the hosts it would launch. For an ec2
  pool those are the planned host or the probe host, never a fallback.
  The planned host is the one the capacity planner expects: the smallest
  of its current template's latest registrations. The probe host is the
  one the planner launches when a Run outgrows that expectation: the
  catalog's size of the template's `instanceType`, with the planned host's
  labels. Before any registration there is only the catalog's size of
  `instanceType`. Fallback types are launched only on a capacity shortage,
  so they do not count; an `instanceType` the catalog lacks gives no probe
  host, and before any registration is not refused on size. Hosts it no
  longer has (terminated) do not count. A static pool no host has
  registered in is not refused.

The answer's `placement` is `{pool, poolId, from: {name, id},
platformUnchecked}`, and the `resume.requested` event carries `pool:
{from, to, platformUnchecked}` when the pool changed. Naming the pool the
Run is in already moves nothing. On a Run already resuming, naming the
pool its resume moved it to is a retry (202, with that resume's
`placement`); any other pool is 409 `not_resumable`.

A move into a [shaped pool](#shaped-pools) sizes the Run there, before
the checks above: what the resume asked for in `resources`, or, when it
asked nothing, the Run's current cpus, memory and disk rounded up to the
destination's sizes (more than its whole host: 422 `size_too_large`); a
size it asked for is raised there for its saved state as above. The
answer's `resize` says which size. A move out of a shaped pool keeps the
Run's resources as they are, and it no longer has a size.

`GET /v1/pools` gives each pool's `platforms` and
`capabilities.nestedContainers` from the same facts, absent or null when
lux does not know them.

### Checking a resume first

`validateOnly: true` (`lux resume --dry-run`) runs every check a resume
would and changes nothing (no state, spec, pool, secrets or events): 200
with `{valid: true, resize, placement}`, or the same error the resume
would get. Secret values may be left out; the names sent are still
checked. A live Run (scheduled, starting, running or stopping) is checked
as if it were stopped, its state given in `stateUnchecked`, so an agent
can ask before it is parked. The tenant's Run quota is not checked for a
live Run, since its own placement counts against it until it stops.

## Secrets

The caller passes secret values with each submit and resume. lux never
stores them:

- luxd keeps values **in memory** only until the Run is placed, and sends
  them to the runner over its connection. The database holds each secret's
  name and a fingerprint, never the value. If luxd restarts before a queued
  Run is placed, the Run stops after a short grace period, with a reason
  saying to resume it with its secrets.
- `env` secrets are environment variables. `file` secrets are written on
  a tmpfs (`/.lux/secrets`, readable only by the workload's user) and
  linked at their `path`, so they are never in a snapshot.
- **Output is redacted** before it is written anywhere. That covers the
  exact value and its common encodings (base64, URL, hex, JSON-escaped),
  overlapping values, and values split across writes or streamed chunks:
  a tail that could start a secret is held back until the rest arrives, a
  turn ends, or 2 seconds pass. A secret whose pieces arrive more than 2
  seconds apart can leak its first part. Values shorter than 4 bytes are
  not redacted.
- **A resume needs every secret again**, and is refused before scheduling
  if one is missing. A resume can supply new values, which rotates them.
- **A resume can add a secret**: a `secrets` entry whose name the Run does
  not have (and that is not the credential of a repository the same resume
  adds) declares it, checked as at submit (`as` env, file or none, env by
  default). From that resume on the workload gets it, output redacts it,
  and every later resume must supply it. A problem with it is 422
  `invalid_spec`, and nothing changes.
- **A resume can remove a secret**: `removeSecrets: [name]` (`lux resume
  --remove-secret`) takes it out of the Run, so the workload no longer has
  it and later resumes no longer need it. Refused (422 `invalid_spec`,
  nothing changed): a name the Run does not have, a git or registry
  credential, a secret valuing an MCP server's or service's header, and a
  name also in the same request's `secrets` or the credential of a
  repository it adds. An operator's resume from the values luxd holds can
  remove, but adding needs values.
- A resume of a Run already resuming changes no secrets: the first
  resume's stand, and a retry is answered 202.
- The `resume.requested` event lists the names as `addedSecrets` and
  `removedSecrets`.
- **Git credentials** are used by the runner only and never enter the
  container (see [the RunSpec](runspec.md#git)).
- **What lux does not do:** it does not scan state volumes. A secret that
  the workload itself writes to a state volume is snapshotted as written.
  This is accepted, and documented here on purpose.

## Output

The tenant's **event feed**, `GET /v1/events` (`lux events --all`), is
every event of its Runs and servers as server-sent events, each with its
id: a client resumes after a disconnect with `Last-Event-ID`. Server
events are listed under [Servers](#servers).

A placement's stdout, stderr and structured events are written by the shim
to a file on the host. Every record has a sequence number and secrets are
redacted before the record is written. While the placement is live, luxd
relays the records from the host. After it exits, the file is uploaded and
served from S3. A **cursor** (`<epoch>.<seq>`) addresses the Run's whole
output across placements, so `lux logs -f` continues seamlessly across a
move.

If a host dies unannounced, the output of its live placement is lost along
with its state. Everything before that placement is safe.

Besides the agent's own messages, an agent Run's structured events include
lux's: its session (`lux.session`), activity (`lux.activity`), each
input's phases (`lux.input`, `lux.input.consumed`, `lux.input.failed`),
warnings (`lux.warning`) and each compaction of the agent's context with
its summary (`lux.compacted`); see [Adapters](adapters.md#compactions), and
[Where the record lands](adapters.md#where-the-record-lands) for its
place relative to the agent's own announcement. With OpenCode, the
subagents its `task` tool runs are reported too (`opencode.session`,
`opencode.task`, `opencode.part`, `opencode.message`,
`opencode.permission`); see [OpenCode subagents](adapters.md#opencode-subagents).

A Run's servers write their output into the same file, as records with
`ch: "server"`, `server: <name>` and `stream: stdout | stderr` (and their
starts and exits as `lux.server` events there). `GET /v1/runs/{id}/output`
leaves them out unless asked: `servers=true` for everything,
`server=<name>` for one server's alone (`lux logs --servers`,
`lux logs --server web`), so a client that knows nothing of servers
never sees them mixed into the workload's output.

## Artifacts

**Artifacts** are the files a Run hands back. A workload publishes one
while it runs by calling `/.lux/bin/lux-shim publish FILE --name NAME
[--description TEXT]` in its container: the shim takes a copy of the
bytes, the runner reports it to luxd and uploads it, and the Run's event
stream gets `artifact.published` once it can be downloaded. The Run keeps
running throughout. Files matching the spec's `artifacts.paths` are
collected at every exit instead. Nothing a workload merely writes to disk
becomes an artifact. Publishing a name again makes a new version; nothing
is overwritten. A host lost before its runner reported a published file
loses it. See [the RunSpec](runspec.md#artifacts).

## Servers

A **server** is a named URL that reaches a port in a Run, optionally with a
command lux runs in the Run's container ([the RunSpec](runspec.md#servers)
has the fields). It is the tenant's own resource (`srv_…`), independent of
Runs: it is **attached** to at most one Run at a time, and outlives Runs if
its lifetime says so. Its process does not outlive a placement: a
placement's end (a stop, a migration, a lost host) stops it, and **every
placement** of its Run (the first, a resume, a migration, a resume after
`lost`) starts every attached server with a command, unless someone
stopped it (its desired state, below).

```
lux server add <run> web 3000 -- npm run dev -- --host 0.0.0.0 --port 3000   # a Run's (lifetime run)
lux server create web 3000 --wake request --hostname web.pr9.<domain> -- npm run dev   # the tenant's
lux server ls [--state asleep] [-l pr=9]       lux server ls <run>
lux server attach srv_… <run>                  lux server detach srv_…
lux server start|stop|restart|rm srv_…  (or <run> <name>)
```

- **Process states** (`GET /v1/runs/{id}/servers`, and `process` on
  `/v1/servers`): `stopped` → (start) `starting` → `ready` once its port
  accepts connections. A `ready` server whose port refuses twice in a row
  is `unreachable` (and `ready` again when it answers). A command that
  ends is `exited`, with its exit code and the last line it wrote to
  stderr (`error`). The runner checks each port every 3 seconds, from the
  host, on the container's address.
- **Server states** (`state` on `/v1/servers`, derived from the process
  and its Run, never stored twice): `ready` (its Run runs, its port
  answers), `waking` (a wake asked for, its Run starting or moving, its
  command starting), `asleep` (wakes on request, nothing serves it),
  `stopped` (does not wake and its Run is not running, or stopped by
  someone), `unreachable`, `exited`, `no answer` (a wake asked for longer
  than `wakeTimeout` ago and no Run came up; the next request asks again).
- **Desired state** is `down` once someone stops it (`stopReason:
  stopped`) until it is started again. Otherwise `up`.
- **Without a command**, only the port is exposed, and lux watches it
  whenever the Run runs: `waking` until it accepts connections, then
  `ready`, whatever it was. There is nothing to start (409 `no_command`);
  stopping one stops the watching until the Run's next placement.
- **`stopReason`** says why one is `stopped`: `stopped` (asked for),
  `run stopped`, `migrated`, `host lost` (its placement ended) or
  `detached`, with `stoppedEpoch`, the placement it stopped in.
- **Attach and detach** work while the Run runs or is stopped: attached to
  a Run on a host, the command starts now; to a stopped one, at its next
  placement. Detaching stops the command and never touches the Run.
  Attaching a server another Run serves is 409 `attached`.
- **Lifetime.** `run` (the default of `lux server add`,
  `POST /v1/runs/{id}/servers` and `workload.servers`): deleted when its
  Run can never run again, `terminated` (a `failed` or `succeeded` Run can
  be resumed: its servers stay). `owner` (every server that wakes on
  request): kept until its owner deletes it, or until `expireAfter`
  (default 30 days) passes without a request (`server.expired`). An owner
  server whose Run is terminated is detached. Deleting a server
  detaches it first; its hostname then answers 404 "This preview is
  gone".
- **Changes:** adding, editing (`PUT` on the Run's, `PATCH` on
  `/v1/servers/{id}`) and removing work in any state of the Run but
  finished. An edit applies at the server's next start. The Run API
  removes only the Run's own servers: `DELETE /v1/runs/{id}/servers/{name}`
  on an owner server is 409 `lifetime_owner`; detach or delete it through
  `/v1/servers/{id}`. A change of a server that is attached or detached
  twice while it runs is refused, 409 `conflict`: retry it.
- **Events** go on the Run's events (when attached) and on the tenant's
  feed, `GET /v1/events` (see [Output](#output)): `server.created`,
  `updated`, `deleted`, `attached`, `detached`, `state`,
  `wake_requested`, `idle`, `expired` (and the Run API's `server.added`,
  `server.removed`). Each carries `serverId` and `runId`, its attached Run
  (null only when attached to none; a Run's own events always have one),
  and in `data` the server's `serverId`, `name`, `host`, `hostname`, `url`,
  `labels` and `runId`. `GET /v1/servers/{id}/events` lists one
  server's, a deleted server's included (its last is `server.deleted` or
  `server.expired`); an id with none is an empty list, never 404.
- **Ports:** `lux port-forward <run> web <local-port>` reaches a server by
  its name, as it reaches `network.ports`.
- **Previews:** with previews configured, each server has a URL,
  `https://<hostname>`, that reaches it from a browser wherever its Run is
  now ([operators](operators.md#previews)). The hostname is the default,
  `<name>-<8 characters of its id>.<domain>` (`web-k3x9ab2c.<domain>`), or,
  for a server created with `POST /v1/servers` (`lux server create
  --hostname`), its owner's choice: any name under the preview domain (any
  number of labels) that no other server of any tenant has
  (`web.t123.p9.<domain>`). It is stable for the server's life and never
  names a Run.

### Waking on request

A branch preview sleeps while nobody looks and wakes when someone does.
lux never starts, resumes or stops a Run for a server: it tells the
server's **owner** (whatever created it: an orchestrator following the
feed) and the owner acts.

1. A signed-in request (never an unauthenticated one) to a server with
   `wake: request` and no running Run serving it gets the **waking page**,
   and lux emits `server.wake_requested` (`by`: the person's email or the
   key's name, `path`) **once per wake**: until the server becomes ready,
   or `wakeTimeout` (default 5 minutes) passes, further requests, tabs and
   the page's own polls ask nothing more.
2. The owner resumes the attached Run, with `sync` to bring the code
   current (see [Resume](#resume)), or submits a Run and attaches the
   server to it.
3. The Run starts (its state restored), the server starts, the page drops
   into the app at the path asked for. No request is held open meanwhile:
   the page polls every 3 seconds.
4. After `idleAfter` (default 10 minutes; 0: never) with no request while
   its Run runs, lux emits `server.idle`, once per idle period (the next
   request starts a new one). Every proxied request counts (pages, assets,
   API calls); an open WebSocket or event stream is one request, however
   long it stays open. The owner stops the Run; its state is snapshotted,
   as on every stop.

`lastRequestAt`, when the server was last requested, is written at most
every `preview.activity_every` (30s by default), so idleness is that
precise. A `wake: never` server whose Run is not running shows "not
running", and nothing is emitted. See
`examples/preview-orchestrator` for an owner in 300 lines, and
[development](development.md#try-branch-previews-locally) to try it.

## Hosts and pools

A **host** runs `lux-runner`. It belongs to a **pool**. Pools are `static`
(hosts registered by hand) or `ec2` (luxd launches hosts on demand). Hosts
belong to a tenant by default. A platform pool can be marked `shared`, which
lets several tenants' Runs share its hosts. A Run that names no pool goes
to the tenant's **default pool**: the one pool it marked, else the
platform's marked one, else the pool named `default`.

An `ec2` pool sizes itself from what its hosts report. Each pass simulates
placing its waiting Runs on ready hosts, then on hosts already starting,
then on new hosts, and launches only the new hosts that simulation needs
(plus warm hosts). A new host's capacity is the smallest of what registered
hosts of the same pool and tenant, launched from exactly the pool's current
template, reported. Before any such host has registered, that capacity is
unknown, and the pool launches one host to learn it. The simulation is a
reservation for sizing only; the scheduler still places each Run on its
own. See [Telemetry](telemetry.md#capacity-planning). `GET /v1/pools`
says that size as each pool's `hostSize` (with `hostSizeFrom` and the
latest host's `instanceType`), also for a pool scaled to zero.

### Shaped pools

A pool may declare a **shape**: the nominal whole host its Runs are sized
against, set by its operator (`shape` on `POST /v1/pools`, `lux pools set
--shape`, `luxd admin create-pool --shape`):

```
family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2
```

Its **sizes** halve the whole host down to `minCpus`, each named
`<family>-<cpus>-<arch>` (`05` for half a CPU): here `m-05-arm64` (0.5
CPU, 2 GiB, 10 GiB), `m-1-arm64`, `m-2-arm64` (the default),
`m-4-arm64` and `m-8-arm64` (the whole host). `GET /v1/pools` lists them
as each shaped pool's `sizes`, with `fraction` of the whole host, and
`lux pools sizes <pool>` prints them. The family is the operator's label,
never read from the provider; the arch is explicit; the disk is the
launch template's, not the instance type's. Two pools of one family with
different disks are told apart by their names.

luxd checks a shape when the pool is set and refuses it with 422
`invalid_pool`: `cpus` and `minCpus` powers of two (`minCpus` at least
0.5), `memory` and `disk` that divide exactly down to the smallest size,
`defaultCpus` one of the sizes, names of at most 63 characters. For an
ec2 pool, its template's `instanceType` and `fallbackInstanceTypes` that
the instance-type catalog describes must have the shape's arch and at
least its CPUs and memory; a larger fallback still offers only the
shape's sizes. A type the catalog does not know yet is not checked.

A Run on a shaped pool asks for a size, or has its cpus, memory and disk
rounded up to one ([`resources.size`](runspec.md#rules)); its
`spec.resources` is that size's, and its `sizing` says what it asked for
and what it got. Setting, changing or removing a pool's shape never
resizes a Run already submitted: a Run is sized when it is submitted and
when a resume resizes or moves it.

After a shape is removed or changed, a Run keeps its resources, and its
`sizing.resolved` is shown only while its pool is shaped and that pool
still has a size of the same name with the Run's cpus, memory and disk;
otherwise it is absent (`sizing.requested` stays). A change can keep a
size's name and change its memory or disk (e.g. `m-4-arm64` after the
shape's memory doubles): the Run's size is then stale too. Its next
resume settles it:

- into an unshaped pool (its own, its shape removed, or another): the Run
  has no size from then on, and keeps its resources unless the resume
  asks for others;
- in its own pool whose sizes no longer include the Run's size with its
  resources, asking for no resources: its current cpus, memory and disk
  are rounded up to one of the pool's sizes, as on a move into a shaped
  pool (refused like any resize, e.g. 422 `size_too_large` or
  `size_below_saved_state`). A Run larger than the new shape's whole
  host gets `size_too_large` on every such resume, and leaves only by
  moving to another pool (`lux resume --pool`);
- asking for resources: those are resolved, as on any resume.

A pool without a shape is unchanged in every way: it takes no `size`, and
cpus, memory and disk as given, with luxd's defaults.

A shaped pool's Runs are placed only on hosts that advertise exactly its
nominal host (`capacity` cpus, memory and disk equal to the shape's) and
scale disk to it (the `disk-scale` capability: `lux-runner --disk` with
`--disk-headroom`). A host luxd launches for the pool is started that way,
with the shape's `cpus`, `memory`, `disk` and `diskHeadroom` (10 GiB
unless set), whatever its instance type: a larger
fallback type advertises the shape, not its hardware. Any other host in
the pool (an older lux-runner, or one started by hand with its own
capacity) is skipped, and a waiting Run's reason says how many hosts do
not advertise the shape. A static shaped pool's hosts must be started with
those flags by hand ([operations](operations.md#shaped-pools)).

On such a host a Run's disk limit is its size's disk scaled to what the
filesystem really has:

```
limit = disk × min(1, (filesystem capacity − diskHeadroom) / shape disk)
```

in whole bytes, rounded down. For a 160 GiB shape on a 160 GiB filesystem
with 10 GiB of headroom, `m-2-arm64` (40 GiB) is held to 37.5 GiB. The
grace counts from that limit, and `disk.exceeded` says both it and the
size's disk. A shaped host reserves each Run's size's disk alongside its
cpus and memory: a Run fits only where all three are free.

#### Disk pressure on shaped hosts

Reservations are nominal, and a Run may be over its limit for its
`diskGrace`, so the physical filesystem can still run low (image caches,
stopped Runs' local copies, snapshots waiting for upload, Runs in their
grace). The two kinds of host guard against that differently:

- **A host that scales disk** (every shaped pool's) has no fixed
  ceiling. Its runner's low-free-space guard watches the filesystem's
  free space: below the low mark (5% of the capacity by default) the host
  takes no new Run, normal, spot or resumed; unused images are removed,
  and the local copies of ended Runs that S3 already holds; then Runs
  over their limit, largest excess first, are stopped with a
  `disk.pressure_stop` event and end `failed`. A stopped Run's copy goes
  once its snapshot is uploaded. A Run at or under its limit is never
  stopped for it. The host takes Runs again once free
  space is back at the recovery mark (10% by default). Stopped Runs and
  retained snapshots count only through the free space they take, never
  as cpu or memory reservations.
- **A host that does not scale disk** (an unshaped pool's, unless its
  runner was started with `--disk-headroom`) keeps the fixed ceiling: a
  Run at twice its disk limit is stopped at once, whatever its grace. It
  has no low-free-space guard.

[Operations](operations.md#disk-pressure) has the marks, what an operator
sees and how to tune them.

### Placement: spread or pack

A pool's `placement` says how the scheduler chooses among the hosts a Run
fits (all of them: pool, tenancy, labels, nested containers, capacity,
sync modes and, on a shaped pool, the shape):

- `spread`: the host with the fewest Runs, after affinity (the host
  holding the Run's snapshot, then the image and git mirrors cached, then
  `placement.prefers` labels). Every unshaped pool's default.
- `pack`: the most allocated host, measured as the largest share of its
  cpus, memory or disk already reserved (not its Run count), so whole
  hosts stay free for the large sizes and idle hosts can scale down. Among
  hosts allocated alike, affinity as above decides, then the lowest host
  id. Every shaped pool's default. A resource a host does not advertise
  counts as empty, so pack on hosts that advertise none fills the lowest
  host id first. Spot Runs' reservations count as free here, as they do
  for scale-down: a host running only spot work ranks empty, so normal
  Runs fill hosts that already have normal work and spot-only hosts can
  still scale down. Among hosts allocated alike, the one already holding
  spot work comes first (before affinity), so a fully empty host stays
  free for a whole-host Run without reclaiming spot work. When a normal
  Run fits nowhere without reclaiming, pack reclaims on the most allocated
  host whose spot work suffices, ties by id (not by spot work, which would
  kill more of it); spread tries hosts by id. Spot Runs
  themselves are placed by spot admission, not by `placement`.

A pool without a placement packs while shaped and spreads otherwise, so
adding or removing a shape changes how it places.

Either way a Run whose snapshot only its host holds (not uploaded yet)
waits for that host, which may be less packed, and a host the Run is
moving away from comes last. Under pack, an uploaded snapshot's host is
only a tie-break, so a resume may restore from the blob store instead of
a host copy. Live Runs are never moved to pack hosts
tighter. Packing fills hosts first; it does not make every sequence fit:
two hosts with a quarter free each still cannot take a half-host size, which
waits for a host or launches one. Set it with `placement` on `POST
/v1/pools`, `lux pools set --placement` or `luxd admin create-pool
--placement`.

### Memory: the host's terms, scaled to what Linux sees

A host offers memory in the machine's own terms: its gross size
(`lux-runner --memory`; an EC2 host launched by luxd offers its instance
type's memory). Runs ask in those terms, and the scheduler packs them
against it. Linux never sees all of a machine, and the kernel needs some of
what it does see, so each runner scales every Run's memory by one factor,
computed once at start:

```
factor = min(1, (MemTotal − headroom) / offered)
```

`headroom` is `lux-runner --memory-headroom` (512 MiB by default). The
container's limit (`--memory`, with no swap beyond it) is
`floor(resources.memory × factor)`, and a placement reports it as
`memoryLimit`. A 32 GiB machine whose Linux sees 30.5 GiB leaves 30 GiB
after headroom: two Runs asking 16 GiB each fill it, and each gets 15 GiB.
Every Run pays the kernel's share in proportion. A host offering no more
than MemTotal less headroom is not scaled.

## Tenants and keys

Everything is scoped to a **tenant**. API keys carry scopes:

- `read`: see Runs, output, hosts.
- `run`: submit, steer, stop, resume, terminate.
- `admin`: pools, drain.

Each scope includes the ones before it. Runners authenticate with
per-host tokens.
