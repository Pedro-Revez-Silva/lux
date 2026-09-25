"""Version switch and rollback: luxhost.release (ported from the unit tests
of the deploy script it replaces). Offline, against a temp directory
standing in for /usr/local/lux; systemctl and the health check are stubbed
through the functions' runner=/sleep=/now= hooks."""
import os
import subprocess
import tarfile

import pytest

from luxhost import release
from luxhost.host import HostError


def fake_run(returncode: int, stderr: str = "", stdout: str = ""):
    def runner(*args, **kwargs):
        return subprocess.CompletedProcess(args, returncode, stdout=stdout, stderr=stderr)

    return runner


@pytest.fixture
def layout(tmp_path):
    root = str(tmp_path)
    bin_dir = os.path.join(root, "bin")
    runner_bin_dir = os.path.join(root, "runner")
    current_link = os.path.join(root, "current")
    old_dir = os.path.join(root, "versions", "v1")
    new_dir = os.path.join(root, "versions", "v2")
    for d in (old_dir, new_dir):
        os.makedirs(os.path.join(d, "bin"))
        os.makedirs(os.path.join(d, "lib", "lux", "runner"))
        open(os.path.join(d, "bin", "luxd"), "w").close()
        open(os.path.join(d, "bin", "lux"), "w").close()
    # An already-deployed v1, as switch_symlinks would have left it.
    links = release.stable_links(current_link, bin_dir, runner_bin_dir)
    os.symlink(old_dir, current_link)
    for link_path, target in links.items():
        os.makedirs(os.path.dirname(link_path), exist_ok=True)
        os.symlink(os.path.join(old_dir, os.path.relpath(target, current_link)), link_path)
    return current_link, links, old_dir, new_dir


def test_switch_points_every_link_at_the_new_version(layout):
    current_link, links, _, new_dir = layout
    release.switch_symlinks(new_dir, current_link, links)
    assert os.path.realpath(current_link) == new_dir
    for link_path in links:
        assert os.path.realpath(link_path).startswith(new_dir + os.sep)


def test_restore_after_switch_returns_every_link_to_v1(layout):
    current_link, links, old_dir, new_dir = layout
    previous = release.switch_symlinks(new_dir, current_link, links)
    release.restore_symlinks(previous, current_link, links)
    assert os.path.realpath(current_link) == old_dir
    for link_path in links:
        assert os.path.realpath(link_path).startswith(old_dir + os.sep)


def test_restore_with_no_previous_deploy_removes_the_links(layout):
    current_link, links, _, new_dir = layout
    for link_path in [current_link, *links]:
        os.remove(link_path)
    previous = {current_link: None, **{k: None for k in links}}
    release.switch_symlinks(new_dir, current_link, links)
    release.restore_symlinks(previous, current_link, links)
    assert not os.path.islink(current_link)
    for link_path in links:
        assert not os.path.islink(link_path)


def _make_tarball(root, version):
    src = os.path.join(root, "src")
    os.makedirs(os.path.join(src, "bin"), exist_ok=True)
    with open(os.path.join(src, "bin", "luxd"), "w") as f:
        f.write(version)
    path = os.path.join(root, f"{version}.tar.gz")
    with tarfile.open(path, "w:gz") as tf:
        tf.add(src, arcname=".")
    subprocess.run(["rm", "-r", src], check=True)
    return path


def test_extract_never_touches_the_currently_running_version_dir(tmp_path):
    versions_root = str(tmp_path / "versions")
    old_dir = release.extract_release(_make_tarball(str(tmp_path), "v1"), versions_root, "v1")
    marker = os.path.join(old_dir, "bin", "luxd")
    new_dir = release.extract_release(_make_tarball(str(tmp_path), "v2"), versions_root, "v2")
    assert os.path.exists(marker), "extracting v2 deleted v1's files"
    with open(os.path.join(new_dir, "bin", "luxd")) as f:
        assert f.read() == "v2"


def test_extract_replaces_a_stale_partial_directory_for_the_same_version(tmp_path):
    versions_root = str(tmp_path / "versions")
    stale = os.path.join(versions_root, "v1")
    os.makedirs(stale)
    with open(os.path.join(stale, "leftover"), "w") as f:
        f.write("partial")
    version_dir = release.extract_release(_make_tarball(str(tmp_path), "v1"), versions_root, "v1")
    assert not os.path.exists(os.path.join(version_dir, "leftover"))
    assert os.path.exists(os.path.join(version_dir, "bin", "luxd"))


def test_verify_sha256_refuses_a_mismatch_and_an_unlisted_tarball(tmp_path):
    tarball = tmp_path / "lux_v1_linux_arm64.tar.gz"
    tarball.write_bytes(b"release")
    sums = tmp_path / "SHA256SUMS"
    sums.write_text("0" * 64 + "  lux_v1_linux_arm64.tar.gz\n")
    with pytest.raises(HostError, match="checksum mismatch"):
        release.verify_sha256(str(tarball), str(sums), tarball.name)
    with pytest.raises(HostError, match="not listed"):
        release.verify_sha256(str(tarball), str(sums), "lux_v1_linux_amd64.tar.gz")


def test_run_migrate_reports_success_and_failure():
    assert release.run_migrate("/bin/true", "postgres://x", runner=fake_run(0))[0]
    ok, stderr = release.run_migrate("/bin/false", "postgres://x", runner=fake_run(1, stderr="boom"))
    assert not ok and stderr == "boom"


def test_restart_luxd_reports_failure():
    ok, stderr = release.restart_luxd(runner=fake_run(1, stderr="unit not found"))
    assert not ok and stderr == "unit not found"


def test_is_luxd_active_reads_systemctl_stdout():
    assert release.is_luxd_active(runner=fake_run(0, stdout="active\n"))
    assert not release.is_luxd_active(runner=fake_run(3, stdout="failed\n"))


def test_wait_healthy_returns_true_as_soon_as_both_checks_pass():
    calls = {"n": 0}

    def check_active():
        calls["n"] += 1
        return calls["n"] >= 2

    assert release.wait_healthy(check_active, lambda: True, timeout_s=10, interval_s=1,
                                sleep=lambda s: None, now=lambda: 0)
    assert calls["n"] == 2


def test_wait_healthy_returns_false_once_the_deadline_passes():
    clock = {"t": 0}

    def sleep(s):
        clock["t"] += s

    assert not release.wait_healthy(lambda: False, lambda: False, timeout_s=5, interval_s=2,
                                    sleep=sleep, now=lambda: clock["t"])
