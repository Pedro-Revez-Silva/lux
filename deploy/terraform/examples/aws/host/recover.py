"""Restore an interrupted luxd switch before systemd launches its binary."""
import fcntl
import os
import sys

from luxhost.release import pair_lock, recover


def admit(install_root="/usr/local/lux", lock_path="/etc/lux/.reconcile.lock"):
    with open(lock_path, "a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            # A reconcile may start luxd only after persisting its paired candidate.
            if not os.path.exists(os.path.join(install_root, ".switch", "ready")):
                sys.exit(1)
            return True
        else:
            recover(install_root)
            return False


def start(install_root="/usr/local/lux", config_path="/etc/lux/luxd.toml",
          lock_path="/etc/lux/.reconcile.lock", execute=os.execve):
    contended = admit(install_root, lock_path)
    with pair_lock(install_root):
        # Admission can precede rollback: recheck while holding the pair lock.
        if contended and not os.path.exists(os.path.join(install_root, ".switch", "ready")):
            sys.exit(1)
        binary = os.open(os.path.join(install_root, "current", "bin", "luxd"), os.O_RDONLY)
        try:
            config = os.open(config_path, os.O_RDONLY)
            try:
                os.set_inheritable(binary, True)
                os.set_inheritable(config, True)
            except BaseException:
                os.close(config)
                raise
        except BaseException:
            os.close(binary)
            raise
    binary_path = f"/proc/self/fd/{binary}"
    config_fd_path = f"/proc/self/fd/{config}"
    try:
        execute(binary_path, [binary_path, "--config", config_fd_path, "serve"], os.environ.copy())
    finally:
        os.close(config)
        os.close(binary)


if __name__ in ("__main__", "<run_path>"):
    if len(sys.argv) > 1 and sys.argv[1] == "start":
        start()
    else:
        admit()
