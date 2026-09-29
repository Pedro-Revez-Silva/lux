"""Scheduling: where Runs go, why they wait, and what they may touch."""

from __future__ import annotations

import time

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_unplaceable_runs_do_not_block_the_queue(lux, runners, hosts):
    """Runs no host can take must not hide the ones behind them."""
    runners.start(hosts[0])
    blocked = [lux.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"gpu": "yes"}}))
               for _ in range(25)]
    ok = lux.submit(generic(ALPINE_IMAGE, "echo", "placed"))
    assert lux.wait_state(ok, "succeeded", "failed", timeout=60)["state"] == "succeeded"
    # And the waiting ones say why, with the reason committed.
    run = lux.get(blocked[0])
    assert run["state"] == "submitted"
    assert run["stateReason"] == "no host matches", run


def test_drain_only_touches_the_callers_host(tenant_factory, runners, hosts, env):
    """Two tenants each have a host called host-x; draining one must not
    stop the other's Runs."""
    from conftest import Runners
    a, b = tenant_factory(), tenant_factory()
    ra, rb = Runners(env, a), Runners(env, b)
    try:
        # Both register as host-x, each in its own tenant.
        ra.start(hosts[0], name="host-x")
        rb.start(hosts[1], name="host-x")
        run_b = b.submit(generic(ALPINE_IMAGE, "sleep", "300"))
        b.wait_state(run_b, "running")
        a.run("hosts", "drain", "host-x")
        time.sleep(3)
        assert b.get(run_b)["state"] == "running", "tenant A's drain stopped tenant B's Run"
        b.run("cancel", run_b)
    finally:
        ra.stop_all()
        rb.stop_all()


def test_drain_moves_runs_elsewhere(lux, runners, hosts, fake_image):
    from conftest import fake_agent
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "write f.txt moved\necho ready"))
    lux.wait_output(run_id, "ready")
    lux.wait_activity(run_id, "idle")
    runners.start(hosts[1])
    lux.run("hosts", "drain", hosts[0].name, "--force-evict")
    # Stopped with reason drain, then automatically resumed on the other host.
    run = wait_until(lambda: (lambda r: r if len(r["placements"]) == 2 and r["state"] == "running" else None)(lux.get(run_id)),
                     90, 0.5, "drained Run never resumed elsewhere")
    assert [p["hostName"] for p in run["placements"]] == [hosts[0].name, hosts[1].name], run["placements"]
    assert run["placements"][0]["stopReason"] == "drain"
    lux.run("steer", run_id, "read f.txt")
    lux.wait_output(run_id, "moved")
    hs = {h["name"]: h for h in lux.json("hosts", "ls")}
    assert hs[hosts[0].name]["draining"]
    assert hs[hosts[0].name]["times"]["drainRequested"]


def test_plain_drain_leaves_running_run_and_places_new_elsewhere(lux, runners, hosts):
    """Without --force-evict, drain only cordons the host: its running Run
    keeps its one placement, and a new Run lands on the other host."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    lux.wait_state(run_id, "running")
    runners.start(hosts[1])
    lux.run("hosts", "drain", hosts[0].name)
    h = wait_until(lambda: (lambda x: x if x["draining"] else None)(lux.json("hosts", "get", hosts[0].name)),
                   15, 0.3, "the host was never cordoned")
    assert h["times"]["drainRequested"]
    run = lux.get(run_id)
    assert run["state"] == "running" and len(run["placements"]) == 1, run
    assert run["placements"][0]["hostName"] == hosts[0].name
    assert not run["placements"][0].get("stopRequestedAt"), run["placements"][0]
    other = lux.submit(generic(ALPINE_IMAGE, "echo", "elsewhere"))
    lux.wait_state(other, "succeeded")
    assert lux.get(other)["placements"][0]["hostName"] == hosts[1].name
    lux.run("cancel", run_id)


def test_force_evict_on_an_already_draining_host_moves_its_run(lux, runners, hosts):
    """--force-evict on a host that is already draining (a plain drain
    happened first) still evicts its current placement."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    lux.wait_state(run_id, "running")
    lux.run("hosts", "drain", hosts[0].name)
    wait_until(lambda: lux.json("hosts", "get", hosts[0].name)["draining"], 15, 0.3, "plain drain never took")
    run = lux.get(run_id)
    assert run["state"] == "running", "the plain drain stopped the Run"
    assert not run["placements"][0].get("stopRequestedAt"), run["placements"][0]
    runners.start(hosts[1])
    lux.run("hosts", "drain", hosts[0].name, "--force-evict")
    run = wait_until(lambda: (lambda r: r if len(r["placements"]) == 2 and r["state"] == "running" else None)(lux.get(run_id)),
                     90, 0.5, "force-evicted Run never resumed elsewhere")
    assert [p["hostName"] for p in run["placements"]] == [hosts[0].name, hosts[1].name], run["placements"]
    assert run["placements"][0]["stopReason"] == "drain"
    lux.run("cancel", run_id)


def test_cancel_right_after_submit_is_not_lost(lux, runners, hosts):
    """A cancel that reaches the runner together with its assign must still
    stop the Run."""
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "300"))
    lux.run("cancel", run_id)
    run = lux.wait_state(run_id, "cancelled", timeout=60)
    assert run["state"] == "cancelled"


def test_resumed_failed_run_is_not_finished(lux, runners, hosts):
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "test -f /d/second && sleep 300; touch /d/second; exit 1",
                   volumes=[{"name": "d", "path": "/d", "kind": "state"}])
    run_id = lux.submit(spec)
    assert lux.wait_state(run_id, "failed")["finishedAt"]
    lux.run("resume", run_id)
    run = lux.wait_state(run_id, "running")
    assert not run.get("finishedAt"), run
    lux.run("cancel", run_id)


def test_resume_retry_keeps_secrets(lux, runners, hosts):
    """A second resume while the first is pending must not wipe the secret
    values the first supplied."""
    spec = generic(ALPINE_IMAGE, "sh", "-c", 'echo "len=${#TOKEN}"; sleep 300',
                   secrets=[{"name": "TOKEN", "value": "abcdef123456"}])
    run_id = lux.submit(spec)
    lux.run("stop", run_id)  # stops before any host exists
    assert lux.get(run_id)["state"] == "stopped"
    lux.run("resume", run_id, "--secret", "TOKEN=abcdef123456")
    # Retried without secrets: an idempotent no-op that keeps the first's.
    lux.run("resume", run_id)
    runners.start(hosts[0])
    lux.wait_output(run_id, "len=12")
    lux.run("cancel", run_id)


def test_a_run_naming_no_pool_goes_to_the_tenants_default(lux, runners, hosts):
    """The tenant marks a pool not named "default" as its default: a Run
    naming no pool is placed there, its spec and submitted event say so,
    and it stays there after the default moves."""
    runs: list[str] = []
    try:
        lux.run("pools", "set", "arm64", "--provider", "static")
        lux.run("pools", "set", "other", "--provider", "static")
        runners.start(hosts[0], token=runners.token("--pool", "arm64"))
        # Nothing marked: the pool named "default", where no host is.
        before = lux.submit(generic(ALPINE_IMAGE, "true"))
        runs.append(before)
        assert lux.get(before)["spec"]["placement"]["pool"] == "default"
        data = lux.events(before, "submitted")[0]["data"]
        assert data["poolFrom"] == "fallback" and "poolOwner" not in data, data

        lux.run("pools", "set", "arm64", "--default")
        assert [p["name"] for p in lux.json("pools", "ls") if p.get("isDefault")] == ["arm64"]
        # The table: NAME DEFAULT PROVIDER ..., DEFAULT empty but for "*".
        header, *lines = lux.run("pools", "ls").stdout.splitlines()
        assert header.split()[:3] == ["NAME", "DEFAULT", "PROVIDER"], header
        marks = {line.split()[0]: line.split()[1] == "*" for line in lines}
        assert marks == {"arm64": True, "other": False}, lines

        run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo on-default; sleep 300"))
        runs.append(run_id)
        run = lux.wait_state(run_id, "running")
        assert run["host"] == hosts[0].name and run["spec"]["placement"]["pool"] == "arm64", run
        data = lux.events(run_id, "submitted")[0]["data"]
        assert data == {**data, "pool": "arm64", "poolFrom": "tenant-default", "poolOwner": "tenant"}, data

        # Moving the default leaves the submitted Run where it is.
        lux.run("pools", "set", "other", "--default")
        assert [p["name"] for p in lux.json("pools", "ls") if p.get("isDefault")] == ["other"]
        assert lux.get(run_id)["spec"]["placement"]["pool"] == "arm64"
        moved = lux.submit(generic(ALPINE_IMAGE, "true"))
        runs.append(moved)
        assert lux.get(moved)["spec"]["placement"]["pool"] == "other"
    finally:
        for r in runs:
            lux.run("cancel", r, check=False)
        for pool in ("arm64", "other"):
            lux.run("pools", "rm", pool, check=False)
