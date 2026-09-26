# host/: the control host's reconciler

The control host runs `reconcile.py` from its checkout of this repo
(`/var/lib/lux/config`) every 5 minutes, as `lux-reconcile.service`
triggered by `lux-reconcile.timer`. Python 3.13 standard library only; it
shells out to `git`, `aws`, `systemctl` and the Postgres tools.

## What a run does

1. Reads `/etc/lux/host.json` (region, SSM prefix, checkout path; written
   once by cloud-init) and the infrastructure parameters under the SSM
   prefix (buckets, public URL, Access team/AUD, the Postgres volume id,
   the tunnel token's parameter name, the config repo URL/ref/deploy key).
2. Fetches the ref and hard-resets the checkout to it (local edits on the
   host are discarded). If anything under `host/` other than
   `lux-host.toml` changed, it re-executes the new `reconcile.py` once.
3. Validates `lux-host.toml`. A bad file fails the run here, before
   anything on the host is touched.
4. Postgres: waits up to 300s for the data volume, formats it only if it
   has no filesystem, mounts it at `/var/lib/postgresql/18` and moves the
   cluster onto it (skipped once mounted); generates the owner and
   `lux_app` passwords once (`/root/.lux-*`, 0600), sets the owner's
   password, creates the database.
5. Writes the systemd units (luxd, cloudflared, the backup timer, its own
   timer/service, the Postgres mount drop-in) and runs `daemon-reload`
   only if one changed.
6. Renders `/etc/lux/luxd.toml` (0600, atomic).
7. If `lux_version` differs from the installed version: downloads the
   release, verifies it against `SHA256SUMS`, runs `luxd migrate` with
   the new binary, switches the symlinks, restarts luxd and waits ~30s for
   `/health`; on failure restores the previous binaries and restarts luxd.
   Otherwise, if the config or luxd's unit changed and luxd is running, it
   restarts luxd.
   With a version installed, every run also enables and starts a stopped
   luxd (`systemctl enable --now`, never a restart), so to keep luxd
   stopped, disable `lux-reconcile.timer` first.
8. Writes the cloudflared token from SSM and (re)starts cloudflared only
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
`[database]`, `[s3]`, `[console]`) come from Terraform through SSM and are
refused here. Secrets never go in this file.

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
```
