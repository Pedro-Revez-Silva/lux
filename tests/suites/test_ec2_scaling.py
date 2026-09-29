"""EC2 pools, sizing: luxd launches hosts when Runs wait for them, keeps
warm and minimum hosts (while the pool is in use, with warm-while-active),
and drains and terminates idle ones."""

from __future__ import annotations

import json
import re
import time

import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool, pool_events  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


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
    wait_until(lambda: not ec2.running(), 60, 0.3, "the idle host was never terminated")
    wait_until(lambda: not ec2_hosts(lux, states=("ready", "draining")), 30, 0.3, "the host is still listed")

    # The pool's log tells the story, in order: why it scaled up, the
    # launch, the host registering, the Run placed on it, and its release.
    host_id = lux.get(run_id)["placements"][0]["host"]
    evs = wait_until(lambda: (e := pool_events(lux)) and e[-1]["type"] == "pool.host_released" and e, 30, 0.5,
                     "no pool.host_released")
    types = [e["type"] for e in evs]
    story = ["pool.scale_up", "pool.launch_requested", "pool.host_launched", "pool.host_registered", "pool.placement", "pool.host_released"]
    assert [t for t in types if t in story] == story, types
    by = {e["type"]: e for e in evs}
    assert by["pool.scale_up"]["data"]["reason"] == "waiting runs" and by["pool.scale_up"]["data"]["waiting"] == 1, by["pool.scale_up"]
    assert by["pool.placement"]["data"] == {"run": run_id, "epoch": 1, "host": host_id}, by["pool.placement"]
    assert by["pool.host_registered"]["data"]["host"] == host_id
    released = by["pool.host_released"]["data"]
    assert released["host"] == host_id and released["reason"] == "idle" and released["idleSeconds"] >= 3, released
    # The Run's own log names the pool it was placed from.
    scheduled = [e for e in lux.json("events", run_id) if e["type"] == "state" and e["data"]["state"] == "scheduled"]
    assert scheduled[0]["data"]["pool"] == "burst" and scheduled[0]["data"]["host"] == host_id, scheduled
    # And the host's own log, from registration to termination.
    htypes = [e["type"] for e in reversed(lux.json("hosts", "events", host_id))]
    hstory = ["host.registered", "host.ready", "host.placement_assigned", "host.placement_ended", "host.drain_requested",
              "host.terminate_requested", "host.terminated"]
    assert [t for t in htypes if t in hstory] == hstory, htypes
    # The text form: one line each.
    text = lux.run("pools", "events", "burst", "--limit", "20").stdout
    assert re.search(r"pool\.host_released\s+\S+ released: idle for \d+s", text), text


def test_template_tags_are_kept_and_lux_tags_are_reserved(lux, ec2):
    """A template's own tags reach the instance; a lux:* key is refused when
    the pool is set. (Production 2026-09-28: a Terraform-generated template
    carried lux:pool, so every RunInstances named it twice and EC2 refused
    it.)"""
    fake_only(ec2)
    bad = {**ec2.template, "tags": {"lux:pool": "burst"}}
    r = lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(bad), check=False)
    assert r.returncode != 0 and "lux:* tags are set by lux" in r.stderr, r.stderr
    lux.run("pools", "set", "burst", "--provider", "ec2", "--max", "1",
            "--template", json.dumps({**ec2.template, "tags": {"team": "platform"}}))
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "tagged", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)
    [inst] = ec2.running()
    assert inst["tags"]["team"] == "platform", inst["tags"]
    assert inst["tags"]["lux:pool"].endswith("/burst"), inst["tags"]
    # The pool as listed saves back unchanged (no stored lux:* tag).
    [stored] = [p for p in lux.json("pools", "ls") if p["name"] == "burst"]
    assert stored["template"]["tags"] == {"team": "platform"}, stored["template"]


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
    wait_until(lambda: len(ec2_hosts(lux)) >= 1, 90, 0.3, "no warm host")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "warm", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=60)
    run = lux.get(run_id)
    assert run["placements"][0]["hostName"] in {h["name"] for h in ec2_hosts(lux, states=("ready", "draining"))}
    # Scaling down never goes below the minimum (1) and warm (1): one host,
    # steadily, past the scale-down delay (3s). Two in a row a few seconds
    # apart, as a replacement can come up while another goes.
    def one_steady():
        if len(ec2.running()) != 1:
            return False
        time.sleep(4)
        return len(ec2.running()) == 1
    wait_until(one_steady, 90, 0.3, "not steady at one host")


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
    wait_until(lambda: not ec2.running(), 90, 0.3, "never terminated")
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
    wait_until(lambda: not ec2.running(), 90, 0.3, "the idle, cordoned host was never terminated")


def test_warm_while_active_scales_an_idle_pool_to_zero(lux, ec2):
    """--warm-while-active keeps the warm host only while the pool is in
    use: an unused pool has none; after a Run, an idle host is kept for the
    pool's --scale-down-after (the next Run needs no boot); then the pool
    goes down to its minimum, 0."""
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(ec2.template),
            "--max", "3", "--warm", "1", "--warm-while-active", "--scale-down-after", "8s")
    pl = next(p for p in lux.json("pools", "ls") if p["name"] == "burst")
    assert pl["warmWhileActive"] and pl["scaleDownAfter"] == "8s", pl
    # Idle from the start: no warm host is launched for a pool nobody uses
    # (a few provisioner passes, at a tick each).
    time.sleep(3)
    assert not ec2.running(), "a warm host was launched for an unused pool"

    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "hi", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=180)
    # In use: an idle host is kept as the warm one (the host that ran it,
    # or a spare launched while it ran), within the scale-down time.
    ready = {h["name"] for h in ec2_hosts(lux)}
    assert ready, "no host kept warm within the scale-down time"
    # Within the scale-down time the hosts stay: the next Run lands on one
    # of them, with no boot.
    again = lux.submit(generic(ALPINE_IMAGE, "echo", "again", placement={"pool": "burst"}))
    lux.wait_state(again, "succeeded", timeout=60)
    assert lux.get(again)["placements"][0]["hostName"] in ready, "the next Run waited for a new host"
    # Quiet past the scale-down time: back to zero.
    wait_until(lambda: not ec2.running(), 120, 0.3, "the idle pool never scaled to zero")


def test_renaming_a_pool_keeps_its_running_host(lux, ec2):
    """lux pools rename with a host running: the instance stays up, keeps
    its lux:pool-id and is re-tagged with the new name, a Run naming the
    new pool schedules on the same host, and nothing is launched or
    terminated."""
    pool(lux, ec2, min=1, max=1)
    [host] = wait_until(lambda: ec2_hosts(lux), 120, 0.3, "no host")
    [inst] = ec2.running()
    pool_id = inst["tags"]["lux:pool-id"]
    launches = ec2.calls.count("RunInstances")
    out = lux.run("pools", "rename", "burst", "burst-eu").stdout
    assert "1 hosts" in out, out
    assert [h["name"] for h in ec2_hosts(lux, "burst-eu")] == [host["name"]]
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "renamed", placement={"pool": "burst-eu"}))
    lux.wait_state(run_id, "succeeded", timeout=60)
    assert lux.get(run_id)["placements"][0]["hostName"] == host["name"]
    wait_until(lambda: ec2.running()[0]["tags"]["lux:pool"].endswith("/burst-eu"), 60, 0.3, "the instance was never re-tagged")
    assert ec2.running()[0]["tags"]["lux:pool-id"] == pool_id
    assert [i["id"] for i in ec2.running()] == [inst["id"]]
    if not ec2.real:
        assert "TerminateInstances" not in ec2.calls, ec2.calls
        assert ec2.calls.count("RunInstances") == launches
