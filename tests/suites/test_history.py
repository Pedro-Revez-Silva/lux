"""History: resource use of hosts and Runs, and the system's state, over
time — through the CLI, scoped like everything else."""

from __future__ import annotations

import re

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
    # And the runner process's own use, its CPU a rate once two samples exist.
    rs = [s["runner"] for s in hh["samples"] if "runner" in s]
    assert len(rs) >= 2, hh
    r = rs[-1]
    assert r["started"] and r["cpuCores"] >= 0 and r["goroutines"] > 0, r
    # The peak is absent where the process cannot reset the kernel's mark.
    assert 0 < r["heapBytes"] and 0 < r["rssBytes"] <= r.get("peakRssBytes", r["rssBytes"]), r
    assert "runner rss" in lux.run("history", "--host", hosts[0].name).stdout
    # The text form draws sparklines; CPU in cores, or millicores under one.
    out = lux.run("history", run_id).stdout
    assert re.search(r"^cpu .* (\d+(\.\d+)?m|[\d.]+ cores?)  \(", out, re.M), out
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
    # The system tick samples luxd's own machine, its Postgres and itself:
    # the operator's whole-system history carries them once a couple exist.
    def control(lux, *args):
        return lux.json("history", "--since", "5m", *args).get("control")
    c = wait_until(lambda: (lambda c: c if c and c["luxd"] and len(c["luxd"][-1]["samples"]) >= 2 else None)(control(operator)), 60, 1,
                   "no control host in the operator's history")
    # This environment's luxd is one process, on one machine: its id is its own, not the hostname.
    luxd, machine = c["luxd"][-1], c["machines"][-1]
    assert luxd["instance"].startswith("luxd_") and luxd["hostname"] == machine["hostname"], luxd["instance"]
    m, pg = machine["samples"][-1], c["postgres"][-1]
    assert m["cpus"] > 0 and m["cpuCores"] >= 0 and 0 < m["memoryBytes"] <= m["memoryTotal"], m
    assert pg["bytes"] > 0 and pg["connections"] >= 1, pg
    assert [d["path"] for d in m["disks"]] == ["/"], m
    d = m["disks"][0]
    assert 0 < d["usedBytes"] and d["usedBytes"] + d["freeBytes"] <= d["totalBytes"], d
    # luxd's own process: one start across the samples, so a CPU rate.
    p = luxd["samples"][-1]
    assert p["started"] and p["cpuCores"] >= 0 and p["goroutines"] > 0, p
    assert 0 < p["heapBytes"] and 0 < p["rssBytes"] <= p.get("peakRssBytes", p["rssBytes"]), p
    out = operator.run("history", "--since", "5m").stdout
    assert "luxd goroutines" in out and f"machine {machine['hostname']}" in out and "postgres" in out, out
    # A tenant, and an operator narrowed to that tenant, never see it. An idle
    # tenant has no samples of its own; the unit tests cover the gate on
    # tenant samples that share the control samples' instants.
    assert control(a) is None
    assert control(operator, "--tenant", a.tenant_id) is None
