"""Shared by the EC2 suites (test_ec2_*.py): luxd's EC2 provider against a
fake EC2 (the real provider code, a fake API); --real-ec2 runs them against
AWS."""

from __future__ import annotations

import json

import pytest

from env import wait_until


@pytest.fixture(autouse=True)
def _clean(lux, ec2):
    """Each test's pool is removed after it, and its instances with it."""
    yield
    lux.run("pools", "rm", "burst", check=False)
    wait_until(lambda: not ec2.running(), 90, 0.3, "the removed pool's instances were not terminated")


def pool(lux, ec2, name="burst", spot=False, **kw):
    template = {**ec2.template, "spot": True} if spot else ec2.template
    args = ["pools", "set", name, "--provider", "ec2", "--template", json.dumps(template)]
    for k, v in kw.items():
        args += [f"--{k}", str(v)]
    lux.run(*args)


def ec2_hosts(lux, pool_name="burst", states=("ready",)):
    return [h for h in lux.json("hosts", "ls") if h["pool"] == pool_name and h["state"] in states]


def pool_events(lux, pool_name="burst"):
    """The pool's events since it was last set (each test sets it; a pool
    removed and set again keeps its events, from pool.restored on), oldest
    first."""
    evs = list(reversed(lux.json("pools", "events", pool_name, "--all")))
    last_set = max((i for i, e in enumerate(evs) if e["type"] in ("pool.config_changed", "pool.restored")), default=-1)
    return evs[last_set + 1:]
