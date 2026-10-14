# Fractional pool sizes

Status: approved direction; implementation in phases. Existing unshaped pools retain their behavior. This document specifies the new contract, not features already shipped.

## Contract

A shaped pool declares a nominal host shape (CPU, memory, disk), an architecture, a family name, and a minimum CPU size. Sizes are whole-host, half-host, quarter-host, etc., stopping before CPU drops below the minimum (normally 0.5). A size is `<family>-<cpus>-<arch>`; the GitHub bridge prefixes `lux-`. CPU token `05` is 0.5.

For an `m` pool of 8 CPUs, 32 GiB nominal memory and 160 GiB nominal disk, available names are `m-05-arm64`, `m-1-arm64`, `m-2-arm64`, `m-4-arm64`, `m-8-arm64`. A 4-CPU pool with minimum 0.5 stops at 1/8. Family is explicit pool metadata, not guessed from a provider name: typical ratios are c=2, m=4, r=8 GiB/CPU. Architecture is always explicit. Validate that shape and minimum produce a finite power-of-two ladder; reject non-finite/negative values, impossible minimums, and names exceeding API limits.

A shape is a stable pool contract. Derive/check CPU and memory against the primary instance type; disk is explicit because it comes from the launch template. Fallback types must meet or exceed the declared shape and match architecture. Larger fallbacks advertise only the nominal shape for scheduling; they do not silently create different size semantics. Pool names disambiguate two pools with the same family but different disks.

## Request and resolution

Add `resources.size`, alongside the existing raw fields. A Run selects its pool as today through `placement.pool`. Explicit size must exist in that pool; do not combine it with raw CPU/memory/disk overrides. No global AWS-family inference and no hidden cross-pool moves.

On shaped pools, raw resource requests are rounded UP to the smallest ladder entry covering every supplied dimension. Missing dimensions must not be filled from global luxd resource defaults before rounding: otherwise a CPU-only request can be inflated by an unrelated default disk or memory. A request with none of size/CPU/memory/disk uses the pool's declared default size, initially the 2-CPU entry if available; validate that default explicitly. Pids and diskGrace remain orthogonal knobs.

Refuse requests larger than the largest size, reporting requested values and the pool maximum. Reject contradictory explicit-size/raw-field requests. Resolve at submission and when changing size/pool on resume, never continuously mutate a running Run as the pool changes.

Persist requested and resolved resources separately (retain original request for idempotency comparisons and explanation). Existing resolved `resources.cpus/memory/disk` remain the scheduler/cost contract. Return requested size/resources and resolved size/resources through submit/get/list and resume validation. Add size listings to pool responses (and CLI `lux pool sizes` using those responses). CLI `lux run --size m-2-arm64` and `lux resume --size ...`; regenerate OpenAPI and CLI docs.

Raw requests on unshaped pools retain current normalization and limits. Existing Runs never retroactively resize when enabling a shape.

## Gross versus allocatable

Schedule in NOMINAL units, like memory today. The runner receives nominal CPU/memory/disk capacities plus headroom; a job's effective disk budget is:

`nominal request disk × min(1, (filesystem capacity − disk headroom) / advertised nominal disk)`.

Use filesystem capacity usable by the runner (statfs, with consistent treatment of filesystem-reserved blocks), not current free bytes; current free bytes change as jobs write and must not resize all active jobs. Validate positive allocatable capacity. Work in integer bytes and round DOWN for effective limits, so rounding cannot exceed the total. Disk limits remain sampled writable-layer/state-volume limits, not filesystem quotas.

Example in GiB (infrastructure UI may label EBS GiB as GB): nominal disk 160, usable capacity 160, headroom 10:

| CPUs | nominal memory GiB | nominal disk GiB | effective disk GiB |
|---|---|---|---|
| 0.5 | 2 | 10 | 9.375 |
| 1 | 4 | 20 | 18.75 |
| 2 | 8 | 40 | 37.5 |
| 4 | 16 | 80 | 75 |
| 8 | 32 | 160 | 150 |

Filesystem formatting/partitions can reduce the actual capacity, so a real host's effective disk may be slightly below this table. Memory independently uses existing `(MemTotal − memory headroom)/nominal memory`. CPU is quota, not exclusive pinning.

Add `--disk-headroom` and an explicit nominal `--disk`/environment setting to provisioned runners. Keep legacy disk=0 (no disk reservations) on unshaped pools only. Shaped pools require hosts to advertise the declared nominal capacity and support effective-limit scaling; old agents cannot be admitted for shaped placement. Record nominal and effective disk limits, headroom and filesystem/free bytes in telemetry.

The 10 GiB headroom is an initial configured floor, NOT a proven sufficient allowance. It includes OS/tools and shared image storage; image cache and retained snapshots are variable. Measure idle-host baseline, filesystem use after representative jobs, image-cache use and retained state separately. Do not subtract currently running jobs twice. Set image cleanup thresholds compatibly with the reserve; reclaim unused caches before destructive pressure recovery, without removing in-use images/snapshots.

## Scheduler and disk pressure

Existing scheduler already sums disk reservations, but disk=0 hosts bypass that fit check. Shaped hosts must reserve nominal disk alongside nominal CPU/memory. Preserve maxRuns, tenancy, labels, image architecture and chosen-host constraints.

Add per-pool placement policy `spread` (legacy default) or `pack`. For shaped pools use pack. Prefer the most allocated fitting host by normalized reserved fraction, not Run count. Retain mandatory snapshot accessibility and avoid-host rules. Among equally packed candidates prefer cached image/mirrors and deterministic host id. A local-only snapshot can force a less-packed host. This is not permission to migrate live jobs.

Fractions eliminate cross-dimension fragmentation, not scattered free capacity after departures. For example, two hosts each 1/4 free cannot take a half-host job. Fill-first reduces that problem; it does not guarantee optimal packing for every arrival/departure sequence. A full packing example is 1/2 + 1/4 + 1/8 + 1/16 + 1/16. Compaction/preemption of live jobs is out of scope.

A grace permits physical overcommit temporarily despite nominal reservations. Shared caches/snapshots can also consume physical storage outside those reservations. Host free-space telemetry and admission safeguards are therefore required, not just shape arithmetic.

`resources.diskGrace` remains the user-selected grace for continuous over-limit usage; dropping back under resets it. Replace the proposed fixed 2× per-job ceiling in shaped/reserved-disk mode with a host low-free-space guard. At low free space: reject new admissions, reclaim unused caches safely, then stop over-limit placements, largest excess first, re-measuring between victims and considering already-stopping placements. Do not kill conforming jobs merely because one exceeds. If no safe victim/reclaim exists, report host pressure and remain admission-blocked. Define configurable low/recovery watermarks with hysteresis; exact thresholds need measured baseline. Legacy unshaped behavior and any interim ceiling must be documented separately rather than silently altered.

## Resume, snapshots and compatibility

A resize on a shaped pool must choose a whole coupled size. If saved state plus the existing disk safety margin cannot fit the requested smaller size's effective disk, round UP again to the smallest size that fits the saved state and all requested dimensions. If none fits, reject (validateOnly reports the same result); never retain an oversized disk while lowering CPU/RAM, which breaks the shaped-pool contract. Cross-pool moves re-resolve against the destination shape, preserving existing architecture/snapshot checks.

Stopped state and retained snapshots occupy disk although not live Run reservations. Account for them in physical pressure/admission guards, not as permanently running CPU/memory reservations. Keep existing snapshot availability and resize rules on unshaped pools.

Roll out additive APIs/schema first; deploy compatible runners before enabling shape policy. Use explicit protocol capability to gate scaling/shape support. A mixed old/new pool must not schedule shaped Runs on incompatible agents. Launch-template increases affect new hosts only: old smaller-volume hosts must be drained/replaced or excluded from the shaped pool. Do not pretend a 100 GiB filesystem is 160 GiB with ordinary proportional scaling and accept unexpectedly smaller quotas.

Cost accounting continues pricing resolved reservations and each actual host/volume, never the user's smaller raw request. Historical telemetry must keep request, nominal reservation and effective limits distinguishable.

## Phase 1 concrete model policies

`internal/sizing` uses `float64` CPUs and `spec.Bytes` (signed `int64`) for nominal memory/disk, matching Run resources. Architecture uses the existing GOARCH string convention, restricted here to `arm64` and `amd64`. Family is an explicit lowercase alphanumeric/hyphen slug, starting and ending alphanumeric; it is not interpreted as provider identity. Canonical names are limited to 63 ASCII characters. Parsing is exact membership in the pool ladder, not permissive numeric parsing.

Nominal and minimum CPUs must be exact powers of two, with minimum at least 0.5 and nominal at most 2^53. The 2^53 bound is a policy cap, not a precision requirement: every power of two formats exactly, and int64 byte division already limits ladder depth. The explicit default CPU must be a ladder entry; the model supplies no implicit default. Nominal byte totals must divide exactly into positive integer bytes at every tier through the minimum: non-divisible totals are rejected rather than rounded. Integer halving avoids multiplication overflow and byte precision loss.

A validated model owns an ascending ladder and returns independent list copies. Each entry includes canonical name, nominal dimensions and whole-host fraction. Request fields use value-plus-presence records, including size, distinguishing omission from explicit invalid zero or empty size; resolution retains the request by value alongside the resolved entry. Only present CPU/memory/disk dimensions participate in ceil-to-cover comparisons, with no epsilon; omitted fields' values are neither inspected nor retained. Oversized errors carry the request and maximum entry, and their message names only the present requested dimensions. Pids and diskGrace are outside this model. Fallback validation accepts only matching architecture and finite CPU/byte capacities covering the shape; larger fallback capacities never change the nominal ladder. Runner capabilities and effective-limit scaling remain later-phase gates. Phase 1 types carry no serialization tags; phase 2 chooses the persisted form.

Errors are classified for callers with `errors.Is`/`errors.As`: `ErrInvalidShape` (shape validation, or a zero `Model`), `ErrInvalidRequest` (a present CPU/memory/disk that is not finite and positive), `ErrConflict` (explicit size combined with a raw field), `ErrUnknownSize` (a size not in this pool's ladder) and `*OversizedError` (larger than the largest size).

## Implementation phases and gates

1. **Pure model and resolution.** Shape types, finite size ladder, parse/name/list, ceil-to-cover resolution, validation. Unit/property tests for all boundaries, fractional CPUs, overflow/precision, oversized requests, missing fields/default size, fallback compatibility. No live normalization changes yet.
2. **Pool/API/request persistence.** Additive pool metadata, requested/resolved fields, create/idempotency/validateOnly and CLI; schema migration and generated docs. DB tests for backward compatibility and shaped raw rounding; guard SQL and collected test identities.
3. **Runner nominal disk and scaling.** Provisioning values, capacity/capability reporting, effective limits/telemetry, diskGrace integration. Unit tests plus Linux integration for actual limits; no old-agent shaped placement.
4. **Scheduling and resume.** Shape-capacity fit, pack policy, destination rounding and saved-state handling. DB tests for competing submissions, half/quarter packing, scattered holes, affinity, stale hosts and mixed capabilities.
5. **Physical pressure safeguards.** Measured host reserve, admission blocking, cache reclaim and fair victim selection; deterministic fake-filesystem tests plus a bounded live pressure test. *Implemented*: disk-scale runners take `--disk-low-free` / `--disk-recover-free` (default 5% / 10% of the filesystem, conservative until shaped hosts are measured) and replace the 2× ceiling with the guard; runners without disk scaling keep the ceiling ([operations](../operations.md#disk-pressure)). Reclaim covers lux's unused images, then the local copies of Runs no longer live on the host that S3 holds (every snapshot reported and uploaded, no upload pending), oldest exit first; a stopped Run's copy is freed once its upload ends, not at its host TTL. Unuploaded snapshots and git mirrors are never removed by it.
6. **Consumers and rollout.** GitHub labels `lux-<family>-<cpus>-<arch>` map to Lux sizes; other clients use sizes/raw rounding. Pool volume 160 GiB, initial disk headroom 10 GiB, replace old hosts, validate CPU/memory/disk and cancellation end to end. No Terraform apply or production rollout without operator authorization.

Every code phase goes through performance/code-quality/test-quality reviews, fixes, simplifier and its full local gates before opening a ready PR. Design approval is implementation authorization, not merge/tag/deployment authorization.
