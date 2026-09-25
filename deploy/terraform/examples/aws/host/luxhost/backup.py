"""pg_dump -Fc of the lux database, streamed to S3.

Run as the postgres user by lux-pg-backup.timer. The bucket's lifecycle
expiry deletes old dumps; this only uploads. A pg_dump failure partway
through fails the run even though `aws s3 cp` exits 0 on a truncated
stream: both exit codes are checked.
"""
import datetime
import subprocess

from .host import HostError


def object_key(db_name: str, now: datetime.datetime) -> str:
    return f"{db_name}-{now.strftime('%Y%m%dT%H%M%SZ')}.dump"


def backup(db_name: str, bucket: str, region: str, popen=subprocess.Popen, now=None) -> str:
    """Uploads one dump; returns its s3:// URL."""
    stamp = now or datetime.datetime.now(datetime.UTC)
    url = f"s3://{bucket}/{object_key(db_name, stamp)}"
    dump = popen(["pg_dump", "-Fc", "-d", db_name], stdout=subprocess.PIPE)
    try:
        upload = popen(["aws", "s3", "cp", "-", url, "--region", region], stdin=dump.stdout)
    finally:
        # Only the upload holds the read end now: pg_dump sees EPIPE if it dies.
        dump.stdout.close()
    upload_rc = upload.wait()
    dump_rc = dump.wait()
    if dump_rc != 0:
        raise HostError(f"pg_dump exited {dump_rc}; {url} is incomplete")
    if upload_rc != 0:
        raise HostError(f"aws s3 cp to {url} exited {upload_rc}")
    return url
