# Concepts

## Run

A **Run** is one unit of work, from submission to a terminal state. It is
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

## States

```
submitted → scheduled → starting → running ─┬─▶ succeeded
    ▲                                        ├─▶ failed
    │                                        ├─▶ cancelled
    │                        stopping ◀──────┘ (stop, drain, timeout)
    │                            │
    └── resuming ◀── stopped ◀───┘
                        │
         (host lost while live) ──▶ lost ──(resume)──▶ resuming
```

| State | Meaning |
| --- | --- |
| `submitted` | Accepted, waiting for a host. |
| `provisioning` | Waiting for a provider to launch a host (EC2 pools). |
| `scheduled` | A host has been chosen and sent the assignment. |
| `starting` | Image ready, volumes restored, container starting, init running. |
| `running` | The workload is running. `activity` says whether an agent is `busy` or `idle` (waiting for input). |
| `stopping` | Asked to wind down. The adapter stops the workload gracefully, then it is killed after the grace period. |
| `stopped` | Exited on request with its state saved. **Resumable.** |
| `resuming` | Waiting for a host for its next placement. |
| `succeeded` / `failed` / `cancelled` | Terminal. A failed Run can still be resumed. |
| `lost` | Its host stopped heartbeating while it was live. Resumable from the last snapshot taken *before* the lost placement. Work since then is gone. |

`stateReason` explains the current state, for example `exit code 3`,
`waiting for capacity`, or `lease expired: host stopped heartbeating`.

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

**What does not survive a move:** running processes, memory, open
connections, background servers, and anything outside the state volumes,
including the container's writable layer. Anything a workload installs at
runtime belongs in the image or in the init script, and the init script runs
on every start, so it must be idempotent.

## Resume

- **Same host, local snapshot still there:** nothing moves. The scheduler
  strongly prefers this host.
- **Another host:** the new runner downloads the snapshot from S3 through a
  short-lived presigned URL, imports the volumes and starts a new
  container. If the snapshot has not finished uploading from its host, the
  scheduler waits for the upload.
- The adapter's **resume** path runs with the stored session id. An agent
  reloads its conversation from its transcript on the restored volume.
- A resume must supply the Run's **secrets** again, because lux never stores
  secret values. This also means a resume can rotate credentials.

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
- **Git credentials** are used by the runner only and never enter the
  container (see [the RunSpec](runspec.md#git)).
- **What lux does not do:** it does not scan state volumes. A secret that
  the workload itself writes to a state volume is snapshotted as written.
  This is accepted, and documented here on purpose.

## Output

A placement's stdout, stderr and structured events are written by the shim
to a file on the host. Every record has a sequence number and secrets are
redacted before the record is written. While the placement is live, luxd
relays the records from the host. After it exits, the file is uploaded and
served from S3. A **cursor** (`<epoch>.<seq>`) addresses the Run's whole
output across placements, so `lux logs -f` continues seamlessly across a
move.

If a host dies unannounced, the output of its live placement is lost along
with its state. Everything before that placement is safe.

## Hosts and pools

A **host** runs `lux-runner`. It belongs to a **pool**. Pools are `static`
(hosts registered by hand) or `ec2` (luxd launches hosts on demand). Hosts
belong to a tenant by default. A platform pool can be marked `shared`, which
lets several tenants' Runs share its hosts. A Run that names no pool goes
to the tenant's **default pool**: the one pool it marked, else the
platform's marked one, else the pool named `default`.

## Tenants and keys

Everything is scoped to a **tenant**. API keys carry scopes:

- `read`: see Runs, output, hosts.
- `run`: submit, steer, stop, resume, cancel.
- `admin`: pools, drain.

Each scope includes the ones before it. Runners authenticate with
per-host tokens.
