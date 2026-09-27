"""Installs a lux release: download, verify, migrate, switch, health-check.

A release is lux_<version>_linux_<arch>.tar.gz plus SHA256SUMS under
<release_base_url>/<version>/. The tarball's checksum is verified and it
is extracted into <install_root>/versions/<version>. `luxd migrate` runs
with that binary and a private candidate config before anything is switched.
Only then do the stable symlinks and service config move and luxd restart;
the restart is polled (systemctl is-active plus GET /health) for about 30s.
On failure, the old config and symlinks are restored before restarting luxd.
"""
import hashlib
import fcntl
import json
import os
import shutil
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
from contextlib import contextmanager

from .host import Host, HostError, read_file, write_if_changed

BIN_TARGETS = ["bin/luxd", "bin/lux"]
RUNNER_ARCHES = ["linux-arm64", "linux-amd64"]
DISCARDED_JOURNAL = ".switch-discard"
HEALTH_TIMEOUT_S = 30
HEALTH_POLL_INTERVAL_S = 2


@contextmanager
def pair_lock(install_root: str):
    # Kept outside the journal so its inode is stable across journal disposal.
    os.makedirs(install_root, exist_ok=True)
    with open(os.path.join(install_root, ".pair.lock"), "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


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
    from a failed attempt at the same version is replaced, unless selected.
    """
    os.makedirs(versions_root, exist_ok=True)
    version_dir = os.path.join(versions_root, version)
    current_link = os.path.join(os.path.dirname(versions_root), "current")
    if os.path.islink(current_link) and os.path.realpath(current_link) == os.path.realpath(version_dir):
        raise HostError(f"refusing to replace selected release {version}")
    tmp_extract = tempfile.mkdtemp(prefix=f"{version}.", dir=versions_root)
    try:
        with tarfile.open(tarball_path) as tf:
            tf.extractall(tmp_extract, filter="data")
    except Exception:
        shutil.rmtree(tmp_extract, ignore_errors=True)
        raise
    try:
        if os.path.islink(current_link) and os.path.realpath(current_link) == os.path.realpath(version_dir):
            raise HostError(f"refusing to replace selected release {version}")
        if os.path.lexists(version_dir):
            shutil.rmtree(version_dir)
        os.replace(tmp_extract, version_dir)
    finally:
        if os.path.exists(tmp_extract):
            shutil.rmtree(tmp_extract)
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


def _sync_dir(path: str) -> None:
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _persist_pair(install_root: str, config_path: str, links: dict) -> None:
    for path in (config_path, os.path.join(install_root, "CURRENT_VERSION")):
        if os.path.exists(path):
            with open(path, "rb") as f:
                os.fsync(f.fileno())
    for directory in {install_root, os.path.dirname(config_path),
                      *(os.path.dirname(path) for path in links)}:
        _sync_dir(directory)


def _persist_release(version_dir: str) -> None:
    for directory, _, files in os.walk(version_dir):
        for name in files:
            path = os.path.join(directory, name)
            if not os.path.islink(path):
                with open(path, "rb") as f:
                    os.fsync(f.fileno())
    for directory, _, _ in os.walk(version_dir, topdown=False):
        _sync_dir(directory)
    _sync_dir(os.path.dirname(version_dir))


def _discard_journal(install_root: str) -> None:
    journal = os.path.join(install_root, ".switch")
    discarded = os.path.join(install_root, DISCARDED_JOURNAL)
    if os.path.exists(discarded):
        shutil.rmtree(discarded)
    os.replace(journal, discarded)
    _sync_dir(install_root)
    shutil.rmtree(discarded)
    _sync_dir(install_root)


def recover(install_root: str) -> bool:
    with pair_lock(install_root):
        return _recover_locked(install_root)


def _recover_locked(install_root: str) -> bool:
    """Restore the last paired state before any service start or retry."""
    journal = os.path.join(install_root, ".switch")
    if not os.path.lexists(journal):
        return False
    record = read_file(os.path.join(journal, "state.json"))
    if record is None:
        raise HostError("switch journal missing state; refusing to start luxd")
    try:
        state = json.loads(record)
        config_path = state["config_path"]
        targets = state["targets"]
        version = state["version"]
        had_config = state["had_config"]
        current_link = os.path.join(install_root, "current")
        if (not isinstance(version, str) or not isinstance(config_path, str) or
                not isinstance(had_config, bool) or not isinstance(targets, dict) or
                any(not isinstance(k, str) or (v is not None and not isinstance(v, str))
                    for k, v in targets.items()) or current_link not in targets):
            raise ValueError("invalid switch state")
        if targets[current_link] is not None and not version:
            raise ValueError("selected release without previous version")
        if version and targets[current_link] is None:
            raise ValueError("previous release without selected link")
    except (ValueError, TypeError, KeyError) as e:
        raise HostError(f"invalid switch journal; refusing to start luxd: {e}") from e
    saved = read_file(os.path.join(journal, "luxd.toml"))
    if had_config and saved is None:
        raise HostError("switch journal missing previous config")
    if version and not had_config:
        raise HostError("switch journal missing previous config for installed release")
    if version and targets.get(current_link) != os.path.join(install_root, "versions", version):
        raise HostError("switch journal has mismatched previous version")
    if version and not os.path.exists(os.path.join(install_root, "versions", version, "bin", "luxd")):
        raise HostError("previous luxd binary missing; refusing to start luxd")
    ready = os.path.join(journal, "ready")
    if os.path.exists(ready):
        os.remove(ready)
        _sync_dir(journal)
    if saved is not None and had_config:
        write_if_changed(config_path, saved, 0o600)
    elif os.path.exists(config_path):
        os.remove(config_path)
    restore_symlinks(targets, current_link, {path: None for path in targets if path != current_link})
    marker = os.path.join(install_root, "CURRENT_VERSION")
    if version:
        write_if_changed(marker, version + "\n")
    elif os.path.exists(marker):
        os.remove(marker)
    _persist_pair(install_root, config_path, targets)
    _discard_journal(install_root)
    return True


def check_config(host: Host, binary: str, config: str, config_path: str) -> bool:
    with tempfile.TemporaryDirectory(prefix=".luxd-config-", dir=os.path.dirname(config_path)) as tmp:
        staged = os.path.join(tmp, "luxd.toml")
        write_if_changed(staged, config, 0o600)
        result = host.run([binary, "--config", staged, "check-config"], check=False)
        if result.returncode == 0:
            return True
        # Releases predating check-config still load strictly before opening the DB.
        if "usage: luxd" not in (result.stderr or ""):
            return False
        probe = host.run([binary, "--config", staged, "admin", "create-key"], check=False,
                         env={"LUX_DATABASE_URL": "postgres://probe:probe@127.0.0.1:1/probe?sslmode=disable"})
        return probe.returncode != 0 and "connect to database" in (probe.stderr or "").lower()


def run_migrate(host: Host, luxd_bin: str, dsn: str, config_path: str):
    result = host.run([luxd_bin, "--config", config_path, "migrate"], check=False, env={"LUX_DATABASE_URL": dsn})
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


def deploy(host: Host, wanted: str, base_url: str, migrate_dsn: str, health_url: str,
           config_path: str, config: str) -> None:
    """Installs `wanted` and switches luxd to it, or raises HostError with
    the previous version (if any) still in place."""
    p = host.paths
    install_root = p.install_root
    current_link = os.path.join(install_root, "current")
    versions_root = os.path.join(install_root, "versions")
    os.makedirs(install_root, exist_ok=True)
    recover(install_root)
    discarded = os.path.join(install_root, DISCARDED_JOURNAL)
    if os.path.exists(discarded):
        shutil.rmtree(discarded)
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
    _persist_release(version_dir)
    _sync_dir(install_root)

    # Migration reads the candidate through --config, without exposing it to the old service.
    with tempfile.TemporaryDirectory(prefix=".luxd-config-", dir=os.path.dirname(config_path)) as tmp:
        staged = os.path.join(tmp, "luxd.toml")
        write_if_changed(staged, config, 0o600)
        migrated, migrate_err = run_migrate(host, os.path.join(version_dir, "bin", "luxd"), migrate_dsn, staged)
    if not migrated:
        raise HostError(f"luxd migrate for {wanted} failed, leaving {running} running: {migrate_err.strip()}")

    links = stable_links(current_link, p.bin_dir, p.runner_bin_dir)
    previous_config = read_file(config_path)
    previous_targets = {path: os.readlink(path) if os.path.islink(path) else None
                         for path in (current_link, *links)}
    journal = tempfile.mkdtemp(prefix=".switch-prep-", dir=install_root)
    os.chmod(journal, 0o700)
    try:
        if previous_config is not None:
            write_if_changed(os.path.join(journal, "luxd.toml"), previous_config, 0o600)
        write_if_changed(os.path.join(journal, "state.json"), json.dumps({
            "version": current, "config_path": config_path,
            "had_config": previous_config is not None, "targets": previous_targets,
        }), 0o600)
        # The undo record must survive power loss before any live pointer changes.
        for name in ("state.json", "luxd.toml"):
            path = os.path.join(journal, name)
            if os.path.exists(path):
                with open(path, "rb") as f:
                    os.fsync(f.fileno())
        _sync_dir(journal)
        os.replace(journal, os.path.join(install_root, ".switch"))
        journal = os.path.join(install_root, ".switch")
        _sync_dir(install_root)
    except BaseException:
        if journal != os.path.join(install_root, ".switch") and os.path.isdir(journal):
            shutil.rmtree(journal)
        raise
    try:
        with pair_lock(install_root):
            switch_symlinks(version_dir, current_link, links)
            write_if_changed(config_path, config, 0o600)
            write_if_changed(os.path.join(install_root, "CURRENT_VERSION"), wanted + "\n")
            _persist_pair(install_root, config_path, links)
            ready = os.path.join(journal, "ready")
            write_if_changed(ready, "", 0o600)
            _sync_dir(journal)
        restarted, restart_err = restart_luxd(host)
        healthy = restarted and wait_healthy(
            lambda: is_luxd_active(host),
            lambda: is_healthy(health_url, host.urlopen),
            sleep=host.sleep,
            now=host.now,
        )
        if not healthy:
            detail = "ok" if restarted else "failed: " + restart_err.strip()
            raise HostError(f"{wanted} did not come up healthy (restart {detail})")
    except Exception as e:
        recover(install_root)
        rolled_back, rollback_err = restart_luxd(host)
        msg = f"{e}; rolled back to {current or 'nothing'}"
        if not rolled_back:
            msg += f"; restart after rollback also failed: {rollback_err.strip()}"
        raise HostError(msg) from e

    _discard_journal(install_root)
    host.log(f"deployed {wanted}")
