"""Step 11: interactive access through the relay — exec, attach and port
forwarding, all through luxd (the client never reaches the host)."""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time

import pytest

from conftest import CLIError, fake_agent, generic
from env import ALPINE_IMAGE, BIN_DIR, free_port, wait_until


def sleeper(**extra) -> dict:
    return generic(ALPINE_IMAGE, "sh", "-c", "echo up; sleep 600", **extra)


def test_exec_runs_as_the_workload_with_its_environment(lux, runners, hosts, fake_image):
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo ready", env={"GREETING": "hello"}))
    lux.wait_activity(run_id, "idle")
    out = lux.run("exec", run_id, "-T", "--", "sh", "-c", "id -un; echo $GREETING; echo to-stderr >&2; pwd", input="").stdout
    assert out.split() [:2] == ["agent", "hello"], out
    # stdin, stdout and the exit code are the command's
    p = lux.run("exec", run_id, "-T", "--", "sh", "-c", "tr a-z A-Z; exit 7", input="shout\n", check=False)
    assert p.returncode == 7 and p.stdout == "SHOUT\n", (p.returncode, p.stdout, p.stderr)
    lux.run("terminate", run_id)


def test_exec_with_a_terminal(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(sleeper())
    lux.wait_output(run_id, "up")
    out = lux.run("exec", run_id, "-t", "--", "sh", "-c", "tty; stty size", input="").stdout
    assert "/dev/pts/" in out, out
    lux.run("terminate", run_id)


def test_exec_needs_a_running_run(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "true"))
    lux.wait_state(run_id, "succeeded")
    with pytest.raises(CLIError) as e:
        lux.run("exec", run_id, "--", "true")
    assert "succeeded" in e.value.stderr, e.value.stderr


def test_exec_does_not_outlive_its_client(lux, runners, hosts):
    """A client that goes away takes its command with it."""
    host = runners.start(hosts[0])
    run_id = lux.submit(sleeper())
    lux.wait_output(run_id, "up")
    marker = "7" + str(int(time.time() * 1000) % 10**7)
    p = lux.popen("exec", run_id, "-T", "--", "sleep", marker)
    wait_until(lambda: host.running("sleep", marker), 20, 0.3, "the exec never started")
    p.kill()
    p.wait()
    wait_until(lambda: not host.running("sleep", marker), 20, 0.3, "the exec'd command outlived its client")
    lux.run("terminate", run_id)


def test_attach_to_a_terminal_workload(lux, runners, hosts):
    """A generic workload with workload.tty: attach sees its output and
    types into it; its output is also the Run's output."""
    runners.start(hosts[0])
    spec = generic(ALPINE_IMAGE, "sh", "-c", "echo started; while read l; do echo \"got:$l\"; done")
    spec["workload"]["tty"] = True
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "started")
    p = lux.popen("attach", run_id, stdin=subprocess.PIPE)

    def typed():
        # Typed again until it lands: what comes before attach connects is lost.
        assert p.poll() is None, f"attach exited {p.returncode}: {p.stderr.read()}"
        p.stdin.write("hello-attach\n")
        p.stdin.flush()
        return "got:hello-attach" in lux.logs(run_id)
    wait_until(typed, 20, 0.5, "input never reached the terminal")
    p.stdin.close()
    p.terminate()
    p.wait(timeout=10)
    lux.run("terminate", run_id)


def test_attach_needs_a_terminal(lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(sleeper())
    lux.wait_output(run_id, "up")
    with pytest.raises(CLIError) as e:
        lux.run("attach", run_id, input="")
    assert "tty" in e.value.stderr, e.value.stderr
    lux.run("terminate", run_id)


def test_attach_restore_redraws_the_terminal(lux, runners, hosts, tmp_path):
    """lux attach --restore, in a real terminal (tmux): the screen a
    full-screen program drew before attach is redrawn, and when it leaves,
    the main screen it covered comes back, with the lines that scrolled
    off it in the terminal's history."""
    if shutil.which("tmux") is None:
        if os.environ.get("CI"):
            pytest.fail("needs tmux, as the attaching terminal: CI installs it, so this must run there")
        pytest.skip("needs tmux, as the attaching terminal")
    runners.start(hosts[0])
    script = (
        "for i in $(seq 1 40); do echo line-$i; done; "
        "printf '\\033]2;restored-title\\007\\033[?1049h\\033[2J\\033[H\\033[7mFULLSCREEN-APP\\033[m\\033[5;3Hpress enter'; "
        "read x; printf '\\033[?1049l'; echo left-full-screen; sleep 600"
    )
    spec = generic(ALPINE_IMAGE, "sh", "-c", script)
    spec["workload"]["tty"] = True
    run_id = lux.submit(spec)
    lux.wait_output(run_id, "press enter")

    sock = str(tmp_path / "tmux.sock")

    def tmux(*args: str) -> str:
        return subprocess.run(["tmux", "-S", sock, "-f", "/dev/null", *args], env={**lux._env(), "TERM": "xterm-256color"},
                              capture_output=True, text=True, check=True, timeout=20).stdout

    tmux("new-session", "-d", "-x", "100", "-y", "30", str(BIN_DIR / "lux"), "attach", "--restore", run_id)
    try:
        def screen() -> str:
            return tmux("capture-pane", "-p", "-t", "0")

        def alternate() -> bool:
            return tmux("display-message", "-p", "-t", "0", "#{alternate_on}").strip() == "1"

        # Restored without anything new being written: the full-screen
        # program's screen, on the alternate screen, and its title.
        wait_until(lambda: "FULLSCREEN-APP" in screen() and alternate(), 30, 0.3,
                   "the full-screen program's screen was not restored")
        assert "line-40" not in screen(), screen()
        assert tmux("display-message", "-p", "-t", "0", "#{pane_title}").strip() == "restored-title"
        lines = screen().splitlines()
        assert lines[4].startswith("  press enter"), lines[:6]

        # The program leaves the full screen: the main screen is back.
        tmux("send-keys", "-t", "0", "Enter")
        wait_until(lambda: "left-full-screen" in screen() and not alternate(), 30, 0.3,
                   "leaving the full screen did not bring the main screen back")
        visible = screen()
        assert "line-40" in visible and "FULLSCREEN-APP" not in visible, visible
        assert visible.count("line-40\n") == 1, visible
        # 40 lines and more on a 30-row screen: the first went to history.
        # Each line once: the snapshot's and the live output do not overlap.
        history = tmux("capture-pane", "-p", "-t", "0", "-S", "-", "-E", "-")
        assert "line-1\n" in history and "line-2\n" in history, history
        for n in (1, 20, 40):
            assert history.count(f"line-{n}\n") == 1, (n, history)
    finally:
        subprocess.run(["tmux", "-S", sock, "kill-server"], capture_output=True)
    lux.run("terminate", run_id)


def test_port_forward_reaches_a_declared_port_only(lux, runners, hosts):
    runners.start(hosts[0])
    serve = "while :; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\nhello-from-run\\n' | nc -l -p 8080 >/dev/null; done"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", serve, network={"ports": [{"port": 8080, "name": "web"}]}))
    lux.wait_state(run_id, "running")
    local = free_port()
    p = lux.popen("port-forward", run_id, "web", str(local))
    try:
        body = wait_until(lambda: "hello-from-run" in (b := http_get(local)) and b, 30, 0.5, "the tunnel never answered")
        assert "hello-from-run" in body
        # twice: every connection gets its own tunnel
        assert "hello-from-run" in http_get(local)
    finally:
        p.terminate()
        p.wait(timeout=10)
    with pytest.raises(CLIError) as e:
        lux.run("port-forward", run_id, "ssh", str(free_port()))
    assert "no port named" in e.value.stderr, e.value.stderr
    lux.run("terminate", run_id)


def test_streams_need_the_runs_tenant(lux, tenant_factory, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(sleeper())
    lux.wait_output(run_id, "up")
    other = tenant_factory()
    with pytest.raises(CLIError) as e:
        other.run("exec", run_id, "--", "true")
    assert "not found" in e.value.stderr.lower(), e.value.stderr
    lux.run("terminate", run_id)


def test_port_forward_after_a_runner_restart(lux, runners, hosts):
    """A Run re-adopted by a restarted runner (which has no assignment for
    it any more) is still reachable on its declared ports, and only them."""
    runners.start(hosts[0])
    serve = "while :; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\nstill-here\\n' | nc -l -p 8080 >/dev/null; done"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", serve, network={"ports": [{"port": 8080, "name": "web"}]}))
    lux.wait_state(run_id, "running")
    runners.stop(hosts[0], "KILL")
    runners.start(hosts[0])
    # The restarted runner's connection is up once a check passes.
    wait_until(lambda: lux.api(f"/v1/runs/{run_id}/ports/web").ok, 30, 0.5, "host never reconnected")
    local = free_port()
    p = lux.popen("port-forward", run_id, "web", str(local))
    try:
        body = wait_until(lambda: "still-here" in (b := http_get(local)) and b, 30, 0.5, "no answer after restart")
        assert "still-here" in body
    finally:
        p.terminate()
        p.wait(timeout=10)
    lux.run("terminate", run_id)


def test_a_stream_ends_when_the_runner_goes_away(lux, runners, hosts):
    """The runner's connection drops mid-exec: the client gets an error
    promptly instead of waiting forever."""
    runners.start(hosts[0])
    run_id = lux.submit(sleeper())
    lux.wait_output(run_id, "up")
    p = lux.popen("exec", run_id, "-T", "--", "sleep", "300", stdin=subprocess.PIPE)
    wait_until(lambda: hosts[0].running("sleep", "300"), 20, 0.3, "the exec never started")
    runners.stop(hosts[0], "KILL")
    try:
        p.wait(timeout=30)
    except subprocess.TimeoutExpired:
        p.kill()
        raise AssertionError("exec hung after its runner went away")
    assert p.returncode != 0
    assert "disconnected" in p.stderr.read(), "no reason given"


def http_get(port: int) -> str:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=3) as s:
            s.sendall(b"GET / HTTP/1.0\r\n\r\n")
            data = b""
            while chunk := s.recv(4096):
                data += chunk
            return data.decode()
    except OSError:
        return ""
