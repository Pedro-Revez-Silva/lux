"""The fake EC2's answers, over HTTP, as luxd's provider sees them: needs
neither Docker nor an environment.

    cd tests && uv run pytest test_fake_ec2.py    # or: make harness-unit
"""

from __future__ import annotations

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
