"""Whole reconcile runs against FakeSh/FakeWeb and a real local Git remote."""
import datetime
import os
import subprocess
import tomllib

import pytest
from conftest import PREFIX, FakeWeb, desired, make_release

from luxhost import backup, desired as desired_mod
from luxhost.host import HostError


def summary(capsys) -> str:
    lines = [ln for ln in capsys.readouterr().out.splitlines() if ln.startswith("lux-reconcile: status=")]
    assert len(lines) == 1, lines
    return lines[0]


def read(env, rel):
    with open(env.path(rel)) as f:
        return f.read()


def luxd_toml(env) -> dict:
    return tomllib.loads(read(env, "etc/lux/luxd.toml"))


def installed(env) -> str:
    try:
        return read(env, "usr/local/lux/CURRENT_VERSION").strip()
    except FileNotFoundError:
        return ""


def restarts(env, unit="luxd"):
    return [c for c in env.sh.commands("systemctl") if c[1] == "restart" and c[2] == unit]


def deploy_version(env, capsys, version):
    env.web.releases[version] = make_release(version)
    env.sh.healthy_versions.add(version)
    env.repo.set_desired(desired(version))
    assert env.run() == 0, summary(capsys)
    capsys.readouterr()


def test_first_run_without_a_version_sets_up_the_host_and_installs_nothing(env, capsys):
    assert env.run() == 0
    line = summary(capsys)
    assert line.startswith("lux-reconcile: status=ok version=none changed=[")
    assert env.sh.mounted and env.sh.has_filesystem
    assert "lux" in env.sh.databases
    assert os.stat(env.path("etc/lux/luxd.toml")).st_mode & 0o777 == 0o600
    assert installed(env) == ""
    assert env.web.fetched == []
    assert "luxd" not in env.sh.enabled
    assert {"lux-reconcile.timer", "lux-pg-backup.timer", "cloudflared"} <= env.sh.enabled
    assert read(env, "etc/cloudflared/token") == "tunnel-token\n"


def test_second_run_changes_nothing(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.sh.calls.clear()
    assert env.run() == 0
    assert summary(capsys) == "lux-reconcile: status=ok version=v1.0.0 changed=[]"
    assert restarts(env) == [] and restarts(env, "cloudflared") == []
    assert not [c for c in env.sh.calls if c[:2] == ["systemctl", "daemon-reload"]]


def test_luxd_toml_carries_infra_and_desired_settings(env, capsys):
    env.repo.set_desired(desired(extra='[luxd]\ndebug = true\n[luxd.defaults]\nmemory = "16Gi"\ncpus = 4\n'))
    assert env.run() == 0
    cfg = luxd_toml(env)
    assert cfg["listen"] == "0.0.0.0:7070"
    assert cfg["public_url"] == "https://lux.example.com"
    assert cfg["runner_url"] == "http://10.60.0.10:7070"
    assert cfg["debug"] is True
    assert cfg["defaults"] == {"memory": "16Gi", "cpus": 4}
    assert cfg["s3"] == {"bucket": "lux-blobs-123456789012-eu-north-1", "region": "eu-north-1"}
    assert cfg["console"]["auth"] == "cloudflare-access"
    assert cfg["console"]["cloudflare_access"] == {"team": "acme", "aud": "aud-tag"}
    app_pw = read(env, "root/.lux-app-password")
    assert cfg["database"]["app_password"] == app_pw
    assert cfg["database"]["url"] == f"postgres://lux_app:{app_pw}@127.0.0.1:5432/lux?sslmode=disable"


def test_no_access_team_means_key_auth(env, capsys):
    del env.sh.ssm[f"{PREFIX}/cf_access_team"]
    del env.sh.ssm[f"{PREFIX}/cf_access_aud"]
    assert env.run() == 0
    assert luxd_toml(env)["console"]["auth"] == "key"


def test_config_change_restarts_a_running_luxd_once_and_only_then(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.sh.calls.clear()

    env.repo.set_desired(desired("v1.0.0", '[luxd]\nscale_down_after = "30m"\n'))
    assert env.run() == 0
    line = summary(capsys)
    assert "luxd.toml" in line and "luxd-restarted" in line
    assert luxd_toml(env)["scale_down_after"] == "30m"
    assert len(restarts(env)) == 1

    env.sh.calls.clear()
    assert env.run() == 0
    assert restarts(env) == []


def test_ssm_change_rerenders_the_config(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.sh.ssm[f"{PREFIX}/public_url"] = "https://lux2.example.com"
    env.sh.calls.clear()
    assert env.run() == 0
    assert luxd_toml(env)["public_url"] == "https://lux2.example.com"
    assert len(restarts(env)) == 1


def test_config_change_does_not_start_a_stopped_luxd(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.sh.active.discard("luxd")
    env.sh.enabled.discard("luxd")
    env.sh.healthy_versions.clear()
    env.sh.calls.clear()
    env.repo.set_desired(desired("v1.0.0", "[luxd]\ndebug = true\n"))
    env.run()
    assert restarts(env) == []


def test_version_switch_installs_migrates_and_restarts(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    assert installed(env) == "v1.0.0"
    assert "luxd" in env.sh.enabled and "luxd" in env.sh.active
    assert os.path.realpath(env.path("usr/local/bin/luxd")) == env.path("usr/local/lux/versions/v1.0.0/bin/luxd")

    env.web.releases["v1.1.0"] = make_release("v1.1.0")
    env.sh.healthy_versions.add("v1.1.0")
    env.repo.set_desired(desired("v1.1.0"))
    env.sh.calls.clear()
    assert env.run() == 0
    assert "version:v1.1.0" in summary(capsys)
    assert installed(env) == "v1.1.0"
    migrate = [c for c in env.sh.calls if c[-1] == "migrate"]
    assert migrate == [[env.path("usr/local/lux/versions/v1.1.0/bin/luxd"), "migrate"]]
    assert os.path.realpath(env.path("usr/local/bin/luxd")) == env.path("usr/local/lux/versions/v1.1.0/bin/luxd")


def test_unhealthy_release_rolls_back_and_fails_the_run(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.web.releases["v2.0.0"] = make_release("v2.0.0")  # never healthy
    env.repo.set_desired(desired("v2.0.0"))
    assert env.run() == 1
    line = summary(capsys)
    assert line.startswith("lux-reconcile: status=error version=v1.0.0 ")
    assert "rolled back to v1.0.0" in line
    assert installed(env) == "v1.0.0"
    assert os.path.realpath(env.path("usr/local/bin/luxd")) == env.path("usr/local/lux/versions/v1.0.0/bin/luxd")
    assert os.path.realpath(env.path("usr/local/lib/lux/runner")) == env.path("usr/local/lux/versions/v1.0.0/lib/lux/runner")
    assert "luxd" in env.sh.active


def test_failed_migrate_switches_nothing(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    env.web.releases["v2.0.0"] = make_release("v2.0.0")
    env.sh.healthy_versions.add("v2.0.0")
    env.sh.migrate_rc = 1
    env.repo.set_desired(desired("v2.0.0"))
    env.sh.calls.clear()
    assert env.run() == 1
    assert "migration 7 failed" in summary(capsys)
    assert installed(env) == "v1.0.0"
    assert os.path.realpath(env.path("usr/local/lux/current")) == env.path("usr/local/lux/versions/v1.0.0")
    assert restarts(env) == []


def test_checksum_mismatch_switches_nothing(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    rel = make_release("v2.0.0")
    rel["SHA256SUMS"] = rel["SHA256SUMS"].replace(rel["SHA256SUMS"][:8], b"00000000")
    env.web.releases["v2.0.0"] = rel
    env.repo.set_desired(desired("v2.0.0"))
    assert env.run() == 1
    line = summary(capsys)
    assert "checksum mismatch" in line and "leaving v1.0.0 running" in line
    assert installed(env) == "v1.0.0"


def test_first_install_that_fails_health_leaves_no_links(env, capsys):
    env.web.releases["v1.0.0"] = make_release("v1.0.0")
    env.repo.set_desired(desired("v1.0.0"))
    assert env.run() == 1
    summary(capsys)
    assert installed(env) == ""
    assert not os.path.lexists(env.path("usr/local/bin/luxd"))
    assert not os.path.lexists(env.path("usr/local/lux/current"))


def test_bad_desired_state_fails_without_touching_the_running_install(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    toml_before = read(env, "etc/lux/luxd.toml")
    units_before = read(env, "etc/systemd/system/luxd.service")
    env.web.releases["v2.0.0"] = make_release("v2.0.0")
    env.sh.healthy_versions.add("v2.0.0")
    env.repo.set_desired(desired("v2.0.0", '[luxd]\nlisten = "0.0.0.0:1"\n[luxd.defaults]\nmemory = "lots"\n'))
    env.sh.calls.clear()
    env.web.fetched.clear()
    assert env.run() == 1
    line = summary(capsys)
    assert line.startswith("lux-reconcile: status=error version=v1.0.0 ")
    assert "desired-state" in line and "luxd.listen: unknown key" in line and "luxd.defaults.memory" in line
    assert installed(env) == "v1.0.0"
    assert read(env, "etc/lux/luxd.toml") == toml_before
    assert read(env, "etc/systemd/system/luxd.service") == units_before
    assert env.web.fetched == []
    # Only reads: SSM and the checkout's own git commands.
    assert {c[0] for c in env.sh.calls} == {"aws"}


def test_unparseable_desired_state_fails(env, capsys):
    env.repo.set_desired("lux_version = \n")
    assert env.run() == 1
    assert "desired-state" in summary(capsys)
    assert not os.path.exists(env.path("etc/lux/luxd.toml"))


def test_checkout_follows_the_remote_and_drops_local_edits(env, capsys):
    assert env.run() == 0
    with open(os.path.join(env.checkout, "host", "lux-host.toml"), "a") as f:
        f.write("junk = 1\n")
    open(os.path.join(env.checkout, "stray"), "w").close()
    capsys.readouterr()
    assert env.run() == 0
    summary(capsys)
    assert not os.path.exists(os.path.join(env.checkout, "stray"))
    with open(os.path.join(env.checkout, "host", "lux-host.toml")) as f:
        assert "junk" not in f.read()


def test_host_code_change_re_executes_the_new_reconciler_once(env, capsys):
    assert env.run() == 0
    capsys.readouterr()
    env.repo.write("host/luxhost/marker.py", "# a change to the host code\n")
    env.repo.commit("host code")
    assert env.run() == 0
    assert len(env.reexecs) == 1
    assert env.reexecs[0][1] == os.path.join(env.checkout, "host", "reconcile.py")
    line = summary(capsys)
    assert "checkout" in line

    # Next run: the checkout is current, no re-exec.
    assert env.run() == 0
    assert len(env.reexecs) == 1


def test_desired_state_only_change_does_not_re_exec(env, capsys):
    assert env.run() == 0
    env.repo.set_desired(desired(extra="[luxd]\ndebug = true\n"))
    assert env.run() == 0
    assert env.reexecs == []


def test_re_executed_run_does_not_re_exec_again(env, capsys):
    assert env.run() == 0
    env.repo.write("host/luxhost/marker.py", "# 1\n")
    env.repo.commit("host code")
    from luxhost.reconcile import main

    calls = []
    rc = main(host=env.host, bootstrap_path=env.bootstrap, reexec=lambda *a: calls.append(a),
              environ={"LUX_RUNNER_IP": "10.60.0.10", "LUX_RECONCILE_REEXECED": "checkout"})
    assert rc == 0 and calls == []


def test_public_repo_reads_no_deploy_key(env, capsys):
    assert env.run() == 0
    key_reads = [c for c in env.sh.commands("aws") if c[2] == "get-parameter" and "deploy" in c[c.index("--name") + 1]]
    assert key_reads == []
    assert not os.path.exists(env.path("root/.ssh/lux-config-deploy-key"))


def test_deploy_key_is_written_from_ssm_owner_only(env, capsys):
    env.sh.ssm[f"{PREFIX}/config_repo_deploy_key_parameter"] = "/acme/lux-config-deploy-key"
    env.sh.ssm["/acme/lux-config-deploy-key"] = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"
    assert env.run() == 0
    assert "deploy-key" in summary(capsys)
    path = env.path("root/.ssh/lux-config-deploy-key")
    assert os.stat(path).st_mode & 0o777 == 0o600
    assert read(env, "root/.ssh/lux-config-deploy-key").endswith("-----END OPENSSH PRIVATE KEY-----\n")
    assert env.run() == 0
    assert "deploy-key" not in summary(capsys)


def test_missing_ssm_parameter_fails_before_touching_anything(env, capsys):
    del env.sh.ssm[f"{PREFIX}/blob_bucket"]
    assert env.run() == 1
    assert "blob_bucket" in summary(capsys)
    assert not env.sh.mounted


def test_existing_filesystem_is_never_formatted(env, capsys):
    env.sh.has_filesystem = True
    assert env.run() == 0
    assert env.sh.commands("mkfs.ext4") == []
    assert env.sh.mounted


def test_volume_that_never_appears_fails_hard(env, capsys):
    for f in os.listdir(env.path("dev/disk/by-id")):
        os.remove(env.path(f"dev/disk/by-id/{f}"))
    assert env.run() == 1
    line = summary(capsys)
    assert "postgres" in line and "did not appear after 300s" in line
    assert env.sh.commands("mount") == [] and env.sh.commands("pg_createcluster") == []
    assert not os.path.exists(env.path("etc/lux/luxd.toml"))


def test_mounted_volume_is_left_alone_and_fstab_written_once(env, capsys):
    assert env.run() == 0
    assert read(env, "etc/fstab").count("LABEL=pgdata") == 1
    env.sh.calls.clear()
    assert env.run() == 0
    assert env.sh.commands("pg_createcluster") == [] and env.sh.commands("blkid") == []


def test_passwords_are_generated_once(env, capsys):
    assert env.run() == 0
    owner = read(env, "root/.lux-pg-owner-password")
    assert len(owner) == 32 and owner.isalnum()
    assert env.run() == 0
    assert read(env, "root/.lux-pg-owner-password") == owner


def test_unit_change_in_the_repo_reloads_systemd_and_restarts_cloudflared(env, capsys, monkeypatch):
    assert env.run() == 0
    from luxhost import units
    monkeypatch.setattr(units, "CLOUDFLARED_SERVICE", units.CLOUDFLARED_SERVICE.replace("RestartSec=5s", "RestartSec=10s"))
    env.sh.calls.clear()
    capsys.readouterr()
    assert env.run() == 0
    assert "unit:cloudflared.service" in summary(capsys)
    assert ["systemctl", "daemon-reload"] in env.sh.calls
    assert len(restarts(env, "cloudflared")) == 1


def test_rotated_tunnel_token_restarts_cloudflared(env, capsys):
    assert env.run() == 0
    env.sh.ssm[f"{PREFIX}/cloudflare-tunnel-token"] = "new-token"
    env.sh.calls.clear()
    assert env.run() == 0
    assert read(env, "etc/cloudflared/token") == "new-token\n"
    assert len(restarts(env, "cloudflared")) == 1


def test_legacy_deploy_timer_is_removed(env, capsys):
    sd = env.path("etc/systemd/system")
    os.makedirs(sd, exist_ok=True)
    for u in ("lux-deploy.timer", "lux-deploy.service"):
        open(os.path.join(sd, u), "w").close()
    legacy = env.path("usr/local/sbin/lux-render-config.sh")
    os.makedirs(os.path.dirname(legacy))
    open(legacy, "w").close()
    assert env.run() == 0
    assert "legacy-removed" in summary(capsys)
    assert not os.path.exists(os.path.join(sd, "lux-deploy.timer"))
    assert not os.path.exists(legacy)
    assert ["systemctl", "disable", "--now", "lux-deploy.timer", "lux-deploy.service"] in env.sh.calls


@pytest.mark.parametrize("text,problem", [
    ('lux_version = 3\n', "lux_version"),
    ('lux_version = "v1 ; rm"\n', "not a release tag"),
    ('release_repo = "nope"\n', "release_repo"),
    ('[luxd]\ndebug = "yes"\n', "luxd.debug"),
    ('[luxd]\nlease = "30"\n', "luxd.lease"),
    ('[luxd]\noutdated_drain_percent = 0\n', "outdated_drain_percent"),
    ('[luxd.defaults]\ncpus = -1\n', "luxd.defaults.cpus"),
    ('[luxd.database]\nurl = "x"\n', "luxd.database: unknown key"),
    ('extra = 1\n', "extra: unknown key"),
])
def test_desired_state_validation(text, problem):
    with pytest.raises(HostError, match=problem.replace(".", r"\.").replace("(", r"\(")):
        desired_mod.parse(text)


def test_bad_release_repo_type_is_one_problem():
    with pytest.raises(HostError) as e:
        desired_mod.parse("release_repo = 1\n")
    assert "release_repo: 1" in str(e.value) and "release_base_url" not in str(e.value)


def test_desired_state_defaults():
    d = desired_mod.parse("")
    assert d.lux_version is None
    assert d.release_base_url == "https://github.com/marcioapm/lux/releases/download"
    assert desired_mod.parse('lux_version = "none"').lux_version is None
    assert desired_mod.parse('release_repo = "acme/lux"').release_base_url == "https://github.com/acme/lux/releases/download"


def test_example_desired_state_file_parses():
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    with open(os.path.join(here, "lux-host.toml")) as f:
        desired_mod.parse(f.read())


class FakeProc:
    def __init__(self, rc, stdout=None):
        self.rc = rc
        self.stdout = stdout

    def wait(self):
        return self.rc


def _popen(dump_rc, upload_rc, calls):
    def popen(argv, stdout=None, stdin=None):
        calls.append(argv)
        if argv[0] == "pg_dump":
            return FakeProc(dump_rc, open(os.devnull, "rb"))
        return FakeProc(upload_rc)
    return popen


NOON = datetime.datetime(2026, 9, 25, 0, 0, 1, tzinfo=datetime.UTC)


def test_backup_streams_pg_dump_to_s3():
    calls = []
    url = backup.backup("lux", "lux-pg-backups-123456789012-eu-north-1", "eu-north-1", _popen(0, 0, calls), NOON)
    assert url == "s3://lux-pg-backups-123456789012-eu-north-1/lux-20260925T000001Z.dump"
    assert calls == [
        ["pg_dump", "-Fc", "-d", "lux"],
        ["aws", "s3", "cp", "-", url, "--region", "eu-north-1"],
    ]


def test_backup_fails_when_pg_dump_fails_even_if_the_upload_succeeds():
    with pytest.raises(HostError, match="pg_dump exited 1"):
        backup.backup("lux", "b", "eu-north-1", _popen(1, 0, []), NOON)


def test_backup_fails_when_the_upload_fails():
    with pytest.raises(HostError, match="aws s3 cp"):
        backup.backup("lux", "b", "eu-north-1", _popen(0, 1, []), NOON)


def test_backup_pipes_a_real_stream(tmp_path):
    """The real Popen wiring: pg_dump's stdout reaches the uploader's stdin."""
    bindir = tmp_path / "bin"
    bindir.mkdir()
    (bindir / "pg_dump").write_text("#!/bin/sh\nprintf 'PGDMP-bytes'\n")
    out = tmp_path / "uploaded"
    (bindir / "aws").write_text(f"#!/bin/sh\ncat > {out}\n")
    for f in bindir.iterdir():
        f.chmod(0o755)
    env_path = f"{bindir}:{os.environ['PATH']}"

    def popen(argv, **kw):
        return subprocess.Popen(argv, env={**os.environ, "PATH": env_path}, **kw)

    backup.backup("lux", "b", "eu-north-1", popen, NOON)
    assert out.read_text() == "PGDMP-bytes"


def test_release_base_url_is_used_for_downloads(env, capsys):
    deploy_version(env, capsys, "v1.0.0")
    assert env.web.fetched[0].startswith(FakeWeb.BASE_URL + "/v1.0.0/lux_v1.0.0_linux_")
