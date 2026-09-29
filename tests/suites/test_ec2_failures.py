"""EC2 pools, failures: hosts EC2 lost or purged, hosts that never
register, failed launches, and instances whose launch reply was lost."""

from __future__ import annotations

import json

import psycopg
import pytest

from conftest import fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


def env_sql(env, q: str, *args):
    """A write luxd's API cannot make: state an older luxd would leave."""
    with psycopg.connect(env.owner_dsn) as conn:
        conn.execute(q, args)


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


def test_a_rename_whose_retag_is_refused_terminates_nothing(lux, ec2):
    """Without IAM's ec2:CreateTags the rename still stands (the pool, its
    host and Runs take the new name), and luxd keeps trying to update the
    instance's name tag; the instance, found by lux:pool-id, keeps running
    and serves Runs, and another pool can take the old name at once
    without taking the instance for its orphan. Once CreateTags is
    allowed, the name tag follows."""
    fake_only(ec2)
    pool(lux, ec2, min=1, max=1)
    [host] = wait_until(lambda: ec2_hosts(lux), 120, 0.3, "no host")
    [inst] = ec2.running()
    assert inst["tags"]["lux:pool-id"], inst["tags"]
    ec2.deny_create_tags = True
    try:
        lux.run("pools", "rename", "burst", "burst-eu")
        # Several provider checks (LUX_PROVIDER_CHECK_EVERY is 2s), each refused.
        wait_until(lambda: ec2.calls.count("CreateTags") >= 3, 60, 0.3, "the name tag was not retried")
        [inst] = ec2.running()
        assert inst["tags"]["lux:pool"].endswith("/burst"), inst["tags"]
        assert "TerminateInstances" not in ec2.calls, ec2.calls
        assert [h["name"] for h in ec2_hosts(lux, "burst-eu")] == [host["name"]]
        run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "ok", placement={"pool": "burst-eu"}))
        lux.wait_state(run_id, "succeeded", timeout=60)
        # The old name: refused for a Run while no pool has it, then free.
        refused = lux.run("run", "-f", "-", check=False,
                          input=json.dumps(generic(ALPINE_IMAGE, "true", placement={"pool": "burst"})))
        assert refused.returncode == 4 and 'renamed "burst-eu"' in refused.stderr, refused.stderr
        pool(lux, ec2, max=0)
        calls = len(ec2.calls)
        wait_until(lambda: ec2.calls[calls:].count("DescribeInstances") >= 6, 60, 0.3, "no provider checks")
        assert "TerminateInstances" not in ec2.calls, ec2.calls
        assert [i["id"] for i in ec2.running()] == [inst["id"]]
        lux.run("pools", "rm", "burst")
        ec2.deny_create_tags = False
        wait_until(lambda: ec2.running()[0]["tags"]["lux:pool"].endswith("/burst-eu"), 60, 0.3, "never re-tagged")
        assert "TerminateInstances" not in ec2.calls, ec2.calls
    finally:
        ec2.deny_create_tags = False


def test_a_pool_from_before_lux_pool_id_is_migrated(lux, ec2):
    """An instance launched without lux:pool-id (by an older luxd): its pool
    is listed by name, cannot be renamed until luxd has tagged the instance
    with the pool's id and seen it, and then renames with the instance
    running."""
    fake_only(ec2)
    ec2.untagged_launches = True
    ec2.deny_create_tags = True
    try:
        pool(lux, ec2, min=1, max=1)
        [host] = wait_until(lambda: ec2_hosts(lux), 120, 0.3, "no host")
        [inst] = ec2.running()
        assert "lux:pool-id" not in inst["tags"], inst["tags"]
        # The launch recorded it as tagged; an older luxd's would not have.
        env_sql(lux.env, "UPDATE hosts SET pool_id_tagged = false WHERE name = %s", host["name"])
        env_sql(lux.env, "UPDATE pools SET id_migrated_at = NULL WHERE name = %s", "burst")
        refused = lux.run("pools", "rename", "burst", "burst-eu", check=False)
        assert refused.returncode == 4 and "not yet confirmed to carry" in refused.stderr, refused.stderr
    finally:
        ec2.untagged_launches = False
        ec2.deny_create_tags = False
    wait_until(lambda: ec2.running()[0]["tags"].get("lux:pool-id"), 60, 0.3, "never tagged with the pool's id")
    wait_until(lambda: lux.run("pools", "rename", "burst", "burst-eu", check=False).returncode == 0,
               60, 1, "never renamable")
    assert [i["id"] for i in ec2.running()] == [inst["id"]]
    assert [h["name"] for h in ec2_hosts(lux, "burst-eu")] == [host["name"]]
    wait_until(lambda: ec2.running()[0]["tags"]["lux:pool"].endswith("/burst-eu"), 60, 0.3, "name tag never followed")
    assert "TerminateInstances" not in ec2.calls, ec2.calls


def test_a_host_missing_from_its_listing_is_described_and_kept(lux, ec2):
    """A live instance whose lux:pool no longer names its pool (a late
    CreateTags from an expired luxd), and which the tag listings miss: luxd
    looks it up by id, finds it running, keeps it and re-tags it."""
    fake_only(ec2)
    pool(lux, ec2, min=1, max=1)
    [host] = wait_until(lambda: ec2_hosts(lux), 120, 0.3, "no host")
    [inst] = ec2.running()
    tag = inst["tags"]["lux:pool"]
    try:
        ec2.unlisted.add(inst["id"])
        ec2.set_tag(inst["id"], "lux:pool", tag + "-stale")
        # Listed by no name; LUX_LISTING_LAG (4s) after its launch it is
        # looked up by id, and re-tagged.
        wait_until(lambda: ec2.running()[0]["tags"]["lux:pool"] == tag, 60, 0.3, "never re-tagged")
        assert "TerminateInstances" not in ec2.calls, ec2.calls
        assert [h["name"] for h in ec2_hosts(lux)] == [host["name"]]
    finally:
        ec2.unlisted.clear()
