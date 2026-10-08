"""Services (workload.services): HTTP services the workload calls through a
local socket, with the credential added by lux-shim. The workload never
holds it: not in its environment, files, or the shim's memory, even as
container root. The upstream is the test MCP server's /svc endpoints."""

from __future__ import annotations

import json

import pytest

from conftest import CLIError, fake_agent

PROTOCOLS = [pytest.param("acp", id="acp-fake"), pytest.param("claude-code", id="claude-fake"),
             pytest.param("codex", id="codex-fake")]


def svc_spec(image: str, mcp_server, prompt: str, adapter: str = "acp", url: str | None = None, **extra) -> dict:
    spec = fake_agent(image, prompt, adapter=adapter, **extra)
    spec["workload"]["services"] = [{"name": "tools", "url": url or f"http://{mcp_server.ip}:8080/svc",
                                     "headers": [{"name": "Authorization", "secret": "TOOLS_AUTH"}]}]
    spec["secrets"] = spec.get("secrets", []) + [{"name": "TOOLS_AUTH", "value": f"Bearer {mcp_server.token}"}]
    spec["network"] = {"egress": [{"cidr": f"{mcp_server.ip}/32"}]}
    return spec


def everything_visible(lux, run_id: str) -> str:
    return "\n".join([lux.logs(run_id), lux.run("events", run_id, "-o", "json").stdout,
                      lux.run("get", run_id, "-o", "json").stdout])


@pytest.mark.parametrize("adapter", PROTOCOLS)
def test_the_workload_calls_a_service_with_the_credential_added(lux, runners, hosts, fake_image, mcp_server, adapter):
    """The workload's own Authorization is replaced by the service's; bodies
    and streams go through; the credential appears nowhere."""
    runners.start(hosts[0])
    script = "\n".join([
        "http -H 'Authorization: Bearer wrong' tools GET /whoami?x=1",
        'http tools POST /echo {"hello":"there"}',
        "http tools GET /stream",
        "echo done",
    ])
    run_id = lux.submit(svc_spec(fake_image, mcp_server, script, adapter))
    out = lux.wait_output(run_id, "done", timeout=90)
    assert out.count("status 200") == 3, out
    start = out.index('{"auth"')
    who = json.loads(out[start:out.index("}", start) + 1])
    assert who == {"auth": True, "method": "GET", "path": "/svc/whoami", "query": "x=1"}, who
    assert '{"hello":"there"}' in out
    assert all(f"tick {i}" in out for i in range(3)), out
    assert mcp_server.token not in everything_visible(lux, run_id)
    lux.run("terminate", run_id)


def test_an_unreachable_service_is_a_502(lux, runners, hosts, fake_image, mcp_server):
    runners.start(hosts[0])
    spec = svc_spec(fake_image, mcp_server, "http tools GET /x\necho done", url=f"http://{mcp_server.ip}:9/svc")
    run_id = lux.submit(spec)
    out = lux.wait_output(run_id, "done", timeout=90)
    assert "status 502" in out and "service tools unreachable" in out, out
    lux.run("terminate", run_id)


def test_the_credential_is_out_of_reach_even_for_root(lux, runners, hosts, fake_image, mcp_server):
    """The value lives only in lux-shim's memory, and the shim is not
    dumpable: a workload running as container root, without ptrace, can't
    read /proc/1/mem or /proc/1/environ, and nothing else holds it."""
    runners.start(hosts[0])
    spec = svc_spec(fake_image, mcp_server, "http tools GET /whoami\necho ready")
    spec["workload"]["user"] = "root"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "ready", timeout=90)
    probe = (
        "id -u; env; "
        # The shim is not dumpable: its /proc entries are closed even to
        # container root (whatever the host's ptrace policy).
        "cat /proc/1/environ >/dev/null 2>&1 && echo environ-readable; "
        "dd if=/proc/1/mem bs=4096 skip=16 count=4096 2>/dev/null | strings | head -c 200000; "
        "grep -rsl " + mcp_server.token + " /.lux /etc /tmp /workspace /root 2>/dev/null; "
        "echo end"
    )
    out = lux.run("exec", run_id, "--", "sh", "-c", probe).stdout
    assert out.splitlines()[0] == "0", "the probe did not run as root"
    assert "environ-readable" not in out, "the shim's /proc entries are open: it is dumpable"
    assert "end" in out and mcp_server.token not in out, "the credential was readable from the container"
    assert "LUX_SERVICE_TOOLS=unix:/.lux/services/tools.sock" in out
    assert mcp_server.token not in everything_visible(lux, run_id)
    lux.run("terminate", run_id)


def test_services_work_after_a_resume(lux, runners, hosts, fake_image, mcp_server):
    runners.start(hosts[0])
    run_id = lux.submit(svc_spec(fake_image, mcp_server, "http tools GET /whoami\necho first"))
    lux.wait_output(run_id, "first", timeout=90)
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    since = lux.records(run_id)[-1]["cursor"]
    lux.run("resume", run_id, "--wait", "--input", "http tools GET /whoami\necho second",
            "--secret", f"TOOLS_AUTH=Bearer {mcp_server.token}")
    out = lux.wait_output(run_id, "second", timeout=90)
    after = lux.logs(run_id, "--since", since)
    assert "status 200" in after and '"auth": true' in after, after
    lux.run("terminate", run_id)


def test_submit_refused_when_egress_does_not_allow_the_service(env, lux, fake_image, mcp_server):
    spec = svc_spec(fake_image, mcp_server, "echo never")
    spec["network"] = {"egress": [{"cidr": "10.255.255.1/32"}]}
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    assert "does not allow service" in e.value.stderr, e.value.stderr
    from urllib.parse import urlparse
    luxd = urlparse(env.luxd_url).hostname
    spec = svc_spec(fake_image, mcp_server, "echo never", url=f"http://{luxd}:8080/svc")
    spec["network"] = {"egress": [{"cidr": f"{luxd}/32"}]}
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    assert "control plane" in e.value.stderr, e.value.stderr
