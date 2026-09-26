"""The Postgres data volume, cluster, passwords and database.

The data volume (aws_ebs_volume.pg_data) is attached after the instance
exists, so the first run may start before it shows up: it is waited for by
its stable EBS device id, formatted only if it carries no filesystem at
all, and the PGDG default cluster is recreated on it. Once the volume is
mounted where Postgres keeps its data, that part is skipped.
"""
import os
import re
import secrets
import string

from .host import Host, HostError, read_file, write_if_changed

PG_MAJOR = "18"
DEVICE_WAIT_S = 300
DEVICE_POLL_S = 5
_DB_NAME = re.compile(r"^[a-z_][a-z0-9_]{0,62}$")


def device_path(host: Host, volume_id: str) -> str:
    return os.path.join(host.paths.dev_by_id, "nvme-Amazon_Elastic_Block_Store_" + volume_id.replace("-", ""))


def ensure_volume(host: Host, volume_id: str) -> bool:
    """Mounts the data volume and moves the cluster onto it. Returns True
    if it did anything; raises if the device never appears."""
    mnt = host.paths.pg_mount
    if host.ok(["mountpoint", "-q", mnt]):
        return False
    dev = device_path(host, volume_id)
    waited = 0
    while not os.path.exists(dev):
        if waited >= DEVICE_WAIT_S:
            raise HostError(f"{dev} did not appear after {waited}s")
        host.sleep(DEVICE_POLL_S)
        waited += DEVICE_POLL_S
    # blkid exits non-zero when it finds no signature: only then format.
    if not host.ok(["blkid", dev]):
        host.run(["mkfs.ext4", "-L", "pgdata", dev])
    host.run(["pg_dropcluster", "--stop", PG_MAJOR, "main"], check=False)
    os.makedirs(mnt, exist_ok=True)
    host.run(["mount", "LABEL=pgdata", mnt])
    fstab = read_file(host.paths.fstab) or ""
    if not any(line.startswith("LABEL=pgdata") for line in fstab.splitlines()):
        with open(host.paths.fstab, "a") as f:
            f.write(f"LABEL=pgdata {mnt} ext4 defaults,nofail 0 2\n")
    host.run(["chown", "postgres:postgres", mnt])
    host.run(["pg_createcluster", PG_MAJOR, "main", "-d", f"{mnt}/main", "--start"])
    return True


def _password_file(path: str) -> tuple[str, bool]:
    existing = read_file(path)
    if existing:
        return existing.strip(), False
    pw = "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(32))
    write_if_changed(path, pw, 0o600)
    return pw, True


def psql(host: Host, sql: str, db: str = "postgres") -> str:
    # SQL on stdin, not argv: it may carry a password.
    out = host.run(
        ["runuser", "-u", "postgres", "--", "psql", "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-d", db, "-f", "-"],
        input=sql,
    )
    return out.stdout.strip()


def ensure_database(host: Host, db_name: str) -> tuple[dict, list]:
    """Passwords (generated once, kept only in root-only files), the
    owner's password in Postgres, the database. Returns the credentials
    and what changed."""
    if not _DB_NAME.match(db_name):
        raise HostError(f"db_name {db_name!r} is not a plain identifier")
    changed = []
    home = host.paths.root_home
    owner_pw, new_owner = _password_file(os.path.join(home, ".lux-pg-owner-password"))
    app_pw, new_app = _password_file(os.path.join(home, ".lux-app-password"))
    if new_owner or new_app:
        changed.append("pg-passwords")
    # Every run: a cluster recreated on a fresh volume starts without it.
    psql(host, f"ALTER USER postgres WITH PASSWORD '{owner_pw}';")
    if psql(host, f"SELECT 1 FROM pg_database WHERE datname = '{db_name}';") != "1":
        host.run(["runuser", "-u", "postgres", "--", "createdb", db_name])
        changed.append("database")
    migrate_dsn = f"postgres://postgres:{owner_pw}@127.0.0.1:5432/{db_name}?sslmode=disable"
    # Kept for running `luxd migrate` by hand.
    if write_if_changed(os.path.join(home, ".lux-migrate-dsn"), migrate_dsn + "\n", 0o600):
        changed.append("migrate-dsn")
    return {"app_password": app_pw, "migrate_dsn": migrate_dsn}, changed
