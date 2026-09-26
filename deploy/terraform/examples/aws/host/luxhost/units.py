"""systemd units the reconciler owns: luxd, cloudflared, the backup timer,
the Postgres mount drop-in, and its own timer and service. Changing one is
a PR to the config repo; the next run rewrites it and reloads systemd."""
import os

from .host import Host, write_if_changed

RECONCILE_TIMER = """\
[Unit]
Description=Reconcile the lux control host with its config repo every 5 minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min
AccuracySec=30s

[Install]
WantedBy=timers.target
"""

RECONCILE_SERVICE = """\
[Unit]
Description=Reconcile the lux control host with its config repo
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=PYTHONDONTWRITEBYTECODE=1
ExecStart=/usr/bin/python3 {host_dir}/reconcile.py
"""

LUXD_SERVICE = """\
[Unit]
Description=luxd
After=network-online.target postgresql.service
Wants=network-online.target
RequiresMountsFor={pg_mount}

[Service]
ExecStart=/usr/local/bin/luxd serve
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
"""

CLOUDFLARED_SERVICE = """\
[Unit]
Description=Cloudflare Tunnel
After=network-online.target luxd.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/cloudflared tunnel --no-autoupdate run --token-file {token_file}
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
"""

BACKUP_TIMER = """\
[Unit]
Description=Daily Postgres backup at 00:00 UTC

[Timer]
OnCalendar=*-*-* 00:00:00 UTC
Persistent=true
AccuracySec=1min

[Install]
WantedBy=timers.target
"""

BACKUP_SERVICE = """\
[Unit]
Description=pg_dump -Fc, uploaded to S3
After=postgresql.service

[Service]
Type=oneshot
User=postgres
Environment=PYTHONDONTWRITEBYTECODE=1
ExecStart=/usr/bin/python3 {host_dir}/backup.py --db {db_name} --bucket {bucket} --region {region}
"""

PG_MOUNT_DROPIN = """\
[Unit]
RequiresMountsFor={pg_mount}
"""

TIMERS = ["lux-reconcile.timer", "lux-pg-backup.timer"]


def render(host: Host, host_dir: str, infra: dict, region: str) -> dict:
    """Unit contents by path relative to the systemd directory."""
    p = host.paths
    return {
        "lux-reconcile.timer": RECONCILE_TIMER,
        "lux-reconcile.service": RECONCILE_SERVICE.format(host_dir=host_dir),
        "luxd.service": LUXD_SERVICE.format(pg_mount=p.pg_mount),
        "cloudflared.service": CLOUDFLARED_SERVICE.format(token_file=os.path.join(p.cloudflared_dir, "token")),
        "lux-pg-backup.timer": BACKUP_TIMER,
        "lux-pg-backup.service": BACKUP_SERVICE.format(
            host_dir=host_dir, db_name=infra["db_name"], bucket=infra["backup_bucket"], region=region,
        ),
        "postgresql@18-main.service.d/lux-pgdata-mount.conf": PG_MOUNT_DROPIN.format(pg_mount=p.pg_mount),
    }


def apply(host: Host, units: dict) -> list:
    """Writes every unit; daemon-reload once if any changed. Returns the
    changed unit names."""
    changed = [
        name for name, content in units.items()
        if write_if_changed(os.path.join(host.paths.systemd_dir, name), content, 0o644)
    ]
    if changed:
        host.run(["systemctl", "daemon-reload"])
    return changed


def enable_now(host: Host, unit: str) -> bool:
    """Enables and starts unit unless it already is both. True if it did."""
    if host.ok(["systemctl", "is-enabled", "--quiet", unit]) and host.ok(["systemctl", "is-active", "--quiet", unit]):
        return False
    host.run(["systemctl", "enable", "--now", unit])
    return True
