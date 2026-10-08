"""Operator actions: migrate a Run, inspect and force-resume a stopped one,
and watch every Run's events. Tenants keep their own limits."""

from __future__ import annotations

import json
import time

import pytest
import requests

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, wait_until


def test_operator_migrates_an_agent_to_another_host(operator, lux, runners, hosts, fake_image):
    """A migration is a stop and an immediate resume elsewhere: the agent's
    state moves with it, and the input is delivered on arrival."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(fake_agent(fake_image, "write f.txt moved\necho ready"))
    lux.wait_output(run_id, "ready")
    lux.wait_activity(run_id, "idle")
    first = lux.get(run_id)["host"]
    other = next(h.name for h in hosts[:2] if h.name != first)
    # Tenants cannot migrate.
    with pytest.raises(CLIError) as e:
        lux.run("migrate", run_id)
    assert "operator" in e.value.stderr
    run = operator.json("migrate", run_id, "--to", other, "--input", "read f.txt", "--wait", timeout=200)
    assert run["host"] == other and run["state"] == "running", run
    assert lux.get(run_id)["placements"][0]["stopReason"] == "migrate"
    lux.wait_output(run_id, "moved")
    assert lux.events(run_id, "migrate.requested")
    lux.run("terminate", run_id)


def test_migrate_without_a_target_avoids_the_current_host(operator, lux, runners, hosts):
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    first = lux.wait_state(run_id, "running")["host"]
    run = operator.json("migrate", run_id, "--wait", timeout=200)
    assert run["host"] != first, run
    lux.run("terminate", run_id)


def test_migrate_refuses_what_it_cannot_do(operator, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    host = lux.wait_state(run_id, "running")["host"]
    with pytest.raises(CLIError) as e:
        operator.run("migrate", run_id, "--to", host)
    assert "already on that host" in e.value.stderr
    lux.run("stop", run_id, "--wait")
    with pytest.raises(CLIError) as e:
        operator.run("migrate", run_id)
    assert "resume --to" in e.value.stderr
    lux.run("terminate", run_id)


def test_operator_inspects_and_force_resumes_a_stopped_run(operator, lux, runners, hosts):
    """A stopped Run says what a resume would take. While luxd still holds
    its secrets, an operator resumes it without them, on a host it chooses;
    a tenant still has to supply them, and cannot choose."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    token = "operator-held-1"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", 'echo "len=${#TOKEN}"; sleep 300',
                                secrets=[{"name": "TOKEN", "value": token}],
                                volumes=[{"name": "d", "path": "/d", "kind": "state"}]))
    lux.wait_output(run_id, f"len={len(token)}")
    lux.run("stop", run_id, "--wait")
    run = operator.get(run_id)
    rs = run["resume"]
    assert rs["secrets"] == ["TOKEN"] and rs["secretsHeld"], rs
    assert rs["snapshot"] and not rs.get("blockers"), rs
    assert "held by luxd" in operator.run("get", run_id).stdout
    assert run_id in {r["id"] for r in operator.json("ls", "--resumable", "--tenant", lux.tenant_id)}
    with pytest.raises(CLIError):
        lux.run("resume", run_id)
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--secret", f"TOKEN={token}", "--to", hosts[1].name)
    assert "operator" in e.value.stderr
    first = run["placements"][-1]["hostName"]
    other = next(h.name for h in hosts[:2] if h.name != first)
    operator.run("resume", run_id, "--to", other)
    assert lux.wait_state(run_id, "running")["host"] == other
    wait_until(lambda: lux.logs(run_id).count(f"len={len(token)}") == 2, 60, 0.5, "resumed Run did not get its secret")
    lux.run("terminate", run_id)


def test_force_resume_is_refused_once_luxd_forgot_the_secrets(env, operator, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300", secrets=[{"name": "TOKEN", "value": "v"}]))
    lux.wait_state(run_id, "running")
    lux.run("stop", run_id, "--wait")
    env.stop_luxd()
    env.start_luxd()
    assert not operator.get(run_id)["resume"]["secretsHeld"]
    with pytest.raises(CLIError) as e:
        operator.run("resume", run_id)
    assert "only the tenant can resume it" in e.value.stderr
    lux.run("terminate", run_id)


def _feed(lux, after: int, *args: str) -> list[dict]:
    out = lux.run("events", "--all", "--follow=false", "--after", str(after), "-o", "json", *args).stdout
    return [json.loads(l) for l in out.splitlines()]


def test_events_feed(operator, tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    start = max([e["id"] for e in _feed(operator, 0)] or [0])
    ra = a.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    rb = b.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    assert {ra, rb} <= {e["runId"] for e in _feed(operator, start)}
    assert {e["runId"] for e in _feed(operator, start, "--tenant", a.tenant_id)} & {ra, rb} == {ra}
    # A tenant's feed is its own.
    assert {e["runId"] for e in _feed(b, start)} & {ra, rb} == {rb}
    # Followed: new events arrive as they happen, pushed (not polled: well
    # under the feed's fallback of 5s).
    # Streamed from just before a's last event: that one arriving shows the
    # stream has caught up to now.
    last = max(e["id"] for e in _feed(operator, start, "--tenant", a.tenant_id))
    p = operator.popen("events", "--all", "-o", "json", "--tenant", a.tenant_id, "--after", str(last - 1))
    try:
        assert json.loads(p.stdout.readline())["id"] == last
        start = time.monotonic()
        a.run("terminate", ra)
        assert json.loads(p.stdout.readline())["runId"] == ra
        assert time.monotonic() - start < 2.5, "the event was not pushed"
    finally:
        p.kill()
    b.run("terminate", rb)


def test_migrate_with_nowhere_else_to_go_comes_back(operator, lux, runners, hosts):
    """Avoiding the host it left is a preference: with no other host, the
    migrated Run runs there again rather than waiting forever."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    host = lux.wait_state(run_id, "running")["host"]
    run = operator.json("migrate", run_id, "--wait", timeout=200)
    assert run["host"] == host and run["epoch"] == 2, run
    lux.run("terminate", run_id)


def test_migrate_refuses_a_run_already_stopping(operator, lux, runners, hosts):
    """One reason per stop: a Run being stopped is not also migrated, and
    a tenant's stop wins over a migration in flight."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'sleep 3; exit 0' TERM; sleep 300 & wait"))
    lux.wait_state(run_id, "running")
    operator.run("migrate", run_id)
    with pytest.raises(CLIError) as e:
        operator.run("migrate", run_id)
    assert "already being stopped" in e.value.stderr
    lux.run("stop", run_id)
    run = lux.wait_state(run_id, "stopped", timeout=60)
    assert run["placements"][-1]["stopReason"] == "stop", run
    time.sleep(3)
    assert lux.get(run_id)["state"] == "stopped", "the tenant's stop was overridden"
    lux.run("terminate", run_id)


def test_migrate_leaves_a_forced_eviction_to_the_drain(operator, lux, runners, hosts):
    """A force-evicted host's Runs are already being moved: a migration
    without a target would only race the eviction. A plainly drained
    (cordoned) host is not moving its Runs, so migrate still works there."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'sleep 5; exit 0' TERM; sleep 300 & wait"))
    host = lux.wait_state(run_id, "running")["host"]
    lux.run("hosts", "drain", host, "--force-evict")
    with pytest.raises(CLIError) as e:
        operator.run("migrate", run_id)
    assert "already being stopped" in e.value.stderr
    lux.run("terminate", run_id)


def test_migrate_a_run_on_a_plainly_drained_host_succeeds(operator, lux, runners, hosts):
    """A plain drain only cordons its host: nothing is moving the Run, so
    an operator can still migrate it off elsewhere."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    host = lux.wait_state(run_id, "running")["host"]
    lux.run("hosts", "drain", host)
    run = operator.json("migrate", run_id, "--wait", timeout=200)
    assert run["host"] != host and run["state"] == "running", run
    lux.run("terminate", run_id)


def ended_or_replaced(run):
    """run, once it is no longer running or stopping, or has a second
    placement; else None."""
    return run if run["state"] not in ("running", "stopping") or len(run["placements"]) > 1 else None


def test_a_run_never_resumed_is_not_migrated_and_terminated_when_force_evicted(operator, lux, runners, hosts):
    """resumePolicy: never is for one-shot work that cannot continue on
    another host. migrate refuses it and leaves it running; a cordon-only
    drain leaves it running too; a force-evicting drain stops it, and it
    ends terminated, not placed again, though another host is free."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "trap 'exit 0' TERM; sleep 300 & wait", resumePolicy="never"))
    host = lux.wait_state(run_id, "running")["host"]
    with pytest.raises(CLIError) as e:
        operator.run("migrate", run_id)
    assert "resumePolicy is never" in e.value.stderr, e.value.stderr
    run = lux.get(run_id)
    assert run["state"] == "running" and not run["placements"][0].get("stopRequestedAt"), run

    lux.run("hosts", "drain", host)
    wait_until(lambda: lux.json("hosts", "get", host)["draining"], 15, 0.3, "plain drain never took")
    run = lux.get(run_id)
    assert run["state"] == "running" and not run["placements"][0].get("stopRequestedAt"), run

    lux.run("hosts", "drain", host, "--force-evict")
    run = wait_until(lambda: ended_or_replaced(lux.get(run_id)), 60, 0.3, "the force-evicted Run never ended")
    assert run["state"] == "terminated", run
    assert run["stateReason"] == "drain: not resumed (resumePolicy never)", run
    assert run["placements"][0]["stopReason"] == "drain", run["placements"]
    # Waits out several scheduler passes (luxd's tick is 1s): none places it.
    time.sleep(3)
    run = lux.get(run_id)
    assert run["state"] == "terminated" and len(run["placements"]) == 1, run
    assert not [e for e in lux.events(run_id, "state") if e["data"]["state"] == "resuming"]

    # never: no one may resume it, its tenant or an operator.
    for who in (lux, operator):
        with pytest.raises(CLIError) as e:
            who.run("resume", run_id)
        assert e.value.code != 0 and "resumePolicy never: this Run cannot be resumed" in e.value.stderr, e.value.stderr
    r = requests.post(f"{lux.env.luxd_url}/v1/runs/{run_id}/resume", json={},
                      headers={"Authorization": f"Bearer {lux.api_key}"}, timeout=10)
    assert r.status_code == 409 and r.json()["error"]["code"] == "not_resumable", r.text
    assert run_id not in {r["id"] for r in lux.json("ls", "--resumable")}
    # Waits out scheduler passes after the refused resumes (tick 1s): none placed it.
    time.sleep(2)
    run = lux.get(run_id)
    assert run["state"] == "terminated" and len(run["placements"]) == 1, run


def test_a_run_never_resumed_that_succeeds_ends_terminated(lux, runners, hosts):
    """A resumePolicy: never Run can never be resumed, so when it succeeds
    it ends terminated rather than resting succeeded: its exit code kept,
    its outcome in the reason, not resumable, its resume refused."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo done", resumePolicy="never"))
    run = lux.wait_state(run_id, "terminated", "succeeded", "failed", timeout=60)
    assert run["state"] == "terminated", run
    assert run["stateReason"] == "succeeded; resumePolicy never", run
    assert run["exitCode"] == 0 and run["resumable"] is False, run
    assert [e["data"]["state"] for e in lux.events(run_id, "state")][-1] == "terminated"
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id)
    assert "resumePolicy never: this Run cannot be resumed" in e.value.stderr, e.value.stderr


def test_migrate_restarts_a_restart_run_from_scratch(operator, lux, runners, hosts):
    """A Run with resumePolicy: restart can be migrated: it starts again on
    the target host from scratch, with an empty state volume, its command
    run from the start."""
    runners.start(hosts[0])
    runners.start(hosts[1])
    script = "echo start >> /w/log; echo starts=$(wc -l < /w/log); trap 'exit 0' TERM; sleep 300 & wait"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, resumePolicy="restart",
                                volumes=[{"name": "w", "path": "/w", "kind": "state"}]))
    lux.wait_output(run_id, "starts=1")
    first = lux.get(run_id)["host"]
    other = next(h.name for h in hosts[:2] if h.name != first)
    run = operator.json("migrate", run_id, "--to", other, "--wait", timeout=200)
    assert run["host"] == other and run["state"] == "running", run
    assert run["placements"][0]["stopReason"] == "migrate", run["placements"]
    states = [e["data"] for e in lux.events(run_id, "state")]
    assert {"state": "resuming", "reason": "auto-restart after migrate"} in states, states
    assert [d for d in states if d["state"] == "scheduled"][-1].get("snapshotId") is None, states
    wait_until(lambda: lux.logs(run_id).count("starts=1") == 2, 60, 0.5, "the restart did not start from an empty volume")
    assert "starts=2" not in lux.logs(run_id)
    lux.run("terminate", run_id)
