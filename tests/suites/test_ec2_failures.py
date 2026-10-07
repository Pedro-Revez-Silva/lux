"""EC2 pools, failures: hosts EC2 lost or purged, hosts that never
register, failed launches, and instances whose launch reply was lost."""

from __future__ import annotations

import time

import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool, pool_events  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


def test_a_lost_instance_is_replaced(lux, ec2):
    pool(lux, ec2, min=1, max=2)
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 0.3, "no host")
    [inst] = ec2.running()
    ec2.kill(inst["id"])
    wait_until(lambda: [i for i in ec2.running() if i["id"] != inst["id"]], 90, 0.3, "no replacement launched")
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 0.3, "the replacement never registered")


def test_a_host_that_never_registers_is_terminated(lux, ec2):
    fake_only(ec2)
    ec2.no_boot = True
    pool(lux, ec2, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    wait_until(lambda: ec2.calls.count("RunInstances") == 1, 30, 0.5, "never launched")
    # LUX_LAUNCH_TIMEOUT is 8s against the fake EC2 (FAKE_EC2_TIMERS).
    wait_until(lambda: "TerminateInstances" in ec2.calls, 120, 0.3, "the stuck host was never terminated")
    ec2.no_boot = False
    lux.wait_state(run_id, "succeeded", timeout=180)


def test_a_failed_launch_is_retried(lux, ec2):
    fake_only(ec2)
    ec2.fail_launches = True
    pool(lux, ec2, max=1)
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    # Every attempt failed the same way: one event, counted, not one each.
    def counted():
        failed = [e for e in pool_events(lux) if e["type"] == "pool.launch_failed"]
        return failed if failed and failed[0]["count"] >= 2 and failed[0].get("lastTime") else None
    failed = wait_until(counted, 30, 0.5, "no repeated launch_failed")
    assert len(failed) == 1, failed
    assert failed[0]["lastTime"] > failed[0]["time"], failed
    assert "InsufficientInstanceCapacity" in failed[0]["data"]["error"], failed
    assert "RequestID" not in failed[0]["data"]["error"], failed
    assert "(×" in lux.run("pools", "events", "burst").stdout
    # Each refused launch is a terminated host whose launch failed: no
    # instance, no terminated time; the list filters them apart.
    refused = lux.json("hosts", "ls", "--state", "launch_failed")
    assert refused, refused
    for h in refused:
        assert h["launch"]["outcome"] == "failed", h
        assert "InsufficientInstanceCapacity" in h["launch"]["error"], h
        assert h["launch"].get("finishedAt"), h
        assert not h.get("providerId") and not h["times"].get("terminated"), h
    assert "launch failed" in lux.run("hosts", "ls", "--state", "launch_failed").stdout
    # At least two sweeps (one request per subnet each) before capacity
    # comes back, so the interval between them is observed.
    wait_until(lambda: len(lux.json("hosts", "ls", "--state", "launch_failed")) >= 2, 45, 0.3, "no second sweep")
    with ec2.lock:
        ec2.fail_launches = False
        attempts = list(zip(ec2.launch_attempted_at, ec2.launch_attempts))
    refused = lux.json("hosts", "ls", "--state", "launch_failed")
    lux.wait_state(run_id, "succeeded", timeout=120)
    launched = [h for h in lux.json("hosts", "ls", "--all") if h["pool"] == "burst" and h.get("providerId")]
    assert launched and all(h["launch"]["outcome"] == "launched" for h in launched), launched
    assert {h["id"] for h in refused}.isdisjoint({h["id"] for h in launched}), (refused, launched)
    # The fake answers InsufficientInstanceCapacity with a 500 naming the
    # zone, as EC2 does. A refused launch makes one request per candidate
    # (the pool's two subnets), with no SDK retries, and the pool then
    # launches nothing for its launch backoff (15s after the first
    # failure): so each (type, subnet) is asked once per sweep, the second
    # sweep about 15s after the first (bounds [13, 25] leave room for
    # request latency and a loaded host), not on every pass (~1s) nor at
    # the no-capacity marks' 3s; one refused host per sweep.
    # Capacity-mark suppression is covered by TestLaunchSweepDoesNotSplit / TestLaunchRetriesThePrimaryOnceItsMarkExpires.
    by_key: dict[tuple[str, str], list[float]] = {}
    for at, key in attempts:
        by_key.setdefault(key, []).append(at)
    assert set(by_key) == {("m7i.large", "subnet-a"), ("m7i.large", "subnet-b")}, by_key
    for key, times in by_key.items():
        gaps = [b - a for a, b in zip(times, times[1:])]
        assert gaps and all(13 <= g <= 25 for g in gaps), (key, times)
    assert len(attempts) == 2 * len(refused), (attempts, refused)


def test_a_failing_launch_backs_off_until_the_pool_is_fixed(lux, ec2):
    """A launch EC2 refuses for a reason other than capacity (a launch
    template deleted under the pool) is not retried every pass: the pool
    waits 15s, then 30s, ... and says so in one pool.scale_blocked. Setting
    the pool again (the operator's fix) is tried at once."""
    fake_only(ec2)
    ec2.launch_failures = {("m7i.large", "*"): "InvalidLaunchTemplateName.NotFound"}
    pool(lux, ec2, min=1, max=1)

    # Wait for work done, not for time: two failures committed. Without a
    # backoff the second follows the first on the next pass (~1s); with it,
    # 15s later, and the third is not due for another 30s.
    def two_completed_failures():
        failed = [e for e in pool_events(lux) if e["type"] == "pool.launch_failed"]
        return failed if sum(e["count"] for e in failed) >= 2 else None

    failed = wait_until(two_completed_failures, 60, 0.2, "no second completed launch failure")
    with ec2.lock:
        times = list(ec2.launch_attempted_at)
        calls = ec2.calls.count("RunInstances")
    assert len(times) == 2 and calls == 2, (times, calls)
    assert times[1] - times[0] >= 15, times
    assert all("InvalidLaunchTemplateName.NotFound" in e["data"]["error"] for e in failed), failed
    [blocked] = [e for e in pool_events(lux) if e["type"] == "pool.scale_blocked"]
    assert blocked["data"]["cause"] == "launch_backoff", blocked
    assert blocked["count"] >= 1, blocked
    assert "InvalidLaunchTemplateName.NotFound" in blocked["data"]["detail"], blocked
    assert blocked["data"]["detail"].startswith("launch backing off after "), blocked
    assert "launch backing off after" in lux.run("pools", "events", "burst").stdout

    # The fix: the pool set again with another instance type, mid-backoff.
    pool(lux, ec2, min=1, max=1, template={**ec2.template, "instanceType": "m6i.large"})
    fixed_at = time.monotonic()
    wait_until(lambda: ("m6i.large", "subnet-a") in ec2.launch_attempts or ("m6i.large", "subnet-b") in ec2.launch_attempts,
               5, 0.1, "the fixed pool waited out its backoff")
    with ec2.lock:
        at = next(t for t, k in zip(ec2.launch_attempted_at, ec2.launch_attempts) if k[0] == "m6i.large")
    assert at - fixed_at < 4, at - fixed_at
    wait_until(lambda: len(ec2_hosts(lux)) == 1, 90, 0.3, "the fixed pool's host never registered")


def test_a_launch_falls_back_to_the_next_instance_type(lux, ec2):
    """No capacity for the pool's instance type in either subnet: the same
    launch tries each subnet, then the fallback type, and the host is the
    fallback's."""
    fake_only(ec2)
    ec2.launch_failures = {("m7i.large", "*"): "InsufficientInstanceCapacity"}
    pool(lux, ec2, max=1, template={**ec2.template, "fallbackInstanceTypes": ["m6i.large"]})
    run_id = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=180)
    [host] = [h for h in lux.json("hosts", "ls", "--all") if h["pool"] == "burst" and h.get("providerId")]
    assert host["instanceType"] == "m6i.large", host
    assert host["launch"]["outcome"] == "launched", host
    assert ec2.launch_attempts[:3] == [("m7i.large", "subnet-a"), ("m7i.large", "subnet-b"), ("m6i.large", "subnet-a")], \
        ec2.launch_attempts
    assert not [e for e in pool_events(lux) if e["type"] == "pool.launch_failed"], pool_events(lux)


def test_a_purged_instance_does_not_write_off_the_others(lux, ec2):
    """EC2 refuses a whole DescribeInstances when one id is unknown (it
    purges terminated instances). The other hosts are still alive."""
    fake_only(ec2)
    pool(lux, ec2, min=2, max=2)
    wait_until(lambda: len(ec2_hosts(lux)) == 2, 120, 0.3, "no hosts")
    first, second = ec2.running()
    ec2.purge(first["id"])
    # A replacement comes for the purged one; the other is never replaced.
    wait_until(lambda: len(ec2.running()) == 2 and second["id"] in {i["id"] for i in ec2.running()}
               and first["id"] not in {i["id"] for i in ec2.running()}, 240, 0.3, "not replaced as expected")
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
    wait_until(lambda: orphan not in {i["id"] for i in ec2.running()}, 120, 0.3, "the orphan was never terminated")
