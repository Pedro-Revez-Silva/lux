# host/: the control host's reconciler

The control host runs `reconcile.py` from its checkout of this repo
(`/var/lib/lux/config`) every 5 minutes, as `lux-reconcile.service`
triggered by `lux-reconcile.timer`. Python 3.13 standard library only; it
shells out to `git`, `aws`, `systemctl`, `apt-get`/`dpkg` and the Postgres
tools.

## What a run does

1. Reads `/etc/lux/host.json` (region, SSM prefix, checkout path; written
   once by cloud-init) and the infrastructure parameters under the SSM
   prefix (buckets, public URL, Access team/AUD, the Postgres volume id,
   the tunnel token's parameter name, the config repo URL/ref/deploy key).
2. Fetches the ref and hard-resets the checkout to it (local edits on the
   host are discarded). If anything under `host/` other than
   `lux-host.toml` changed, it re-executes the new `reconcile.py` once.
3. Validates `lux-host.toml` and its Access settings against SSM. A bad
   file fails here after checkout/deploy-key sync, before packages, units,
   database or luxd are changed.
4. Packages: installs Postgres 18 (PGDG apt repo; `dpkg -s postgresql-18`)
   and cloudflared (the GitHub release `.deb` for `dpkg
   --print-architecture`; present when dpkg says `install ok installed`
   and `/usr/local/bin/cloudflared`, the link its postinst makes to
   `/usr/bin/cloudflared`, exists) if missing.
   apt-get waits up to 300s for the dpkg and lists locks (cloud-init's own
   apt run). Once both are installed this runs no apt command; a failed
   install fails the run (`packages:`) and the next run retries.
5. Postgres: waits up to 300s for the data volume, formats it only if it
   has no filesystem, mounts it at `/var/lib/postgresql/18` and runs the
   cluster from it: the volume's own cluster if it holds one (a replaced
   host), else a new one (skipped once mounted with a cluster); generates
   the owner and
   `lux_app` passwords once (`/root/.lux-*`, 0600), sets the owner's
   password, creates the database.
6. Writes the systemd units (luxd, cloudflared, the backup timer, its own
   timer/service, the Postgres mount drop-in) and runs `daemon-reload`
   only if one changed. luxd's unit sets `LUX_HISTORY_DISK_PATHS=/,/var/lib/postgresql/18`:
   the filesystems the console's Control host row charts. It is in the
   environment, not `luxd.toml`, so an older pinned release (which refuses
   unknown keys in its file) still starts.
7. Renders the candidate config, leaving the installed config untouched until
   a version switch is ready. The installed binary validates a changed
   candidate before a config-only Access transition; unsupported settings
   leave the live config untouched.
8. If `lux_version` differs from the installed version: downloads the
   release, verifies it against `SHA256SUMS`, runs `luxd migrate` with
   the new binary and candidate config, journals the previous config (0600)
   and links privately (0700), switches the symlinks and config,
   restarts luxd and waits ~30s for `/health`; on failure restores the
   previous config and binaries before restarting the previous luxd. A
   killed switch is restored by luxd's `ExecStartPre` on boot/restart or at
   the start of the next reconcile, before extraction or service start.
   The unit's start wrapper pins the binary and config as open file descriptors
   under `/usr/local/lux/.pair.lock` and rechecks admission there; if the
   reconciler has exited, it recovers any interrupted switch before selection.
   Rollback and config-only writes hold the same lock. A start admitted before
   rollback either pins the candidate pair or fails closed after rollback.
   Otherwise, if the config or luxd's unit changed and luxd is running, it
   restarts luxd after releasing the reconcile lock, so its startup admission
   does not reject a config-only restart.
   With a version installed, every run also enables and starts a stopped
   luxd (`systemctl enable --now`, never a restart), so to keep luxd
   stopped, disable `lux-reconcile.timer` first.
9. Writes the cloudflared token from SSM and (re)starts cloudflared only
   if it or its unit changed.

A second run with nothing changed changes nothing.

## lux-host.toml

```toml
lux_version = "v0.5.0"          # a release tag; "none" or absent: install nothing
release_repo = "marcioapm/lux"  # GitHub owner/repo publishing the releases (default)
# release_base_url = "https://mirror.example.com/lux"  # instead of GitHub: <url>/<tag>/<file>

[luxd]                          # optional; absent keys keep luxd's defaults
debug = false
lease = "30s"                   # also: tick, scale_down_after, launch_timeout,
                                # provider_check_every, lost_grace, listing_lag
outdated_drain_percent = 10

[luxd.defaults]                 # Run defaults
cpus = 2
memory = "8Gi"
disk = "20Gi"
pids = 1024

[luxd.history]                  # sample_every, raw, minutes, hours
sample_every = "10s"
```

Unknown keys are errors. Infrastructure settings (`listen`, `public_url`,
`[database]`, `[s3]`, console auth and Cloudflare team/AUD) come from Terraform
through SSM and are refused here. Secrets never go in this file.

When Cloudflare Access is enabled in Terraform, add the following table to the
**private** config repo's `host/lux-host.toml` before reconciling or upgrading
the host. Do not put actual operator addresses or tenant IDs in a public repo:

```toml
[console.cloudflare_access]
operators = ["operator@example.com"]
default_tenant = "ten_aaaaaaaaaaaaaaaa"
```

Replace the placeholders with the explicit operator email allowlist and the
ID of an existing tenant created with `luxd admin create-tenant` (the stable
`ten_` ID, not its name). Operator email matching is case-insensitive; the
list must be nonempty and have no duplicates. Both settings are required in
Access mode and rejected in key mode. A missing or invalid setting fails the
run before package changes, `luxd.toml` writes, or a binary switch. Migrating
an existing host requires adding this private table along with enabling
Access; an already-compatible installed binary can switch config only,
otherwise upgrade `lux_version` to a release supporting these fields.
Staging the table on an incompatible binary fails reconciliation but keeps the
old config and old binary running; the periodic timer retries after the bump.
Failed downloads, migrations and health checks likewise keep or restore the
old config. For key mode, omit `[console.cloudflare_access]` entirely and leave
the Terraform Cloudflare Access team and AUD unset; the public example
`lux-host.toml` uses this mode. Do not rely on environment variables or a
manually edited `/etc/lux/luxd.toml`, since reconciliation overwrites it.

## Changing the version

Open a PR changing `lux_version`, merge it to the ref the host follows.
Within 5 minutes the host installs it; a failed install leaves the
previous version running and the run failing until the file changes.

## Seeing the result

```bash
journalctl -u lux-reconcile -n 20     # one summary line per run:
# lux-reconcile: status=ok version=v0.5.0 changed=[luxd.toml,luxd-restarted]
# lux-reconcile: status=error version=v0.4.0 changed=[] error='version: ...'
systemctl status lux-reconcile        # failed while the last run failed
systemctl start lux-reconcile         # run now
journalctl -u lux-pg-backup           # daily backups
```

## Tests

```bash
python3 -m pytest host/tests          # unit tests, fakes for aws/systemctl/Postgres
go test ./cmd/luxd                    # loads host-rendered Access and key configs with luxd
```

From a lux checkout, `make host-test` also runs the container smoke test (a new host, then a replaced one on the same Postgres volume); `make host-test HOST_DIR=/path/to/your/host` runs both against your copy of this directory.
