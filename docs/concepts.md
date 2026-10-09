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
| `stopped` | Exited on request with its state saved. **Resumable.** A Run stopped by a move (force-evicting drain, spot preemption, migrate) is then resumed by lux at once, unless its spec's `resumePolicy` says otherwise: `restart` starts it again from scratch, `manual` ends it `failed` and `never` ends it `terminated` ([resume policy](runspec.md#resume-policy)). |
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

## Volumes and snapshots

A spec declares **volumes**, which are named directories in the container:

- `state` volumes are **snapshotted every time the container exits**,
  however it exits. These are the only thing guaranteed to survive a move.
  Put the git checkout and the agent's session transcript here.
- `ephemeral` volumes start empty on every placement.
- File secrets live on a tmpfs, which is never part of a snapshot.

A **snapshot** is the state volumes as of one exit, plus a manifest. The
runner exports each volume (`podman volume export`, zstd) and keeps the
snapshot locally. It then uploads it through luxd to S3 in the background.
Runners never hold S3 credentials.

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

### Resizing on resume

A resume can change what a stopped, lost, failed or succeeded Run gets from then on
(`resources: {cpus, memory, disk}` in the request; `lux resume --cpus
--memory --disk`). A running Run's limits never change: stop it first.
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
  ([disk is measured](runspec.md#rules)).
- The answer's `resize` and the `resume.requested` event's `resources`
  say what was asked (`requested`), what the Run has now (`applied`),
  and, when a disk was kept, `disk: {requested, kept, reason,
  measuredBytes, neededBytes}`.
- A new size is placed like a submit of that size. The snapshot's host is
  only preferred: if the new size does not fit there, the Run goes to
  another host of its pool that has room, or, in a provisioned pool, a new
  host is asked for; with no host that could fit it, it waits for
  capacity, saying which resource is short.
- A resume of a Run already resuming is a retry of the resume it waits
  on, and its `resources` are compared with what that resume asked for
  (its `requested`, not what was applied):
  - the same `cpus`, `memory` and `disk` (each present or absent alike):
    202, with that first resume's `resize`, a disk it kept included;
  - no `resources`, or all of them absent or `disk` 0: 202 without
    `resize`, whatever the first resume asked;
  - anything else, including a request where the first had none: 409
    `not_resumable`. Invalid values (cpus or memory 0 or less, a
    negative disk) are 422 `invalid_spec` first, as on any resume.
  - A Run lux resumed itself, after a `drain`, `preempt` or `migrate`
    move, counts as a resume that asked for no resources, even when an
    earlier resume of yours resized it. While it is resuming, a resume
    with `resources` gets 409; one without gets 202. With `resumePolicy:
    restart` it is restarted from scratch instead (no snapshot restored);
    with `manual` or `never` it is not placed again: it ends `failed`
    ([resume policy](runspec.md#resume-policy)).

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
