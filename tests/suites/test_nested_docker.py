"""Docker inside a Run: sandbox.nestedContainers also runs rootless Docker
(dockerd, BuildKit, compose), not just Podman, and at native disk speed.

The runner puts each engine's store (~/.local/share/docker and
~/.local/share/containers for a non-root workload) on its own ephemeral
volume. On the container's own overlay root, dockerd cannot use overlay2
(Linux has no overlay on overlay) and falls back to fuse-overlayfs, at
about twice the time for file-heavy builds."""

from __future__ import annotations

from build import DOCKER_IMAGE
from conftest import generic
from env import ALPINE_IMAGE

# Rootless dockerd as the workload user, with no storage driver named: it
# picks overlay2 only where it can. Load alpine so nothing reaches a
# registry. Its runtime directory is cleared first: a reused container
# (same-host resume) keeps /tmp, and a pid file from before.
DOCKERD = f"""
rm -rf /tmp/xdg
export XDG_RUNTIME_DIR=/tmp/xdg DOCKER_HOST=unix:///tmp/xdg/docker.sock; mkdir -p /tmp/xdg
dockerd-rootless >/tmp/dockerd.log 2>&1 &
for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || {{ echo DOCKERD-FAILED; tail -20 /tmp/dockerd.log; exit 1; }}
echo "driver=$(docker info --format '{{{{.Driver}}}}')"
docker load -q -i /opt/alpine.tar >/dev/null
"""

FROM = f"FROM {ALPINE_IMAGE}"


def docker(script: str, **extra) -> dict:
    return generic(DOCKER_IMAGE, "sh", "-c", DOCKERD + script, sandbox={"nestedContainers": True}, **extra)


def test_docker_runs_builds_and_composes(lux, runners, hosts):
    """docker run, both builders, and compose with name resolution between
    services."""
    runners.start(hosts[0], "--nested")
    compose = (
        "services:\n"
        f"  a:\n    image: {ALPINE_IMAGE}\n"
        "    command: sh -c 'sleep 2; nc -w 3 b 8080 </dev/null | grep -q hi && echo compose-ok'\n"
        "    depends_on: [b]\n"
        f"  b:\n    image: {ALPINE_IMAGE}\n"
        "    command: sh -c 'while :; do echo hi | nc -l -p 8080; done'\n")
    script = f"""
docker run --rm {ALPINE_IMAGE} echo run-ok
mkdir -p /tmp/b && printf '{FROM}\\nRUN echo built > /built\\nCMD cat /built\\n' > /tmp/b/Dockerfile
DOCKER_BUILDKIT=0 docker build -q -t classic /tmp/b >/dev/null && docker run --rm classic | sed 's/^/classic-/'
docker build -q -t kit /tmp/b >/dev/null && docker run --rm kit | sed 's/^/buildkit-/'
printf "{compose}" > /tmp/b/compose.yaml
cd /tmp/b && docker compose up --abort-on-container-exit --exit-code-from a 2>&1 | grep -o compose-ok
"""
    run_id = lux.submit(docker(script))
    run = lux.wait_state(run_id, "succeeded", "failed", timeout=180)
    out = lux.logs(run_id).split()
    assert run["state"] == "succeeded", (run.get("stateReason"), out)
    for want in ("run-ok", "classic-built", "buildkit-built", "compose-ok"):
        assert want in out, (want, out)


def test_the_engine_store_is_on_a_volume(lux, runners, hosts):
    """dockerd finds overlay2 without the spec asking for a volume, and its
    store is a mount of its own (not the container's overlay root). The
    same for Podman's store."""
    runners.start(hosts[0], "--nested")
    script = """
awk '$5 == "/home/agent/.local/share/docker" || $5 == "/home/agent/.local/share/containers" {print "mount:" $5}' /proc/self/mountinfo
stat -c 'owner:%u:%n' /home/agent/.local /home/agent/.local/share
mkdir -p /home/agent/.local/share/other && echo home-writable
"""
    run_id = lux.submit(docker(script))
    lux.wait_state(run_id, "succeeded", "failed", timeout=120)
    out = lux.logs(run_id).split()
    # Kernel overlay, whichever name the image store gives it (overlay2, or
    # overlayfs with the containerd store): never fuse-overlayfs or vfs.
    assert any(w in out for w in ("driver=overlay2", "driver=overlayfs")), out
    assert "mount:/home/agent/.local/share/docker" in out, out
    assert "mount:/home/agent/.local/share/containers" in out, out
    # The directories the mounts made stay the workload's.
    assert "owner:1000:/home/agent/.local" in out and "owner:1000:/home/agent/.local/share" in out, out
    assert "home-writable" in out, out


def test_images_are_not_snapshotted(lux, runners, hosts):
    """The engine stores are ephemeral: a stop does not snapshot images and
    layers, even with a state volume over the home they are in, and a
    resume starts with an empty store. The container itself is kept for a
    same-host resume (emptying the store must not remove it)."""
    runners.start(hosts[0], "--nested")
    home = [{"name": "home", "path": "/home/agent", "kind": "state"}]
    # A marker image made from alpine: DOCKERD reloads alpine on every
    # start, never this one.
    script = f"""
echo "images:$(docker images -q marker | wc -l)"
printf '{FROM}\\nRUN touch /marker\\n' | docker build -q -t marker - >/dev/null
echo marker >> /home/agent/kept; echo "kept:$(wc -l < /home/agent/kept)"
sleep 600
"""
    run_id = lux.submit(docker(script, volumes=home))
    lux.wait_output(run_id, "kept:1", timeout=120)
    lux.run("stop", run_id, "--wait")
    snaps = lux.json("snapshots", run_id)
    # Only the home volume, and far smaller than alpine's ~8 MB of layers.
    vols = [v for s in snaps for v in s["manifest"]["volumes"]]
    assert [v["name"] for v in vols] == ["home"], vols
    assert vols[0]["size"] < 1 << 20, vols
    lux.run("resume", run_id, "--wait")
    out = lux.wait_output(run_id, "kept:2", timeout=120)
    assert out.split().count("images:0") == 2, out  # the marker image is gone
    assert lux.events(run_id, "container.reused"), "the same-host resume did not reuse the container"
    lux.run("cancel", run_id, "--wait")


# A file-heavy build, then a run of it: 20k small files and 512 MiB. The
# same work runs on fuse-overlayfs (the store on the container's root, as
# it was before the runner mounted a volume there) for comparison.
BENCH = f"""
mkdir -p /tmp/bench && cd /tmp/bench
printf '{FROM}\\nRUN i=0; while [ $i -lt 20000 ]; do echo $i > /f$i; i=$((i+1)); done\\nRUN tar cf /t.tar /f1* && rm /f*\\nRUN dd if=/dev/zero of=/big bs=1M count=512 2>/dev/null && sha256sum /big >/dev/null\\n' > Dockerfile
t() {{ cut -d' ' -f1 /proc/uptime | tr -d .; }}
s=$(t); docker build -q --no-cache -t bench . >/dev/null || exit 1
docker run --rm bench sh -c 'i=0; while [ $i -lt 20000 ]; do echo $i > /tmp/g$i; i=$((i+1)); done; sha256sum /t.tar >/dev/null' || exit 1
echo "bench-cs=$(( $(t)-s ))"
"""

SLOW = """
kill $(cat /tmp/xdg/docker.pid); for i in $(seq 100); do [ -e /tmp/xdg/docker.pid ] || break; sleep 0.2; done
while docker info >/dev/null 2>&1; do sleep 0.2; done
dockerd-rootless --storage-driver=fuse-overlayfs --data-root /tmp/fuse-store >/tmp/dockerd2.log 2>&1 &
for i in $(seq 120); do [ "$(docker info --format '{{.Driver}}' 2>/dev/null)" = fuse-overlayfs ] && break; sleep 0.5; done
[ "$(docker info --format '{{.Driver}}' 2>/dev/null)" = fuse-overlayfs ] || { echo FUSE-DOCKERD-FAILED; tail -20 /tmp/dockerd2.log; exit 1; }
docker load -q -i /opt/alpine.tar >/dev/null
"""


def test_disk_speed_is_native(lux, runners, hosts):
    """On its volume the store is overlay2 and a file-heavy build is well
    faster than on fuse-overlayfs, which is what a store on the container's
    root falls back to: a regression to that fails here. A busy machine
    squeezes both, so the best of two tries counts."""
    runners.start(hosts[0], "--nested")
    ratios = []
    for _ in range(2):
        run_id = lux.submit(docker(BENCH + SLOW + BENCH))
        run = lux.wait_state(run_id, "succeeded", "failed", timeout=600)
        out = lux.logs(run_id).split()
        assert run["state"] == "succeeded", (run.get("stateReason"), out)
        fast, slow = (int(w.split("=")[1]) for w in out if w.startswith("bench-cs="))
        print(f"overlay2 {fast / 100:.1f}s, fuse-overlayfs {slow / 100:.1f}s")
        ratios.append(slow / fast)
        # Measured 1.6-2.2x apart; the same store (a regression) is ~1x.
        if ratios[-1] > 1.3:
            return
    raise AssertionError(f"overlay2 is not clearly faster than fuse-overlayfs: {ratios}")
