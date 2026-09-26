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
| `lost` | It missed heartbeats for a whole lease period. |

Plus `lastHeartbeat`, which is updated with every heartbeat.

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
kept as samples, in three tables, at three resolutions (`res`: 0 raw, 60,
3600 seconds):

| Table | Written | What |
| --- | --- | --- |
| `host_samples` | each heartbeat | the host's CPU seconds (counter), memory and disk in use; its live placements and the CPU and memory they asked for |
| `placement_samples` | each heartbeat, per live placement | CPU seconds (counter), memory and pids now, disk, network counters |
| `system_samples` | every `LUX_SAMPLE_EVERY` | for the system (tenant `''`) and each tenant with anything live: Runs by state, busy, idle, queued, started and finished since the last sample, time to start p50/p95, hosts by state, capacity, allocated |

| `control_samples`, `control_disk_samples` | with the whole system's sample, at its instant | the control host, the machine luxd runs on: CPU seconds (counter) and cores, memory used and total; its Postgres database's size and connections (read with SQL, so a remote database works too); and for each of `LUX_HISTORY_DISK_PATHS` its filesystem's used, free (writable without root) and total bytes |

With several luxd instances on one database, each writes the control
sample of its own machine; at a given instant the first one wins.

Every minute, luxd rolls complete buckets up into the next resolution
(levels averaged, counters and peaks their maximum, starts and finishes
summed, states the bucket's last) and deletes what is older than that
resolution's retention (`LUX_HISTORY_RAW`, `_MINUTES`, `_HOURS`). The API
(`/v1/history`, `/v1/hosts/{id}/history`, `/v1/runs/{id}/history`) serves
counters as rates. `/v1/history` carries the control host (`control` on
each sample) only for an operator key reading the whole system: never for
a tenant key, nor for an operator's `?tenant=`. See
[Operators](operators.md#history).
