"""Walks the branch-preview demo in a headless browser, as a person would,
and saves a screenshot of each step: asleep → open the URL (signed in) →
the waking page → the app on commit A, visit 1 → idle → stopped by the
orchestrator → a new commit B → open again → woken on B, the visit count
carried on.

    scripts/demo-wake.sh up
    (cd tests && uv run python ../scripts/demo-wake-verify.py [SHOTS_DIR])

Needs the demo up (scripts/demo-wake.sh up) and Playwright's Chromium
(`uv run playwright install chromium`). Chromium resolves *.localhost to
this machine by itself.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parent.parent
SHOTS = Path(sys.argv[1] if len(sys.argv) > 1 else "/var/tmp/lux-wake-shots")
DEMO = str(ROOT / "scripts" / "demo-wake.sh")


def demo(*args: str) -> str:
    return subprocess.run([DEMO, *args], check=True, capture_output=True, text=True).stdout.strip()


def server() -> dict:
    return lux_json("server", "show", "web.pr1.lux.localhost")


def wait(fn, timeout: float, what: str):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if v := fn():
            return v
        time.sleep(1)
    raise SystemExit(f"timed out waiting for {what}")


def main() -> None:
    SHOTS.mkdir(parents=True, exist_ok=True)
    assert server()["state"] == "asleep", server()
    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        page = browser.new_page(viewport={"width": 900, "height": 640})
        # Not signed in: sent to sign in (the console), nothing woken.
        url = server()["url"]
        page.goto(url + "/goals", wait_until="commit")
        print("unsigned: sent to", page.url)
        assert server()["wakes"] == 0
        # Sign in with a ticket link; the waking page.
        page.goto(demo("open").replace("to=/", "to=/goals"))
        page.wait_for_selector("ol.steps")
        page.screenshot(path=str(SHOTS / "1-waking.png"))
        print("waking page:", page.inner_text("h1"))
        assert "Asked the orchestrator to start it" in page.content()
        # It drops into the app by itself.
        page.wait_for_selector("#commit", timeout=180_000)
        page.screenshot(path=str(SHOTS / "2-ready-commit-a.png"))
        a, visits_a = page.inner_text("#commit"), int(page.inner_text("#visits"))
        print("ready on", a, "visits", visits_a, "at", page.url)
        assert page.url.endswith("/goals") and "commit A" in page.content()
        # Idle, then stopped by the orchestrator.
        s = wait(lambda: (x := server())["state"] == "asleep" and x, 240, "idle and stopped")
        print("asleep again; wakes so far", s["wakes"])
        # A new commit while it sleeps; the next visit wakes it on it.
        b = demo("push", "hello from commit B")
        print("pushed", b)
        page.reload()
        page.wait_for_selector("ol.steps")
        page.screenshot(path=str(SHOTS / "3-waking-again.png"))
        time.sleep(2.5)
        if page.query_selector("ol.steps"):
            page.screenshot(path=str(SHOTS / "3b-waking-progress.png"))
        page.wait_for_selector("#commit", timeout=180_000)
        page.screenshot(path=str(SHOTS / "4-ready-commit-b.png"))
        got, visits_b = page.inner_text("#commit"), int(page.inner_text("#visits"))
        print("woke on", got, "visits", visits_b)
        assert got == b and "commit B" in page.content() and visits_b == visits_a + 1, (got, b, visits_a, visits_b)
        # The other pages: a hostname nothing answers to; a server nobody
        # wakes (no orchestrator follows it), after its wake timeout.
        page.goto(url.replace("web.pr1.", "gone."))
        page.screenshot(path=str(SHOTS / "5-gone.png"))
        assert "This preview is gone" in page.content()
        lone = lux_json("server", "create", "lone", "8080", "--wake", "request", "--wake-timeout", "3s",
                        "--hostname", "lone.lux.localhost", "--", "true")
        page.goto(ticket_link(lone["id"], lone["url"]))
        page.wait_for_selector("ol.steps")
        time.sleep(7)
        page.reload()
        page.screenshot(path=str(SHOTS / "6-no-answer.png"))
        assert "No answer from its owner" in page.content() and "Ask again" in page.content()
        lux_json("server", "rm", lone["id"])
        browser.close()
    print("ok; screenshots in", SHOTS)


def env() -> dict:
    # lux-dev-env.json under the log root holds the path of the dev env's JSON.
    pointer = Path(os.environ.get("LUX_TEST_LOG_ROOT", "/tmp")) / "lux-dev-env.json"
    return json.loads(Path(pointer.read_text().strip()).read_text())


def lux_json(*args: str) -> dict:
    e = env()
    out = subprocess.run([str(ROOT / "bin" / "lux"), "-o", "json", *args], check=True, capture_output=True, text=True,
                         env={**os.environ, "LUX_URL": e["luxd_url"], "LUX_API_KEY": e["api_key"]})
    return json.loads(out.stdout or "{}")


def ticket_link(sid: str, url: str) -> str:
    e = env()
    req = urllib.request.Request(f"{e['luxd_url']}/v1/servers/{sid}/tickets", method="POST",
                                 headers={"Authorization": f"Bearer {e['api_key']}"})
    with urllib.request.urlopen(req) as resp:
        t = json.load(resp)["ticket"]
    return f"{url}/.lux/auth?ticket={t}&to=/"


if __name__ == "__main__":
    main()
