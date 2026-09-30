"""Step 7: egress. Default deny: a Run reaches only the CIDRs and hostnames
its spec allows. Its DNS is a stub that answers only allowed names and
records every lookup. The metadata service, the host and the control plane
are never reachable."""

from __future__ import annotations


from conftest import generic
from env import ALPINE_IMAGE, wait_until


def fetch(target: str, timeout: int = 3) -> str:
    """A shell line that prints OK:<target> or NO:<target>."""
    return f"wget -q -T {timeout} -O /dev/null http://{target}/ && echo OK:{target} || echo NO:{target}"


def probe(lux, run_id: str) -> str:
    lux.wait_state(run_id, "succeeded", "failed", timeout=60)
    return lux.logs(run_id)


def test_default_deny(lux, runners, egress_hosts, net_targets):
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(t.allowed_ip), fetch(t.denied_ip)])))
    out = probe(lux, run_id)
    assert f"NO:{t.allowed_ip}" in out and f"NO:{t.denied_ip}" in out, out


def test_allowed_cidr_only(lux, runners, egress_hosts, net_targets):
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(t.allowed_ip), fetch(t.denied_ip)]),
                                network={"egress": [{"cidr": f"{t.allowed_ip}/32"}]}))
    out = probe(lux, run_id)
    assert f"OK:{t.allowed_ip}" in out and f"NO:{t.denied_ip}" in out, out


def test_allowed_hostname_resolves_and_connects(lux, runners, egress_hosts, net_targets):
    """A hostname rule: the runner resolves it, the stub answers it, and
    only its addresses are reachable. Other names do not resolve at all."""
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([
        fetch(t.allowed_name), fetch(t.denied_name), fetch(t.denied_ip),
        f"nslookup {t.denied_name} >/dev/null 2>&1 && echo RESOLVED:denied || echo UNRESOLVED:denied",
    ]), network={"egress": [{"host": t.allowed_name}]}))
    out = probe(lux, run_id)
    assert f"OK:{t.allowed_name}" in out, out
    assert f"NO:{t.denied_name}" in out and f"NO:{t.denied_ip}" in out, out
    assert "UNRESOLVED:denied" in out, out
    dns = [e["data"] for e in lux.json("events", run_id) if e["type"] == "dns"]
    assert {"name": t.allowed_name, "allowed": True} .items() <= next(d for d in dns if d["name"] == t.allowed_name).items()
    assert any(d["name"] == t.denied_name and not d["allowed"] for d in dns), dns


def test_dns_to_other_servers_is_redirected(lux, runners, egress_hosts, net_targets):
    """A container asking another DNS server directly gets the stub's
    answer (refused for unlisted names): DNS is not a way out."""
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c",
                                f"nslookup {t.denied_name} {t.dns_ip} >/dev/null 2>&1 && echo LEAK || echo REFUSED",
                                network={"egress": [{"host": t.allowed_name}]}))
    assert "REFUSED" in probe(lux, run_id)


def test_metadata_and_control_plane_are_blocked(env, lux, runners, egress_hosts, net_targets):
    """Even allowed explicitly (by CIDR), the metadata range and the control
    plane are never reachable. luxd is really listening, and the same fetch
    from an unrestricted Run succeeds, so a NO here is the rule, not a
    missing route."""
    runners.start(egress_hosts[0])
    luxd = f"{env.gateway}:{env.luxd_port}"
    check = f"wget -q -T 3 -O /dev/null http://{luxd}/health && echo OK:luxd || echo NO:luxd"
    denied = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([check, fetch("169.254.169.254"), fetch("meta.lux.test")]),
                                network={"egress": [{"cidr": "169.254.0.0/16"}, {"cidr": f"{env.gateway}/32"},
                                                    {"host": "meta.lux.test"}]}))
    out = probe(lux, denied)
    assert "OK:" not in out, out
    control = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", check, network={"egress": [{"cidr": "0.0.0.0/0"}]}))
    assert "NO:luxd" in probe(lux, control), "0.0.0.0/0 must not open the control plane either"


def test_hard_blocks_hold_against_allow_all(env, lux, runners, egress_hosts, net_targets):
    """The address a hostname resolves to cannot open a blocked range: an
    allowed name pointing at the metadata address stays unreachable, and
    DNS still answers (the stub does not hide the name, the firewall
    blocks the address)."""
    runners.start(egress_hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "nslookup meta.lux.test >/dev/null 2>&1 && echo RESOLVED || echo UNRESOLVED",
                                network={"egress": [{"host": "meta.lux.test"}]}))
    probe(lux, run_id)
    dns = [e["data"] for e in lux.json("events", run_id) if e["type"] == "dns" and e["data"]["name"] == "meta.lux.test"]
    assert dns and dns[0]["allowed"] and not dns[0].get("answers"), dns


def test_unrestricted(lux, runners, egress_hosts, net_targets):
    """Anything but the hard blocks, and names resolve through the host's
    resolvers (not the stub: denied.lux.test is not in any rule)."""
    runners.start(egress_hosts[0])
    t = net_targets
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "; ".join([fetch(t.denied_ip), fetch(t.denied_name)]),
                                network={"unrestricted": True}))
    out = probe(lux, run_id)
    assert f"OK:{t.denied_ip}" in out and f"OK:{t.denied_name}" in out, out


def test_dns_events_are_distinct_lookups(lux, runners, egress_hosts, net_targets):
    """Each (name, allowed) is one event however often it is looked up: the
    workload chooses the names, and must not flood the control plane."""
    runners.start(egress_hosts[0])
    t = net_targets
    lookups = "; ".join(f"nslookup {n} >/dev/null 2>&1" for n in [t.allowed_name, t.denied_name] * 5)
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", lookups, network={"egress": [{"host": t.allowed_name}]}))
    probe(lux, run_id)
    names = sorted(e["data"]["name"] for e in lux.json("events", run_id) if e["type"] == "dns")
    assert names == sorted([t.allowed_name, t.denied_name]), names


def test_rules_survive_a_runner_restart(lux, runners, egress_hosts, net_targets):
    """The runner restarts while a Run is live: the Run keeps its rules,
    with no window where it can reach what it may not."""
    runners.start(egress_hosts[0])
    t = net_targets
    script = (f"while :; do {fetch(t.allowed_ip, 1)}; {fetch(t.denied_ip, 1)}; sleep 0.5; done")
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", script,
                                network={"egress": [{"cidr": f"{t.allowed_ip}/32"}]}))
    lux.wait_output(run_id, f"OK:{t.allowed_ip}")
    runners.stop(egress_hosts[0], "KILL")
    runners.start(egress_hosts[0])
    wait_until(lambda: lux.logs(run_id).count(f"OK:{t.allowed_ip}") > 10, 60, 1, "the Run lost its allowed egress")
    assert f"OK:{t.denied_ip}" not in lux.logs(run_id)
    lux.run("cancel", run_id)


def test_runs_cannot_reach_each_other(lux, runners, egress_hosts, net_targets):
    """A Run allowed everything (0.0.0.0/0) still cannot reach another Run
    on the same host. Control: the host itself can, so the block is the
    rule, not a missing route."""
    host = egress_hosts[0]
    runners.start(host)
    serve = "hostname -i; while :; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\nhi\\n' | nc -l -p 8080 >/dev/null; done"
    server = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", serve, network={"unrestricted": True}))
    ip = wait_until(lambda: lux.logs(server).strip(), 30, 0.5, "no address")
    assert "hi" in host.exec("sh", "-c", f"curl -s -m 3 http://{ip}:8080/ || true")
    client = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", fetch(f"{ip}:8080"),
                                network={"egress": [{"cidr": "0.0.0.0/0"}]}))
    assert f"NO:{ip}:8080" in probe(lux, client)
    lux.run("cancel", server)
