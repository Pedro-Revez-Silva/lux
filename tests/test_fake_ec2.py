"""The fake EC2's answers, over HTTP, as luxd's provider sees them: needs
neither Docker nor an environment.

    cd tests && uv run pytest test_fake_ec2.py    # or: make harness-unit
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from types import SimpleNamespace

import pytest

from fake_ec2 import FakeEC2

TEMPLATE = {"region": "eu-north-1", "launchTemplate": "lux-runner", "subnets": ["subnet-a", "subnet-b"]}


@pytest.fixture
def ec2():
    fake = FakeEC2(SimpleNamespace(gateway="127.0.0.1"), TEMPLATE)
    yield fake
    fake.server.shutdown()


def dry_run(ec2, instance_type="m7i.large", subnet="subnet-a", lt="lux-runner") -> tuple[int, str]:
    """A dry-run RunInstances: (HTTP status, EC2 error code)."""
    body = urllib.parse.urlencode({"Action": "RunInstances", "DryRun": "true", "InstanceType": instance_type,
                                   "SubnetId": subnet, "LaunchTemplate.LaunchTemplateName": lt}).encode()
    try:
        with urllib.request.urlopen(urllib.request.Request(ec2.url, data=body), timeout=5) as r:
            return r.status, ""
    except urllib.error.HTTPError as e:
        text = e.read().decode()
        return e.code, text.split("<Code>", 1)[1].split("</Code>", 1)[0]


def test_a_dry_run_that_would_launch_is_dry_run_operation(ec2):
    assert dry_run(ec2) == (412, "DryRunOperation")
    assert dry_run(ec2, lt="gone") == (400, "InvalidLaunchTemplateName.NotFound")


def test_a_dry_run_returns_an_injected_service_error(ec2):
    ec2.launch_failures = {("m7i.large", "subnet-b"): "InternalError"}
    assert dry_run(ec2, subnet="subnet-a") == (412, "DryRunOperation")
    assert dry_run(ec2, subnet="subnet-b") == (500, "InternalError")


@pytest.mark.parametrize("code", ["InsufficientInstanceCapacity", "InsufficientCapacity"])
def test_a_dry_run_does_not_test_capacity(ec2, code):
    ec2.launch_failures = {("m7i.large", "*"): code}
    assert dry_run(ec2) == (412, "DryRunOperation")


def test_a_missing_permission_is_403(ec2):
    ec2.launch_failures = {("m7i.large", "subnet-a"): "UnauthorizedOperation"}
    assert dry_run(ec2) == (403, "UnauthorizedOperation")


def describe_volumes(ec2, *ids: str) -> tuple[int, str]:
    q = {"Action": "DescribeVolumes", "Filter.1.Name": "attachment.instance-id"}
    q.update({f"Filter.1.Value.{n}": i for n, i in enumerate(ids, 1)})
    try:
        with urllib.request.urlopen(urllib.request.Request(ec2.url, data=urllib.parse.urlencode(q).encode()), timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def test_describe_volumes_answers_the_filtered_instances_root_disk(ec2):
    with ec2.lock:
        ec2.instances["i-aaa"] = {"state": "running", "tags": {}, "volumes": [dict(ec2.volumes[0])]}
        ec2.instances["i-bbb"] = {"state": "terminated", "tags": {}, "volumes": [dict(ec2.volumes[0])]}
    code, xml = describe_volumes(ec2, "i-aaa", "i-bbb", "i-unknown")
    assert code == 200
    assert xml.count("<instanceId>") == 1 and "<instanceId>i-aaa</instanceId>" in xml
    for part in ("<size>100</size>", "<volumeType>gp3</volumeType>", "<iops>3000</iops>",
                 "<throughput>125</throughput>", "<deleteOnTermination>true</deleteOnTermination>"):
        assert part in xml
    ec2.volumes_error = "UnauthorizedOperation"
    assert describe_volumes(ec2, "i-aaa")[0] == 403


def get_products(ec2, filters: dict, token: str = "") -> dict:
    body = {"ServiceCode": "AmazonEC2", "Filters": [{"Type": "TERM_MATCH", "Field": k, "Value": v} for k, v in filters.items()]}
    if token:
        body["NextToken"] = token
    req = urllib.request.Request(ec2.url, data=json.dumps(body).encode(),
                                 headers={"X-Amz-Target": "AWSPriceListService.GetProducts",
                                          "Content-Type": "application/x-amz-json-1.1"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read())


def test_get_products_pages_the_real_gp3_answer(ec2):
    units, token = [], ""
    while True:
        page = get_products(ec2, {"regionCode": "eu-north-1", "volumeApiName": "gp3"}, token)
        for p in page["PriceList"]:
            for term in json.loads(p)["terms"]["OnDemand"].values():
                units += [(d["unit"], d["pricePerUnit"]["USD"]) for d in term["priceDimensions"].values()]
        token = page.get("NextToken", "")
        if not token:
            break
    assert sorted(units) == [("GB-Mo", "0.0836000000"), ("GiBps-mo", "42.8032000000"), ("IOPS-Mo", "0.0052000000")]
    assert get_products(ec2, {"regionCode": "eu-north-1", "volumeApiName": "io2"})["PriceList"] == []


def call(ec2, **form) -> tuple[int, str]:
    body = urllib.parse.urlencode(form).encode()
    try:
        with urllib.request.urlopen(urllib.request.Request(ec2.url, data=body), timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def test_market_actions_answer_as_ec2(ec2):
    status, xml = call(ec2, Action="DescribeSubnets", **{"SubnetId.1": "subnet-a", "SubnetId.2": "subnet-b"})
    assert status == 200 and "<availabilityZone>eu-north-1a</availabilityZone>" in xml, xml
    assert "<availabilityZoneId>eun1-az2</availabilityZoneId>" in xml, xml
    status, xml = call(ec2, Action="GetSpotPlacementScores", TargetCapacity="1", **{"InstanceType.1": "m7i.large",
                                                                                   "RegionName.1": "eu-north-1"})
    assert status == 200 and xml.count("<score>7</score>") == 2, xml
    ec2.placement_scores_error = "UnauthorizedOperation"
    status, xml = call(ec2, Action="GetSpotPlacementScores", TargetCapacity="1")
    assert status == 403 and "<Code>UnauthorizedOperation</Code>" in xml, xml
    status, xml = call(ec2, Action="DescribeSpotPriceHistory", AvailabilityZone="eu-north-1a", **{"InstanceType.1": "m7i.large"})
    assert status == 200 and "<spotPrice>0.0312</spotPrice>" in xml, xml


def test_an_unknown_action_is_invalid_action(ec2):
    status, xml = call(ec2, Action="DescribeNothing")
    assert status == 400 and "<Code>InvalidAction</Code>" in xml, xml
