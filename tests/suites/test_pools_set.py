"""lux pools set against a real luxd: it prints what it changes, refuses a
set that would drop settings unless --replace, writes nothing with
--dry-run, and says when it creates a pool. (Production 2026-10-07: a set
pasted from a Terraform output dropped nestedContainers and --max 4, and
one naming a renamed pool created an empty one.)"""

from __future__ import annotations

import json

import pytest

from conftest import fake_only
from ec2_helpers import _clean  # noqa: F401 (_clean is an autouse fixture)

pytestmark = pytest.mark.ec2


def stored(lux, name="burst"):
    [pl] = [p for p in lux.json("pools", "ls") if p["name"] == name]
    return pl


def test_a_set_that_drops_settings_is_refused_unless_replace(lux, ec2):
    fake_only(ec2)
    nested = {**ec2.template, "nestedContainers": True}
    r = lux.run("pools", "set", "burst", "--provider", "ec2", "--max", "4", "--template", json.dumps(nested))
    assert r.stdout.startswith("creating pool burst (no pool of that name for tenant "), r.stdout

    pasted = ["pools", "set", "burst", "--provider", "ec2", "--max", "10", "--template", json.dumps(ec2.template)]
    r = lux.run(*pasted, "--dry-run")
    assert r.stdout.splitlines() == ["maxHosts 4→10", "template.nestedContainers true→-"], r.stdout
    assert stored(lux)["maxHosts"] == 4

    r = lux.run(*pasted, check=False)
    assert r.returncode == 4, (r.returncode, r.stderr)
    assert "would remove template.nestedContainers from pool burst" in r.stderr and "--replace" in r.stderr, r.stderr
    pl = stored(lux)
    assert pl["maxHosts"] == 4 and pl["template"]["nestedContainers"] is True, pl

    # A value change alone is written, and said.
    r = lux.run("pools", "set", "burst", "--provider", "ec2", "--max", "6", "--template", json.dumps(nested))
    assert r.stdout.splitlines() == ["maxHosts 4→6"], r.stdout
    assert stored(lux)["maxHosts"] == 6

    r = lux.run(*pasted, "--replace")
    assert r.stdout.splitlines() == ["maxHosts 6→10", "template.nestedContainers true→-"], r.stdout
    pl = stored(lux)
    assert pl["maxHosts"] == 10 and "nestedContainers" not in pl["template"], pl
    # What the CLI printed is what luxd recorded.
    [changed] = [e for e in lux.json("pools", "events", "burst") if e["type"] == "pool.config_changed"][:1]
    assert set(changed["data"]["changes"]) == {"maxHosts", "template.nestedContainers"}, changed


def test_a_set_naming_a_renamed_pool_says_it_creates_one(lux, ec2):
    lux.run("pools", "set", "burst", "--provider", "static", "--max", "2", "--scale-down-after", "5m")
    lux.run("pools", "rename", "burst", "arm64")
    try:
        r = lux.run("pools", "set", "burst", "--provider", "static", "--max", "2", "--dry-run")
        assert r.stdout.startswith("creating pool burst (no pool of that name for tenant "), r.stdout
        assert "burst" not in {p["name"] for p in lux.json("pools", "ls")}
        # An optional setting dropped is a removal too.
        r = lux.run("pools", "set", "arm64", "--provider", "static", "--max", "2", check=False)
        assert r.returncode == 4 and "would remove scaleDownAfterSeconds" in r.stderr, r.stderr
        assert stored(lux, "arm64")["scaleDownAfter"] == "5m0s"
    finally:
        lux.run("pools", "rm", "arm64", check=False)
