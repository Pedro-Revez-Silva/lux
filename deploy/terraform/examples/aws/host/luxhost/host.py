"""The seams every reconcile step goes through: commands, files, clocks.

Steps never call subprocess, time or urllib directly; they take a Host, so
tests replace `sh` (a subprocess.run stand-in), the clock and urlopen, and
point every path under a temporary root.
"""
import dataclasses
import os
import subprocess
import tempfile
import time
import urllib.request
from typing import Callable


class HostError(Exception):
    """A reconcile step failed; the message is what the summary line shows."""


@dataclasses.dataclass
class Paths:
    etc_lux: str = "/etc/lux"
    systemd_dir: str = "/etc/systemd/system"
    root_home: str = "/root"
    usr_local: str = "/usr/local"
    install_root: str = "/usr/local/lux"
    bin_dir: str = "/usr/local/bin"
    runner_bin_dir: str = "/usr/local/lib/lux/runner"
    fstab: str = "/etc/fstab"
    cloudflared_dir: str = "/etc/cloudflared"
    pg_mount: str = "/var/lib/postgresql/18"
    dev_by_id: str = "/dev/disk/by-id"
    pgdg_key_dir: str = "/usr/share/postgresql-common/pgdg"
    apt_sources_dir: str = "/etc/apt/sources.list.d"

    @classmethod
    def under(cls, root: str) -> "Paths":
        """Every default path re-rooted at root (tests)."""
        return cls(**{f.name: os.path.join(root, f.default.lstrip("/")) for f in dataclasses.fields(cls)})


@dataclasses.dataclass
class Host:
    sh: Callable = subprocess.run
    paths: Paths = dataclasses.field(default_factory=Paths)
    sleep: Callable = time.sleep
    now: Callable = time.monotonic
    urlopen: Callable = urllib.request.urlopen
    log: Callable = lambda msg: print(f"lux-reconcile: {msg}", flush=True)

    def run(self, argv: list, *, check: bool = True, input: str | None = None, env: dict | None = None):
        result = self.sh(
            argv,
            input=input,
            env={**os.environ, **env} if env else None,
            capture_output=True, text=True, check=False,
        )
        if check and result.returncode != 0:
            raise HostError(f"{' '.join(argv[:3])}: exit {result.returncode}: {(result.stderr or '').strip()}")
        return result

    def ok(self, argv: list) -> bool:
        return self.run(argv, check=False).returncode == 0


def read_file(path: str) -> str | None:
    try:
        with open(path) as f:
            return f.read()
    except FileNotFoundError:
        return None


def write_if_changed(path: str, content: str, mode: int = 0o644) -> bool:
    """Atomically replaces path with content (same directory, then rename).
    Returns False, and touches nothing, when the file already has exactly
    that content and mode."""
    try:
        st = os.stat(path)
        if read_file(path) == content and (st.st_mode & 0o7777) == mode:
            return False
    except FileNotFoundError:
        pass
    d = os.path.dirname(path)
    os.makedirs(d, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=f".{os.path.basename(path)}.", dir=d)
    try:
        with os.fdopen(fd, "w") as f:
            f.write(content)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise
    return True
