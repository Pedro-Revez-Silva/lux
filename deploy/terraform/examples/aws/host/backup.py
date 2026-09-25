#!/usr/bin/env python3
"""Entry point for lux-pg-backup.service: one pg_dump -Fc upload to S3."""
import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from luxhost.backup import backup  # noqa: E402
from luxhost.host import HostError  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--bucket", required=True)
    ap.add_argument("--region", required=True)
    args = ap.parse_args()
    try:
        url = backup(args.db, args.bucket, args.region)
    except HostError as e:
        print(f"lux-pg-backup: status=error error={str(e)!r}", flush=True)
        return 1
    print(f"lux-pg-backup: status=ok object={url}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
