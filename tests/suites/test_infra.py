"""The environment itself: hosts up, Podman working in them, Postgres and
S3 reachable from them. Everything else in the suite assumes this."""

from __future__ import annotations

import json
import uuid

import pytest
import requests

from env import ALPINE_IMAGE, PG_PORT, S3_HEALTH_PATH, S3_PORT, TestEnvironment

pytestmark = pytest.mark.infra


def test_hosts_run_podman(hosts):
    for host in hosts:
        info = json.loads(host.podman("info", "--format", "json"))
        assert info["host"]["cgroupVersion"] == "v2"
        assert info["host"]["networkBackend"] == "netavark"
        assert int(info["version"]["Version"].split(".")[0]) >= 5


def test_hosts_have_separate_storage(hosts):
    if len(hosts) < 2:
        pytest.skip("needs two hosts")
    a, b = hosts[:2]
    name = f"only-on-a-{uuid.uuid4().hex[:6]}"
    a.podman("volume", "create", name)
    try:
        assert name not in b.podman("volume", "ls", "--format", "{{.Name}}")
    finally:
        a.podman("volume", "rm", name)


def test_userns_auto_gives_each_container_its_own_range(hosts):
    host = hosts[0]
    # Two at once: sequential --rm containers may reuse a freed range.
    names = [f"userns-{uuid.uuid4().hex[:6]}" for _ in range(2)]
    for n in names:
        host.podman("run", "-d", "--name", n, "--userns=auto", ALPINE_IMAGE, "sleep", "30")
    try:
        starts = {host.podman("exec", n, "cat", "/proc/self/uid_map").split()[1] for n in names}
        assert len(starts) == 2, starts
        assert all(int(s) > 0 for s in starts)
    finally:
        host.podman("rm", "-f", *names)


def test_volume_moves_between_hosts(hosts):
    """`podman volume export`/`import` is the snapshot primitive."""
    if len(hosts) < 2:
        pytest.skip("needs two hosts")
    a, b = hosts[:2]
    vol = f"move-{uuid.uuid4().hex[:6]}"
    a.podman("volume", "create", vol)
    a.podman("run", "--rm", "-v", f"{vol}:/data", ALPINE_IMAGE, "sh", "-c", "echo hello-from-a > /data/f")
    tar = a.exec("sh", "-c", f"podman volume export {vol} | base64 -w0")
    b.podman("volume", "create", vol)
    b.exec("sh", "-c", f"base64 -d | podman volume import {vol} -", input=tar.encode())
    out = b.podman("run", "--rm", "-v", f"{vol}:/data", ALPINE_IMAGE, "cat", "/data/f")
    a.podman("volume", "rm", vol)
    b.podman("volume", "rm", vol)
    assert out.strip() == "hello-from-a"


def test_services_reachable_from_hosts(env: TestEnvironment, hosts):
    for host in hosts:
        # S3, via the gateway address luxd and presigned URLs use.
        out = host.exec("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
                        f"http://{env.gateway}:{S3_PORT}{S3_HEALTH_PATH}")
        assert out == "200"
        # Postgres port open (hosts never talk to it, but the address is right).
        host.exec("bash", "-c", f"exec 3<>/dev/tcp/{env.gateway}/{PG_PORT}")


def test_gateway_reachable_from_hosts(env: TestEnvironment, hosts):
    """luxd listens on the gateway; check a listener there is reachable."""
    import http.server
    import threading

    from env import free_port

    port = free_port()
    srv = http.server.HTTPServer((env.gateway, port), http.server.SimpleHTTPRequestHandler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        for host in hosts:
            code = host.exec("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", f"http://{env.gateway}:{port}/")
            assert code == "200"
    finally:
        srv.shutdown()


def test_bucket_exists(env: TestEnvironment):
    env.s3().head_bucket(Bucket=env.bucket)
    assert requests.get(f"http://127.0.0.1:{S3_PORT}{S3_HEALTH_PATH}", timeout=2).ok
