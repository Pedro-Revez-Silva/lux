"""Network transfer per billing class, counted by the runner's nftables
counters on real hosts. The harness's hosts are static (no provider
catalog), so traffic on the hosts' own network is local and anything else
unknown, never internet; dropped traffic counts nowhere."""

from __future__ import annotations

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def fetch(target: str, timeout: int = 3) -> str:
    """A shell line that prints OK:<target> or NO:<target>."""
    return f"wget -q -T {timeout} -O /dev/null http://{target}/ && echo OK:{target} || echo NO:{target}"


def probe(lux, run_id: str) -> str:
    lux.wait_state(run_id, "succeeded", "failed", timeout=60)
    return lux.logs(run_id)


def net_classes(lux, run_id: str) -> dict:
    return (lux.get(run_id).get("usage") or {}).get("netClasses") or {}


def test_forwarded_bytes_are_local_both_ways(lux, runners, egress_hosts, net_targets):
    """A Run fetching a server on the hosts' network: bytes in local, tx
    and rx (the reply counts too), and no internet without a catalog."""
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(t.allowed_ip)] * 5),
                                network={"unrestricted": True}))
    assert f"OK:{t.allowed_ip}" in probe(lux, run_id)
    # The exit status carries the counters' final reading.
    net = wait_until(lambda: net_classes(lux, run_id) or None, 30, 1, "no netClasses reported")
    assert net.get("local", {}).get("tx", 0) > 0 and net.get("local", {}).get("rx", 0) > 0, net
    assert set(net) <= {"local", "unknown"}, net
    placement = lux.get(run_id)["placements"][-1]
    assert placement.get("netClasses") == net, placement


def test_dropped_traffic_is_not_counted(lux, runners, egress_hosts, net_targets):
    """Every packet of a restricted Run to an address it may not reach is
    dropped before the counters, and its DNS goes to its host's stub."""
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(t.denied_ip, 1)] * 5 + [fetch(t.denied_name, 1)]),
                                network={"egress": [{"cidr": f"{t.allowed_ip}/32"}]}))
    out = probe(lux, run_id)
    assert f"NO:{t.denied_ip}" in out, out
    assert net_classes(lux, run_id) == {}, net_classes(lux, run_id)


def test_host_counts_its_own_traffic(lux, runners, egress_hosts):
    """The runner's own link to luxd is host traffic, local on this
    network, and shows on the host."""
    host = egress_hosts[0]
    runners.start(host)
    net = wait_until(lambda: lux.json("hosts", "get", host.name).get("netClasses") or None, 90, 2, "no host netClasses")
    assert net.get("local", {}).get("tx", 0) > 0 and net.get("local", {}).get("rx", 0) > 0, net
    assert "internet" not in net, net
