"""Step 6: git workspaces. Repositories are cloned into a Run's workspace
volume from host-local mirrors, at the ref asked for; pushes go to the
spec's branch with credentials only the runner holds, never overwriting a
branch that moved."""

from __future__ import annotations

import pytest

from env import wait_until
from conftest import CLIError, fake_agent


def repo_spec(image: str, git_server, prompt: str, *, repo: str = "app", ref: str = "main",
              push: str | None = "lux/work", credential: bool = True, **extra) -> dict:
    spec = fake_agent(image, prompt, **extra)
    spec["git"] = {"repositories": [{"name": repo, "url": git_server.url(repo), "ref": ref}]}
    spec["workload"]["workdir"] = f"/workspace/repos/{repo}"
    if credential:
        spec["git"]["repositories"][0]["credential"] = "GIT_TOKEN"
        spec["secrets"] = [{"name": "GIT_TOKEN", "value": git_server.token}]
    if push:
        spec["git"]["push"] = {"branch": push}
    return spec


def test_clone_at_a_branch(lux, runners, hosts, fake_image, git_server):
    git_server.create("app", {"README.md": "hello from main\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "read README.md"))
    lux.wait_output(run_id, "hello from main")
    ev = [e for e in lux.json("events", run_id) if e["type"] == "git.checkout"][0]["data"]
    assert ev["branch"] == "main" and len(ev["base"]) == 40


def test_clone_at_a_sha_or_tag(lux, runners, hosts, fake_image, git_server):
    first = git_server.create("pinned", {"v.txt": "one\n"})
    git_server.commit_on("pinned", "main", "v.txt", "two\n")
    git_server.sh(f"git -C /repos/pinned.git tag v1 {first}")
    runners.start(hosts[0])
    for ref in (first, "v1"):
        run_id = lux.submit(repo_spec(fake_image, git_server, "read v.txt", repo="pinned", ref=ref))
        out = lux.wait_output(run_id, "one")
        assert "two" not in out
        lux.run("terminate", run_id)


def test_credentials_never_enter_the_container(lux, runners, hosts, fake_image, git_server):
    """The token is used by the runner for fetch and push; the container has
    no copy of it: not in its environment, not in the checkout's config."""
    git_server.create("secret-repo", {"a.txt": "x\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server,
                                  "print-secret GIT_TOKEN\nread .git/config", repo="secret-repo"))
    out = lux.wait_output(run_id, "[remote")
    assert git_server.token not in out
    assert "REDACTED" not in out.split("[remote")[0], "the token was in the environment (redacted)"
    assert f"url = {git_server.url('secret-repo')}" in out
    # The clone went through the forge's auth: the runner held the token.
    assert any("auth" in r and "secret-repo" in r for r in git_server.requests())


def test_a_private_repo_needs_its_credential(lux, runners, hosts, fake_image, git_server):
    git_server.create("private", {"a.txt": "x\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "echo never", repo="private", credential=False))
    run = lux.wait_state(run_id, "failed")
    assert "git" in run["stateReason"] and git_server.token not in run["stateReason"], run


def test_push_with_the_runners_credentials(lux, runners, hosts, fake_image, git_server):
    git_server.create("pushme", {"a.txt": "base\n"})
    runners.start(hosts[0])
    script = ("write a.txt changed\n"
              "echo committing")
    run_id = lux.submit(repo_spec(fake_image, git_server, script, repo="pushme"))
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "pushme", "change a")
    out = lux.run("push", run_id, "--wait").stdout
    assert "pushed" in out, out
    assert git_server.show("pushme", "lux/work", "a.txt") == "changed\n"
    # Pushing again with nothing new is a no-op.
    assert "up-to-date" in lux.run("push", run_id, "--wait").stdout


def test_push_never_overwrites_a_branch_that_moved(lux, runners, hosts, fake_image, git_server):
    git_server.create("lease", {"a.txt": "base\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "write a.txt mine", repo="lease"))
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "lease", "mine")
    lux.run("push", run_id, "--wait")
    # Someone else pushes to the branch.
    theirs = git_server.commit_on("lease", "lux/work", "a.txt", "theirs\n")
    lux.run("steer", run_id, "write a.txt mine-again")
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "lease", "mine again")
    p = lux.run("push", run_id, "--wait", check=False)
    assert p.returncode != 0 and "rejected" in p.stdout, p.stdout
    assert git_server.rev("lease", "lux/work") == theirs


def test_work_survives_a_move_and_pushes_from_the_new_host(lux, runners, hosts, fake_image, git_server):
    """The checkout is on the workspace volume: it moves with the Run, and
    the new host (which has no mirror) pushes it."""
    git_server.create("moving", {"a.txt": "base\n"})
    a, b = hosts[0], hosts[1]
    runners.start(a)
    run_id = lux.submit(repo_spec(fake_image, git_server, "write a.txt from-a", repo="moving"))
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, a, "moving", "from a")
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    lux.run("resume", run_id, "--wait", "--secret", f"GIT_TOKEN={git_server.token}", "--input", "read a.txt")
    lux.wait_output(run_id, "from-a")
    out = lux.run("push", run_id, "--wait").stdout
    assert "pushed" in out, out
    assert git_server.show("moving", "lux/work", "a.txt") == "from-a\n"


def test_mirror_is_reused(lux, runners, hosts, fake_image, git_server):
    git_server.create("mirrored", {"a.txt": "x\n"})
    runners.start(hosts[0])
    for _ in range(2):
        run_id = lux.submit(repo_spec(fake_image, git_server, "read a.txt", repo="mirrored", push=None))
        lux.wait_output(run_id, "x")
        lux.run("terminate", run_id)
    clones = [r for r in git_server.requests() if "mirrored" in r and "git-upload-pack" in r and "POST" in r]
    # First Run clones; the second only fetches (both upload-pack, but the
    # host holds one mirror).
    mirrors = hosts[0].exec("sh", "-c", "cat /var/lib/lux/mirrors/*/lux-url").split()
    assert mirrors.count(git_server.url("mirrored")) == 1
    assert clones


def test_push_without_a_push_branch_is_refused(lux, runners, hosts, fake_image, git_server):
    git_server.create("nopush", {"a.txt": "x\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "echo ok", repo="nopush", push=None))
    lux.wait_activity(run_id, "idle")
    with pytest.raises(CLIError) as e:
        lux.run("push", run_id)
    assert "git.push" in e.value.stderr


def commit(lux, run_id: str, host, repo: str, message: str):
    """Have the Run's agent (lux-fake) commit in its checkout; returns the sha."""
    before = lux.logs(run_id).count("committed ")
    lux.run("steer", run_id, f"commit {message}")
    out = wait_until(lambda: (o := lux.logs(run_id)).count("committed ") > before and o, 30, 0.3, "no commit")
    lux.wait_activity(run_id, "idle")
    return out.rsplit("committed ", 1)[1].split()[0]


def test_a_hostile_checkout_cannot_hijack_the_push(lux, runners, hosts, fake_image, git_server):
    """The workload controls its .git. A pre-push hook and a url rewrite in
    its config must not run as the runner nor redirect the push (and the
    token with it): the runner never runs git in the checkout."""
    git_server.create("hostile", {"a.txt": "base\n"})
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "write a.txt mine", repo="hostile"))
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "hostile", "mine")
    in_container(hosts[0], run_id, "hostile",
                 "printf '#!/bin/sh\\ntouch /tmp/HOOK-RAN\\n' > .git/hooks/pre-push && chmod +x .git/hooks/pre-push && "
                 f"git config url.http://attacker.invalid/.insteadOf {git_server.url('hostile')}")
    out = lux.run("push", run_id, "--wait").stdout
    assert "pushed" in out, out
    assert git_server.show("hostile", "lux/work", "a.txt") == "mine\n"
    assert hosts[0].exec("sh", "-c", "ls /tmp/HOOK-RAN 2>/dev/null; true").strip() == "", "the hook ran on the host"


def in_container(host, run_id: str, repo: str, script: str):
    host.exec("sh", "-c", f"podman exec --user agent -w /workspace/repos/{repo} lux-{run_id} sh -c \"{script}\"")


def test_push_onto_an_existing_branch_at_an_expected_commit(lux, runners, hosts, fake_image, git_server):
    """--expect pushes onto a branch that already exists (a work item's
    branch another system owns) only if it is at the given commit: a
    compare-and-swap, for the first push and later ones alike."""
    git_server.create("cas", {"a.txt": "base\n"})
    start = git_server.commit_on("cas", "lux/work", "a.txt", "theirs\n")
    runners.start(hosts[0])
    run_id = lux.submit(repo_spec(fake_image, git_server, "write b.txt mine", repo="cas"))
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "cas", "mine")
    # Without --expect the first push refuses an existing branch.
    p = lux.run("push", run_id, "--wait", check=False)
    assert p.returncode != 0 and "rejected" in p.stdout, p.stdout
    # Wrong expectation: refused, the branch untouched.
    p = lux.run("push", run_id, "--wait", "--expect", "cas=" + "0" * 40, check=False)
    assert p.returncode != 0 and "rejected" in p.stdout, p.stdout
    assert git_server.rev("cas", "lux/work") == start
    # Right expectation: pushed.
    out = lux.run("push", run_id, "--wait", "--expect", f"cas={start}").stdout
    assert "pushed" in out, out
    assert git_server.show("cas", "lux/work", "b.txt") == "mine\n"
    # An unknown repository or a partial id is refused up front.
    for bad in ("nope=" + "0" * 40, "cas=abc123"):
        with pytest.raises(CLIError) as e:
            lux.run("push", run_id, "--expect", bad)
        assert e.value.code != 0


def test_a_repository_marked_push_false_is_never_pushed(lux, runners, hosts, fake_image, git_server):
    """Repositories cloned for context stay untouched: the push skips them
    (reported as skipped), and an expected commit may not name them."""
    git_server.create("work", {"a.txt": "base\n"})
    git_server.create("context", {"b.txt": "base\n"})
    runners.start(hosts[0])
    spec = repo_spec(fake_image, git_server, "write a.txt changed", repo="work")
    spec["git"]["repositories"].append({"name": "context", "url": git_server.url("context"), "ref": "main",
                                        "credential": "GIT_TOKEN", "push": False})
    run_id = lux.submit(spec)
    lux.wait_activity(run_id, "idle")
    commit(lux, run_id, hosts[0], "work", "change a")
    out = lux.json("push", run_id, "--wait")
    by_repo = {r["repo"]: r for r in out}
    assert by_repo["work"]["status"] == "pushed", out
    assert by_repo["context"]["status"] == "skipped" and not by_repo["context"].get("branch"), out
    assert git_server.show("work", "lux/work", "a.txt") == "changed\n"
    assert git_server.rev("context", "lux/work") == "", "a push: false repository got the branch"
    bad = lux.run("push", run_id, "--expect", "context=" + "0" * 40, check=False)
    assert bad.returncode != 0 and "not pushed" in bad.stderr, bad.stderr


def test_one_script_commits_in_two_repositories(lux, runners, hosts, fake_image, git_server):
    """lux-fake's cd moves between checkouts, so one turn can commit in
    each, and one push pushes both."""
    git_server.create("one", {"base.txt": "1\n"})
    git_server.create("two", {"base.txt": "2\n"})
    runners.start(hosts[0])
    script = "\n".join(["cd /workspace/repos/one", "write a.txt x", "commit one",
                        "cd ../two", "write b.txt y", "commit two"])
    spec = repo_spec(fake_image, git_server, script, repo="one")
    spec["git"]["repositories"].append({"name": "two", "url": git_server.url("two"), "ref": "main",
                                        "credential": "GIT_TOKEN"})
    run_id = lux.submit(spec)
    out = wait_until(lambda: (o := lux.logs(run_id)).count("committed ") == 2 and o, 30, 0.3, "no two commits")
    assert "cwd /workspace/repos/two" in out, out
    pushed = {r["repo"]: r["status"] for r in lux.json("push", run_id, "--wait")}
    assert pushed == {"one": "pushed", "two": "pushed"}, pushed
    assert git_server.show("one", "lux/work", "a.txt") == "x\n"
    assert git_server.show("two", "lux/work", "b.txt") == "y\n"
    lux.run("terminate", run_id)


# ---- repositories added on resume ------------------------------------------------

def clones(lux, run_id: str, repo: str) -> list[dict]:
    return [e["data"] for e in lux.events(run_id, "git.clone") if e["data"]["repo"] == repo]


def stopped_with_one(lux, fake_image, git_server) -> str:
    """A Run with repository "one" whose agent has done a turn, stopped."""
    git_server.create("one", {"base.txt": "1\n"})
    run_id = lux.submit(repo_spec(fake_image, git_server, "write a.txt from-one", repo="one"))
    lux.wait_activity(run_id, "idle")
    lux.run("stop", run_id, "--wait")
    return run_id


def test_a_resume_adds_a_repository(lux, runners, hosts, fake_image, git_server):
    """The agent goes on with its conversation and finds the new checkout
    beside the old; its git.clone event names the resume, and a push pushes
    both repositories."""
    runners.start(hosts[0])
    run_id = stopped_with_one(lux, fake_image, git_server)
    session = lux.get(run_id)["sessionId"]
    git_server.create("two", {"b.txt": "from two\n"})
    p = lux.run("resume", run_id, "--wait", "--add-repo", f"two={git_server.url('two')},credential=GIT_TOKEN",
                "--request-id", "r-1", "--secret", f"GIT_TOKEN={git_server.token}",
                "--input", "history\nread /workspace/repos/two/b.txt")
    assert "request r-1" in p.stderr, p.stderr
    out = lux.wait_output(run_id, "from two")
    run = lux.get(run_id)
    assert run["sessionId"] == session, "the conversation continued, not restarted"
    assert "write a.txt from-one" in out[out.index("history:"):], out
    added = [r for r in run["spec"]["git"]["repositories"] if r["name"] == "two"]
    assert added and added[0]["addedBy"] == "r-1", run["spec"]["git"]
    [clone] = clones(lux, run_id, "two")
    assert clone["status"] == "cloned" and clone["requestId"] == "r-1" and len(clone["commit"]) == 40, clone
    assert not clones(lux, run_id, "one")[1:], "one was cloned again on resume"
    [req] = [e["data"] for e in lux.events(run_id, "resume.requested")]
    assert req["requestId"] == "r-1" and req["addedRepositories"] == ["two"], req
    # The credential is declared, runner-only, and never in the container.
    lux.run("steer", run_id, "print-secret GIT_TOKEN")
    lux.wait_activity(run_id, "idle")
    assert git_server.token not in lux.logs(run_id)

    lux.run("steer", run_id, "\n".join(["cd /workspace/repos/one", "commit one",
                                         "cd /workspace/repos/two", "write c.txt y", "commit two"]))
    wait_until(lambda: lux.logs(run_id).count("committed ") == 2, 30, 0.3, "no two commits")
    lux.wait_activity(run_id, "idle")
    pushed = {r["repo"]: r["status"] for r in lux.json("push", run_id, "--wait")}
    assert pushed == {"one": "pushed", "two": "pushed"}, pushed
    assert git_server.show("one", "lux/work", "a.txt") == "from-one\n"
    assert git_server.show("two", "lux/work", "c.txt") == "y\n"
    lux.run("terminate", run_id)


def test_an_added_repository_that_cannot_be_cloned_is_dropped(lux, runners, hosts, fake_image, git_server):
    """The Run goes on without it, with its conversation: the clone's
    failure is an event, and luxd takes the repository out of the spec."""
    runners.start(hosts[0])
    run_id = stopped_with_one(lux, fake_image, git_server)
    session = lux.get(run_id)["sessionId"]
    lux.run("resume", run_id, "--wait", "--add-repo", f"ghost={git_server.url('no-such-repo')},credential=GIT_TOKEN",
            "--request-id", "r-2", "--secret", f"GIT_TOKEN={git_server.token}", "--input", "history")
    out = lux.wait_output(run_id, "history:")
    run = lux.wait_state(run_id, "running")
    assert run["sessionId"] == session and "write a.txt from-one" in out[out.index("history:"):], out
    [clone] = wait_until(lambda: clones(lux, run_id, "ghost"), 30, 0.3, "no git.clone for ghost")
    assert clone["status"] == "failed" and clone["requestId"] == "r-2" and clone["error"], clone
    assert git_server.token not in clone["error"], clone
    names = [r["name"] for r in lux.get(run_id)["spec"]["git"]["repositories"]]
    assert names == ["one"], names
    lux.run("steer", run_id, "unless-exists /workspace/repos/ghost echo no ghost")
    lux.wait_output(run_id, "no ghost")
    # Pushes leave it out too.
    assert [r["repo"] for r in lux.json("push", run_id, "--wait", check=False)] == ["one"]
    lux.run("terminate", run_id)


def test_adding_a_repository_is_refused_when_it_cannot_be(lux, runners, hosts, fake_image, git_server):
    """A name the Run has already: 422. A Run not stopped: 409."""
    runners.start(hosts[0])
    run_id = stopped_with_one(lux, fake_image, git_server)
    secret = ("--secret", f"GIT_TOKEN={git_server.token}")
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--add-repo", f"one={git_server.url('two')}", *secret)
    assert e.value.code == 4 and "duplicate" in e.value.stderr, e.value.stderr
    # A new credential needs its value.
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--add-repo", f"x={git_server.url('x')},credential=OTHER_TOKEN", *secret)
    assert e.value.code == 4 and "OTHER_TOKEN" in e.value.stderr, e.value.stderr
    assert [r["name"] for r in lux.get(run_id)["spec"]["git"]["repositories"]] == ["one"], "a refused resume changed the spec"

    lux.run("resume", run_id, "--wait", *secret)
    with pytest.raises(CLIError) as e:
        lux.run("resume", run_id, "--add-repo", f"late={git_server.url('two')}", *secret)
    assert e.value.code == 4 and "stop it first" in e.value.stderr, e.value.stderr
    lux.run("terminate", run_id)


def test_a_submitted_spec_cannot_set_added_by(lux, fake_image, git_server):
    """Only luxd marks a repository as added on resume. (The CLI never sends
    addedBy: a spec file's is dropped, so this goes to the API.)"""
    import requests
    spec = repo_spec(fake_image, git_server, "echo never", repo="whatever")
    spec["git"]["repositories"][0]["addedBy"] = "r-1"
    r = requests.post(f"{lux.env.luxd_url}/v1/runs", json=spec, timeout=10,
                      headers={"Authorization": f"Bearer {lux.api_key}"})
    assert r.status_code == 422 and r.json()["error"]["code"] == "invalid_spec", r.text
    assert "addedBy" in r.text, r.text
