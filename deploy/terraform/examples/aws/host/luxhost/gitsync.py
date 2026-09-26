"""Keeps the config repo checkout at origin/<ref>.

The checkout is disposable: every run fetches the ref and hard-resets to
it, so local edits on the host never survive. A private repo is read with
a deploy key kept in SSM; with no key parameter the URL is fetched as is
(a public HTTPS repo, or a local path in tests).
"""
import os

from .host import Host, HostError, write_if_changed
from .infra import get_secure_parameter


def deploy_key_path(host: Host) -> str:
    return os.path.join(host.paths.root_home, ".ssh", "lux-config-deploy-key")


def git_env(host: Host, key_parameter: str) -> dict:
    if not key_parameter:
        return {}
    known_hosts = os.path.join(host.paths.root_home, ".ssh", "known_hosts")
    # accept-new: the first fetch (cloud-init's clone) pins the host key.
    return {
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_SSH_COMMAND": (
            f"ssh -i {deploy_key_path(host)} -o IdentitiesOnly=yes -o BatchMode=yes "
            f"-o ConnectTimeout=30 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 "
            f"-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile={known_hosts}"
        ),
    }


def refresh_deploy_key(host: Host, key_parameter: str, region: str) -> bool:
    """Rewrites the key file from SSM (a rotated key reaches the host on
    the next run). No parameter: nothing is read or written."""
    if not key_parameter:
        return False
    key = get_secure_parameter(host, key_parameter, region)
    if not key.strip():
        raise HostError(f"deploy key parameter {key_parameter} is empty")
    os.makedirs(os.path.dirname(deploy_key_path(host)), mode=0o700, exist_ok=True)
    return write_if_changed(deploy_key_path(host), key.rstrip("\n") + "\n", 0o600)


def sync(host: Host, checkout: str, url: str, ref: str, host_rel: str, env: dict) -> tuple[bool, bool]:
    """Fetches ref and resets the checkout to it. Returns (HEAD moved,
    the host code changed)."""
    git = ["git", "-C", checkout]
    if host.run([*git, "remote", "get-url", "origin"], check=False).stdout.strip() != url:
        host.run([*git, "remote", "set-url", "origin", url])
    before = host.run([*git, "rev-parse", "HEAD"], check=False).stdout.strip()
    host.run([*git, "fetch", "--prune", "--no-tags", "origin", ref], env=env)
    host.run([*git, "reset", "--hard", "--quiet", "FETCH_HEAD"])
    host.run([*git, "clean", "-ffdq"])
    after = host.run([*git, "rev-parse", "HEAD"]).stdout.strip()
    if before == after:
        return False, False
    if not before:
        return True, True
    # Anything under host/ other than the desired-state file.
    diff = host.run([*git, "diff", "--name-only", before, after, "--", host_rel, f":(exclude){host_rel}/lux-host.toml"])
    return True, bool(diff.stdout.strip())
