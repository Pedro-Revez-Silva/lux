"""History: resource use of hosts and Runs, and the system's state, over
time — through the CLI, scoped like everything else."""

from __future__ import annotations

import pytest

from conftest import CLIError, Runners, generic
from env import ALPINE_IMAGE, wait_until


def test_run_and_host_history(lux, runners, hosts):
    runners.start(hosts[0])
    # Busy for a while: CPU, memory and a growing pid count to sample.
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c",
                                "head -c 64000000 /dev/zero > /dev/shm/x; while :; do :; done"))
    lux.wait_state(run_id, "running")
    h = wait_until(lambda: (lambda h: h if len([s for s in h["samples"] if s.get("cpuCores")]) >= 2 else None)(
        lux.json("history", run_id)), 60, 1, "no run samples")
    assert h["resolution"] == 0
    s = h["samples"][-1]
    assert s["epoch"] == 1
    assert s["cpuCores"] > 0.3, s  # a busy loop is close to one core
    assert s["memoryBytes"] > 32_000_000, s
    # The host's history has it too, and says what it held.
    hh = lux.json("history", "--host", hosts[0].name)
    assert hh["samples"] and hh["samples"][-1]["placements"] == 1
    assert hh["samples"][-1]["memoryBytes"] > 0
    # The text form draws sparklines.
    out = lux.run("history", run_id).stdout
    assert "cpu" in out and "cores" in out
    lux.run("cancel", run_id)


def test_system_history_is_scoped(tenant_factory, operator, env, hosts):
    a, b = tenant_factory(), tenant_factory()
    ra = Runners(env, a)
    try:
        ra.start(hosts[0])
        run_id = a.submit(generic(ALPINE_IMAGE, "sleep", "300"))
        a.wait_state(run_id, "running")
        # A sample with the Run running, for its tenant, as it and an
        # operator see it.
        def running(lux, *args):
            h = lux.json("history", "--since", "5m", *args)
            return h if any(s.get("runs", {}).get("running") for s in h["samples"]) else None
        wait_until(lambda: running(a), 30, 1, "tenant history never showed the Run")
        wait_until(lambda: running(operator, "--tenant", a.tenant_id), 30, 1, "operator never saw it")
        wait_until(lambda: running(operator), 30, 1, "system history never showed it")
        # Another tenant's history does not.
        assert not running(b)
        a.run("cancel", run_id)
    finally:
        ra.stop_all()


def test_history_of_another_tenants_run_is_not_found(tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    run_id = b.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    with pytest.raises(CLIError) as e:
        a.run("history", run_id)
    assert e.value.code == 3
    b.run("cancel", run_id)


def test_control_host_is_only_in_the_operators_whole_system_history(tenant_factory, operator):
    a = tenant_factory()
    # The system tick samples luxd's own machine: the operator's whole-system
    # history carries it once a couple of samples exist.
    def control(lux, *args):
        h = lux.json("history", "--since", "5m", *args)
        return [s["control"] for s in h["samples"] if "control" in s]
    c = wait_until(lambda: (lambda c: c if len(c) >= 2 else None)(control(operator)), 60, 1,
                   "no control host in the operator's history")[-1]
    assert c["cpus"] > 0 and c["cpuCores"] >= 0 and 0 < c["memoryBytes"] <= c["memoryTotal"], c
    assert c["databaseBytes"] > 0 and c["databaseConnections"] >= 1, c
    assert [d["path"] for d in c["disks"]] == ["/"], c
    d = c["disks"][0]
    assert 0 < d["usedBytes"] and d["usedBytes"] + d["freeBytes"] <= d["totalBytes"], d
    # A tenant, and an operator narrowed to that tenant, never see it.
    wait_until(lambda: a.json("history", "--since", "5m")["samples"] or None, 30, 1, "no tenant samples")
    assert control(a) == []
    assert control(operator, "--tenant", a.tenant_id) == []
