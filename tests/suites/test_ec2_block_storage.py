"""EC2 pools, block storage: a launched host's own disk is recorded from
DescribeVolumes and priced from the Pricing API (both the fake's), and a
Run on it gets a Block storage line next to its compute one, shared by the
same reservation."""

from __future__ import annotations

import json
import subprocess
from datetime import datetime, timedelta, timezone
from decimal import ROUND_HALF_UP, Decimal

import psycopg
import pytest

from conftest import FAKE_EC2_TIMERS, fake_only, generic
from ec2_helpers import _clean, ec2_hosts, pool  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, BIN_DIR, wait_until

pytestmark = pytest.mark.ec2

# The fake's root disk (gp3, 100 GiB, 3000 IOPS, 125 MiB/s) at the real
# eu-north-1 list price: 100 × 0.0836 / 730 per hour, rounded to 9 digits.
GP3_100_PER_HOUR = "0.011452055"
NINE_DIGITS = Decimal("0.000000001")


@pytest.fixture
def priced(env, ec2):
    """luxd as the ec2 fixture starts it, also pricing from the fake's
    Pricing API, with second-scale cost ticks and price refreshes."""
    fake_only(ec2)
    env.stop_luxd()
    env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, LUX_PRICING_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake",
                   AWS_SECRET_ACCESS_KEY="fake", AWS_REGION="us-east-1", LUX_COSTS_EVERY="5s",
                   LUX_COSTS_PRICES_REFRESH="5s", **FAKE_EC2_TIMERS)
    return ec2


def test_a_launched_hosts_disk_is_its_runs_block_storage(env, lux, operator, priced):
    pool(lux, priced, template={**priced.template, "region": "eu-north-1"}, max=1, min=1)
    [host] = wait_until(lambda: ec2_hosts(lux) or None, 120, 0.5, "the pool's host never registered")
    host = wait_until(lambda: (h := next(x for x in ec2_hosts(lux) if x["id"] == host["id"])).get("volumes") and h,
                      60, 0.5, "the host's volumes were never recorded")
    assert host["volumes"] == [{"type": "gp3", "sizeGiB": 100, "iops": 3000, "throughputMiBps": 125}], host["volumes"]
    # The price loop asks on its next pass, not when the volumes are recorded.
    wait_until(lambda: any(c.get("volumeApiName") == "gp3" and c.get("regionCode") == "eu-north-1"
                           for c in priced.pricing_calls),
               60, 0.5, "luxd never asked the Pricing API for gp3 in eu-north-1")

    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "3", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)

    def final_cost():
        c = lux.json("cost", run_id)
        return c if c["status"] == "final" else None
    cost = wait_until(final_cost, 120, 1, "the Run's cost never became final")
    by_family = {line["family"]: line for line in cost["lines"]}
    assert set(by_family) == {"compute", "block-storage"}, cost["lines"]
    bs, compute = by_family["block-storage"], by_family["compute"]
    assert bs["source"] == "compute" and bs["item"] == "gp3:100GiB" and bs["final"], bs
    [p] = bs["details"]["placements"]
    [pc] = compute["details"]["placements"]
    assert p["ratePerHour"] == GP3_100_PER_HOUR and p["hostId"] == host["id"], p
    # The disk is shared by the same reservation, over the same window, as the machine.
    assert p["share"] == pc["share"] and p["from"] == pc["from"] and p["to"] == pc["to"], (p, pc)
    expected = Decimal(compute["amount"]) * Decimal(GP3_100_PER_HOUR) / Decimal(pc["ratePerHour"])
    assert abs(Decimal(bs["amount"]) - expected) <= Decimal("0.000000002"), (bs["amount"], expected)
    assert Decimal(bs["amount"]) > 0
    families = {f["family"]: f.get("displayName") for f in cost["byFamily"]}
    assert families == {"compute": "Compute", "block-storage": "Block storage"}, cost["byFamily"]
    # lux cost names the family as luxd does.
    assert "Block storage (block-storage)" in lux.run("cost", run_id).stdout

    # The host's own block-storage hours, once it is gone: each hour's
    # allocated + unallocated is the disk's rate over the part of the hour
    # the host was billed, rounded once to 9 digits.
    lux.run("pools", "rm", "burst")
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        launched, ended = wait_until(
            lambda: (r := conn.execute("SELECT provision_requested_at, terminated_at FROM hosts WHERE id = %s",
                                       (host["id"],)).fetchone()) and r[1] and r,
            90, 0.5, "the host was never terminated")
    launched, ended = launched.astimezone(timezone.utc), ended.astimezone(timezone.utc)
    expected = {}
    hour = launched.replace(minute=0, second=0, microsecond=0)
    while hour < ended:
        covered = min(ended, hour + timedelta(hours=1)) - max(launched, hour)
        fraction = Decimal(covered // timedelta(microseconds=1)) / Decimal(3_600_000_000)
        expected[hour] = (Decimal(GP3_100_PER_HOUR) * fraction).quantize(NINE_DIGITS, ROUND_HALF_UP)
        hour += timedelta(hours=1)
    window = {"from": launched.replace(minute=0, second=0, microsecond=0).isoformat(),
              "to": (ended + timedelta(hours=1)).isoformat()}

    seen = []

    def block_storage_hours():
        r = operator.api(f"/v1/hosts/{host['id']}/cost", params=window)
        assert r.status_code == 200, r.text
        body = r.json()
        got = {datetime.fromisoformat(h["hour"]): Decimal(h["allocated"]) + Decimal(h["unallocated"])
               for h in body["hours"] if h["family"] == "block-storage"}
        seen[:] = [body]
        return (body, got) if got == expected else None
    # A live host's open hour is rebuilt at most every 2 minutes
    # (DefaultCostsEvery), so its last hour is final within that of the end.
    try:
        body, got = wait_until(block_storage_hours, 180, 2, f"block-storage host hours never became {expected}")
    except AssertionError as e:
        raise AssertionError(f"{e}; the host's cost: {seen}") from None
    assert [r["perHour"] for r in body["rates"] if r["family"] == "block-storage"] == [GP3_100_PER_HOUR], body["rates"]
    assert all(v > 0 for v in got.values()), got

    # The operator's backfill command, through the binary: with the host's
    # volumes unknown again, a dry run prices the stated disk from the
    # fake's Pricing API (a 1s cache refresh forces the call) and reports
    # the host and its Run, changing nothing.
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        conn.execute("UPDATE hosts SET volumes = NULL WHERE id = %s", (host["id"],))
    calls = len(priced.pricing_calls)
    out = subprocess.run(
        [str(BIN_DIR / "luxd"), "admin", "costs", "backfill-volumes", "--pool", "burst", "--tenant", lux.tenant_id,
         "--volume", "type=gp3,size=100,iops=3000,throughput=125", "--dry-run"],
        env={**env._luxd_base_env(), "LUX_DATABASE_URL": env.owner_dsn, "LUX_PRICING_ENDPOINT": priced.url,
             "LUX_COSTS_PRICES_REFRESH": "1s", "AWS_ACCESS_KEY_ID": "fake", "AWS_SECRET_ACCESS_KEY": "fake",
             "AWS_REGION": "us-east-1"},
        capture_output=True, text=True, timeout=60)
    assert out.returncode == 0, out.stderr
    report = json.loads(out.stdout)
    assert report["dryRun"] is True and report["runs"] == 1 and report["skipped"] == [], report
    [bh] = report["hosts"]
    assert bh["id"] == host["id"] and bh["perHour"] == GP3_100_PER_HOUR and bh["runs"] == [run_id], bh
    assert report["hours"] == bh["hours"] == len(expected), (report, expected)
    assert any(c.get("volumeApiName") == "gp3" and c.get("regionCode") == "eu-north-1"
               for c in priced.pricing_calls[calls:]), priced.pricing_calls[calls:]
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        assert conn.execute("SELECT volumes FROM hosts WHERE id = %s", (host["id"],)).fetchone() == (None,)
