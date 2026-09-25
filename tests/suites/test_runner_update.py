"""Step: luxd serving runner binaries for self-update (runner_bin_dir): a
runner downloads /runner/v1/bin/linux-{arch}/{lux-runner,lux-shim} and checks
its sha256 against /runner/v1/bin/manifest and the X-Lux-Sha256 header. Also:
luxd drains a host whose runner or shim no longer match what it holds, once,
and a static host is told to exit once it is drained and idle; a
provisioned one is terminated and replaced instead (the pool's own path)."""

from __future__ import annotations

import hashlib
import json
import os

import psycopg
import pytest
import requests

from conftest import fake_only, FAKE_EC2_TIMERS
from env import wait_until


@pytest.fixture
def runner_bin_dir(env, tmp_path_factory):
    """Restarts luxd with LUX_RUNNER_BIN_DIR pointing at a directory holding
    fake (but real-shaped) binaries for both arches, and yields (dir,
    host_token, sha256-by-arch-by-name)."""
    d = tmp_path_factory.mktemp("runner-bin")
    content = {}
    for arch in ("arm64", "amd64"):
        for name in ("lux-runner", "lux-shim"):
            data = f"fake {arch} {name}\n".encode() * 1000  # ~20 bytes * 1000, not 20MB: fine for a checksum test
            sub = d / f"linux-{arch}"
            sub.mkdir(exist_ok=True)
            (sub / name).write_bytes(data)
            content.setdefault(arch, {})[name] = hashlib.sha256(data).hexdigest()
    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=str(d))
    token = env.luxd_admin("create-host-token")["token"]
    yield d, token, content
    env.stop_luxd()
    env.start_luxd()


def _get(env, path, token):
    return requests.get(f"{env.luxd_url}{path}", headers={"Authorization": f"Bearer {token}"}, timeout=10)


def test_manifest_lists_every_arch_and_binary(env, runner_bin_dir):
    _, token, content = runner_bin_dir
    resp = _get(env, "/runner/v1/bin/manifest", token)
    assert resp.status_code == 200, resp.text
    manifest = resp.json()
    for arch, files in content.items():
        for name, sha in files.items():
            assert manifest[f"linux-{arch}"][name] == sha, (arch, name, manifest)


def test_binary_download_matches_its_sha256_header_and_the_manifest(env, runner_bin_dir):
    _, token, content = runner_bin_dir
    manifest = _get(env, "/runner/v1/bin/manifest", token).json()
    for arch, files in content.items():
        for name, want_sha in files.items():
            resp = _get(env, f"/runner/v1/bin/linux-{arch}/{name}", token)
            assert resp.status_code == 200, resp.text
            assert resp.headers["X-Lux-Sha256"] == want_sha == manifest[f"linux-{arch}"][name]
            assert int(resp.headers["Content-Length"]) == len(resp.content)
            assert hashlib.sha256(resp.content).hexdigest() == want_sha


def test_binary_endpoints_need_a_host_token(env, runner_bin_dir):
    for path in ("/runner/v1/bin/manifest", "/runner/v1/bin/linux-arm64/lux-runner"):
        resp = requests.get(f"{env.luxd_url}{path}", timeout=10)
        assert resp.status_code == 401, (path, resp.status_code)


def test_a_missing_arch_or_binary_is_not_offered(env, runner_bin_dir):
    _, token, _ = runner_bin_dir
    manifest = _get(env, "/runner/v1/bin/manifest", token).json()
    assert "linux-riscv64" not in manifest
    resp = _get(env, "/runner/v1/bin/linux-riscv64/lux-runner", token)
    assert resp.status_code == 404


def test_an_arch_missing_one_of_the_pair_is_not_offered(env, tmp_path_factory):
    """luxd never advertises, or drains for, an arch it can only half
    serve: the manifest only lists an arch when it holds both binaries."""
    d = tmp_path_factory.mktemp("runner-bin-partial")
    (d / "linux-arm64").mkdir()
    (d / "linux-arm64" / "lux-runner").write_bytes(b"only the runner, no shim\n")
    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=str(d))
    try:
        token = env.luxd_admin("create-host-token")["token"]
        manifest = _get(env, "/runner/v1/bin/manifest", token).json()
        assert "linux-arm64" not in manifest, manifest
        # The binary itself is still servable by name (a partial mirror is
        # not a 404), just never offered or drained for.
        resp = _get(env, "/runner/v1/bin/linux-arm64/lux-runner", token)
        assert resp.status_code == 200, resp.text
    finally:
        env.stop_luxd()
        env.start_luxd()


# ---- bootstrap.sh (static hosts) --------------------------------------------

def test_bootstrap_script_is_served_without_auth(env):
    resp = requests.get(f"{env.luxd_url}/runner/v1/bootstrap.sh", timeout=10)
    assert resp.status_code == 200
    assert resp.text.startswith("#!/bin/bash")
    # Shares the unit and fetch-script text with the EC2 renderings
    # (internal/hostboot): both markers must be present verbatim.
    assert "lux-runner.service" in resp.text
    assert "/runner/v1/bin/linux-$larch/$bin" in resp.text


def test_bootstrap_script_fetches_and_verifies_binaries_on_a_host(env, hosts, runner_bin_dir):
    """Runs the documented pipeline shape exactly as docs/operations.md
    gives it: `curl ... | sudo env LUX_URL=... LUX_HOST_TOKEN=... bash`,
    piped from luxd's own endpoint on a simulated host, not a copy of the
    Go source and not run through a bare `sh` (which the docs no longer
    say, and which would silently fail on Debian/Ubuntu's dash). The
    simulated host has no systemd PID 1, so `systemctl` is stubbed out:
    what is checked is bootstrap.sh's own idempotent writes (env file,
    unit file, fetch script) and, run separately exactly as the unit's
    ExecStartPre would (subuid/subgid setup lives there now, shared with
    the Ignition rendering: see internal/hostboot/hostboot.go), that the
    fetch script downloads and verifies both binaries against
    runner_bin_dir and sets up the subuid/subgid range idempotently."""
    _, token, content = runner_bin_dir
    host = hosts[0]
    arch = host.exec("uname", "-m").strip()
    larch = {"aarch64": "arm64", "x86_64": "amd64"}.get(arch, arch)
    host.exec("sh", "-c", "printf '#!/bin/sh\\nexit 0\\n' > /usr/local/bin/systemctl && chmod +x /usr/local/bin/systemctl")
    pipeline = (
        f"curl -fsS {env.luxd_url}/runner/v1/bootstrap.sh | "
        f"sudo env LUX_URL={env.luxd_url} LUX_HOST_TOKEN={token} LUX_HOST_NAME=bootstrap-test bash"
    )
    run = lambda: host.exec("sh", "-c", pipeline)  # noqa: E731
    run()
    env_mode = host.exec("stat", "-c", "%a", "/etc/lux/runner.env").strip()
    assert env_mode == "600", env_mode
    runner_env = host.exec("cat", "/etc/lux/runner.env")
    assert f"LUX_URL={env.luxd_url}" in runner_env
    assert "LUX_HOST_NAME=bootstrap-test" in runner_env
    unit = host.exec("cat", "/etc/systemd/system/lux-runner.service")
    assert "ExecStartPre=/usr/local/bin/lux-fetch-binaries.sh" in unit
    assert "Restart=always" in unit
    fetch_mode = host.exec("stat", "-c", "%a", "/usr/local/bin/lux-fetch-binaries.sh").strip()
    assert fetch_mode == "755", fetch_mode
    # A second run (a reboot) does not fail: bootstrap.sh's own writes are
    # idempotent (systemd itself is stubbed, so ExecStartPre never runs
    # here; the fetch script's own idempotency, including subuid, is
    # exercised below by running it directly, as the unit would).
    run()

    # The fetch script it installed is exactly what lux-runner.service's
    # ExecStartPre would run, with the same EnvironmentFile= it declares:
    # exercise it the same way, as systemd would, twice (a reboot).
    fetch = "set -a; . /etc/lux/runner.env; set +a; exec bash /usr/local/bin/lux-fetch-binaries.sh"
    host.exec("sh", "-c", fetch)
    subuid = host.exec("cat", "/etc/subuid")
    assert "containers:2147483647:2147483648" in subuid
    for name in ("lux-runner", "lux-shim"):
        got = host.exec("sha256sum", f"/usr/local/bin/{name}").split()[0]
        assert got == content[larch][name], (name, got, content[larch][name])
        mode = host.exec("stat", "-c", "%a", f"/usr/local/bin/{name}").strip()
        assert mode == "755", (name, mode)

    host.exec("sh", "-c", fetch)
    assert host.exec("grep", "-c", "^containers:", "/etc/subuid").strip() == "1"


# ---- draining outdated hosts ------------------------------------------------

def _host_arch(lux, name: str) -> str:
    return lux.json("hosts", "get", name)["labels"]["arch"]


def _host_message_count(env, host_id: str, msg_type: str) -> int:
    with psycopg.connect(env.owner_dsn) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT count(*) FROM host_messages WHERE host_id = %s AND type = %s", (host_id, msg_type))
            return cur.fetchone()[0]


def _stop_request_count(env, host_id: str) -> int:
    with psycopg.connect(env.owner_dsn) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT count(*) FROM placements WHERE host_id = %s AND stop_requested_at IS NOT NULL", (host_id,))
            return cur.fetchone()[0]


def _mismatched_bin_dir(base: str, arch: str) -> str:
    """A runner_bin_dir whose binaries for arch are real files but not what
    any built lux-runner/lux-shim hashes to."""
    for name in ("lux-runner", "lux-shim"):
        d = f"{base}/linux-{arch}"
        os.makedirs(d, exist_ok=True)
        with open(f"{d}/{name}", "wb") as f:
            f.write(b"not what any real binary hashes to\n")
    return base


def test_an_outdated_static_host_is_drained_once(env, lux, runners, hosts):
    """luxd holds a binary for this host's arch that differs from what its
    runner reports: it is drained, with a clear reason, cordoned (no Run
    stopped: outdated-binaries drains never call requestStop), and the
    reaper's `exit` message is queued exactly once — a second reaper tick
    before the runner acts on it (exit_requested_at is set in the same
    UPDATE that enqueues the message) must not queue another. This host is
    idle from the start, so the runner receives that exit and exits within
    a couple of seconds, well inside one heartbeat interval: its actually
    exiting (not a fixed sleep, and not waiting on heartbeats that will
    never come) is what proves the reaper's coalescing held, since nothing
    else could have queued a second message once no runner remains to
    receive one."""
    host = runners.start(hosts[0])
    host_id = lux.json("hosts", "get", host.name)["id"]
    arch = _host_arch(lux, host.name)
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        h = wait_until(lambda: (lambda x: x if x["draining"] else None)(lux.json("hosts", "get", host.name)),
                       30, 0.5, "the outdated host was never drained")
        assert h["stateReason"] == "outdated binaries", h

        # Idle and drained: the reaper queues exactly one exit message,
        # and the runner acts on it and exits with the outdated-binaries
        # code (proto.ExitCodeOutdatedBinaries).
        wait_until(lambda: _host_message_count(env, host_id, "exit") == 1, 30, 0.5,
                   "the reaper never queued an exit message")
        code = runners.procs[host.name].wait(timeout=60)
        assert code == 42, f"exit code {code}, want 42 (ExitCodeOutdatedBinaries)"
        assert _host_message_count(env, host_id, "exit") == 1, "a second exit message was queued"
        # Cordon-only: no live placement ever exists here (idle from the
        # start), so no stop request either — proving the outdated-binaries
        # drain does not touch requestStop at all.
        assert _stop_request_count(env, host_id) == 0
    finally:
        env.stop_luxd()
        env.start_luxd()


def test_an_outdated_static_host_is_told_to_exit_once_drained(env, lux, runners, hosts):
    """Once an outdated, drained static host has nothing left running or to
    upload, luxd asks it to exit (a durable `exit` control message, code
    42); its systemd unit would restart it with fresh binaries — here, the
    bare runner process (standing in for the unit's restart) simply exits
    with that code."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin2", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        wait_until(lambda: (lambda x: x if x["draining"] else None)(lux.json("hosts", "get", host.name)),
                  30, 0.5, "the outdated host was never drained")
        # Idle and nothing to upload (no Runs were placed): luxd asks it to
        # exit with the outdated-binaries code.
        code = runners.procs[host.name].wait(timeout=60)
        assert code == 42, f"exit code {code}, want 42 (ExitCodeOutdatedBinaries)"
    finally:
        env.stop_luxd()
        env.start_luxd()


def test_a_host_already_draining_for_another_reason_is_left_alone(env, lux, runners, hosts):
    """Outdated binaries are never a second drain reason for a host already
    draining for something else (e.g. a person asked for it), including
    across a reconnect: the Hello that follows luxd's restart must not wipe
    the reason (`state_reason` survives while still draining)."""
    host = runners.start(hosts[0])
    arch = _host_arch(lux, host.name)
    lux.run("hosts", "drain", host.name)
    wait_until(lambda: lux.json("hosts", "get", host.name)["draining"], 15, 0.3, "manual drain never took")
    before_heartbeat = lux.json("hosts", "get", host.name)["lastHeartbeat"]
    bin_dir = _mismatched_bin_dir(env.log_dir + "/other-bin3", arch)

    env.stop_luxd()
    env.start_luxd(LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        # Wait for an actual reconnect (a fresh Hello answered by the
        # restarted luxd), not a fixed sleep: a newer lastHeartbeat than
        # before luxd restarted proves the Hello that could have wiped the
        # reason has already been processed.
        wait_until(lambda: (lambda h: h if h["lastHeartbeat"] != before_heartbeat else None)(lux.json("hosts", "get", host.name)),
                   30, 0.3, "the runner never reconnected after luxd restarted")
        h = lux.json("hosts", "get", host.name)
        assert h["draining"], h
        assert h["stateReason"] == "drain requested", h  # not overwritten with "outdated binaries"
    finally:
        env.stop_luxd()
        env.start_luxd()


@pytest.mark.ec2
def test_a_provisioned_outdated_host_is_replaced(lux, ec2):
    """luxd holds runner binaries the pool's instances do not match: each
    host is drained on registering, then terminated once idle (the usual
    drained-and-empty path), and a replacement is launched — indistinguishable
    from a lost or unlisted instance being replaced."""
    fake_only(ec2)
    bin_dir = ec2.env.log_dir + "/other-bin-ec2"
    for arch in ("arm64", "amd64"):
        _mismatched_bin_dir(bin_dir, arch)
    ec2.env.stop_luxd()
    ec2.env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake",
                       AWS_REGION="us-east-1", **FAKE_EC2_TIMERS,
                       LUX_RUNNER_BIN_DIR=bin_dir)
    try:
        lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(ec2.template), "--min", "1", "--max", "1")
        # By launches, not by what is running at a poll: an outdated host
        # lives only a second or two (registered, drained, idle,
        # terminated), so a poll can miss every one of them on a busy host.
        wait_until(lambda: ec2.calls.count("RunInstances") >= 1, 60, 0.5, "no host launched")
        wait_until(lambda: ec2.calls.count("RunInstances") >= 2 and ec2.calls.count("TerminateInstances") >= 1,
                   120, 0.5, "the outdated host was not replaced")
    finally:
        # Stop churning (every replacement is equally "outdated" while
        # LUX_RUNNER_BIN_DIR stays mismatched) before the pool is removed.
        lux.run("pools", "rm", "burst", check=False)
        ec2.env.stop_luxd()
        ec2.env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake",
                           AWS_REGION="us-east-1", **FAKE_EC2_TIMERS)
        wait_until(lambda: not ec2.running(), 90, 1, "instances left running after the pool was removed")
