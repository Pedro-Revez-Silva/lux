"""Servers: named ports of a Run, with commands lux runs in its container;
their previews through luxd's preview listener; stream tickets and the
Origin check; lux shell."""

from __future__ import annotations

import json
import socket
import time

import pytest
import requests

from conftest import CLIError, generic
from env import ALPINE_IMAGE, PREVIEW_DOMAIN, free_port, wait_until


def http_get(port: int) -> str:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=3) as s:
            s.sendall(b"GET / HTTP/1.0\r\n\r\n")
            data = b""
            while chunk := s.recv(4096):
                data += chunk
            return data.decode()
    except OSError:
        return ""


def idle(image: str, **extra) -> dict:
    """A Run that is only there for its servers."""
    return generic(image, "sleep", "infinity", **extra)


def serve(port: int, text: str = "") -> list[str]:
    return ["lux-fake", "serve", str(port), *([text] if text else [])]


def add(lux, run_id: str, name: str, port: int, *args: str) -> dict:
    """lux server add; args are its flags, then -- and the command."""
    return json.loads(lux.run("-o", "json", "server", "add", run_id, name, str(port), *args).stdout)


def server(lux, run_id: str, name: str) -> dict:
    return lux.json("server", "ls", run_id, name)


def wait_server(lux, run_id: str, name: str, *states: str, timeout: float = 60) -> dict:
    return lux.json("server", "wait", run_id, name, "--state", ",".join(states), "--timeout", f"{int(timeout)}s",
                    timeout=timeout + 10)


def test_a_server_runs_and_is_reachable(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    sv = add(lux, run_id, "web", 8080, "--env", "GREETING=hi", "--", "sh", "-c",
             "echo $GREETING from $(id -un); exec lux-fake serve 8080 hello-web")
    assert sv["state"] == "starting" and sv["port"] == 8080 and sv["command"][0] == "sh", sv
    assert sv["url"].startswith("https://web-" + run_id.removeprefix("run_") + "." + PREVIEW_DOMAIN), sv
    sv = wait_server(lux, run_id, "web", "ready")
    assert sv["readySince"] and sv["epoch"] == 1, sv
    # A server's port is reachable by its name, like network.ports.
    local = free_port()
    p = lux.popen("port-forward", run_id, "web", str(local))
    try:
        body = wait_until(lambda: "hello-web" in (b := http_get(local)) and b, 30, 0.5, "the tunnel never answered")
        assert "hello-web" in body
    finally:
        p.terminate()
        p.wait(timeout=10)
    # Its output: in lux server logs and logs --server, not in the Run's own.
    wait_until(lambda: any("hi from agent" in l["text"] for l in lux.json("server", "logs", run_id, "web")["lines"]),
               20, 0.5, "no server output")
    assert "hi from agent" in lux.logs(run_id, "--server", "web")
    assert "hi from agent" not in lux.logs(run_id)
    recs = lux.records(run_id, "--servers")
    assert any(r["ch"] == "server" and r["server"] == "web" and r.get("stream") == "stdout" for r in recs), recs
    # get shows it; events record it.
    assert lux.get(run_id)["servers"][0]["name"] == "web"
    assert [e["data"]["state"] for e in lux.events(run_id, "server.state")][:2] == ["starting", "ready"]
    assert lux.events(run_id, "server.added")
    lux.run("cancel", run_id)


def test_a_server_that_exits(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    lux.run("server", "add", run_id, "bad", "9000", "--", "sh", "-c", "echo starting; echo 'listen tcp :9000: bind: address already in use' >&2; exit 3")
    sv = wait_server(lux, run_id, "bad", "exited")
    assert sv["exitCode"] == 3 and sv["error"] == "listen tcp :9000: bind: address already in use", sv
    # Restarting runs it again; it exits again.
    lux.run("server", "restart", run_id, "bad")
    wait_until(lambda: len([e for e in lux.events(run_id, "server.state") if e["data"]["state"] == "exited"]) == 2,
               30, 0.5, "never exited again")
    lux.run("cancel", run_id)


def test_start_stop_and_a_port_started_by_hand(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    lux.run("server", "add", run_id, "web", "8080", "--", *serve(8080))
    wait_server(lux, run_id, "web", "ready")
    sv = lux.json("server", "stop", run_id, "web")
    assert sv["state"] == "stopped" and sv["stopReason"] == "stopped", sv
    wait_until(lambda: http_forward_fails(lux, run_id, "web"), 20, 1, "the server kept serving after stop")
    lux.run("server", "start", run_id, "web")
    wait_server(lux, run_id, "web", "ready")
    # A port with no command: ready when something listens there.
    sv = add(lux, run_id, "manual", 7000)
    assert sv["state"] == "stopped" and sv["command"] is None and sv["stopReason"] is None, sv
    lux.run("server", "start", run_id, "manual")
    time.sleep(4)
    assert server(lux, run_id, "manual")["state"] == "starting"
    p = lux.popen("exec", run_id, "-T", "--", "lux-fake", "serve", "7000", "by-hand")
    try:
        wait_server(lux, run_id, "manual", "ready")
    finally:
        p.kill()
        p.wait()
    # Remove: gone, and 404 afterwards.
    lux.run("server", "rm", run_id, "web")
    with pytest.raises(CLIError) as e:
        lux.run("server", "ls", run_id, "web")
    assert e.value.code == 3
    # Starting needs a running Run; adding does not.
    lux.run("stop", run_id, "--wait", timeout=120)
    with pytest.raises(CLIError) as e:
        lux.run("server", "start", run_id, "manual")
    assert e.value.code == 4 and "a server starts only in a running Run" in e.value.stderr
    lux.run("server", "add", run_id, "later", "8081", "--no-start", "--", *serve(8081))
    lux.run("cancel", run_id)


def http_forward_fails(lux, run_id: str, name: str) -> bool:
    local = free_port()
    p = lux.popen("port-forward", run_id, name, str(local))
    try:
        time.sleep(1.5)
        return "hello" not in http_get(local)
    finally:
        p.terminate()
        p.wait(timeout=10)


def test_servers_stop_with_a_migration_and_spec_servers_start_again(operator, lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    runners.start(hosts[1])
    spec = idle(fake_image)
    spec["workload"]["servers"] = [{"name": "app", "port": 8080, "command": serve(8080, "from-spec")}]
    run_id = lux.submit(spec)
    lux.wait_state(run_id, "running")
    assert wait_server(lux, run_id, "app", "ready")["fromSpec"]
    lux.run("server", "add", run_id, "extra", "8081", "--", *serve(8081))
    wait_server(lux, run_id, "extra", "ready")
    operator.json("migrate", run_id, "--wait", timeout=200)
    # The runtime server stopped with the move and stays so; the spec's
    # starts again on the new placement.
    extra = server(lux, run_id, "extra")
    assert extra["state"] == "stopped" and extra["stopReason"] == "migrated" and extra["stoppedEpoch"] == 1, extra
    app = wait_server(lux, run_id, "app", "ready")
    assert app["epoch"] == 2, app
    stopped = [e for e in lux.events(run_id, "server.state") if e["data"]["state"] == "stopped"]
    assert {e["data"]["name"] for e in stopped} == {"app", "extra"}, stopped
    # And it is reachable on its new host.
    local = free_port()
    p = lux.popen("port-forward", run_id, "app", str(local))
    try:
        wait_until(lambda: "from-spec" in http_get(local), 30, 0.5, "not reachable after the move")
    finally:
        p.terminate()
        p.wait(timeout=10)
    # A plain stop: stopped as "run stopped".
    lux.run("stop", run_id, "--wait", timeout=120)
    assert server(lux, run_id, "app")["stopReason"] == "run stopped"
    lux.run("cancel", run_id)


def test_invalid_servers(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    bad = idle(fake_image)
    bad["workload"]["servers"] = [{"name": "Web", "port": 0}]
    with pytest.raises(CLIError) as e:
        lux.submit(bad)
    assert "workload.servers[0]" in e.value.stderr
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    lux.run("server", "add", run_id, "web", "8080", "--no-start")
    with pytest.raises(CLIError) as e:
        lux.run("server", "add", run_id, "web", "8081")
    assert e.value.code == 4 and "already has a server" in e.value.stderr
    with pytest.raises(CLIError) as e:
        lux.run("server", "add", run_id, "web-", "8081")
    assert "invalid name" in e.value.stderr
    lux.run("cancel", run_id)


# ---- stream tickets and the Origin check ---------------------------------------


def mint(lux, run_id: str, kind: str) -> requests.Response:
    return requests.post(f"{lux.env.luxd_url}/v1/runs/{run_id}/tickets", json={"kind": kind}, timeout=10,
                         headers={"Authorization": f"Bearer {lux.api_key}"})


def test_tickets_are_single_use_and_bound(lux, tenant_factory, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "600"))
    lux.wait_state(run_id, "running")
    r = mint(lux, run_id, "exec")
    assert r.status_code == 201, r.text
    t = r.json()
    assert t["ticket"].startswith("tkt_") and t["kind"] == "exec" and t["runId"] == run_id, t
    url = f"{lux.env.luxd_url}/v1/runs/{run_id}/exec"
    # Once: the stream check with the ticket passes, then it is spent.
    assert requests.get(url, params={"ticket": t["ticket"]}, timeout=10).status_code == 200
    assert requests.get(url, params={"ticket": t["ticket"]}, timeout=10).status_code == 401
    # A preview ticket is not an exec one; another tenant cannot mint one.
    pt = mint(lux, run_id, "preview").json()["ticket"]
    assert requests.get(url, params={"ticket": pt}, timeout=10).status_code == 401
    assert mint(tenant_factory(), run_id, "exec").status_code == 404
    assert mint(lux, run_id, "shell").status_code == 422
    # Nowhere but the stream routes.
    t2 = mint(lux, run_id, "exec").json()["ticket"]
    assert requests.get(f"{lux.env.luxd_url}/v1/runs/{run_id}", params={"ticket": t2}, timeout=10).status_code == 401
    lux.run("cancel", run_id)


def test_streams_refuse_other_origins(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "600"))
    lux.wait_state(run_id, "running")
    url = f"{lux.env.luxd_url}/v1/runs/{run_id}/exec"
    upgrade = {"Authorization": f"Bearer {lux.api_key}", "Connection": "Upgrade", "Upgrade": "websocket",
               "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}
    r = requests.get(url, headers={**upgrade, "Origin": "https://evil.example.com"}, timeout=10)
    assert r.status_code == 403 and r.json()["error"]["code"] == "bad_origin", r.text
    r = requests.get(url, headers={**upgrade, "Origin": lux.env.luxd_url}, timeout=10, stream=True)
    assert r.status_code == 101, r.status_code
    r.close()
    lux.run("cancel", run_id)


def test_lux_shell(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    out = lux.run("shell", run_id, input="echo shell-$((20+22)) $0; exit\n", timeout=60).stdout
    assert "shell-42" in out and "bash" in out, out
    lux.run("cancel", run_id)


# ---- previews ----------------------------------------------------------------------


class Preview:
    """Requests to luxd's preview listener, as a browser would make them
    to https://<server>-<suffix>.<domain> (Host header, no DNS)."""

    def __init__(self, env, run_id: str, name: str):
        self.base = f"http://{env.gateway}:{env.preview_port}"
        self.host = f"{name}-{run_id.removeprefix('run_')}.{PREVIEW_DOMAIN}"
        self.cookie = ""

    def get(self, path: str = "/", html: bool = True, **kw) -> requests.Response:
        headers = {"Host": self.host, **kw.pop("headers", {})}
        if html:
            headers["Accept"] = "text/html,application/xhtml+xml"
        if self.cookie:
            headers["Cookie"] = self.cookie
        return requests.get(self.base + path, headers=headers, allow_redirects=False, timeout=30, **kw)


def sign_in(lux, pv: Preview, run_id: str, to: str = "/"):
    t = mint(lux, run_id, "preview").json()["ticket"]
    r = pv.get(f"/.lux/auth?ticket={t}&to={requests.utils.quote(to, safe='')}")
    assert r.status_code == 302 and r.headers["Location"] == to, (r.status_code, r.headers)
    set_cookie = r.headers["Set-Cookie"]
    assert set_cookie.startswith("__Host-lux_preview=") and "Secure" in set_cookie and "HttpOnly" in set_cookie, set_cookie
    assert "Domain" not in set_cookie
    pv.cookie = set_cookie.split(";")[0]


def test_preview(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    lux.run("server", "add", run_id, "web", "8080", "--", *serve(8080, "hello-preview"))
    pv = Preview(lux.env, run_id, "web")
    # Not signed in: a page goes to sign in at luxd; anything else is 401.
    r = pv.get("/a?b=1")
    assert r.status_code == 302, r.status_code
    assert r.headers["Location"] == f"{lux.env.luxd_url}/preview-auth?to=" + requests.utils.quote(f"https://{pv.host}/a?b=1", safe=""), r.headers
    assert pv.get("/", html=False).status_code == 401
    # Someone else's ticket, or a spent one, signs nobody in.
    t = mint(lux, run_id, "preview").json()["ticket"]
    assert pv.get(f"/.lux/auth?ticket={t}&to=//evil.com").status_code == 400
    assert pv.get(f"/.lux/auth?ticket={t}&to=/").status_code == 302
    assert pv.get(f"/.lux/auth?ticket={t}&to=/").status_code == 401
    sign_in(lux, pv, run_id, "/a?b=1")
    wait_server(lux, run_id, "web", "ready")
    r = pv.get("/")
    assert r.status_code == 200 and "hello-preview" in r.text, r.text
    # The server sees itself as its host, the preview's as forwarded, and
    # none of lux's credentials.
    seen = pv.get("/headers", headers={"Authorization": f"Bearer {lux.api_key}"}).json()
    assert seen.get("X-Forwarded-Host") == [pv.host] and seen.get("X-Forwarded-Proto") == ["https"], seen
    assert "Authorization" not in seen and "Cookie" not in seen and seen.get("X-Lux-User"), seen
    # Cookies it sets are for its own host only.
    r = pv.get("/cookie")
    cookies = r.raw.headers.getlist("Set-Cookie")
    assert len(cookies) == 2 and not any("domain" in c.lower() for c in cookies), cookies
    # Server-sent events stream through.
    r = pv.get("/events", html=False, stream=True)
    lines = [l for l in r.iter_lines(decode_unicode=True) if l.startswith("data:")]
    assert lines == ["data: tick 0", "data: tick 1", "data: tick 2"], lines
    # Its activity is recorded.
    wait_until(lambda: server(lux, run_id, "web").get("lastRequestAt"), 40, 1, "no lastRequestAt")
    # Stopped: its status page, refreshing.
    lux.run("server", "stop", run_id, "web")
    r = pv.get("/")
    assert r.status_code == 503 and "Server stopped" in r.text and r.headers.get("X-Lux-Preview") == "status", r.text
    assert 'http-equiv="refresh"' in r.text and "prefers-color-scheme: dark" in r.text
    # Exited: with its code.
    lux.run("server", "add", run_id, "dies", "9000", "--", "sh", "-c", "echo oops >&2; exit 7")
    wait_server(lux, run_id, "dies", "exited")
    dies = Preview(lux.env, run_id, "dies")
    dies.cookie = ""
    sign_in(lux, dies, run_id)
    r = dies.get("/")
    assert "Server exited" in r.text and "<code>7</code>" in r.text and "oops" in r.text, r.text
    # A host that is no preview, and one of no server.
    assert "No such preview" in requests.get(pv.base, headers={"Host": f"nothing.{PREVIEW_DOMAIN}"}, timeout=10).text
    nosuch = Preview(lux.env, run_id, "nosuch")
    sign_in(lux, nosuch, run_id)
    assert nosuch.get("/").status_code == 404
    # The listener serves nothing of luxd's own.
    assert requests.get(pv.base + "/v1/whoami", headers={"Host": f"x.{PREVIEW_DOMAIN}",
                        "Authorization": f"Bearer {lux.api_key}"}, timeout=10).status_code == 404
    # The Run stops: the page says so.
    lux.run("stop", run_id, "--wait", timeout=120)
    r = pv.get("/")
    assert "not running" in r.text, r.text
    lux.run("cancel", run_id)


def test_preview_holds_a_starting_server(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(idle(fake_image))
    lux.wait_state(run_id, "running")
    # Up after a second: a request made meanwhile waits for it.
    lux.run("server", "add", run_id, "slow", "8080", "--", "sh", "-c", "sleep 1; exec lux-fake serve 8080 finally")
    pv = Preview(lux.env, run_id, "slow")
    sign_in(lux, pv, run_id)
    r = pv.get("/")
    assert r.status_code == 200 and "finally" in r.text, (r.status_code, r.text)
    lux.run("cancel", run_id)
