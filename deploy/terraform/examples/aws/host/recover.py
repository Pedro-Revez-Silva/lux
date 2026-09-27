"""Restore an interrupted luxd switch before systemd launches its binary."""
import fcntl

from luxhost.release import recover

with open("/etc/lux/.reconcile.lock", "a") as lock:
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        # A live reconcile owns the switch and is about to check this start.
        pass
    else:
        recover("/usr/local/lux")
