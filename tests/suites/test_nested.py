"""Step 10: nested containers. A Run that opts in (sandbox.nestedContainers)
can run rootless Podman inside its container, on hosts that offer it
(`lux-runner --nested`), without --privileged: it stays in its own user
namespace, and what it starts inside has the Run's egress and no more."""

from __future__ import annotations

from build import NESTED_IMAGE as NESTED
from conftest import generic
from env import ALPINE_IMAGE, wait_until

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


# A workload that serves Podman's API as real ones do: default runtime
# directories (no XDG_RUNTIME_DIR, so under /tmp), nothing cleaned first.
# It reports what an earlier placement left outside its volumes, then whether
# the engine serves (loads an image, creates a container) and still answers.
# A stale pause.pid is what breaks Podman when it survives: the pid is taken
# by another process here, as a busy workload's processes take it by chance.
ENGINE_SERVICE = """
n=$(echo x >> /home/agent/epochs; wc -l < /home/agent/epochs)
[ -e /tmp/leak ] && echo "LEAK:/tmp/leak"
ls -d /tmp/storage-run-* /tmp/podman-run-* /tmp/containers-user-* 2>/dev/null | sed 's/^/LEFTOVER:/'
pp=$(cat /tmp/storage-run-*/libpod/tmp/pause.pid 2>/dev/null)
if [ -n "$pp" ]; then
  while :; do sh -c : & wait $!; [ $! -ge $((pp-1)) ] && break; done
  sleep 600 & sp=$!
  if [ "$sp" = "$pp" ]; then
    echo "PAUSE-PID-TAKEN:$pp:$sp"
  else
    echo "PAUSE-PID-MISSED:$pp:$sp"
  fi
fi
date > /tmp/leak
r="podman --remote --url unix:///tmp/engine.sock"
podman system service --time=0 unix:///tmp/engine.sock >/tmp/engine.log 2>&1 &
for i in $(seq 60); do $r version >/dev/null 2>&1 && break; sleep 0.5; done
$r load -q -i /opt/alpine.tar >>/tmp/engine.log 2>&1
$r create --network=none """ + ALPINE_IMAGE + """ true >>/tmp/engine.log 2>&1 || echo "INNER-FAILED:$n"
sleep 3
if $r version >/dev/null 2>>/tmp/engine.log; then echo "ENGINE-OK:$n"; else echo "ENGINE-FAILED:$n"; fi
sed "s/^/LOG$n: /" /tmp/engine.log
echo "DONE:$n"
sleep 600
"""


def test_a_same_host_resume_starts_as_on_a_new_host(lux, runners, hosts):
    """A resume on the host the Run stopped on gets a new container, as on
    any other host: only its volumes are kept. Nothing the previous
    placement left in /tmp (an inner engine's runtime state, which points at
    a store the resume emptied, and pids of processes gone) reaches the
    workload, and its engine starts."""
    runners.start(hosts[0], "--nested")
    home = [{"name": "home", "path": "/home/agent", "kind": "state"}]
    run_id = lux.submit(nested(ENGINE_SERVICE, volumes=home))
    out = lux.wait_output(run_id, "DONE:1", timeout=120)
    assert "ENGINE-OK:1" in out and "INNER-FAILED" not in out, out
    lux.run("stop", run_id, "--wait")
    lux.run("resume", run_id, "--wait")
    out = lux.wait_output(run_id, "DONE:2", timeout=120)
    lux.run("cancel", run_id, "--wait")
    assert "ENGINE-OK:2" in out and "INNER-FAILED" not in out, out
    assert "LEAK:" not in out and "LEFTOVER:" not in out, out
    assert lux.events(run_id, "volumes.local"), "the resume did not keep its volumes on the host"


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
