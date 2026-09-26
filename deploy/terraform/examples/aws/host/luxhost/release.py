"""Installs a lux release: download, verify, migrate, switch, health-check.

A release is lux_<version>_linux_<arch>.tar.gz plus SHA256SUMS under
<release_base_url>/<version>/. The tarball's checksum is verified and it
is extracted into <install_root>/versions/<version>. `luxd migrate` runs
with *that* version's binary, before anything is switched: if it fails,
the running version is untouched. Only then do the `current` and
stable-path symlinks move and luxd restart; the restart is polled
(systemctl is-active plus GET /health) for about 30s. If luxd does not
come up healthy, the previous symlink targets are restored, luxd is
restarted again and the step fails: a bad release never runs unmigrated,
and a failure never leaves the host without the previous luxd.
"""
import hashlib
import os
import shutil
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

from .host import Host, HostError, read_file

BIN_TARGETS = ["bin/luxd", "bin/lux"]
RUNNER_ARCHES = ["linux-arm64", "linux-amd64"]
HEALTH_TIMEOUT_S = 30
HEALTH_POLL_INTERVAL_S = 2


def arch() -> str:
    machine = os.uname().machine
    if machine in ("aarch64", "arm64"):
        return "arm64"
    if machine in ("x86_64", "amd64"):
        return "amd64"
    raise HostError(f"unsupported architecture: {machine}")


def installed_version(install_root: str) -> str:
    return (read_file(os.path.join(install_root, "CURRENT_VERSION")) or "").strip()


def download(url: str, dest: str, urlopen=urllib.request.urlopen) -> None:
    with urlopen(url, timeout=60) as resp, open(dest, "wb") as out:
        shutil.copyfileobj(resp, out)


def verify_sha256(tarball_path: str, sums_path: str, tarball_name: str) -> None:
    want = None
    with open(sums_path) as f:
        for line in f:
            parts = line.split()
            if len(parts) == 2 and parts[1].lstrip("*") == tarball_name:
                want = parts[0]
                break
    if want is None:
        raise HostError(f"{tarball_name} not listed in SHA256SUMS")
    h = hashlib.sha256()
    with open(tarball_path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    got = h.hexdigest()
    if got != want:
        raise HostError(f"checksum mismatch for {tarball_name}: want {want}, got {got}")


def extract_release(tarball_path: str, versions_root: str, version: str) -> str:
    """Extracts the tarball into <versions_root>/<version>, atomically.

    Extracts into a fresh temp directory, then renames it into place: a
    version directory is either absent or complete, and a stale leftover
    from a failed attempt at the same version is replaced. Never touches
    any other version directory, so the one `current` resolves to is safe.
    """
    os.makedirs(versions_root, exist_ok=True)
    version_dir = os.path.join(versions_root, version)
    tmp_extract = tempfile.mkdtemp(prefix=f"{version}.", dir=versions_root)
    try:
        with tarfile.open(tarball_path) as tf:
            tf.extractall(tmp_extract, filter="data")
    except Exception:
        shutil.rmtree(tmp_extract, ignore_errors=True)
        raise
    if os.path.lexists(version_dir):
        shutil.rmtree(version_dir)
    os.replace(tmp_extract, version_dir)
    return version_dir


def _atomic_symlink(target: str, link_path: str) -> None:
    os.makedirs(os.path.dirname(link_path), exist_ok=True)
    tmp = link_path + ".new"
    if os.path.lexists(tmp):
        os.remove(tmp)
    os.symlink(target, tmp)
    os.replace(tmp, link_path)


def stable_links(current_link: str, bin_dir: str, runner_bin_dir: str) -> dict:
    """Maps each stable path the system points at (the luxd unit,
    luxd.toml's runner_bin_dir) to its target inside `current`."""
    return {
        os.path.join(bin_dir, "luxd"): os.path.join(current_link, "bin/luxd"),
        os.path.join(bin_dir, "lux"): os.path.join(current_link, "bin/lux"),
        runner_bin_dir: os.path.join(current_link, "lib/lux/runner"),
    }


def switch_symlinks(version_dir: str, current_link: str, links: dict) -> dict:
    """Points `current` and every stable link at the new version; returns
    each link's previous target (None if absent) for restore_symlinks."""
    previous = {
        current_link: os.readlink(current_link) if os.path.islink(current_link) else None,
    }
    _atomic_symlink(version_dir, current_link)
    for link_path, target in links.items():
        previous[link_path] = os.readlink(link_path) if os.path.islink(link_path) else None
        _atomic_symlink(target, link_path)
    return previous


def restore_symlinks(previous: dict, current_link: str, links: dict) -> None:
    """Undoes switch_symlinks; links that did not exist before (a first
    install) are removed."""
    for link_path in [current_link, *links]:
        target = previous.get(link_path)
        if target is not None:
            _atomic_symlink(target, link_path)
        elif os.path.islink(link_path):
            os.remove(link_path)


def run_migrate(host: Host, luxd_bin: str, dsn: str):
    result = host.run([luxd_bin, "migrate"], check=False, env={"LUX_DATABASE_URL": dsn})
    return result.returncode == 0, result.stderr or ""


def restart_luxd(host: Host):
    result = host.run(["systemctl", "restart", "luxd"], check=False)
    return result.returncode == 0, result.stderr or ""


def is_luxd_active(host: Host) -> bool:
    return host.ok(["systemctl", "is-active", "--quiet", "luxd"])


def is_healthy(health_url: str, urlopen=urllib.request.urlopen) -> bool:
    try:
        with urlopen(health_url, timeout=3) as resp:
            return 200 <= resp.status < 300
    except (urllib.error.URLError, OSError, ValueError):
        return False


def wait_healthy(
    check_active,
    check_health,
    timeout_s: float = HEALTH_TIMEOUT_S,
    interval_s: float = HEALTH_POLL_INTERVAL_S,
    sleep=time.sleep,
    now=time.monotonic,
) -> bool:
    """Polls both checks until both pass or timeout_s elapses."""
    deadline = now() + timeout_s
    while True:
        if check_active() and check_health():
            return True
        if now() >= deadline:
            return False
        sleep(interval_s)


def deploy(host: Host, wanted: str, base_url: str, migrate_dsn: str, health_url: str) -> None:
    """Installs `wanted` and switches luxd to it, or raises HostError with
    the previous version (if any) still in place."""
    p = host.paths
    install_root = p.install_root
    current_link = os.path.join(install_root, "current")
    versions_root = os.path.join(install_root, "versions")
    os.makedirs(install_root, exist_ok=True)
    current = installed_version(install_root)
    running = current or "no version"

    host.log(f"deploying {wanted} (currently {current or 'none'})")
    tarball_name = f"lux_{wanted}_linux_{arch()}.tar.gz"
    release_url = f"{base_url}/{wanted}"

    with tempfile.TemporaryDirectory(prefix="lux-deploy-", dir=install_root) as tmp:
        tarball_path = os.path.join(tmp, tarball_name)
        sums_path = os.path.join(tmp, "SHA256SUMS")
        try:
            download(f"{release_url}/{tarball_name}", tarball_path, host.urlopen)
            download(f"{release_url}/SHA256SUMS", sums_path, host.urlopen)
        except Exception as e:
            raise HostError(f"download of {wanted} failed, leaving {running} running: {e}") from None
        try:
            verify_sha256(tarball_path, sums_path, tarball_name)
        except HostError as e:
            raise HostError(f"{e}; leaving {running} running") from None
        version_dir = extract_release(tarball_path, versions_root, wanted)

    for rel in BIN_TARGETS:
        if not os.path.exists(os.path.join(version_dir, rel)):
            raise HostError(f"{rel} missing from {tarball_name}; leaving {running} running")
    for rel_arch in RUNNER_ARCHES:
        if not os.path.isdir(os.path.join(version_dir, "lib", "lux", "runner", rel_arch)):
            host.log(f"note: {rel_arch} runner binaries absent from {wanted}")

    migrated, migrate_err = run_migrate(host, os.path.join(version_dir, "bin", "luxd"), migrate_dsn)
    if not migrated:
        raise HostError(f"luxd migrate for {wanted} failed, leaving {running} running: {migrate_err.strip()}")

    links = stable_links(current_link, p.bin_dir, p.runner_bin_dir)
    previous_targets = switch_symlinks(version_dir, current_link, links)

    restarted, restart_err = restart_luxd(host)
    healthy = restarted and wait_healthy(
        lambda: is_luxd_active(host),
        lambda: is_healthy(health_url, host.urlopen),
        sleep=host.sleep,
        now=host.now,
    )
    if not healthy:
        restore_symlinks(previous_targets, current_link, links)
        rolled_back, rollback_err = restart_luxd(host)
        detail = "ok" if restarted else "failed: " + restart_err.strip()
        msg = f"{wanted} did not come up healthy (restart {detail}); rolled back to {current or 'nothing'}"
        if not rolled_back:
            msg += f"; restart after rollback also failed: {rollback_err.strip()}"
        raise HostError(msg)

    with open(os.path.join(install_root, "CURRENT_VERSION"), "w") as f:
        f.write(wanted + "\n")
    host.log(f"deployed {wanted}")
