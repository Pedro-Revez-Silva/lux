"""Wakeable servers: servers of their own, attached to and detached from
Runs; a signed-in request wakes an asleep one (lux asks its owner, once
per wake, on the feed), idleness is told once, a resume's sync brings the
code current, and every new placement starts every attached server. The
test plays the owner (an orchestrator following /v1/events)."""

from __future__ import annotations

import concurrent.futures
import json
import time
import urllib.parse

import pytest
import requests

from conftest import CLIError, generic
from env import PREVIEW_DOMAIN, wait_until

VOLUMES = [{"name": "workspace", "path": "/workspace", "kind": "state"}]

# The harness's LUX_PREVIEW_IDLE_CHECK, in seconds; a negative idle check
# waits three of them.
IDLE_CHECK = 1
IDLE_SETTLE = 3 * IDLE_CHECK


def preview_spec(image: str, git_server=None, **extra) -> dict:
    """A servers-only Run: nothing but its servers runs (a generic
    `sleep infinity` workload); its checkout and data on a state volume."""
    spec = generic(image, "sleep", "infinity", volumes=[dict(v) for v in VOLUMES], **extra)
    spec["workload"]["workdir"] = "/workspace"
    if git_server is not None:
        spec["git"] = {"repositories": [{"name": "app", "url": git_server.url("app"), "ref": "main",
                                         "credential": "GIT_TOKEN", "path": "/workspace/app"}]}
        spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
    return spec


APP = ["lux-fake", "app", "8080", "/workspace/app", "/workspace/data/visits"]


def api(lux, method: str, path: str, body=None, token: str | None = None) -> requests.Response:
    return requests.request(method, f"{lux.env.luxd_url}{path}", json=body, timeout=30,
                            headers={"Authorization": f"Bearer {token or lux.api_key}"})


def create(lux, **body) -> dict:
    r = api(lux, "POST", "/v1/servers", body)
    assert r.status_code == 201, r.text
    return r.json()


def get(lux, sid: str) -> dict:
    r = api(lux, "GET", f"/v1/servers/{sid}")
    assert r.status_code == 200, r.text
    return r.json()


def events(lux, sid: str, typ: str = "") -> list[dict]:
    evs = api(lux, "GET", f"/v1/servers/{sid}/events").json()["events"]
    return [e for e in evs if not typ or e["type"] == typ]


def wait_state(lux, sid: str, *states: str, timeout: float = 90) -> dict:
    return wait_until(lambda: (s := get(lux, sid))["state"] in states and s, timeout, 0.5,
                      f"server {sid} never {states}")


def exec_in(lux, run_id: str, *argv: str, check: bool = True):
    return lux.run("exec", run_id, "-T", "--", *argv, input="", check=check)


def resume_sync(lux, run_id: str, to: str) -> dict:
    """The data of the Run's last git.sync, once it moved the checkout to `to`."""
    return wait_until(lambda: (e := lux.events(run_id, "git.sync")) and e[-1]["data"].get("to") == to and e[-1]["data"],
                      60, 1, "no git.sync for the resume")


class Browser:
    """A signed-in browser at a server's hostname (Host header, no DNS)."""

    def __init__(self, lux, sv: dict):
        self.lux = lux
        self.base = f"http://{lux.env.gateway}:{lux.env.preview_port}"
        self.host = sv["hostname"]
        self.cookie = ""

    def get(self, path: str = "/", **kw) -> requests.Response:
        headers = {"Host": self.host, "Accept": "text/html", **kw.pop("headers", {})}
        if self.cookie:
            headers["Cookie"] = self.cookie
        return requests.get(self.base + path, headers=headers, allow_redirects=False, timeout=30, **kw)

    def post(self, path: str, data: dict) -> requests.Response:
        return requests.post(self.base + path, data=data, headers={"Host": self.host, "Cookie": self.cookie},
                             allow_redirects=False, timeout=30)

    def sign_in(self, sid: str):
        t = api(self.lux, "POST", f"/v1/servers/{sid}/tickets").json()["ticket"]
        r = self.get(f"/.lux/auth?ticket={t}&to=/")
        assert r.status_code == 302, (r.status_code, r.text)
        self.cookie = r.headers["Set-Cookie"].split(";")[0]
        return self

    def follow(self, path: str = "/", timeout: float = 120) -> requests.Response:
        """As a browser on the waking page: poll /.lux/wait until it lets
        us into the app, then the app's page."""
        def into():
            r = self.get(f"/.lux/wait?to={urllib.parse.quote(path, safe="")}")
            return r.status_code == 303 and r
        wait_until(into, timeout, 1, f"{self.host} never woke")
        return self.get(path)


def test_wake_end_to_end_with_sync_and_state(lux, runners, hosts, fake_image, git_server):
    """The demo, headless: asleep → a request asks the owner once → the
    owner resumes with sync → the page drops into the app on the latest
    commit; idle is told once; stopped; a new commit; woken again: the new
    commit, and the visit counter (on the state volume) carried on."""
    a = git_server.create("app", {"message.txt": "hello from A\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    sv = create(lux, name="web", port=8080, command=APP, wake="request", idleAfter="4s",
                hostname=f"web.pr1.{PREVIEW_DOMAIN}", labels={"pr": "1"}, runId=run_id)
    assert sv["hostname"] == f"web.pr1.{PREVIEW_DOMAIN}" and sv["lifetime"] == "owner", sv
    wait_state(lux, sv["id"], "ready")
    b = Browser(lux, sv).sign_in(sv["id"])
    page = b.get("/")
    assert page.status_code == 200 and a[:7] in page.text and '<b id="visits">1</b>' in page.text, page.text
    # Idle once, after 4s with no request.
    wait_until(lambda: events(lux, sv["id"], "server.idle"), 30, 1, "never idle")
    time.sleep(IDLE_SETTLE)
    assert len(events(lux, sv["id"], "server.idle")) == 1
    # The owner stops it; asleep.
    lux.run("stop", run_id, "--wait", timeout=120)
    wait_state(lux, sv["id"], "asleep")
    # A new commit while it sleeps.
    b_commit = git_server.commit_on("app", "main", "message.txt", "hello from B\n")
    # Ten tabs at once: one wake.
    with concurrent.futures.ThreadPoolExecutor(10) as pool:
        pages = list(pool.map(lambda i: b.get(f"/goals?tab={i}"), range(10)))
    assert all(p.status_code == 503 and "Asked the orchestrator to start it" in p.text for p in pages), pages[0].text
    assert "dude" not in pages[0].text.lower()
    wakes = events(lux, sv["id"], "server.wake_requested")
    assert len(wakes) == 1, wakes
    w = wakes[0]["data"]
    assert w["path"].startswith("/goals") and w["runId"] == run_id and w["hostname"] == sv["hostname"] and w["labels"] == {"pr": "1"}, w
    assert get(lux, sv["id"])["state"] == "waking"
    # The owner answers: resume with sync.
    lux.run("resume", run_id, "--sync", "app=main", "--secret", f"GIT_TOKEN={git_server.token}")
    page = b.follow("/goals?tab=1")
    assert page.status_code == 200 and b_commit[:7] in page.text and "hello from B" in page.text, page.text
    assert '<b id="visits">2</b>' in page.text, page.text
    syncs = lux.events(run_id, "git.sync")
    assert syncs[-1]["data"]["status"] == "fast-forward" and syncs[-1]["data"]["from"] == a and syncs[-1]["data"]["to"] == b_commit, syncs
    assert len(events(lux, sv["id"], "server.wake_requested")) == 1
    lux.run("cancel", run_id)


def test_no_wake_without_sign_in_and_no_answer(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    sv = create(lux, name="web", port=8080, command=["sleep", "1"], wake="request", wakeTimeout="3s")
    assert sv["state"] == "asleep" and sv["runId"] is None, sv
    b = Browser(lux, sv)
    # Not signed in: to sign-in (a page) or 401; nothing asked.
    r = b.get("/")
    assert r.status_code == 302 and "/preview-auth?to=" in r.headers["Location"], r.headers
    assert b.get("/x", headers={"Accept": "application/json"}).status_code == 401
    assert events(lux, sv["id"], "server.wake_requested") == []
    b.sign_in(sv["id"])
    assert "Asked the orchestrator to start it" in b.get("/").text
    # Nobody answers: no answer, and a poll never asks again.
    sv = wait_state(lux, sv["id"], "no answer", timeout=20)
    r = b.get("/.lux/wait?to=/")
    assert "No answer from its owner" in r.text and "Ask again" in r.text, r.text
    assert len(events(lux, sv["id"], "server.wake_requested")) == 1
    # Ask again asks again (a new event); once.
    r = b.post("/.lux/wake", {"to": "/"})
    assert r.status_code == 303, r.status_code
    b.post("/.lux/wake", {"to": "/"})
    assert len(events(lux, sv["id"], "server.wake_requested")) == 2
    assert get(lux, sv["id"])["state"] == "waking"


def test_idle_is_reset_by_requests_not_websockets(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image))
    lux.wait_state(run_id, "running")
    sv = create(lux, name="web", port=8080, command=["lux-fake", "serve", "8080", "hi"], wake="request",
                idleAfter="5s", runId=run_id)
    wait_state(lux, sv["id"], "ready")
    b = Browser(lux, sv).sign_in(sv["id"])
    # Requests every 2s for 10s: never idle meanwhile.
    for _ in range(5):
        assert b.get("/").status_code == 200
        time.sleep(2)
    assert events(lux, sv["id"], "server.idle") == []
    # A server-sent event stream held open is one request: it does not keep
    # the server busy (as an open WebSocket does not). The stream outlasts
    # idleAfter, and is still open when server.idle comes.
    r = b.get("/events?n=60", stream=True, headers={"Accept": "text/event-stream"})
    ticks = r.iter_lines()
    assert next(l for l in ticks if l) == b"data: tick 0"
    wait_until(lambda: events(lux, sv["id"], "server.idle"), 30, 1, "never idle")
    assert next(l for l in ticks if l).startswith(b"data: tick "), "the stream closed before server.idle"
    r.close()
    time.sleep(IDLE_SETTLE)
    assert len(events(lux, sv["id"], "server.idle")) == 1
    # A request resets it: idle again later, once more.
    b.get("/")
    wait_until(lambda: len(events(lux, sv["id"], "server.idle")) == 2, 30, 1, "not idle again")
    lux.run("cancel", run_id)


def test_attach_detach_and_every_placement(operator, lux, runners, hosts, fake_image):
    """Attach to a running Run (starts now) and to a stopped one (starts at
    its next placement); detach stops the command, not the Run; another Run
    cannot take an attached server; after a stop/resume, a migration and a
    resume after a lost host, every attached server is back."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    r1 = lux.submit(preview_spec(fake_image))
    r2 = lux.submit(preview_spec(fake_image))
    lux.wait_state(r1, "running")
    lux.wait_state(r2, "running")
    sv = create(lux, name="web", port=8080, command=["lux-fake", "serve", "8080", "hi"])
    lux.run("server", "attach", sv["id"], r1)
    wait_state(lux, sv["id"], "ready")
    with pytest.raises(CLIError) as e:
        lux.run("server", "attach", sv["id"], r2)
    assert e.value.code == 4 and "attached" in e.value.stderr
    lux.run("server", "detach", sv["id"])
    got = get(lux, sv["id"])
    assert got["runId"] is None and got["state"] == "stopped", got
    assert lux.get(r1)["state"] == "running"
    # To a stopped Run: nothing now; at its resume, it starts.
    lux.run("stop", r2, "--wait", timeout=120)
    lux.run("server", "attach", sv["id"], r2)
    assert get(lux, sv["id"])["process"] == "stopped"
    lux.run("resume", r2, "--wait", timeout=120)
    wait_state(lux, sv["id"], "ready")
    # A run server added at runtime comes back after a migration too.
    lux.run("server", "add", r2, "extra", "8081", "--", "lux-fake", "serve", "8081")
    lux.run("server", "wait", r2, "extra", "--timeout", "60s")
    operator.json("migrate", r2, "--wait", timeout=200)
    wait_state(lux, sv["id"], "ready")
    lux.run("server", "wait", r2, "extra", "--timeout", "60s")
    # A lost host, then a resume: back again.
    host = lux.get(r2)["host"]
    h = next(x for x in hosts if x.name == host)
    other = next(x for x in hosts if x.name != host)
    runners.stop(h, "KILL")
    h.pause()
    try:
        lux.wait_state(r2, "lost", timeout=90)
        assert get(lux, sv["id"])["state"] in ("stopped", "asleep")
        lux.run("resume", r2, "--wait", timeout=180)
        wait_state(lux, sv["id"], "ready", timeout=120)
        assert lux.get(r2)["host"] == other.name
    finally:
        h.unpause()
    lux.run("cancel", r1)
    lux.run("cancel", r2)


def test_lifetimes_deletion_and_expiry(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    # A Run that succeeds: its run servers go; an owner server is detached.
    ok = lux.submit(generic(fake_image, "sh", "-c", "sleep 8"))
    lux.wait_state(ok, "running")
    lux.run("server", "add", ok, "a", "8080", "--no-start")
    owner = create(lux, name="b", port=8081, runId=ok, lifetime="owner")
    lux.wait_state(ok, "succeeded", timeout=60)
    assert lux.get(ok).get("servers", []) == []
    assert get(lux, owner["id"])["runId"] is None
    # A failed one keeps them (it can be resumed).
    bad = lux.submit(generic(fake_image, "sh", "-c", "sleep 5; exit 3"))
    lux.wait_state(bad, "running")
    lux.run("server", "add", bad, "a", "8080", "--no-start")
    lux.wait_state(bad, "failed", timeout=60)
    assert [s["name"] for s in lux.get(bad)["servers"]] == ["a"]
    # A cancelled one: gone.
    c = lux.submit(preview_spec(fake_image))
    lux.wait_state(c, "running")
    lux.run("server", "add", c, "a", "8080", "--no-start")
    lux.run("cancel", c)
    lux.wait_state(c, "cancelled")
    assert lux.get(c).get("servers", []) == []
    # Owner deletion: detached first (its command stops), its URL gone.
    r = lux.submit(preview_spec(fake_image))
    lux.wait_state(r, "running")
    sv = create(lux, name="web", port=8080, command=["lux-fake", "serve", "8080"], runId=r, lifetime="owner")
    wait_state(lux, sv["id"], "ready")
    b = Browser(lux, sv).sign_in(sv["id"])
    assert b.get("/").status_code == 200
    lux.run("server", "rm", sv["id"])
    page = b.get("/")
    assert page.status_code == 404 and "This preview is gone" in page.text, page.text
    assert lux.get(r)["state"] == "running" and lux.get(r).get("servers", []) == []
    # Expiry: an owner server unrequested for expireAfter is deleted.
    exp = create(lux, name="old", port=1, expireAfter="3s")
    wait_until(lambda: api(lux, "GET", f"/v1/servers/{exp['id']}").status_code == 404, 30, 1, "never expired")
    assert [e["type"] for e in events(lux, exp["id"])][-1] == "server.expired"
    lux.run("cancel", r)


def test_sync_running_with_after_sync_and_the_dirty_rule(lux, runners, hosts, fake_image, git_server):
    git_server.create("app", {"message.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    hot = create(lux, name="hot", port=8080, command=APP, runId=run_id)
    cold = create(lux, name="cold", port=8081, command=["lux-fake", "serve", "8081", "cold"],
                  afterSync=["sh", "-c", "echo after-sync ran > /workspace/after-sync"], runId=run_id)
    wait_state(lux, hot["id"], "ready")
    wait_state(lux, cold["id"], "ready")
    hot_epoch_gen = get(lux, hot["id"])["since"]
    # Tracked change in the checkout plus an untracked file: reset to the
    # ref, the change saved, the untracked file kept.
    exec_in(lux, run_id, "sh", "-c",
            "echo local > /workspace/app/message.txt && echo keep > /workspace/app/untracked.txt")
    two = git_server.commit_on("app", "main", "message.txt", "two\n")
    out = lux.json("sync", run_id, "app=main", "--wait", timeout=120)
    assert out[0]["status"] == "reset" and out[0]["dirty"] and out[0]["to"] == two and out[0]["saved"] == "refs/lux/pre-sync", out
    files = exec_in(lux, run_id, "sh", "-c",
                    "cat /workspace/app/message.txt /workspace/app/untracked.txt; git -C /workspace/app show refs/lux/pre-sync:message.txt").stdout
    assert files.split() == ["two", "keep", "local"], files
    # The server with afterSync ran it and restarted; the other kept running.
    wait_until(lambda: "after-sync ran" in exec_in(lux, run_id, "cat", "/workspace/after-sync", check=False).stdout,
               60, 1, "afterSync never ran")
    wait_state(lux, cold["id"], "ready")
    assert get(lux, hot["id"])["since"] == hot_epoch_gen
    # Fast-forward, and a ref that does not exist: failed, the Run goes on.
    three = git_server.commit_on("app", "main", "message.txt", "three\n")
    out = lux.json("sync", run_id, "app=main", "--wait", timeout=120)
    assert out[0]["status"] == "fast-forward" and out[0]["to"] == three, out
    with pytest.raises(CLIError):
        lux.run("sync", run_id, "app=no-such-branch", "--wait", timeout=120)
    failed = lux.events(run_id, "git.sync")[-1]["data"]
    assert failed["status"] == "failed" and "not found" in failed["error"], failed
    assert lux.get(run_id)["state"] == "running"
    # On resume, a sync that fails does not strand the Run.
    lux.run("stop", run_id, "--wait", timeout=120)
    lux.run("resume", run_id, "--sync", "app=nope", "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    assert lux.get(run_id)["state"] == "running"
    assert lux.events(run_id, "git.sync")[-1]["data"]["status"] == "failed"
    wait_state(lux, hot["id"], "ready")
    lux.run("cancel", run_id)


# Drops every commit of the checkout's but a new root one, so it no longer
# has the commit lux knows it at (its clone or its last sync).
FORGET_HISTORY = ("cd /workspace/app && b=forget$(date +%s%N) && git checkout -q --orphan $b"
                  " && git -c user.name=t -c user.email=t@t commit -qm fresh"
                  " && git for-each-ref --format='%(refname)' refs/heads refs/remotes refs/lux | grep -vx refs/heads/$b"
                  " | xargs -r -n1 git update-ref -d && git reflog expire --expire=now --all && git gc -q --prune=now")


def test_sync_bundles_only_new_history_and_falls_back(lux, runners, hosts, fake_image, git_server):
    """A sync bundles only what the checkout lacks; a checkout without its
    known base gets the whole history on one retry, running or before init.
    No bundle stays on the runtime volume."""
    git_server.create("app", {"message.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    no_bundles = "test -z \"$(ls -A /.lux/run/sync 2>/dev/null)\" && echo none"
    two = git_server.commit_on("app", "main", "message.txt", "two\n")
    out = lux.json("sync", run_id, "app=main", "--wait", timeout=120)
    assert out[0]["status"] == "fast-forward" and out[0]["to"] == two and not out[0].get("fullBundle"), out
    assert exec_in(lux, run_id, "sh", "-c", no_bundles).stdout.strip() == "none"
    # Running: the checkout lost its base; the incremental bundle cannot be
    # fetched, the whole history is.
    exec_in(lux, run_id, "sh", "-c", FORGET_HISTORY)
    three = git_server.commit_on("app", "main", "message.txt", "three\n")
    out = lux.json("sync", run_id, "app=main", "--wait", timeout=120)
    assert out[0]["status"] == "reset" and out[0]["to"] == three and out[0]["diverged"] and out[0]["fullBundle"], out
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout == "three\n"
    assert exec_in(lux, run_id, "sh", "-c", no_bundles).stdout.strip() == "none"
    # Before init on a resume: the same retry, through the runner.
    exec_in(lux, run_id, "sh", "-c", FORGET_HISTORY)
    lux.run("stop", run_id, "--wait", timeout=120)
    four = git_server.commit_on("app", "main", "message.txt", "four\n")
    lux.run("resume", run_id, "--sync", "app=main", "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    res = resume_sync(lux, run_id, four)
    assert res["status"] == "reset" and res["fullBundle"], res
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout == "four\n"
    assert exec_in(lux, run_id, "sh", "-c", no_bundles).stdout.strip() == "none"
    lux.run("cancel", run_id)


# Makes the checkout shallow at its HEAD: every commit before it (the clone
# commit among them), other refs and reflogs go.
SHALLOW_AT_HEAD = ("cd /workspace/app && b=$(git symbolic-ref --short HEAD) && git rev-parse HEAD > .git/shallow"
                   " && git for-each-ref --format='%(refname)' | grep -vx refs/heads/$b"
                   " | xargs -r -n1 git update-ref -d && git reflog expire --expire=now --all && git gc -q --prune=now")


def test_a_synced_checkout_is_the_base_after_a_resume(lux, runners, hosts, fake_image, git_server):
    """A sync's commit outlives its placement: after a resume, lux diff
    diffs from it (upstream changes are not the workload's), and the next
    sync bundles only what came after it."""
    a = git_server.create("app", {"message.txt": "one\n"})
    runners.start(hosts[0])
    run_id = lux.submit(preview_spec(fake_image, git_server))
    lux.wait_state(run_id, "running")
    b = git_server.commit_on("app", "main", "message.txt", "two\n")
    out = lux.json("sync", run_id, "app=main", "--wait", timeout=120)
    assert out[0]["status"] == "fast-forward" and out[0]["to"] == b, out
    # A resume without a sync: the diff is from b, and empty.
    lux.run("stop", run_id, "--wait", timeout=120)
    lux.run("resume", run_id, "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    d = lux.json("diff", run_id)["repos"][0]
    assert d["base"] == b and d["head"] == b and d["files"] == 0 and not d.get("patch"), d
    # The checkout keeps b and drops a, its clone commit: a bundle after a
    # would need a whole-history retry, one after b does not.
    exec_in(lux, run_id, "sh", "-c", SHALLOW_AT_HEAD)
    lacks = exec_in(lux, run_id, "git", "-C", "/workspace/app", "cat-file", "-e", a + "^{commit}", check=False)
    assert lacks.returncode != 0, "the checkout still has its clone commit"
    # Resumed with a sync: the bundle's base is b, so no whole-history retry.
    lux.run("stop", run_id, "--wait", timeout=120)
    c = git_server.commit_on("app", "main", "message.txt", "three\n")
    lux.run("resume", run_id, "--sync", "app=main", "--secret", f"GIT_TOKEN={git_server.token}", "--wait", timeout=180)
    res = resume_sync(lux, run_id, c)
    assert res["status"] == "fast-forward" and res["from"] == b and not res.get("fullBundle") and not res.get("missingBase"), res
    assert exec_in(lux, run_id, "cat", "/workspace/app/message.txt").stdout == "three\n"
    lux.run("cancel", run_id)


def test_hostnames_and_tenant_isolation(lux, tenant_factory, runners, hosts, fake_image):
    sv = create(lux, name="web", port=8080, hostname=f"web.iso.{PREVIEW_DOMAIN}")
    other = tenant_factory()
    r = api(other, "POST", "/v1/servers", {"name": "web", "port": 1, "hostname": f"web.iso.{PREVIEW_DOMAIN}"})
    assert r.status_code == 409 and r.json()["error"]["code"] == "hostname_taken", r.text
    assert api(lux, "POST", "/v1/servers", {"name": "x", "port": 1, "hostname": "web.example.org"}).status_code == 422
    assert api(other, "GET", f"/v1/servers/{sv['id']}").status_code == 404
    assert api(other, "POST", f"/v1/servers/{sv['id']}/tickets").status_code == 404
    assert api(other, "GET", "/v1/servers").json()["servers"] == []
    # Another tenant's ticket does not sign in to it.
    runners.start(hosts[0])
    theirs = create(other, name="t", port=1)
    t = api(other, "POST", f"/v1/servers/{theirs['id']}/tickets").json()["ticket"]
    b = Browser(lux, sv)
    assert b.get(f"/.lux/auth?ticket={t}&to=/").status_code == 401


def test_feed_resumes_with_server_events(lux, runners, hosts, fake_image):
    """GET /v1/events: server events among Run events, resumable with
    Last-Event-ID; runId null for an unattached server."""
    def feed(last: str | None) -> list[tuple[str, dict]]:
        headers = {"Authorization": f"Bearer {lux.api_key}"}
        if last is not None:
            headers["Last-Event-ID"] = last
        r = requests.get(f"{lux.env.luxd_url}/v1/events", params={"follow": "false", **({} if last else {"last": "50"})},
                         headers=headers, timeout=30)
        out, eid = [], None
        for line in r.text.splitlines():
            if line.startswith("id: "):
                eid = line[4:]
            elif line.startswith("data: "):
                out.append((eid, json.loads(line[6:])))
        return out
    sv = create(lux, name="web", port=8080, wake="request")
    first = [e for e in feed(None) if e[1].get("serverId") == sv["id"]]
    assert first and first[-1][1]["type"] == "server.created" and first[-1][1]["runId"] is None, first
    last_id = first[-1][0]
    b = Browser(lux, sv).sign_in(sv["id"])
    b.get("/")
    after = feed(last_id)
    assert [e["type"] for _, e in after if e.get("serverId") == sv["id"]] == ["server.wake_requested"], after
    d = after[-1][1]["data"]
    assert d["hostname"] == sv["hostname"] and d["url"] == sv["url"] and d["by"], d
