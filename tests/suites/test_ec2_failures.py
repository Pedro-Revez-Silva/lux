"""EC2 pools, failures: hosts EC2 lost or purged, hosts that never
register, failed launches, and instances whose launch reply was lost."""

from __future__ import annotations

import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool  # noqa: F401 (_clean is an autouse fixture)
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
    wait_until(lambda: ec2.calls.count("RunInstances") >= 2, 30, 0.5, "not retried")
    ec2.fail_launches = False
    lux.wait_state(run_id, "succeeded", timeout=120)


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
