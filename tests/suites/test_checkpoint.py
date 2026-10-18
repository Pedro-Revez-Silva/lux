"""Committed git checkpoints can seed a different Run after the source stops."""

from conftest import fake_agent
from env import wait_until
from suites.test_git import repo_spec, commit, in_container


def test_clone_checkpoint_of_parked_run(lux, runners, hosts, fake_image, git_server, tenant_factory):
    git_server.create("checkpoint", {"a.txt": "base\n"})
    runners.start(hosts[0])
    source = lux.submit(repo_spec(fake_image, git_server, "write a.txt unpublished", repo="checkpoint"))
    lux.wait_activity(source, "idle")
    in_container(hosts[0], source, "checkpoint", "git checkout -b unpublished-branch")
    head = commit(lux, source, hosts[0], "checkpoint", "unpublished")
    lux.run("stop", source, "--wait")
    lux.wait_uploaded(source)
    wait_until(lambda: any(e["type"] == "git.checkpoint" for e in lux.json("events", source)),
               60, 0.3, "checkpoint not uploaded")
    spec = fake_agent(fake_image, "read a.txt")
    spec["workload"]["workdir"] = "/workspace/repos/copy"
    spec["git"] = {"repositories": [{"name": "copy", "from": {"run": source, "repo": "checkpoint"}}]}
    destination = lux.submit(spec)
    lux.wait_output(destination, "unpublished")
    events = lux.json("events", destination)
    cloned = next(e["data"] for e in events if e["type"] == "git.clone")
    assert cloned["commit"] == head
    assert cloned["from"]["run"] == source
    assert cloned["from"]["checkpoint"]
    branch = hosts[0].exec("podman", "exec", "--user", "agent", "-w", "/workspace/repos/copy", f"lux-{destination}", "git", "branch", "--show-current")
    assert branch.strip() == "unpublished-branch"
    other = tenant_factory()
    import requests
    refused = requests.post(f"{other.env.luxd_url}/v1/runs", headers={"Authorization": f"Bearer {other.api_key}"}, json=spec, timeout=10)
    assert refused.status_code == 422, refused.text
    spec["git"]["repositories"][0]["from"]["run"] = "run_missing"
    missing = requests.post(f"{other.env.luxd_url}/v1/runs", headers={"Authorization": f"Bearer {other.api_key}"}, json=spec, timeout=10)
    assert missing.status_code == 422, missing.text
