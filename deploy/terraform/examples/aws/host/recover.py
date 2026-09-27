"""Restore an interrupted luxd switch before systemd launches its binary."""
import fcntl
import os
import sys

from luxhost.release import recover

with open("/etc/lux/.reconcile.lock", "a") as lock:
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        # A reconcile may start luxd only after persisting its paired candidate.
        if not os.path.exists("/usr/local/lux/.switch/ready"):
            sys.exit(1)
    else:
        recover("/usr/local/lux")
