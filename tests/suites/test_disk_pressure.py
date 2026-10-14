"""The low-free-space guard of a runner that scales disk, on a small real
filesystem: the runner's data directory and Podman's volumes on one tmpfs,
filled past its low mark by a Run over its disk limit (within its grace)
beside a Run under its own."""

from __future__ import annotations

import pytest

from conftest import cleanup_host, generic
from env import ALPINE_IMAGE, wait_until

FS_MIB = 512
VOLUMES = "/var/lib/containers/storage/volumes"


def _mount_small_fs(host) -> None:
    """One tmpfs of FS_MIB holding both /var/lib/lux and Podman's volumes,
    so what a Run writes to a state volume is what statfs on the data
    directory sees. Skips when the host cannot mount."""
    script = (f"set -e; mkdir -p /mnt/luxfs /var/lib/lux {VOLUMES}; "
              f"mount -t tmpfs -o size={FS_MIB}m lux-pressure /mnt/luxfs; "
              "mkdir -p /mnt/luxfs/lux /mnt/luxfs/volumes; "
              "mount --bind /mnt/luxfs/lux /var/lib/lux; "
              f"mount --bind /mnt/luxfs/volumes {VOLUMES}")
    try:
        host.exec("sh", "-c", script)
    except RuntimeError as e:
        _unmount_small_fs(host)
        pytest.skip(f"the simulated host cannot mount a tmpfs for the data directory: {e}")


def _unmount_small_fs(host) -> None:
    host.exec("sh", "-c", f"umount -l {VOLUMES} 2>/dev/null; umount -l /var/lib/lux 2>/dev/null; "
              "umount -l /mnt/luxfs 2>/dev/null; true", check=False)


def _pressure(lux, host) -> dict | None:
    return lux.json("hosts", "get", host.name).get("diskPressure")


def test_disk_pressure_stops_the_run_over_its_limit_and_blocks_admissions(lux, runners, hosts):
    """Below the low mark the Run over its disk limit is stopped with the
    reason, while the Run under its own runs on. The guard then discards
    the stopped Run's local copy once its snapshot is uploaded (the default
    host TTL, 24h, plays no part): free space is back over the recovery
    mark and the pressure ends. Then space the guard cannot reclaim (an
    operator's file) holds the host under pressure with no relief: a new
    Run waits, saying why, until the file goes. The host's events record
    each pressure's start and end."""
    host = hosts[0]
    _mount_small_fs(host)
    try:
        # Capacity as the runner reads it: used plus unprivileged-free.
        blocks, free, avail, bsize = map(int, host.exec("stat", "-f", "-c", "%b %f %a %s", "/var/lib/lux").split())
        capacity = (blocks - free + avail) * bsize
        assert capacity == FS_MIB << 20, capacity
        # Nominal disk the whole filesystem, no headroom: limits as asked.
        # Low 40% (~205 MiB free), recover 50% (256 MiB). The host TTL is
        # the default: only the guard can free the stopped Run's copy.
        runners.start(host, "--usage-every", "1s", "--disk", str(capacity), "--disk-headroom", "0",
                      "--disk-low-free", "40%", "--disk-recover-free", "50%")
        assert _pressure(lux, host) is None

        vols = [{"name": "w", "path": "/w", "kind": "state"}]
        under = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "dd if=/dev/zero of=/w/f bs=1M count=32 2>/dev/null; echo wrote; sleep 600",
                                   resources={"disk": "64Mi"}, volumes=vols))
        lux.wait_output(under, "wrote")
        # 300 MiB against a 64 MiB limit, well inside a 30m grace: only the
        # guard stops it.
        over = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "dd if=/dev/zero of=/w/f bs=1M count=300 2>/dev/null; echo wrote; sleep 600",
                                  resources={"disk": "64Mi", "diskGrace": "30m"}, volumes=vols))

        run = lux.wait_state(over, "failed", timeout=120)
        assert run["stateReason"].startswith("stopped: host disk pressure, ") and run["stateReason"].endswith(" over its 64.0 MiB disk limit"), run
        assert run["placements"][-1]["stopReason"] == "disk-pressure", run["placements"]
        stop = lux.events(over, "disk.pressure_stop")
        assert len(stop) == 1 and stop[0]["data"]["limitBytes"] == 64 << 20 and stop[0]["data"]["excessBytes"] > 0, stop
        assert not lux.events(over, "disk.exceeded"), "stopped by the grace or a ceiling, not the guard"

        # The guard discards the stopped Run's local copy once S3 holds it
        # (no host TTL: the default is 24h); free space comes back and the
        # pressure ends.
        wait_until(lambda: _pressure(lux, host) is None, 90, 1, "disk pressure did not end")
        assert not any(over in v for v in host.podman("volume", "ls", "--format", "{{.Name}}").split()), \
            "the stopped Run's volume is still on the host"
        wait_until(lambda: (s := lux.json("snapshots", over)) and not s[0].get("onHost"), 30, 1,
                   "luxd still lists the stopped Run's local copy, or no snapshot of it")
        types = [e["type"] for e in lux.json("hosts", "events", host.name)]
        assert "host.disk_pressure" in types and "host.disk_pressure_ended" in types, types

        # 300 MiB of an operator's own: nothing the guard may reclaim, and
        # the Run left is under its limit. Pressure holds, unrelieved.
        host.exec("dd", "if=/dev/zero", "of=/var/lib/lux/ballast", "bs=1M", "count=300")
        wait_until(lambda: (_pressure(lux, host) or {}).get("noRelief"), 60, 1, "no unrelieved disk pressure")
        pressure = _pressure(lux, host)
        assert pressure and pressure["lowFree"] == "40%" and pressure["freeBytes"] < pressure["lowFreeBytes"], pressure
        waiting = lux.submit(generic(ALPINE_IMAGE, "true", resources={"disk": "8Mi"}))
        wait_until(lambda: "under disk pressure" in lux.get(waiting).get("stateReason", ""), 30, 0.5, "the new Run is not held for disk pressure")
        assert lux.get(waiting)["state"] not in ("scheduled", "running", "succeeded"), lux.get(waiting)
        assert lux.get(under)["state"] == "running", "the Run under its limit was stopped"

        host.exec("rm", "-f", "/var/lib/lux/ballast")
        wait_until(lambda: _pressure(lux, host) is None, 60, 1, "disk pressure did not end once the file went")
        lux.wait_state(waiting, "succeeded", timeout=60)
        assert lux.get(under)["state"] == "running", "the Run under its limit was stopped"
        assert not lux.events(under, "disk.pressure_stop")
        ended = [e["type"] for e in lux.json("hosts", "events", host.name)].count("host.disk_pressure_ended")
        assert ended == 2, ended
        lux.run("terminate", under, "--wait")
    finally:
        host.exec("rm", "-f", "/var/lib/lux/ballast", check=False)
        runners.stop(host)
        cleanup_host(host)
        _unmount_small_fs(host)
