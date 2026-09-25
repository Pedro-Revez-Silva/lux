# lux — build progress

Tracks the v1 build order.
Each step ends with an end-to-end test through `tests/run_tests.py`.

| # | Step | Status |
| --- | --- | --- |
| 0 | Test harness (`--infra-only`) | ✅ done |
| 1 | Skeleton: module, migrations + RLS, `luxd migrate/admin/serve`, API keys, runs API, runner registration, WebSocket + poll, `generic` in hardened Podman, CLI | ✅ done |
| 2 | Output: shim as PID 1, output files, `logs -f` via SSE relay, cursors | ✅ done |
| 3 | Snapshots + blob store, stop/resume same host and cross host, epochs/fencing — the migration test | ✅ done |
| 4 | Steering + adapters: `POST /input`, `lux-fake` over ACP, `acp` adapter, `claude-code` | ✅ done |
| 5 | S3 (straight through luxd), presigned downloads, host-local GC, retention | ✅ done |
| 6 | Git workspace: mirrors, clone at ref, push with runner-held credentials | ✅ done |
| 7 | Egress: per-Run networks, nftables, DNS stub, re-resolution | ✅ done |
| 8 | Secrets: tmpfs, redaction, re-supply on resume | ✅ done |
| 9 | Images from Containerfiles: `podman build`, `FROM` pinning, rebuild warnings | ✅ done |
| 10 | Nested containers (opt-in) | ✅ done |
| 11 | Attach, exec, port forwarding through the relay | ✅ done |
| 12 | Artifacts | ✅ done |
| 13 | EC2 pools (fake EC2 in tests, `--real-ec2` for nightly), autoscaling, draining, warm pools | ✅ done |
| 14 | Codex and OpenCode adapters | ✅ done (pulled forward) |

## Operator console

| # | Step | Status |
| --- | --- | --- |
| C1 | Operator keys: every tenant through the same API and CLI, `--tenant`, `hosts get`, `tenants ls`, `status` | ✅ done |
| C2 | History: host, placement and system samples, rollups, retention, `lux history` | ✅ done |
| C3 | Actions: `migrate`, `resume --to`, force resume with held secrets, resumability, `events --all` | ✅ done |
| C4 | Web console (React, Bun), embedded in luxd at `/console/`; review, simplify, browser tests | ✅ done |
| C5 | Live console: events pushed (LISTEN/NOTIFY), one event stream per tab; route owners declared | ✅ done |
| C6 | luxd TOML config (env overrides); console sign-in through Cloudflare Access | ✅ done |

Also: docs in `docs/` kept current with each step; external-tool findings
recorded in `docs/agent-protocols.md` and `docs/podman.md`.

After v1:

| Item | Status |
| --- | --- |
| API on huma v2; `docs/openapi.yaml` generated (`luxd openapi`) | ✅ done |
| Turn-end usage for Claude Code and Codex | ✅ done |
| `lux push --expect repo=sha` | ✅ done |
| Resource defaults per Run (2 CPUs, 8 GiB, 1024 pids; `LUX_DEFAULT_*`) | ✅ done |
| Spot instances: interruption notice → preempt → resume elsewhere | ✅ done |
| Review + simplify of the above (three review rounds; redaction of split secrets) | ✅ done |
| Disk limit per Run (`resources.disk`, measured; `--disk` reservation opt-in) | ✅ done |
| Per-repository `push: false` | ✅ done |
| MCP servers in the RunSpec (`workload.mcpServers`, headers from secrets) | ✅ done |
| Add repositories on resume (`git.repositories` in the resume request, `lux resume --add-repo`, `git.clone` events; a failed added clone is dropped, not fatal) | ✅ done |
| Services: credential-injecting proxy (`workload.services`) | ✅ done |
| Images: registry login (`image.registryAuth`, runner-only), shared build cache (`image.build.cache`), GC of pulled images and under disk pressure (`--image-disk-high`) | ✅ done |
| Loopback services; MCP servers backed by a service | ✅ done |
| AWS deployment: `runner_url`, luxd self-serving runner binaries with sha256 verification, outdated-host draining, no-custom-AMI EC2 boot (Ignition/Fedora CoreOS default, cloud-init script, or plain env; `GET /runner/bootstrap.sh` for static hosts), `make dist` release tarballs and `release.yml` — Terraform in `deploy/terraform/` | ✅ done |
| Timeouts: none unless set; count running time only (parked Runs keep theirs) | ✅ done |
