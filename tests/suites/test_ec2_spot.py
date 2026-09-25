"""EC2 pools, spot instances: an interruption notice moves the host's Runs
to another host, in time, even when a stop is already under way."""

from __future__ import annotations

import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, pool  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


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
