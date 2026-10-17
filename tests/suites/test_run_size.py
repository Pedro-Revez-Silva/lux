"""Run size: what a Run's processes see of its size. A runner with
--cpuset pins each Run to ceil(resources.cpus) of its CPUs; containers a
nested Run with sandbox.cgroupDelegation starts with Podman default to the
Run's memory and CPUs (a precreate OCI hook, docs/podman.md); Docker
containers in a nested Run without delegation stay in the Run's cgroup; and
real applications that size themselves from their cgroup (ClickHouse, the
JVM) size to the Run, not the host. A runner without --cpuset behaves as
before."""

from __future__ import annotations

import pytest

from build import DOCKER_IMAGE, NESTED_IMAGE, SIZING_IMAGE
from conftest import generic
from env import ALPINE_IMAGE

GIB = 1 << 30
SIZE = {"cpus": 2, "memory": "1Gi"}
# The sizing tests' Run: it asks 2 GiB of a runner offering twice what the
# host can give Runs, so its limit is half of that, ~1 GiB, on any host
# (scaled_runner). What it asked and what it got are then far apart.
SIZED = {"cpus": 2, "memory": "2Gi"}
SIZED_MEMORY = 2 * GIB
INNER = "podman load -q -i /opt/alpine.tar >/dev/null && podman run --rm --network=none"
# One line of what the process sees: its memory.max, cpu.max, nproc and
# affinity. In a script file run inside the Run, not on the host.
SHOW = ("echo mem=$(cat /sys/fs/cgroup/memory.max) cpu=$(tr ' ' _ < /sys/fs/cgroup/cpu.max) "
        "nproc=$(nproc) aff=$(grep Cpus_allowed_list /proc/self/status | cut -f2)")
# SHOW as a file, for inner containers: written by the Run, mounted in.
SHOW_FILE = "cat > /tmp/show.sh <<'EOF'\n" + SHOW + "\nEOF\n"
SHOW_INNER = "-v /tmp/show.sh:/show.sh:ro " + ALPINE_IMAGE + " sh /show.sh"


def fields(out: str, prefix: str) -> dict[str, str]:
    """The key=value words of the output line starting with prefix."""
    line = next((l for l in out.splitlines() if l.startswith(prefix)), "")
    return dict(w.split("=", 1) for w in line.split() if "=" in w)


def nested(script: str, image: str = NESTED_IMAGE, *, cgroup_delegation: bool = False, **extra) -> dict:
    sandbox = {"nestedContainers": True}
    if cgroup_delegation:
        sandbox["cgroupDelegation"] = True
    return generic(image, "sh", "-c", f"exec 2>&1; {script}", sandbox=sandbox, resources=SIZED, **extra)


def cpu_list(s: str) -> set[int]:
    """The CPUs of a kernel cpu-list ("0-2,5")."""
    out: set[int] = set()
    for part in s.strip().split(","):
        lo, _, hi = part.partition("-")
        out |= set(range(int(lo), int(hi or lo) + 1))
    return out


def test_cpu_list_parses_ranges_and_commas():
    assert cpu_list("0-2,5") == {0, 1, 2, 5}
    assert cpu_list("3") == {3}
    assert cpu_list("1,3") == {1, 3}
    assert cpu_list("0-1,4-5\n") == {0, 1, 4, 5}
    assert cpu_list("0-2") & cpu_list("1,3") == {1}


def page_size(host) -> int:
    return int(host.exec("getconf", "PAGESIZE").strip())


def scaled_runner(runners, host, *flags: str) -> int:
    """Starts host's runner offering twice the memory the host can give
    Runs (MemTotal less the default 512 MiB headroom), so a Run's limit is
    half of what it asks, and returns a SIZED Run's limit: SIZED_MEMORY/2
    in whole pages (1 GiB), whatever the host's RAM."""
    kb = next(l.split()[1] for l in host.exec("cat", "/proc/meminfo").splitlines() if l.startswith("MemTotal:"))
    offered = 2 * (int(kb) * 1024 - (512 << 20))
    runners.start(host, "--memory", str(offered), *flags)
    limit = host.memory_limit(SIZED_MEMORY, offered=offered)
    assert limit < SIZED_MEMORY, (limit, offered)
    return limit


def run_to_end(lux, spec: dict, timeout: float = 240) -> str:
    run_id = lux.submit(spec)
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=timeout)
    out = lux.logs(run_id)
    assert run["state"] == "succeeded", (run.get("stateReason"), out)
    return out


def host_cpus(host) -> int:
    return int(host.exec("nproc").strip())


# ---- cpuset ------------------------------------------------------------------

def test_a_run_sees_its_cpus(lux, runners, hosts):
    """With --cpuset a 2-CPU Run is pinned to 2 CPUs: nproc, its affinity,
    its cgroup's cpuset and LUX_CPUSET agree."""
    runners.start(hosts[0], "--cpuset")
    out = run_to_end(lux, generic(ALPINE_IMAGE, "sh", "-c",
                                  f"{SHOW} set=$(cat /sys/fs/cgroup/cpuset.cpus.effective) env=$LUX_CPUSET",
                                  resources=SIZE))
    f = fields(out, "mem=")
    assert f["nproc"] == "2" and f["aff"] == f["set"] == f["env"], f
    assert f["cpu"] == "200000_100000", f


def test_a_fractional_run_gets_one_cpu(lux, runners, hosts):
    runners.start(hosts[0], "--cpuset")
    out = run_to_end(lux, generic(ALPINE_IMAGE, "sh", "-c", SHOW, resources={"cpus": 0.5, "memory": "256Mi"}))
    f = fields(out, "mem=")
    assert f["nproc"] == "1" and f["cpu"] == "50000_100000", f


def test_concurrent_whole_cpu_runs_get_disjoint_sets(lux, runners, hosts):
    runners.start(hosts[0], "--cpuset")
    ids = [lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
           for _ in range(2)]
    sets = []
    for run_id in ids:
        out = lux.wait_output(run_id, "set=", timeout=90)
        sets.append(cpu_list(fields(out, "set=")["set"]))
    for run_id in ids:
        lux.run("terminate", run_id)
    assert all(len(s) == 2 for s in sets) and not sets[0] & sets[1], sets


def test_a_restarted_runner_keeps_the_set(lux, runners, hosts):
    """The set is in run state: a runner restart re-adopts the Run with it,
    and the next Run is pinned beside it, not over it."""
    runners.start(hosts[0], "--cpuset")
    first = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
    before = fields(lux.wait_output(first, "set=", timeout=90), "set=")["set"]
    runners.stop(hosts[0])
    runners.start(hosts[0], "--cpuset")
    lux.wait_state(first, "running")
    second = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo set=$LUX_CPUSET; sleep 600", resources=SIZE))
    after = fields(lux.wait_output(second, "set=", timeout=90), "set=")["set"]
    lux.run("exec", first, "-T", "--", "true", input="")  # still the re-adopted container
    pinned = hosts[0].podman("inspect", f"lux-{first}", "--format", "{{.HostConfig.CpusetCpus}}").strip()
    for r in (first, second):
        lux.run("terminate", r)
    assert pinned == before, (pinned, before)
    b, a = cpu_list(before), cpu_list(after)
    assert len(b) == len(a) == 2 and not b & a, (before, after)


def test_without_cpuset_nothing_is_pinned(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", f"{SHOW} env=${{LUX_CPUSET:-none}}; sleep 600",
                                resources=SIZE))
    out = lux.wait_output(run_id, "mem=", timeout=90)
    pinned = hosts[0].podman("inspect", f"lux-{run_id}", "--format", "{{.HostConfig.CpusetCpus}}").strip()
    lux.run("terminate", run_id)
    f = fields(out, "mem=")
    assert pinned == "" and f["env"] == "none", (pinned, f)
    assert int(f["nproc"]) == host_cpus(hosts[0]), f


# ---- nested containers -------------------------------------------------------

def test_inner_containers_default_to_the_runs_size(lux, runners, hosts):
    """In a nested Run with cgroup delegation, an inner Podman container
    without -m or --cpus has the Run's
    memory.max and CPU quota, and the Run's CPUs; explicit limits within the
    Run's win, larger ones are lowered to the Run's. The explicit sizes are
    derived from the Run's limit, so they hold on any host."""
    limit = scaled_runner(runners, hosts[0], "--nested", "--cpuset")
    inner = limit // 2 // page_size(hosts[0]) * page_size(hosts[0])
    out = run_to_end(lux, nested(
        SHOW_FILE +
        "echo \"run: $(sh /tmp/show.sh)\"; "
        f"echo \"none: $({INNER} {SHOW_INNER})\"; "
        f"echo \"flags: $(podman run --rm --network=none -m {inner}b --cpus 1 {SHOW_INNER})\"; "
        f"echo \"over: $(podman run --rm --network=none -m {2 * limit}b --cpus 4 {SHOW_INNER})\"; "
        f"echo \"info: cpus=$(podman info --format '{{{{.Host.CPUs}}}}')\"", cgroup_delegation=True))
    run, none, flags, over = fields(out, "run:"), fields(out, "none:"), fields(out, "flags:"), fields(out, "over:")
    assert run["mem"] == str(limit) and run["nproc"] == "2", out
    assert none["mem"] == str(limit) and none["cpu"] == "200000_100000" and none["nproc"] == "2", out
    assert none["aff"] == run["aff"], out
    assert flags["mem"] == str(inner) and flags["cpu"] == "100000_100000", out
    assert over["mem"] == str(limit) and over["cpu"] == "200000_100000", out
    assert fields(out, "info:")["cpus"] == "2", out


DOCKERD = """
export XDG_RUNTIME_DIR=/tmp/xdg DOCKER_HOST=unix:///tmp/xdg/docker.sock; mkdir -p /tmp/xdg
dockerd-rootless >/tmp/dockerd.log 2>&1 &
for i in $(seq 90); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 && echo "dockerd: up=1" || { echo "dockerd: up=0"; tail -20 /tmp/dockerd.log; }
docker load -q -i /opt/alpine.tar >/dev/null
"""

_docker_out: dict[str, tuple[str, int]] = {}
# SHOW plus the process's own cgroup and that cgroup's memory.max, which
# ClickHouse and the JVM size from (test_nested_docker.py's CGROUP_PROBE).
SHOW_OWN_FILE = ("cat > /tmp/show.sh <<'EOF'\n" + SHOW +
                 " own=$(cut -d: -f3 /proc/self/cgroup) ownmem=$(cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.max)"
                 "\nEOF\n")


def docker_probe(lux, runners, hosts) -> tuple[str, int]:
    """What an inner rootless Docker container sees in a 2-CPU nested Run
    without cgroup delegation (--cpuset), and the Run's limit; run once per
    session. A dockerd that did not start, or no inner line, fails here."""
    if "out" not in _docker_out:
        limit = scaled_runner(runners, hosts[0], "--nested", "--cpuset")
        run_id = lux.submit(nested(SHOW_OWN_FILE + DOCKERD + f"echo \"inner: $(docker run --rm --network=none {SHOW_INNER})\"; "
                                   "echo \"info: cpus=$(docker info --format '{{.NCPU}}')\"", image=DOCKER_IMAGE))
        run = lux.wait_state(run_id, "succeeded", "failed", timeout=240)
        out = lux.logs(run_id)
        if run["state"] != "succeeded" or fields(out, "dockerd:").get("up") != "1" or "mem" not in fields(out, "inner:"):
            pytest.fail(f"the Docker probe did not run: {run.get('stateReason')}\n{out}")
        _docker_out["out"] = (out, limit)
    return _docker_out["out"]


def test_inner_docker_containers_get_the_runs_cpus(lux, runners, hosts):
    """Docker containers in a pinned nested Run get the Run's CPUs: their
    affinity, and the engine's NCPU."""
    out, _ = docker_probe(lux, runners, hosts)
    assert fields(out, "inner:")["nproc"] == "2", out
    assert fields(out, "info:")["cpus"] == "2", out


def test_inner_docker_containers_see_the_runs_size(lux, runners, hosts):
    """Without cgroup delegation rootless runc leaves Docker's containers in
    the Run's cgroup: their own cgroup is the Run's, with its memory.max and
    CPU quota."""
    out, limit = docker_probe(lux, runners, hosts)
    inner = fields(out, "inner:")
    assert inner["own"] == "/" and inner["ownmem"] == str(limit), out
    assert inner["mem"] == str(limit) and inner["cpu"] == "200000_100000", out


# ---- real applications -------------------------------------------------------

# /tmp/apps.sh LABEL: starts the real ClickHouse server (through the glibc
# loader beside it, tests/images/sizing) and prints what it sized to, then
# each JVM's default heap and CPU count; lines "LABEL-ch:", "LABEL-jvm21:",
# "LABEL-jvm17:". Runs in the Run or, with /opt/apps mounted, in an inner
# alpine container.
APPS = r"""cat > /tmp/apps.sh <<'EOF'
L="/opt/apps/glibc/ld-2.35.so --library-path /opt/apps/glibc /opt/apps/clickhouse"
CH=/tmp/ch-$1; mkdir -p $CH/etc && cp -r /opt/apps/clickhouse-server/. $CH/etc/
sed -i "s#/var/lib/clickhouse/#$CH/#g; s#/var/log/clickhouse-server/#$CH/#g" $CH/etc/config.xml
$L server --config-file=$CH/etc/config.xml -- --logger.console=0 --listen_host=127.0.0.1 >$CH/out.log 2>&1 &
ch=$!
for i in $(seq 600); do $L client -q 'SELECT 1' >/dev/null 2>&1 && break; kill -0 $ch || break; sleep 0.5; done
$L client -q "SELECT '$1-ch: mem=' || value || ' threads=' || toString(getSetting('max_threads')) FROM system.server_settings WHERE name='max_server_memory_usage'" \
  || grep -v '^ *[0-9]*\. ' $CH/out.log | head -5
kill $ch; wait $ch
echo "$1-nproc: n=$(nproc)"
for v in 21 17; do
  /opt/apps/jvm$v/bin/java -XX:+PrintFlagsFinal -XshowSettings:system -version 2>&1 |
    awk -v p="$1-jvm$v:" '/ MaxHeapSize /{h=$4} /Effective CPU Count/{c=$4} END{print p " heap=" h " cpus=" c}'
done
EOF
"""
APPS_INNER = "-v /opt/apps:/opt/apps:ro -v /tmp/apps.sh:/apps.sh:ro " + ALPINE_IMAGE + " sh /apps.sh"


def assert_sized(out: str, label: str, limit: int, cpus: int, wrong: int) -> None:
    """ClickHouse keeps max_server_memory_usage at 0.9 of the memory it
    sees, the JVM's default heap is a quarter of it; both use as many
    threads as CPUs they see. limit is the memory the row must see; wrong,
    the one it would see if sized from the wrong source (what the Run
    asked, or the Run's limit for an explicit -m). The tolerances (1% of
    limit for ClickHouse, 4 MiB for the heap's alignment) must tell them
    apart, or the row proves nothing and the test fails."""
    ch_tol, heap_tol = 0.01 * limit, 4 << 20
    assert abs(0.9 * wrong - 0.9 * limit) > ch_tol and abs(wrong // 4 - limit // 4) > heap_tol, \
        f"{label}: limit {limit} and {wrong} are within the tolerances: the row would prove nothing"
    print(*(l for l in out.splitlines() if l.startswith(f"{label}-")), sep="\n")
    ch = fields(out, f"{label}-ch:")
    assert "mem" in ch and abs(int(ch["mem"]) - 0.9 * limit) < ch_tol and ch["threads"] == str(cpus), \
        (label, ch, limit, out)
    assert abs(int(ch["mem"]) - 0.9 * wrong) > ch_tol, (label, ch, wrong)
    for v in (21, 17):
        jvm = fields(out, f"{label}-jvm{v}:")
        assert jvm.get("heap") and abs(int(jvm["heap"]) - limit // 4) <= heap_tol and jvm["cpus"] == str(cpus), \
            (label, v, jvm, limit, out)
        assert abs(int(jvm["heap"]) - wrong // 4) > heap_tol, (label, v, jvm, wrong)


def test_clickhouse_and_the_jvm_size_to_a_plain_run(lux, runners, hosts):
    limit = scaled_runner(runners, hosts[0], "--cpuset")
    out = run_to_end(lux, generic(SIZING_IMAGE, "sh", "-c", APPS + "sh /tmp/apps.sh run", resources=SIZED),
                     timeout=420)
    assert_sized(out, "run", limit, 2, SIZED_MEMORY)


@pytest.mark.parametrize("cpuset", [True, False], ids=["cpuset", "quota-only"])
def test_clickhouse_and_the_jvm_size_to_a_nested_run_and_its_podman_containers(lux, runners, hosts, cpuset):
    """In a nested Run with cgroup delegation, in its Podman containers
    without -m/--cpus (the
    Run's size, from the precreate defaults) and with them (theirs, derived
    from the Run's limit). quota-only: a runner without --cpuset, so every
    process sees all the host's CPUs and only cpu.max (the hook's default
    quota, inside) holds ClickHouse's and the JVM's threads to the Run's."""
    host_n = host_cpus(hosts[0])
    if not cpuset and host_n <= SIZED["cpus"]:
        pytest.skip(f"the host has {host_n} CPUs: without --cpuset the quota cannot be told from the CPU count")
    limit = scaled_runner(runners, hosts[0], "--nested", *(["--cpuset"] if cpuset else []))
    inner = limit // 2 // page_size(hosts[0]) * page_size(hosts[0])
    out = run_to_end(lux, nested(
        APPS + "sh /tmp/apps.sh run; "
        f"{INNER} {APPS_INNER} none; "
        f"podman run --rm --network=none -m {inner}b --cpus 1 {APPS_INNER} flags", image=SIZING_IMAGE,
        cgroup_delegation=True), timeout=900)
    assert_sized(out, "run", limit, 2, SIZED_MEMORY)
    assert_sized(out, "none", limit, 2, SIZED_MEMORY)
    assert_sized(out, "flags", inner, 1, limit)
    nproc = {label: fields(out, f"{label}-nproc:").get("n") for label in ("run", "none", "flags")}
    assert set(nproc.values()) == {str(2 if cpuset else host_n)}, (nproc, out)
