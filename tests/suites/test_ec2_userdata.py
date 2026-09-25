"""EC2 user data formats (internal/hostboot, internal/ec2's
template.userData): "ignition" (default, Fedora CoreOS), "script"
(cloud-init on a stock image), "env" (a custom AMI's own boot script). One
source of truth rendered three ways; each launches a working runner
against the fake EC2, and a bad value is refused when the pool is set."""

from __future__ import annotations

import json

import pytest

from conftest import CLIError, fake_only, generic
from ec2_helpers import _clean  # noqa: F401 (_clean is an autouse fixture)
from env import ALPINE_IMAGE, wait_until

pytestmark = pytest.mark.ec2


@pytest.mark.parametrize("user_data", ["ignition", "script", "env", None])
def test_the_env_round_trips_through_every_user_data_format(lux, ec2, user_data):
    """This proves the env luxd puts into each userData format survives
    being rendered and decoded, and that a runner started directly with
    that env registers under the name luxd gave it. It does **not** prove
    that Ignition, cloud-init or a custom AMI's boot script would actually
    run any of this on a real instance: the fake's `_boot` decodes the
    user data and starts the harness's lux-runner itself
    (fake_ec2._parse_user_data), never Ignition or a shell. See
    `test_the_script_format_actually_boots_a_runner` for that, for the
    one format the harness can run for real. None: the default (unset),
    which must behave as "ignition"."""
    fake_only(ec2)
    template = dict(ec2.template)
    if user_data is not None:
        template["userData"] = user_data
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(template), "--max", "1")
    run_id = lux.submit(generic(ALPINE_IMAGE, "echo", "booted", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert "booted" in lux.logs(run_id)
    [inst] = ec2.running()
    # The env the fake parsed out of the rendered user data reached the
    # runner: it registered with the name luxd gave it.
    assert lux.get(run_id)["placements"][0]["hostName"] == inst["tags"]["Name"]


def test_the_script_format_actually_boots_a_runner(env, lux, ec2, hosts):
    """Unlike the parametrized round-trip test above, this fetches the
    real rendered `userData: script` cloud-init script the fake EC2 was
    given for a launch, and executes it (as `bash`, as cloud-init would)
    on a fresh simulated host — the same thing
    test_bootstrap_script_fetches_and_verifies_binaries_on_a_host does for
    the static-host bootstrap script, so this format gets the same
    real-execution coverage."""
    fake_only(ec2)
    template = {**ec2.template, "userData": "script"}
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(template), "--min", "1", "--max", "1")
    wait_until(lambda: ec2.running(), 60, 1, "no host launched")
    [inst] = ec2.running()
    script = ec2.userdata(inst["id"])
    assert script.startswith("#!/bin/bash\nexport LUX_URL="), script[:200]

    host = env.add_host("script-userdata-test")
    host.exec("sh", "-c", "cat > /tmp/userdata.sh", input=script.encode())
    host.exec("sh", "-c", "printf '#!/bin/sh\\nexit 0\\n' > /usr/local/bin/systemctl && chmod +x /usr/local/bin/systemctl")
    host.exec("bash", "/tmp/userdata.sh")

    unit = host.exec("cat", "/etc/systemd/system/lux-runner.service")
    assert "ExecStartPre=/usr/local/bin/lux-fetch-binaries.sh" in unit
    runner_env = host.exec("cat", "/etc/lux/runner.env")
    assert f"LUX_URL={env.luxd_url}" in runner_env


def test_an_unknown_user_data_format_is_refused_when_the_pool_is_set(lux, ec2):
    template = {**ec2.template, "userData": "cloud-init-yaml"}
    with pytest.raises(CLIError) as e:
        lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps(template))
    assert "userData" in e.value.stderr, e.value.stderr
