"""Step 10: nested containers. A Run that opts in (sandbox.nestedContainers)
can run rootless Podman inside its container, on hosts that offer it
(`lux-runner --nested`), without --privileged: it stays in its own user
namespace, and what it starts inside has the Run's egress and no more."""

from __future__ import annotations

from build import NESTED_IMAGE as NESTED
from conftest import generic
from env import ALPINE_IMAGE

INNER = f"podman load -q -i /opt/alpine.tar >/dev/null && podman run --rm {ALPINE_IMAGE}"


def nested(script: str, **extra) -> dict:
    # Errors to stdout, which the assertions show: an inner engine that
    # fails says why only on stderr.
    return generic(NESTED, "sh", "-c", f"exec 2>&1; {script}", sandbox={"nestedContainers": True}, **extra)


def test_a_run_runs_containers_inside(lux, runners, hosts):
    runners.start(hosts[0], "--nested")
    run_id = lux.submit(nested(f"{INNER} echo inner-ok"))
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=120)
    assert run["state"] == "succeeded" and "inner-ok" in lux.logs(run_id), (run.get("stateReason"), lux.logs(run_id))


def test_only_on_hosts_that_offer_it(lux, runners, hosts):
    """A nested Run waits for a host started with --nested; a plain Run
    does not, and a host token cannot claim the label."""
    # Neither the runner's --label nor its host token's labels can claim it.
    runners.start(hosts[0], "--label", "nested=true", token=runners.token("--label", "nested=true"))
    run_id = lux.submit(nested("echo placed"))
    plain = lux.submit(generic(ALPINE_IMAGE, "echo", "plain"))
    lux.wait_state(plain, "succeeded")
    assert lux.get(run_id)["state"] in ("submitted", "provisioning"), lux.get(run_id)
    hosts_ls = {h["name"]: h for h in lux.json("hosts", "ls")}
    assert hosts_ls[hosts[0].name]["labels"].get("nested") != "true"
    runners.start(hosts[1], "--nested")
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert lux.get(run_id)["placements"][0]["hostName"] == hosts[1].name


def test_without_the_opt_in_podman_inside_fails(lux, runners, hosts):
    """The same image without sandbox.nestedContainers, on a --nested host:
    the workload cannot start containers (so the opt-in is what grants it)."""
    runners.start(hosts[0], "--nested")
    run_id = lux.submit(generic(NESTED, "sh", "-c", f"{INNER} echo inner-ok || echo NO-NESTING"))
    lux.wait_state(run_id, "succeeded", "failed", timeout=120)
    out = lux.logs(run_id)
    assert "inner-ok" not in out and "NO-NESTING" in out, out


def test_nested_is_not_privileged(lux, runners, hosts):
    """Still an unprivileged uid range on the host, without CAP_SYS_ADMIN,
    and no host devices beyond fuse and tun. The workload starts with no
    capabilities of its own."""
    runners.start(hosts[0], "--nested")
    run_id = lux.submit(nested(
        "awk '{print $2}' /proc/self/uid_map; grep CapBnd /proc/self/status; ls /dev | tr '\\n' ' '; echo; "
        "grep CapEff /proc/self/status"))
    lux.wait_state(run_id, "succeeded", timeout=60)
    host_uid, cap, devs, eff = lux.logs(run_id).split("\n")[:4]
    assert int(eff.split()[1], 16) == 0, f"the workload holds capabilities: {eff}"
    assert host_uid.strip() != "0", "nested Run is host root"
    assert int(cap.split()[1], 16) & (1 << 21) == 0, f"CAP_SYS_ADMIN: {cap}"
    for d in ("sda", "nvme0n1", "kmsg", "mem"):
        assert d not in devs.split(), devs


def test_inner_containers_have_the_runs_egress(lux, runners, egress_hosts, net_targets):
    """Containers the Run starts go out through the Run's network: its
    egress rules, and the hard blocks, apply to them."""
    runners.start(egress_hosts[0], "--nested")
    t = net_targets
    probe = " ; ".join(f"wget -q -T 3 -O /dev/null http://{ip}/ && echo OK:{ip} || echo NO:{ip}"
                       for ip in (t.allowed_ip, t.denied_ip, "169.254.169.254"))
    run_id = lux.submit(nested(f"{INNER} sh -c '{probe}'", network={"egress": [{"cidr": f"{t.allowed_ip}/32"}]}))
    lux.wait_state(run_id, "succeeded", "failed", timeout=120)
    out = lux.logs(run_id)
    assert f"OK:{t.allowed_ip}" in out, out
    assert f"NO:{t.denied_ip}" in out and "NO:169.254.169.254" in out, out
