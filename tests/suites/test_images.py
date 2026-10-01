"""Step 9: images built from Containerfiles, on each host, with no registry.
The first build pins every FROM to a digest and records the result on the
Run; a rebuild elsewhere uses the pinned form, so a moved tag does not change
what a Run runs on. Builds run contained, on the Run's network."""

from __future__ import annotations

from conftest import generic
import uuid

from env import ALPINE_IMAGE, wait_until


def built(containerfile: str, *cmd: str, **extra) -> dict:
    spec = generic(ALPINE_IMAGE, *cmd, **extra)
    spec["image"] = {"build": {"containerfile": containerfile}}
    return spec


def events(lux, run_id: str, typ: str) -> list[dict]:
    return [e["data"] for e in lux.events(run_id, typ)]


def test_build_pins_from_and_runs(lux, runners, hosts):
    runners.start(hosts[0])
    cf = f"FROM {ALPINE_IMAGE}\nARG WHO=nobody\nRUN echo \"built for $WHO\" > /built\n"
    spec = built(cf, "cat", "/built")
    spec["image"]["build"]["args"] = {"WHO": "lux"}
    run_id = lux.submit(spec)
    lux.wait_state(run_id, "succeeded")
    assert "built for lux" in lux.logs(run_id)
    run = lux.get(run_id)
    pinned = run["image"]["containerfile"]
    assert f"FROM {ALPINE_IMAGE}@sha256:" in pinned, pinned
    assert run["image"]["imageId"]
    text = lux.run("get", run_id).stdout
    assert "@sha256:" in text, text
    # A second Run with the same Containerfile on the same host reuses it.
    again = lux.submit(spec)
    lux.wait_state(again, "succeeded")
    assert events(lux, again, "image.built")[0]["cached"] is True


def test_a_moved_run_rebuilds_from_the_pinned_base(lux, runners, hosts, fake_image):
    """The tag the Containerfile names moves between the first build and a
    resume on another host. The rebuild uses the pinned digest, not the tag:
    the Run's image is the same."""
    a, b = hosts[0], hosts[1]
    tag = "localhost/lux-base:moving"
    for h in hosts:
        h.podman("tag", ALPINE_IMAGE, tag)
    cf = f"FROM {tag}\nRUN cat /etc/alpine-release > /release\n"
    runners.start(a)
    run_id = lux.submit(built(cf, "sh", "-c", "cat /release; sleep 300"))
    lux.wait_output(run_id, ".")
    first = lux.get(run_id)["image"]
    lux.run("stop", run_id, "--wait")
    runners.stop(a)
    # On host B the tag now names a different image (the fake agent's).
    b.podman("tag", fake_image, tag)
    runners.start(b)
    lux.run("resume", run_id)
    lux.wait_state(run_id, "running")
    run = lux.get(run_id)
    assert run["placements"][-1]["hostName"] == b.name
    built_b = wait_until(lambda: (e := events(lux, run_id, "image.built"))[1:] and e[-1], 10, 0.3, "no rebuild")
    assert built_b["imageId"] == first["imageId"], (built_b, first)
    assert not events(lux, run_id, "image.rebuild-differs")
    lux.run("cancel", run_id)


def test_a_rebuild_that_differs_is_a_warning(lux, runners, hosts):
    """A build that is not reproducible (it records something that changes)
    still runs after a move, with an image.rebuild-differs event."""
    a, b = hosts[0], hosts[1]
    cf = f"FROM {ALPINE_IMAGE}\nRUN cat /proc/sys/kernel/random/uuid > /id\n"
    runners.start(a)
    run_id = lux.submit(built(cf, "sh", "-c", "cat /id; sleep 300"))
    lux.wait_state(run_id, "running")
    lux.run("stop", run_id, "--wait")
    runners.stop(a)
    runners.start(b)
    lux.run("resume", run_id)
    lux.wait_state(run_id, "running")
    wait_until(lambda: events(lux, run_id, "image.rebuild-differs"), 10, 0.3, "no image.rebuild-differs event")
    lux.run("cancel", run_id)


def test_a_build_reaches_only_what_the_spec_allows(lux, runners, egress_hosts, net_targets):
    """A build runs tenant code on the Run's network: RUN steps have the
    Run's egress, not the host's."""
    runners.start(egress_hosts[0])
    t = net_targets
    probe = (f"(wget -q -T 3 -O /dev/null http://{t.allowed_ip}/ && echo OK:allowed || echo NO:allowed) > /probe; "
             f"(wget -q -T 3 -O /dev/null http://{t.denied_ip}/ && echo OK:denied || echo NO:denied) >> /probe")
    cf = f"FROM {ALPINE_IMAGE}\nRUN {probe}\n"
    run_id = lux.submit(built(cf, "cat", "/probe", network={"egress": [{"cidr": f"{t.allowed_ip}/32"}]}))
    lux.wait_state(run_id, "succeeded")
    out = lux.logs(run_id)
    assert "OK:allowed" in out and "NO:denied" in out, out


def test_a_failing_build_fails_the_run(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(built(f"FROM {ALPINE_IMAGE}\nRUN echo broken-step >&2; exit 3\n", "true"))
    run = lux.wait_state(run_id, "failed")
    assert "image" in run["stateReason"] and "broken-step" in run["stateReason"], run


def test_builds_are_contained(lux, runners, hosts):
    """RUN steps get their own user namespace and no extra capabilities."""
    runners.start(hosts[0])
    cf = (f"FROM {ALPINE_IMAGE}\n"
          "RUN (awk '{print $2}' /proc/self/uid_map; grep CapEff /proc/self/status) > /where\n")
    run_id = lux.submit(built(cf, "cat", "/where"))
    lux.wait_state(run_id, "succeeded")
    host_uid, cap = lux.logs(run_id).split("\n")[:2]
    assert host_uid.strip() != "0", "the build ran as host root"
    assert int(cap.split()[1], 16) & (1 << 21) == 0, f"CAP_SYS_ADMIN in the build: {cap}"


def test_rebuilds_match_whatever_uid_range_they_get(lux, runners, hosts):
    """Each build gets its own user-namespace range. The image must not
    depend on which one: files a RUN step creates as root are root's in the
    image, and a rebuild that got another range is the same image."""
    runners.start(hosts[0])
    cf = f"FROM {ALPINE_IMAGE}\nRUN adduser -D -u 1000 u && touch /made-by-root\n"
    first = lux.submit(built(cf, "sh", "-c", "stat -c '%u %n' /made-by-root /home/u"))
    lux.wait_state(first, "succeeded")
    assert "0 /made-by-root" in lux.logs(first) and "1000 /home/u" in lux.logs(first), lux.logs(first)
    first_id = lux.get(first)["image"]["imageId"]
    # Occupy a range, drop the cached image, and build again.
    hosts[0].podman("run", "-d", "--name", "range-hog", "--userns=auto:size=65536", ALPINE_IMAGE, "sleep", "300")
    tag = events(lux, first, "image.built")[0]["tag"]
    hosts[0].podman("rmi", "-f", tag)
    again = lux.submit(built(cf, "true"))
    lux.wait_state(again, "succeeded")
    hosts[0].podman("rm", "-f", "-t", "0", "range-hog")
    assert events(lux, again, "image.built")[0]["cached"] is False
    assert lux.get(again)["image"]["imageId"] == first_id


def test_builds_are_not_shared_across_tenants_or_egress(lux, tenant_factory, runners, hosts):
    """A build's result depends on what it could reach. Another tenant, or
    the same tenant with other egress, builds its own image; RUN output does
    not leak into the recorded image id."""
    runners.start(hosts[0])
    cf = f"FROM {ALPINE_IMAGE}\nRUN echo noisy-output; echo x > /x\n"
    first = lux.submit(built(cf, "true"))
    lux.wait_state(first, "succeeded")
    image_id = lux.get(first)["image"]["imageId"]
    assert len(image_id) == 64 and "noisy" not in image_id, image_id
    other_rules = lux.submit(built(cf, "true", network={"egress": [{"cidr": "10.9.9.9/32"}]}))
    lux.wait_state(other_rules, "succeeded")
    assert events(lux, other_rules, "image.built")[0]["cached"] is False

    other = tenant_factory()
    other_runners = runners.__class__(runners.env, other)
    other_runners.start(hosts[1])
    theirs = other.submit(built(cf, "true"))
    other.wait_state(theirs, "succeeded")
    assert [e["data"] for e in other.events(theirs, "image.built")][0]["cached"] is False
    other_runners.stop_all()


def test_copy_from_an_image_is_refused(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(built(f"FROM {ALPINE_IMAGE}\nCOPY --from={ALPINE_IMAGE} /etc/os-release /x\n", "true"))
    run = lux.wait_state(run_id, "failed")
    assert "earlier stages" in run["stateReason"], run
    ok = lux.submit(built(f"FROM {ALPINE_IMAGE} AS base\nFROM {ALPINE_IMAGE}\nCOPY --from=base /etc/os-release /x\n",
                          "cat", "/x"))
    lux.wait_state(ok, "succeeded")


def test_cancel_ends_a_build(lux, runners, hosts):
    """A cancel during a long build ends it, and leaves no egress behind."""
    host = runners.start(hosts[0])
    marker = f"6{uuid.uuid4().int % 10**6:06d}"  # a sleep no other process runs
    run_id = lux.submit(built(f"FROM {ALPINE_IMAGE}\nRUN sleep {marker}\n", "true"))
    wait_until(lambda: host.running("sleep", marker), 30, 0.3, "the build step never ran")
    lux.run("cancel", run_id)
    lux.wait_state(run_id, "cancelled", timeout=30)
    wait_until(lambda: not host.running("sleep", marker), 20, 0.5, "the build is still running")
    assert "chain run_" not in host.exec("nft", "list", "table", "inet", "lux")


def test_a_build_has_the_runs_process_limit(lux, runners, hosts):
    runners.start(hosts[0])
    cf = f"FROM {ALPINE_IMAGE}\nRUN for i in $(seq 200); do sleep 3 & done; wait\n"
    spec = built(cf, "true")
    spec["resources"] = {"pids": 64}
    run = lux.wait_state(lux.submit(spec), "failed")
    assert "fork" in run["stateReason"], run


def test_a_build_has_the_runs_scaled_memory(lux, runners, hosts):
    """RUN steps get the Run's memory as its container does: scaled to the
    host's share (the harness's runners offer more than MemTotal). A step
    shares the host's cgroup namespace, so its own cgroup is the path
    /proc/self/cgroup names below /sys/fs/cgroup, not /sys/fs/cgroup."""
    runners.start(hosts[0])
    cf = (f"FROM {ALPINE_IMAGE}\n"
          "RUN cat \"/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.max\" > /build-memory\n")
    run_id = lux.submit(built(cf, "cat", "/build-memory", resources={"memory": "512Mi"}))
    lux.wait_state(run_id, "succeeded")
    limit = hosts[0].memory_limit(512 << 20)
    assert limit < 512 << 20, "the harness host does not scale memory; the test proves nothing"
    assert int(lux.logs(run_id).strip()) == limit, lux.logs(run_id)
