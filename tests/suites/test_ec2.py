"""Step 13: EC2 pools. luxd launches hosts when Runs wait for them, keeps
warm and minimum hosts, drains and terminates idle ones, and replaces
hosts EC2 lost. Against a fake EC2 (the real provider code, a fake API);
--real-ec2 runs them against AWS."""

from __future__ import annotations

import json
import time

import pytest

from conftest import fake_only, generic
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


@pytest.fixture(autouse=True)
def _clean(lux, ec2):
    """Each test's pool is removed after it, and its instances with it."""
    yield
    lux.run("pools", "rm", "burst", check=False)
    wait_until(lambda: not ec2.running(), 90, 1, "the removed pool's instances were not terminated")


def pool(lux, ec2, name="burst", spot=False, **kw):
    template = {**ec2.template, "spot": True} if spot else ec2.template
    args = ["pools", "set", name, "--provider", "ec2", "--template", json.dumps(template)]
    for k, v in kw.items():
        args += [f"--{k}", str(v)]
    lux.run(*args)


def ec2_hosts(lux, pool_name="burst", states=("ready",)):
    return [h for h in lux.json("hosts", "ls") if h["pool"] == pool_name and h["state"] in states]


def test_a_waiting_run_gets_a_host_launched(lux, ec2):
    pool(lux, ec2, max=2)
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "on-ec2", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert "on-ec2" in lux.logs(run_id)
    [inst] = ec2.running()
    assert inst["launchTemplate"] == ec2.template["launchTemplate"]
    assert inst["tags"]["lux:pool"].endswith("/burst")  # tenant pools: <tenant>/<name>
    host = lux.get(run_id)["placements"][0]["hostName"]
    assert host == inst["tags"]["Name"], (host, inst["tags"])
    # Idle past the scale-down delay: drained, then terminated.
    wait_until(lambda: not ec2.running(), 60, 1, "the idle host was never terminated")
    wait_until(lambda: not ec2_hosts(lux, states=("ready", "draining")), 30, 1, "the host is still listed")


def test_max_hosts_is_respected(lux, ec2):
    fake_only(ec2)
    pool(lux, ec2, max=1)
    runs = [lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "sleep 3", placement={"pool": "burst"},
                               resources={"cpus": 0.5})) for _ in range(3)]
    for r in runs:
        lux.wait_state(r, "succeeded", timeout=180)
    assert ec2.calls.count("RunInstances") == 1, ec2.calls


def test_warm_and_minimum_hosts(lux, ec2):
    """A warm host is launched with nothing waiting, and a Run starts on it
    at once; the minimum is kept after scale-down."""
    fake_only(ec2)
    pool(lux, ec2, min=1, warm=1, max=3)
    wait_until(lambda: len(ec2_hosts(lux)) >= 1, 90, 1, "no warm host")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "warm", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=60)
    run = lux.get(run_id)
    assert run["placements"][0]["hostName"] in {h["name"] for h in ec2_hosts(lux, states=("ready", "draining"))}
    # Scaling down never goes below the minimum (1) and warm (1).
    wait_until(lambda: len(ec2.running()) == 1, 60, 1, "not back to one host")
    time.sleep(8)  # past the scale-down delay (4s): still one
    assert len(ec2.running()) == 1


def test_a_lost_instance_is_replaced(lux, ec2):
    pool(lux, ec2, min=1, max=2)
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 1, "no host")
    [inst] = ec2.running()
    ec2.kill(inst["id"])
    wait_until(lambda: [i for i in ec2.running() if i["id"] != inst["id"]], 90, 1, "no replacement launched")
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 1, "the replacement never registered")


def test_a_host_that_never_registers_is_terminated(lux, ec2):
    fake_only(ec2)
    ec2.no_boot = True
    pool(lux, ec2, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") == 1, 30, 0.5, "never launched")
    # LUX_LAUNCH_TIMEOUT is 15s against the fake EC2 (FAKE_EC2_TIMERS).
    wait_until(lambda: "TerminateInstances" in ec2.calls, 120, 1, "the stuck host was never terminated")
    ec2.no_boot = False
    lux.wait_state(run_id, "succeeded", timeout=180)


def test_a_failed_launch_is_retried(lux, ec2):
    fake_only(ec2)
    ec2.fail_launches = True
    pool(lux, ec2, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") >= 2, 30, 0.5, "not retried")
    ec2.fail_launches = False
    lux.wait_state(run_id, "succeeded", timeout=120)


def test_a_busy_host_is_drained_before_it_is_terminated(lux, ec2):
    """Removing a pool with --force-evict never cuts a Run short: its host
    is drained (the Run stops and snapshots), and terminated only once
    that snapshot is uploaded — never while the Run is still live on it."""
    pool(lux, ec2, max=1)
    # Slow to stop (a grace period it uses in full), so a terminate that
    # does not wait for the drain would catch it live.
    script = "trap 'sleep 6; exit 0' TERM; echo state > /w/f; echo up; while :; do sleep 1; done"
    spec = generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"},
                   volumes=[{"name": "w", "path": "/w", "kind": "state"}])
    spec["workload"]["grace"] = "30s"
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "up", timeout=120)
    lux.run("pools", "rm", "burst", "--force-evict")
    # While the Run stops, its instance is still there.
    wait_until(lambda: lux.get(run_id)["state"] == "stopping", 30, 0.3, "never asked to stop")
    assert ec2.running(), "terminated while its Run was still stopping"
    wait_until(lambda: not ec2.running(), 90, 1, "never terminated")
    snaps = lux.json("snapshots", run_id)
    assert snaps and snaps[-1]["uploaded"], snaps
    run = lux.get(run_id)
    assert run["placements"][-1]["exitReason"] != "lost", run


def test_pools_rm_without_force_evict_stops_nothing(lux, ec2):
    """Without --force-evict, removing a pool only cordons its hosts: a
    running Run keeps its placement and finishes on its own; the host is
    still terminated once it goes idle."""
    pool(lux, ec2, max=1)
    script = "echo up; sleep 5; echo done"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script, placement={"pool": "burst"}))
    lux.wait_output(run_id, "up", timeout=120)
    lux.run("pools", "rm", "burst")
    wait_until(lambda: ec2_hosts(lux, states=("draining",)), 30, 0.5, "the pool's host was never cordoned")
    # The Run was never asked to stop; it runs to completion on its own.
    run = lux.get(run_id)
    assert not run["placements"][0].get("stopRequestedAt"), run["placements"][0]
    lux.wait_state(run_id, "succeeded", timeout=60)
    # Idle now: terminated by the same path as scale-down.
    wait_until(lambda: not ec2.running(), 90, 1, "the idle, cordoned host was never terminated")


def test_a_purged_instance_does_not_write_off_the_others(lux, ec2):
    """EC2 refuses a whole DescribeInstances when one id is unknown (it
    purges terminated instances). The other hosts are still alive."""
    fake_only(ec2)
    pool(lux, ec2, min=2, max=2)
    wait_until(lambda: len(ec2_hosts(lux)) == 2, 120, 1, "no hosts")
    first, second = ec2.running()
    ec2.purge(first["id"])
    # A replacement comes for the purged one; the other is never replaced.
    wait_until(lambda: len(ec2.running()) == 2 and second["id"] in {i["id"] for i in ec2.running()}
               and first["id"] not in {i["id"] for i in ec2.running()}, 240, 1, "not replaced as expected")
    assert second["id"] in {i["id"] for i in ec2.running()}
    assert ec2.calls.count("RunInstances") == 3, ec2.calls.count("RunInstances")


def test_an_instance_whose_launch_reply_was_lost_is_terminated(lux, ec2):
    """RunInstances succeeds but luxd never gets the reply (it stops, or
    the network drops it): the instance is found by its lux tags and
    terminated, and a host is launched properly."""
    fake_only(ec2)
    ec2.orphan_next_launch()
    ec2.no_boot = True  # the orphan must not register on its own
    pool(lux, ec2, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "ok", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") >= 1, 30, 0.5, "never launched")
    orphan = wait_until(lambda: ec2.running() and ec2.running()[0]["id"], 60, 0.5, "no instance")
    assert "lux:host" in ec2.running()[0]["tags"]
    ec2.no_boot = False
    lux.wait_state(run_id, "succeeded", timeout=240)
    wait_until(lambda: orphan not in {i["id"] for i in ec2.running()}, 120, 1, "the orphan was never terminated")


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

    ec2.interrupt(inst["id"], seconds=90)
    # Moved before EC2 takes the instance: a new placement, its state carried.
    lux.wait_output(run_id, "starts=2", timeout=85)
    run = lux.get(run_id)
    assert run["state"] == "running", run
    first, second = run["placements"][-2:]
    assert first["host"] != second["host"], run["placements"]
    assert first["stopReason"] == "preempt" and first["state"] == "exited", first
    # The interrupted host was drained and let go of, not written off.
    wait_until(lambda: inst["id"] not in {i["id"] for i in ec2.running()}, 90, 1, "the interrupted instance stayed")
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
    ec2.interrupt(inst["id"], seconds=30)
    run = lux.wait_state(run_id, "stopped", timeout=28)
    assert run["placements"][-1]["exitReason"] != "lost", run
    wait_until(lambda: (s := lux.json("snapshots", run_id)) and s[-1]["uploaded"], 30, 0.5, "snapshot not uploaded in time")


def test_warm_while_active_scales_an_idle_pool_to_zero(lux, ec2):
    """--warm-while-active keeps the warm host only while the pool is in
    use: an unused pool has none; after a Run, an idle host is kept for the
    pool's --scale-down-after (the next Run needs no boot); then the pool
    goes down to its minimum, 0."""
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(ec2.template),
            "--max", "3", "--warm", "1", "--warm-while-active", "--scale-down-after", "20s")
    pl = next(p for p in lux.json("pools", "ls") if p["name"] == "burst")
    assert pl["warmWhileActive"] and pl["scaleDownAfter"] == "20s", pl
    # Idle from the start: no warm host is launched for a pool nobody uses.
    time.sleep(6)
    assert not ec2.running(), "a warm host was launched for an unused pool"

    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "hi", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=180)
    # In use: an idle host is kept as the warm one (the host that ran it,
    # or a spare launched while it ran), within the scale-down time.
    time.sleep(8)
    ready = {h["name"] for h in ec2_hosts(lux)}
    assert ready, "no host kept warm within the scale-down time"
    # Within the scale-down time the hosts stay: the next Run lands on one
    # of them, with no boot.
    again = lux.submit(generic(ALPINE_IMAGE, "echo", "again", placement={"pool": "burst"}))
    lux.wait_state(again, "succeeded", timeout=60)
    assert lux.get(again)["placements"][0]["hostName"] in ready, "the next Run waited for a new host"
    # Quiet past the scale-down time: back to zero.
    wait_until(lambda: not ec2.running(), 120, 1, "the idle pool never scaled to zero")
