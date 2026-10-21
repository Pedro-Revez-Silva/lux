"""Managed images: image.build.publish pushes a Run's build to luxd's
managed registry repository for its tenant, with luxd's own credential,
and records the exact digest on the Run (publishedRef, platform); later
Runs use that reference as image.ref with no registry credentials. Builds
get a context: files given inline (contextFiles) and git trees at a
commit (contexts). The registry here is the harness's private one."""

from __future__ import annotations

import json
import uuid
from pathlib import Path
from types import SimpleNamespace

import pytest

from conftest import CLIError, generic
from env import ALPINE_IMAGE, wait_until


def events(lux, run_id: str, typ: str) -> list[dict]:
    return [e["data"] for e in lux.events(run_id, typ)]


def exists(host, ref: str) -> bool:
    return "yes" in host.exec("sh", "-c", f"podman image exists {ref} && echo yes || echo no")


def assert_nowhere(lux, env, run_id: str, *secrets: str):
    """No secret is in what lux shows or logs about the Run."""
    seen = {
        "get": json.dumps(lux.get(run_id)),
        "events": json.dumps(lux.json("events", run_id)),
        "logs": lux.run("logs", run_id, check=False).stdout,
        "luxd.log": (Path(env.log_dir) / "luxd.log").read_text(errors="replace"),
    }
    for h in env.hosts:
        log = Path(h.log_dir) / "runner.log"
        if log.exists():
            seen[f"{h.name} runner.log"] = log.read_text(errors="replace")
    for where, text in seen.items():
        for secret in secrets:
            assert secret not in text, f"a credential is in {where}"


@pytest.fixture
def managed(env, registry, require):
    """luxd with a managed image registry: <registry>/managed, its
    credential in a file luxd reads for each assignment."""
    require("luxd", "lux-runner", "lux-shim", "lux")
    cred = Path(env.log_dir) / f"managed-{uuid.uuid4().hex[:8]}.cred"
    cred.write_text(registry.creds + "\n")
    cred.chmod(0o600)
    root = f"{registry.address}/managed"
    overrides = {"LUX_IMAGES_REPOSITORY": root, "LUX_IMAGES_CREDENTIAL_FILE": str(cred),
                 "LUX_IMAGES_SHARED": f"{registry.address}/shared/base"}
    env.stop_luxd()
    env.start_luxd(**overrides)
    yield SimpleNamespace(root=root, registry=registry, cred=cred, overrides=overrides)
    env.stop_luxd()
    env.start_luxd()


def alpine_digest(host) -> str:
    return host.podman("image", "inspect", "--format", "{{.Digest}}", ALPINE_IMAGE).strip()


def published(lux, managed, containerfile: str, *cmd: str, **build) -> dict:
    spec = generic(ALPINE_IMAGE, *cmd)
    spec["image"] = {"build": {"containerfile": containerfile, "publish": True, **build}}
    return spec


def test_publish_then_a_clean_host_runs_the_exact_digest(env, lux, runners, hosts, managed, git_server):
    """Host A builds with a context (inline files and a git tree at a
    commit), publishes, and records the digest; after a luxd restart, a
    clean host B runs that digest as image.ref with no credentials of the
    Run's, without building. No credential is anywhere the tenant sees."""
    a, b = hosts[0], hosts[1]
    tag = uuid.uuid4().hex[:8]
    marker = f"inline-{tag}"
    repo = f"ctx-{tag}"
    sha = git_server.create(repo, {"go.mod": f"module {tag}\n", "data.txt": f"from-git-{tag}\n"})
    cf = (f"FROM {ALPINE_IMAGE}@{alpine_digest(a)}\n"
          "COPY setup.txt /opt/setup.txt\n"
          "COPY --from=repo go.mod data.txt /seed/\n")
    spec = published(lux, managed, cf, "sh", "-c", "cat /opt/setup.txt /seed/go.mod /seed/data.txt; env",
                     contextFiles={"setup.txt": marker + "\n"},
                     contexts={"repo": {"git": {"url": git_server.url(repo), "ref": sha, "credential": "GIT"}}})
    spec["secrets"] = [{"name": "GIT", "value": git_server.token}]
    spec["network"] = {"egress": [{"cidr": f"{git_server.ip}/32"}]}

    runners.start(a)
    first = lux.submit(spec)
    lux.wait_state(first, "succeeded", timeout=180)
    out = lux.logs(first)
    assert marker in out and f"module {tag}" in out and f"from-git-{tag}" in out, out
    image = lux.get(first)["image"]
    repo_ref = f"{managed.root}/{lux.tenant_id.lower()}"
    assert image["publishedRef"].startswith(repo_ref + "@sha256:"), image
    arch = a.exec("uname", "-m").strip()
    assert image["platform"] == "linux/" + {"x86_64": "amd64", "aarch64": "arm64"}[arch], image
    assert [e["commit"] for e in events(lux, first, "image.context")] == [sha]
    pub = events(lux, first, "image.published")
    assert pub and pub[0]["ref"] == image["publishedRef"], pub
    key = events(lux, first, "image.built")[0]["tag"].split(":")[-1]
    assert key in managed.registry.tags(f"managed/{lux.tenant_id.lower()}")
    # Nothing of the credential in the image's history either.
    history = a.podman("history", "--no-trunc", "--format", "{{.CreatedBy}}", image["publishedRef"])
    assert managed.registry.password not in history and git_server.token not in history
    assert "LUX_IMAGES" not in out and managed.registry.password not in out
    runners.stop(a)

    # The credential is luxd's configuration, not anything stored.
    env.stop_luxd()
    env.start_luxd(**managed.overrides)
    runners.start(b)
    assert not exists(b, image["publishedRef"])
    second = lux.submit(generic(image["publishedRef"], "sh", "-c", "cat /opt/setup.txt /seed/data.txt"))
    lux.wait_state(second, "succeeded")
    assert marker in lux.logs(second)
    assert [e["ref"] for e in events(lux, second, "image.pull")] == [image["publishedRef"]]
    assert not events(lux, second, "image.build")
    got = b.podman("image", "inspect", "--format", "{{.Id}}", image["publishedRef"]).strip()
    assert got.removeprefix("sha256:") == image["imageId"].removeprefix("sha256:"), (got, image)
    assert "managedImages" not in json.dumps(lux.get(second))
    for run_id in (first, second):
        assert_nowhere(lux, env, run_id, managed.registry.password, git_server.token)


def test_a_private_shared_base_needs_no_credentials_of_the_runs(env, lux, runners, hosts, managed):
    """FROM a private base in a shared repository ([images] shared), then
    published: luxd's one credential pulls the base and pushes the build;
    the Run has no registryAuth. Another private repository on the same
    registry is not offered that credential."""
    host = runners.start(hosts[0])
    base = managed.registry.push(host, ALPINE_IMAGE, f"shared/base:{uuid.uuid4().hex[:8]}")
    other = managed.registry.push(host, ALPINE_IMAGE, f"private/other:{uuid.uuid4().hex[:8]}")
    run_id = lux.submit(published(lux, managed, f"FROM {base}\nRUN echo on-shared > /m\n", "cat", "/m"))
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert "on-shared" in lux.logs(run_id)
    image = lux.get(run_id)["image"]
    assert f"FROM {base}@sha256:" in image["containerfile"] and image["publishedRef"], image
    assert [e["ref"] for e in events(lux, run_id, "image.pull")][0] == base
    denied = lux.submit(published(lux, managed, f"FROM {other}\nRUN true\n", "true"))
    run = lux.wait_state(denied, "failed", timeout=120)
    assert any(w in run["stateReason"].lower() for w in ("unauthorized", "authentication required")), run
    assert_nowhere(lux, env, run_id, managed.registry.password)
    assert_nowhere(lux, env, denied, managed.registry.password)


def test_a_moved_run_runs_its_published_image(lux, runners, hosts, managed):
    """A build that is not reproducible: after a move, the Run runs the
    image it published, pulled by digest, rather than a rebuild."""
    a, b = hosts[0], hosts[1]
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(a)}\nRUN cat /proc/sys/kernel/random/uuid > /id\n"
    runners.start(a)
    run_id = lux.submit(published(lux, managed, cf, "sh", "-c", "echo id=$(cat /id); sleep 300"))
    first = lux.wait_output(run_id, "id=")
    ref = lux.get(run_id)["image"]["publishedRef"]
    lux.run("stop", run_id, "--wait")
    runners.stop(a)
    runners.start(b)
    lux.run("resume", run_id)
    lux.wait_state(run_id, "running")
    assert lux.get(run_id)["placements"][-1]["hostName"] == b.name
    built = wait_until(lambda: (e := events(lux, run_id, "image.built"))[1:] and e[-1], 30, 0.3, "no image.built on B")
    assert built.get("fromPublished") is True and built["publishedRef"] == ref, built
    assert len(events(lux, run_id, "image.build")) == 1, "host B rebuilt"
    assert not events(lux, run_id, "image.rebuild-differs")
    ids = wait_until(lambda: (i := [l for l in lux.logs(run_id).splitlines() if l.startswith("id=")])[1:] and i,
                     30, 0.5, "no output on B")
    assert len(set(ids)) == 1 and first.strip().splitlines()[0] in ids, ids
    assert lux.get(run_id)["image"]["publishedRef"] == ref
    lux.run("terminate", run_id)


def test_an_identical_build_is_reused_from_the_managed_registry(lux, runners, hosts, managed):
    """Another Run of the same build on another host pulls it from the
    tenant's managed repository rather than building it; a changed
    context file is another build."""
    a, b = hosts[0], hosts[1]
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(a)}\nCOPY f /f\n"
    spec = published(lux, managed, cf, "cat", "/f", contextFiles={"f": f"one-{uuid.uuid4().hex}\n"})
    runners.start(a)
    first = lux.submit(spec)
    lux.wait_state(first, "succeeded")
    runners.stop(a)
    runners.start(b)
    second = lux.submit(spec)
    lux.wait_state(second, "succeeded")
    hits = [e for e in events(lux, second, "image.cache") if e.get("hit")]
    assert hits and hits[0].get("managed") is True, events(lux, second, "image.cache")
    assert not events(lux, second, "image.build")
    assert lux.get(second)["image"]["publishedRef"] == lux.get(first)["image"]["publishedRef"]
    spec["image"]["build"]["contextFiles"] = {"f": "two\n"}
    third = lux.submit(spec)
    lux.wait_state(third, "succeeded")
    assert lux.logs(third).strip() == "two"
    assert events(lux, third, "image.build"), "a changed context file reused the old build"
    assert lux.get(third)["image"]["publishedRef"] != lux.get(first)["image"]["publishedRef"]


def test_managed_images_are_the_tenants_own(lux, tenant_factory, runners, hosts, managed):
    """Tenant B can't name tenant A's managed images, as its image or as a
    base, or use the managed registry as a build cache: refused at submit."""
    a, b = lux, tenant_factory()
    runners.start(hosts[0])
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(hosts[0])}\nRUN echo a > /a\n"
    run_a = a.submit(published(a, managed, cf, "cat", "/a"))
    a.wait_state(run_a, "succeeded")
    ref = a.get(run_a)["image"]["publishedRef"]
    for spec, want in [
        (generic(ref, "true"), "another tenant's managed repository"),
        ({**generic(ALPINE_IMAGE, "true"), "image": {"build": {"containerfile": f"FROM {ref}\n"}}}, "another tenant's managed repository"),
        ({**generic(ALPINE_IMAGE, "true"), "image": {"build": {"containerfile": "FROM alpine\n", "cache": f"{managed.root}/{b.tenant_id.lower()}"}}},
         "not a build cache"),
    ]:
        with pytest.raises(CLIError) as e:
            b.submit(spec)
        assert want in e.value.stderr + e.value.stdout, e.value
    # A's own later Run may.
    again = a.submit(generic(ref, "cat", "/a"))
    a.wait_state(again, "succeeded")


def test_bad_contexts_are_refused(lux, managed):
    """Paths that would escape the context and refs that move are refused
    at submit, every problem at once."""
    spec = published(lux, managed, "FROM alpine\n", "true",
                     contextFiles={"../escape": "x", "/etc/passwd": "x", "ok.txt": "x"},
                     contexts={"repo": {"git": {"url": "file:///etc", "ref": "main"}},
                               "other": {"git": {"url": "https://github.com/a/b", "ref": "0" * 40}}})
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    msg = e.value.stderr + e.value.stdout
    for want in ['"../escape"', '"/etc/passwd"', "contexts.repo.git.url", "contexts.repo.git.ref",
                 "network.egress does not allow github.com"]:
        assert want in msg, (want, msg)


def test_publish_needs_a_managed_registry(lux, require):
    require("luxd", "lux")
    spec = generic(ALPINE_IMAGE, "true")
    spec["image"] = {"build": {"containerfile": "FROM alpine\n", "publish": True}}
    with pytest.raises(CLIError) as e:
        lux.submit(spec)
    assert "image.build.publish: this luxd has no managed image registry" in e.value.stderr + e.value.stdout


def test_an_older_runner_is_not_given_a_publishing_run(lux, runners, hosts, managed):
    """A runner that predates managed images would build without the
    context and not publish: the Run waits for one that has them."""
    runners.start(hosts[0], environ={"LUX_TEST_WITHOUT_CAPABILITIES": "managed-images-v1"})
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(hosts[0])}\nCOPY f /f\n"
    run_id = lux.submit(published(lux, managed, cf, "cat", "/f", contextFiles={"f": "ok\n"}))
    run = wait_until(lambda: (r := lux.get(run_id)).get("stateReason") and r, 30, 0.5, "no wait reason")
    assert run["state"] == "submitted", run
    assert run["stateReason"] == ("waiting for capacity: 1 host in its pool runs a lux-runner without managed images"
                                  " (its image is published, built with a context, or in the managed registry)"), run
    runners.start(hosts[1])
    lux.wait_state(run_id, "succeeded")
    assert lux.get(run_id)["placements"][-1]["hostName"] == hosts[1].name


def test_an_older_runner_is_not_given_a_managed_image(lux, runners, hosts, managed):
    """A Run of a published image (image.ref, no registryAuth) needs the
    credential luxd supplies: a runner that predates managed images would
    pull it without and fail, so the Run waits for one that has them, and
    then runs on it, pulling the digest."""
    a, b = hosts[0], hosts[1]
    runners.start(a)
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(a)}\nRUN echo {uuid.uuid4().hex} > /m\n"
    first = lux.submit(published(lux, managed, cf, "cat", "/m"))
    lux.wait_state(first, "succeeded", timeout=120)
    ref = lux.get(first)["image"]["publishedRef"]
    runners.stop(a)
    runners.start(b, environ={"LUX_TEST_WITHOUT_CAPABILITIES": "managed-images-v1"})
    run_id = lux.submit(generic(ref, "cat", "/m"))
    run = wait_until(lambda: (r := lux.get(run_id)).get("stateReason") and r, 30, 0.5, "no wait reason")
    assert run["state"] == "submitted" and "without managed images" in run["stateReason"], run
    assert not lux.events(run_id, "image.pull")
    runners.stop(b)
    runners.start(b)
    lux.wait_state(run_id, "succeeded", timeout=120)
    assert lux.get(run_id)["placements"][-1]["hostName"] == b.name
    assert [e["ref"] for e in events(lux, run_id, "image.pull")] == [ref]
    assert lux.logs(run_id) == lux.logs(first)
    # A Run with its own registry credentials needs nothing new.
    runners.stop(b)
    runners.start(b, environ={"LUX_TEST_WITHOUT_CAPABILITIES": "managed-images-v1"})
    other = managed.registry.push(b, ALPINE_IMAGE, f"private/legacy:{uuid.uuid4().hex[:8]}")
    legacy = generic(other, "echo", "legacy")
    legacy["secrets"] = [{"name": "REG", "value": managed.registry.creds}]
    legacy["image"]["registryAuth"] = [{"registry": managed.registry.address, "secret": "REG"}]
    legacy_id = lux.submit(legacy)
    lux.wait_state(legacy_id, "succeeded", timeout=120)


def test_a_failed_publish_fails_the_run(env, lux, runners, hosts, managed):
    """A push the registry refuses fails the placement (no Run goes on
    without its published image) and says why without the credential; a
    corrected credential file is read for the next assignment, with no
    restart."""
    runners.start(hosts[0])
    wrong = "wrongpw-" + uuid.uuid4().hex
    managed.cred.write_text(f"{managed.registry.user}:{wrong}")
    cf = f"FROM {ALPINE_IMAGE}@{alpine_digest(hosts[0])}\nRUN echo {uuid.uuid4().hex} > /m\n"
    run_id = lux.submit(published(lux, managed, cf, "cat", "/m"))
    run = lux.wait_state(run_id, "failed", timeout=120)
    reason = run["stateReason"].lower()
    assert "image" in reason and "publishing" in reason and ("unauthorized" in reason or "authentication" in reason), run
    assert "publishedRef" not in (lux.get(run_id).get("image") or {})
    assert_nowhere(lux, env, run_id, wrong)
    managed.cred.write_text(managed.registry.creds)
    ok = lux.submit(published(lux, managed, cf, "cat", "/m"))
    lux.wait_state(ok, "succeeded", timeout=120)
    assert lux.get(ok)["image"]["publishedRef"]


def test_terminate_ends_a_context_fetch(lux, runners, hosts, managed, git_server):
    """A terminate while a build context is being fetched ends the
    placement; nothing of the build is left in the runner's tmp."""
    host = runners.start(hosts[0])
    repo = f"slow-{uuid.uuid4().hex[:8]}"
    sha = git_server.create(repo, {"a": "b"})
    git_server.slow(repo, 60)
    spec = published(lux, managed, "FROM scratch\nCOPY --from=repo a /a\n", "true",
                     contexts={"repo": {"git": {"url": git_server.url(repo), "ref": sha}}})
    spec["network"] = {"egress": [{"cidr": f"{git_server.ip}/32"}]}
    before = len(git_server.requests())
    run_id = lux.submit(spec)
    wait_until(lambda: len(git_server.requests()) > before, 60, 0.3, "the context fetch never started")
    lux.run("terminate", run_id)
    lux.wait_state(run_id, "terminated", timeout=45)
    wait_until(lambda: not host.exec("sh", "-c", "ls /var/lib/lux/tmp 2>/dev/null; true").split(), 30, 0.5,
               "build files left in the runner's tmp")
    assert not events(lux, run_id, "image.published")
