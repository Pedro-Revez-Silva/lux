"""EC2 pools, spot instances: an interruption notice moves the host's Runs
to another host, in time, even when a stop is already under way."""

from __future__ import annotations

import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, pool  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


def ended_or_replaced(run):
    """run, once it is no longer running or stopping, or has a second
    placement; else None."""
    return run if run["state"] not in ("running", "stopping") or len(run["placements"]) > 1 else None


def running_again(run):
    """run, once it runs on a second placement; else None."""
    return run if len(run["placements"]) == 2 and run["state"] == "running" else None


def test_a_spot_interruption_moves_runs_to_another_host(lux, ec2):
    """EC2 takes a spot instance back, with two minutes' notice. Its runner
    sees the notice; the Run on it stops, snapshots, and resumes on a new
    instance with its state, before the old one is gone. Nothing is lost."""
    fake_only(ec2)
    pool(lux, ec2, spot=True, max=2)
    script = "echo start >> /w/log; echo starts=$(wc -l < /w/log); trap 'exit 0' TERM; while :; do sleep 1; done"
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"},
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}])
    spec["workload"]["grace"] = "30s"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "starts=1", timeout=120)
    (inst,) = ec2.running()
    assert inst["market"] == "spot", inst

    ec2.interrupt(inst["id"], seconds=40)
    # Moved before EC2 takes the instance: a new placement, its state carried.
    lux.wait_output(run_id, "starts=2", timeout=85)
    run = lux.get(run_id)
    assert run["state"] == "running", run
    first, second = run["placements"][-2:]
    assert first["host"] != second["host"], run["placements"]
    assert first["stopReason"] == "preempt" and first["state"] == "exited", first
    # The interrupted host was drained and let go of, not written off.
    wait_until(lambda: inst["id"] not in {i["id"] for i in ec2.running()}, 90, 0.3, "the interrupted instance stayed")
    lux.run("cancel", run_id, "--wait")


def test_a_spot_interruption_fails_a_run_that_is_never_resumed(lux, ec2):
    """A one-shot Run (resumePolicy: never) on a spot instance EC2 takes
    back is stopped and ends failed, saying why; it is not placed again,
    even with room for it in the pool."""
    fake_only(ec2)
    pool(lux, ec2, spot=True, max=2)
    spec = generic(ALPINE_IMAGE, "sh", "-c", "echo up; trap 'exit 0' TERM; while :; do sleep 1; done",
                   placement={"pool": "burst"}, resumePolicy="never")
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "up", timeout=120)
    (inst,) = ec2.running()

    ec2.interrupt(inst["id"], seconds=40)
    run = wait_until(lambda: ended_or_replaced(lux.get(run_id)), 85, 0.3, "the interrupted Run never ended")
    assert run["state"] == "failed", run
    assert run["stateReason"] == "preempt: not resumed (resumePolicy never)", run
    assert run["placements"][-1]["stopReason"] == "preempt", run["placements"]
    wait_until(lambda: inst["id"] not in {i["id"] for i in ec2.running()}, 90, 0.3, "the interrupted instance stayed")
    run = lux.get(run_id)
    assert run["state"] == "failed" and len(run["placements"]) == 1, run
    assert not [e for e in lux.events(run_id, "state") if e["data"]["state"] == "resuming"]
    assert ec2.running() == [], "an instance was launched for a Run that is not resumed"


def test_a_spot_interruption_restarts_a_restart_run_from_scratch(lux, ec2):
    """A Run with resumePolicy: restart is started again on another
    instance from scratch: its state volume is empty there (what the first
    placement wrote is not restored) and its command runs from the start."""
    fake_only(ec2)
    pool(lux, ec2, spot=True, max=2)
    script = "echo start >> /w/log; echo starts=$(wc -l < /w/log); trap 'exit 0' TERM; while :; do sleep 1; done"
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"},
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}], resumePolicy="restart")
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "starts=1", timeout=120)
    (inst,) = ec2.running()

    ec2.interrupt(inst["id"], seconds=40)
    run = wait_until(lambda: running_again(lux.get(run_id)), 120, 0.5, "the interrupted Run never ran again")
    first, second = run["placements"]
    assert first["host"] != second["host"] and first["stopReason"] == "preempt", run["placements"]
    states = [e["data"] for e in lux.events(run_id, "state")]
    assert {"state": "resuming", "reason": "auto-restart after preempt"} in states, states
    # Scheduled from no snapshot: its volumes start empty.
    assert [d for d in states if d["state"] == "scheduled"][-1].get("snapshotId") is None, states
    wait_until(lambda: lux.logs(run_id).count("starts=1") == 2, 60, 0.5, "the restart did not start from an empty volume")
    assert "starts=2" not in lux.logs(run_id)
    lux.run("cancel", run_id, "--wait")


def test_a_spot_interruption_shortens_a_stop_already_under_way(lux, ec2):
    """A Run already stopping with a long grace when the notice comes still
    snapshots and uploads before the instance goes."""
    fake_only(ec2)
    pool(lux, ec2, spot=True, max=1)
    # Ignores SIGTERM: only the grace's SIGKILL ends it.
    script = "trap '' TERM; echo kept > /w/f; echo up; while :; do sleep 1; done"
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"},
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}])
    spec["workload"]["grace"] = "10m"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "up", timeout=120)
    (inst,) = ec2.running()
    lux.run("stop", run_id)
    wait_until(lambda: lux.get(run_id)["state"] == "stopping", 30, 0.3, "never stopping")
    ec2.interrupt(inst["id"], seconds=16)
    run = lux.wait_state(run_id, "stopped", timeout=15)
    assert run["placements"][-1]["exitReason"] != "lost", run
    wait_until(lambda: (s := lux.json("snapshots", run_id)) and s[-1]["uploaded"], 30, 0.5, "snapshot not uploaded in time")
