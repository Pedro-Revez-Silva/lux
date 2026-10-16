"""EC2 pools, network transfer and public IPv4 as cost: a launched host in
eu-north-1 with the fake's catalog (ip-ranges.json) and the real
eu-north-1 transfer and public IPv4 list prices (the fake's Pricing API).
A Run's transfer to a billable class is a Data transfer line; the host's
public address is a public-ip rate period."""

from __future__ import annotations

import ipaddress
from decimal import ROUND_HALF_UP, Decimal

import pytest

from conftest import FAKE_EC2_TIMERS, fake_only, generic
from ec2_helpers import (  # noqa: F401 (_clean is an autouse fixture)
    _clean,
    ec2_hosts,
    pool,
)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2

# eu-north-1 list prices in internal/ec2/testdata/datatransfer-pricing-eu-north-1.json.
INTERNET_OUT_PER_GB = Decimal("0.09")
PUBLIC_IPV4_PER_HOUR = "0.005"
# Classes AWS does not charge for: no line, never a zero line.
FREE = {"local", "region-service", "provider-global-free"}


@pytest.fixture
def priced(env, ec2):
    """luxd as the ec2 fixture starts it, also pricing from the fake's
    Pricing API and classifying with the fake's ip-ranges.json, with
    second-scale cost ticks and price refreshes."""
    fake_only(ec2)
    env.stop_luxd()
    env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, LUX_PRICING_ENDPOINT=ec2.url, LUX_IPRANGES_URL=ec2.url + "/ip-ranges.json",
                   AWS_ACCESS_KEY_ID="fake", AWS_SECRET_ACCESS_KEY="fake", AWS_REGION="us-east-1",
                   LUX_COSTS_EVERY="5s", LUX_COSTS_PRICES_REFRESH="5s", **FAKE_EC2_TIMERS)
    return ec2


def fetch(target: str) -> str:
    return f"wget -q -T 5 -O /dev/null http://{target}/ && echo OK:{target} || echo NO:{target}"


def expected_class(ec2, ip: str) -> str:
    """The class the fake's catalog puts ip in, for a host in eu-north-1
    (its subnets local or zone; the catalog's prefixes by region and
    service; anything else internet)."""
    addr = ipaddress.ip_address(ip)
    for sn in ec2.subnets.values():
        if addr in ipaddress.ip_network(sn["cidr"]):
            return "local" if sn["zone"] == "eu-north-1a" else "zone"
    for p in ec2.ipranges["prefixes"]:
        if addr in ipaddress.ip_network(p["ip_prefix"]):
            if p["region"] == "GLOBAL":
                return "provider-global-free" if p["service"] == "CLOUDFRONT" else "provider-global"
            if p["region"] != "eu-north-1":
                return "provider:" + p["region"]
            return "region-service" if p["service"] in ("S3", "DYNAMODB") else "region-public"
    return "internet"


def test_transfer_and_public_ip_are_priced(env, lux, operator, priced, net_targets):
    pool(lux, priced, template={**priced.template, "region": "eu-north-1"}, max=1, min=1)
    [host] = wait_until(lambda: ec2_hosts(lux) or None, 120, 0.5, "the pool's host never registered")
    host = wait_until(lambda: (h := next(x for x in ec2_hosts(lux) if x["id"] == host["id"])).get("network") and h,
                      60, 0.5, "the host's network was never recorded")
    assert host["network"]["publicIPv4"] == 1, host["network"]
    wait_until(lambda: any(c.get("ServiceCode") == "AWSDataTransfer" and c.get("fromRegionCode") == "eu-north-1"
                           for c in priced.pricing_calls),
               60, 0.5, "luxd never asked the Pricing API for eu-north-1's data transfer")

    target = net_targets.allowed_ip
    cls = expected_class(priced, target)
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(target)] * 5),
                                network={"unrestricted": True}, placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", "failed", timeout=120)
    assert f"OK:{target}" in lux.logs(run_id)
    net = wait_until(lambda: (lux.get(run_id).get("usage") or {}).get("netClasses") or None, 30, 1, "no netClasses reported")
    assert cls in net and net[cls]["tx"] > 0, (cls, net)
    # Inbound as the provider delivered it: never less than what passed the filters.
    assert net[cls].get("rxDelivered", 0) >= net[cls]["rx"] > 0, net[cls]

    def final_cost():
        c = lux.json("cost", run_id)
        return c if c["status"] == "final" else None
    cost = wait_until(final_cost, 120, 1, "the Run's cost never became final")
    transfer = {line["item"]: line for line in cost["lines"] if line["family"] == "data-transfer"}
    if cls in FREE:
        assert transfer == {}, transfer
    else:
        tx = transfer[f"{cls}:tx"]
        d = tx["details"]
        assert d["class"] == cls and d["direction"] == "tx" and d["bytes"] == net[cls]["tx"] and d["currency"] == "USD", d
        if cls == "internet":
            assert Decimal(d["perGB"]) == INTERNET_OUT_PER_GB, d
            # GB is 10^9 bytes; inbound from the internet is free: no rx line.
            expected = (Decimal(d["bytes"]) * INTERNET_OUT_PER_GB / Decimal(10**9)).quantize(Decimal("0.000000001"), ROUND_HALF_UP)
            assert Decimal(tx["amount"]) == expected, (tx["amount"], expected)
            assert f"{cls}:rx" not in transfer, transfer
        assert tx["source"] == "compute" and tx["final"], tx
        assert "Data transfer (data-transfer)" in lux.run("cost", run_id).stdout
    assert cost.get("unpriced", []) == [], cost["unpriced"]

    # The host's public address: a public-ip period at the region's
    # in-use price × its one address, shared like compute.
    def public_ip_rates():
        r = operator.api(f"/v1/hosts/{host['id']}/cost", params={"since": "2h"})
        assert r.status_code == 200, r.text
        rates = [x for x in r.json().get("rates", []) if x["family"] == "public-ip"]
        return rates or None
    [rate] = wait_until(public_ip_rates, 60, 1, "no public-ip period opened")
    assert rate["perHour"] == PUBLIC_IPV4_PER_HOUR and rate["source"] == "ec2-vpc-pricing", rate
    assert rate["details"] == {"addresses": 1, "perAddressHour": PUBLIC_IPV4_PER_HOUR}, rate
    public_ip = wait_until(lambda: next((line for line in lux.json("cost", run_id)["lines"] if line["family"] == "public-ip"), None),
                           60, 1, "the Run got no public-ip line")
    assert public_ip["item"] == "ipv4" and Decimal(public_ip["amount"]) > 0, public_ip
    assert "Public IPv4 (public-ip)" in lux.run("cost", run_id).stdout
