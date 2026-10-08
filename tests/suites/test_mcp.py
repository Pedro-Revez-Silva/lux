"""MCP servers in the RunSpec: the agent is given remote (streamable HTTP)
MCP servers through its own protocol, with header values from secrets.
Every agent protocol lux-fake speaks is covered; the server is a real MCP
server on the test network, so auth and the exchange are real too."""

from __future__ import annotations

from urllib.parse import urlparse

import pytest

from conftest import CLIError, fake_agent
from env import wait_until

PROTOCOLS = [pytest.param("acp", id="acp-fake"), pytest.param("claude-code", id="claude-fake"),
             pytest.param("codex", id="codex-fake")]

# Where each protocol reports a tool call: (event type, what marks it).
TOOL_EVENTS = {
    "acp": ("acp.tool_call", "acp.tool_call_update"),
    "claude-code": ("claude.assistant", "claude.user"),
    "codex": ("codex.item/started", "codex.item/completed"),
}


def mcp_spec(image: str, mcp_server, prompt: str, adapter: str, **extra) -> dict:
    """A fake agent given the test MCP server, its egress allowed by address."""
    spec = fake_agent(image, prompt, adapter=adapter, **extra)
    spec["workload"]["mcpServers"] = [{"name": "tools", "url": mcp_server.url,
                                       "headers": [{"name": "Authorization", "secret": "MCP_TOKEN"}]}]
    spec["secrets"] = spec.get("secrets", []) + [{"name": "MCP_TOKEN", "value": f"Bearer {mcp_server.token}"}]
    spec["network"] = {"egress": [{"cidr": f"{mcp_server.ip}/32"}]}
    return spec


def tool_events(lux, run_id: str, adapter: str) -> list[dict]:
    """The Run's tool-call events, as its adapter recorded them."""
    types = TOOL_EVENTS[adapter]
    out = []
    for r in lux.records(run_id, "--events"):
        ev = r.get("event") if r.get("ch") == "event" else None
        if not ev or ev.get("type") not in types:
            continue
        s = str(ev.get("data"))
        if any(k in s for k in ("tool_call", "tool_use", "tool_result", "mcpToolCall")):
            out.append(ev)
    return out


def everything_visible(lux, run_id: str) -> str:
    return "\n".join([lux.logs(run_id), lux.run("logs", run_id, "--events", "-o", "json").stdout,
                      lux.run("events", run_id, "-o", "json").stdout, lux.run("get", run_id, "-o", "json").stdout])


def replied(lux, run_id: str, text: str = "echo: hello", since: str | None = None):
    """Wait for the call's reply: its result, or an error."""
    args = ("--since", since) if since else ()
    wait_until(lambda: any(t in lux.logs(run_id, *args) for t in (text, "error:")), 60, 0.5, "the agent never replied")


def calls_since(mcp_server, n: int) -> list[dict]:
    return mcp_server.calls()[n:]


@pytest.mark.parametrize("adapter", PROTOCOLS)
def test_agent_calls_an_mcp_server(env, lux, runners, hosts, fake_image, mcp_server, adapter):
    """The agent reaches the server through the adapter's delivery (ACP
    session/new, Claude's --mcp-config, Codex's -c overrides), with the
    header from the secret; the call shows as the protocol's tool events,
    and the token appears nowhere."""
    runners.start(hosts[0])
    before = len(mcp_server.calls())
    run_id = lux.submit(mcp_spec(fake_image, mcp_server, "mcp-call tools echo hello", adapter))
    replied(lux, run_id)

    calls = calls_since(mcp_server, before)
    assert calls and all(c["auth"] for c in calls), f"the server saw wrong or no auth: {calls}"
    assert [c["method"] for c in calls][:3] == ["initialize", "notifications/initialized", "tools/call"], calls
    assert calls[2]["tool"] == "echo"
    assert "echo: hello" in lux.logs(run_id)

    events = tool_events(lux, run_id, adapter)
    assert len(events) >= 2, events
    assert "echo: hello" in str(events[-1]["data"]), events

    visible = everything_visible(lux, run_id)
    assert mcp_server.token not in visible, "the MCP token leaked into output, events or the Run"
    from pathlib import Path
    assert mcp_server.token not in (Path(env.log_dir) / "luxd.log").read_text(errors="replace")
    assert mcp_server.token not in (Path(hosts[0].log_dir) / "runner.log").read_text(errors="replace")
    lux.run("terminate", run_id)


def test_acp_mcp_servers_after_session_load(lux, runners, hosts, fake_image, mcp_server):
    """A resumed ACP session is loaded (session/load) with the servers
    again: the call works after a stop and resume with the secret."""
    runners.start(hosts[0])
    spec = mcp_spec(fake_image, mcp_server, "mcp-call tools echo first", "acp")
    before = len(mcp_server.calls())
    run_id = lux.submit(spec)
    replied(lux, run_id, "echo: first")
    calls = calls_since(mcp_server, before)
    assert calls and all(c["auth"] for c in calls), f"the server saw wrong or no auth: {calls}"
    session = lux.wait_activity(run_id, "idle")["sessionId"]
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)

    before = len(mcp_server.calls())
    since = lux.records(run_id)[-1]["cursor"]
    lux.run("resume", run_id, "--wait", "--input", "mcp-call tools echo again",
            "--secret", f"MCP_TOKEN=Bearer {mcp_server.token}")
    replied(lux, run_id, "echo: again", since)
    calls = calls_since(mcp_server, before)
    assert calls and all(c["auth"] for c in calls), f"the server saw wrong or no auth: {calls}"
    assert "echo: again" in lux.logs(run_id, "--since", since)
    assert lux.get(run_id)["sessionId"] == session, "the session was not loaded"
    assert mcp_server.token not in everything_visible(lux, run_id)
    lux.run("terminate", run_id)


def test_acp_agent_without_http_mcp_gets_a_warning(lux, runners, hosts, fake_image, mcp_server):
    """An ACP agent that does not advertise HTTP MCP is sent none, and the
    Run says why."""
    runners.start(hosts[0])
    spec = mcp_spec(fake_image, mcp_server, "mcp-call tools echo hello", "acp")
    spec["workload"]["command"] = ["lux-fake", "--no-mcp-http"]
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "no MCP server tools")
    warnings = [r["event"] for r in lux.records(run_id, "--events") if r.get("ch") == "event"
                and r["event"].get("type") == "lux.warning" and "HTTP MCP" in str(r["event"].get("data"))]
    assert warnings, lux.records(run_id, "--events")
    lux.run("terminate", run_id)


def test_submit_refused_when_egress_does_not_allow_the_server(lux, fake_image, mcp_server):
    spec = mcp_spec(fake_image, mcp_server, "echo hi", "acp")
    spec["network"] = {"egress": [{"cidr": "192.0.2.0/24"}]}
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    assert e.value.code == 4, e.value  # 422
    assert "egress" in e.value.stderr and "tools" in e.value.stderr, e.value.stderr
    # Unrestricted needs no rule.
    spec["network"] = {"unrestricted": True}
    lux.submit(spec)


def test_submit_refused_when_the_server_is_the_control_plane(env, lux, fake_image, mcp_server):
    """luxd's own address is blocked for every Run: an MCP server there
    could never be reached, so the spec is refused up front."""
    host = urlparse(env.luxd_url).hostname
    spec = mcp_spec(fake_image, mcp_server, "echo hi", "acp")
    spec["workload"]["mcpServers"][0]["url"] = f"http://{host}:9999/mcp"
    spec["network"] = {"egress": [{"cidr": f"{host}/32"}]}
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    assert e.value.code == 4, e.value  # 422
    assert "control plane" in e.value.stderr, e.value.stderr


def test_a_header_only_secret_is_not_in_the_workloads_environment(lux, runners, hosts, fake_image, mcp_server):
    """A secret used only as an MCP header reaches the agent's MCP client,
    not the workload's environment (init, lux exec, the agent's own env)."""
    runners.start(hosts[0])
    run_id = lux.submit(mcp_spec(fake_image, mcp_server, "print-secret MCP_TOKEN\necho done", "acp"))
    lux.wait_output(run_id, "done", timeout=60)
    out = lux.run("exec", run_id, "--", "sh", "-c", "env; echo end").stdout
    assert "end" in out and "MCP_TOKEN" not in out and mcp_server.token not in out, out
    assert mcp_server.token not in lux.logs(run_id)


@pytest.mark.parametrize("adapter", PROTOCOLS)
def test_an_mcp_server_backed_by_a_service(lux, runners, hosts, fake_image, mcp_server, adapter):
    """An MCP server named by a loopback service: the agent is given
    http://127.0.0.1:<port>, the service adds the header, and the token is
    in nothing the agent or the workload can see."""
    runners.start(hosts[0])
    spec = fake_agent(fake_image, "mcp-call tools echo hello", adapter=adapter)
    spec["workload"]["services"] = [{"name": "tools", "url": f"http://{mcp_server.ip}:8080",
                                     "headers": [{"name": "Authorization", "secret": "MCP_TOKEN"}], "loopback": True}]
    spec["workload"]["mcpServers"] = [{"name": "tools", "service": "tools", "path": "/mcp"}]
    spec["secrets"] = [{"name": "MCP_TOKEN", "value": f"Bearer {mcp_server.token}"}]
    spec["network"] = {"egress": [{"cidr": f"{mcp_server.ip}/32"}]}
    before = len(mcp_server.calls())
    run_id = lux.submit(spec)
    replied(lux, run_id)
    calls = calls_since(mcp_server, before)
    assert calls and all(c["auth"] for c in calls), f"the server saw wrong or no auth: {calls}"
    assert "echo: hello" in lux.logs(run_id)
    probe = ("env; cat /.lux/secrets/* 2>/dev/null; cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\0' ' '; "
             "cat /proc/[0-9]*/environ 2>/dev/null | tr '\\0' '\\n'; echo end")
    out = lux.run("exec", run_id, "--", "sh", "-c", probe).stdout
    assert "end" in out and mcp_server.token not in out, "the token is visible in the container"
    assert "LUX_SERVICE_TOOLS_URL=http://127.0.0.1:41000" in out
    assert mcp_server.token not in everything_visible(lux, run_id)
    lux.run("terminate", run_id)
