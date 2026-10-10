"""The web console, in a headless browser: it signs in with a key, shows
what that key sees, and acts through the same API as the CLI. Uses the
system's Chrome, or Playwright's own Chromium if installed
(`uv run playwright install chromium`); skipped when neither is there."""

from __future__ import annotations

import json
import math
import re
import time
from datetime import datetime, timedelta, timezone
from decimal import ROUND_HALF_EVEN, Decimal

import psycopg
import pytest
from playwright.sync_api import TimeoutError as PlaywrightTimeoutError, expect

from conftest import FAKE_EC2_TIMERS, fake_only, generic
from env import ALPINE_IMAGE, wait_until
from fake_cost_plugin import FakeCostPlugin

pytestmark = pytest.mark.console


@pytest.fixture(scope="module")
def _console_page(browser, env):
    """One browser page for the whole file: a context and page cost more
    than the checks most tests make. A new suite file that drives the
    console gets its own, so keep a file's tests alike."""
    ctx = browser.new_context(viewport=VIEWPORT)
    # Every document starts with the key sign_in set (window.name survives
    # navigation within the tab, unlike a fresh sessionStorage write racing
    # the app's first read).
    ctx.add_init_script("""(() => {
        const k = window.name.startsWith('lux-key:') ? window.name.slice(8) : '';
        if (k) sessionStorage.setItem('lux.key', k); else sessionStorage.removeItem('lux.key');
    })()""")
    pg = ctx.new_page()
    pg.errors = []
    pg.on("pageerror", lambda e: pg.errors.append(str(e)))
    # The console first asks whoami without a key (is luxd behind Cloudflare
    # Access?): its 401 is expected, and browsers log it.
    pg.on("console", lambda m: m.type == "error" and "401" not in m.text and pg.errors.append(m.text))

    def sign_in(key: str, path: str = "/"):
        pg.evaluate("k => { window.name = 'lux-key:' + k }", key)
        pg.goto(env.luxd_url + path)
    pg.sign_in = sign_in
    yield pg
    ctx.close()


VIEWPORT = {"width": 1400, "height": 900}


@pytest.fixture
def page(_console_page, env):
    """The file's page, reset for a test: signed out, default size and theme,
    no errors carried over. Sign in with page.sign_in(key, path)."""
    pg = _console_page
    pg.set_viewport_size(VIEWPORT)
    pg.errors.clear()
    yield pg
    # Signed out and on a blank page before the next test: an open page
    # keeps its live stream to luxd (which a test may restart), and local
    # settings (theme, rail) would carry over.
    if pg.url.startswith(env.luxd_url):
        pg.evaluate("() => localStorage.clear()")
    pg.evaluate("() => { window.name = '' }")
    pg.goto("about:blank")


def test_console_is_served_with_client_routing(env):
    import requests
    for path in ("/", "/runs/whatever", "/hosts"):
        r = requests.get(env.luxd_url + path, timeout=10)
        assert r.status_code == 200 and "<div id=\"root\"" in r.text.replace("'", '"'), path
    # The API keeps its own errors: never the console.
    r = requests.get(env.luxd_url + "/v1/nope", timeout=10)
    assert r.status_code == 404 and "root" not in r.text, r.text
    # Old links redirect to the same page at the root.
    for old, new in (("/console", "/"), ("/console/runs/x?tenant=t", "/runs/x?tenant=t")):
        r = requests.get(env.luxd_url + old, timeout=10, allow_redirects=False)
        assert r.status_code == 301 and r.headers["Location"] == new, (old, r.status_code, r.headers)


def _assert_unclipped(tip):
    """The tooltip is drawn whole: inside the viewport, inside every ancestor
    that clips its overflow, and on top at its centre and corners."""
    hidden = tip.evaluate("""t => {
        const r = t.getBoundingClientRect();
        const vw = document.documentElement.clientWidth, vh = document.documentElement.clientHeight;
        if (r.left < 0 || r.top < 0 || r.right > vw || r.bottom > vh) return 'viewport';
        for (let e = t.parentElement; e; e = e.parentElement) {
            if (getComputedStyle(e).overflow === 'visible') continue;
            const c = e.getBoundingClientRect();
            if (r.left < c.left || r.right > c.right || r.top < c.top || r.bottom > c.bottom) return e.tagName + '.' + e.className;
        }
        // A tooltip ignores the pointer; hit-test it for a moment. Corners
        // are probed inside its border radius, which hit-testing honours.
        t.style.pointerEvents = 'auto';
        try {
            const i = 4, pts = [[(r.left + r.right) / 2, (r.top + r.bottom) / 2], [r.left + i, r.top + i], [r.right - i, r.top + i], [r.left + i, r.bottom - i], [r.right - i, r.bottom - i]];
            for (const [x, y] of pts) {
                const hit = document.elementFromPoint(x, y);
                if (!hit || !t.contains(hit)) return `covered at ${x},${y} by ${hit && hit.tagName + '.' + hit.className}`;
            }
        } finally { t.style.pointerEvents = ''; }
        return null;
    }""")
    assert hidden is None, hidden


def _cores(n: float) -> str:
    """The design system's formatCores: 0, 100m, 1 core, 2.5 cores."""

    def trim(s: str) -> str:
        return re.sub(r"\.?0+$", "", s) if "." in s else s

    if n == 0:
        return "0"
    if abs(n * 1000) < 10:
        return f"{trim(f'{n * 1000:.1f}')}m"
    if abs(n) < 1:
        return f"{round(n * 1000)}m"
    return f"{trim(f'{n:.2f}')} {'core' if abs(n) == 1 else 'cores'}"


def _parked(lux, name: str) -> str:
    """A Run that waits for a host that will never come: it stays listed."""
    return lux.submit(generic(ALPINE_IMAGE, "true", name=name, placement={"requires": {"nowhere": "yes"}}))


def test_sign_in_and_see_every_tenant(page, env, operator, tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    na, nb = f"console-{a.tenant_id[-6:]}", f"console-{b.tenant_id[-6:]}"
    ra, rb = _parked(a, na), _parked(b, nb)
    # Without a key: the sign-in screen.
    page.goto(env.luxd_url + "/")
    page.get_by_placeholder("lux_", exact=False).wait_for(timeout=10_000)
    page.sign_in(operator.api_key, "/runs")
    page.get_by_text(na, exact=True).wait_for(timeout=15_000)
    page.get_by_text(nb, exact=True).wait_for(timeout=15_000)
    # Narrowed to one tenant through the URL, as the tenant picker does.
    page.goto(env.luxd_url + f"/runs?tenant={a.tenant_id}")
    page.get_by_text(na, exact=True).wait_for(timeout=15_000)
    assert page.get_by_text(nb, exact=True).count() == 0
    assert not page.errors, page.errors
    a.run("terminate", ra)
    b.run("terminate", rb)


def test_a_tenant_key_sees_only_its_own(page, tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    na, nb = f"mine-{a.tenant_id[-6:]}", f"theirs-{b.tenant_id[-6:]}"
    ra, rb = _parked(a, na), _parked(b, nb)
    page.sign_in(a.api_key, "/runs")
    page.get_by_text(na, exact=True).wait_for(timeout=15_000)
    assert page.get_by_text(nb, exact=True).count() == 0
    # No tenants page for a tenant.
    assert page.get_by_role("link", name="Tenants").count() == 0
    a.run("terminate", ra)
    b.run("terminate", rb)


def test_run_page_streams_output_and_stops_the_run(page, operator, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo console-hello; sleep 300"))
    lux.wait_output(run_id, "console-hello")
    page.sign_in(operator.api_key, f"/runs/{run_id}")
    page.get_by_text("console-hello").first.wait_for(timeout=20_000)
    page.get_by_role("button", name="Stop", exact=True).click()
    page.get_by_role("button", name="Stop run", exact=True).click()
    wait_until(lambda: lux.get(run_id)["state"] == "stopped", 60, 0.5, "the console's stop never took effect")
    assert not page.errors, page.errors
    lux.run("terminate", run_id)


def test_run_output_tabs_separate_the_workloads_lines_from_luxs(page, env, operator, lux, runners, hosts):
    runners.start(hosts[0])
    # A generic workload's steer arrives on stdin: the later line comes once the Output tab is up.
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "echo tabs-err >&2; echo tabs-out; read l; echo \"tabs-$l\"; sleep 300"))
    lux.wait_output(run_id, "tabs-out")
    page.sign_in(operator.api_key, f"/runs/{run_id}")
    log = page.locator(".logview")
    tabs = page.locator(".tabs-sm")

    def tab(name: str):
        return tabs.get_by_role("tab", name=re.compile(rf"^{name}\b"))

    def texts(cls: str) -> list[str]:
        return log.locator(f".logline-{cls} .logline-text").all_inner_texts()

    # All (the default): the workload's lines and Lux's own, interleaved.
    expect(log.get_by_text("tabs-out", exact=True)).to_have_count(1, timeout=20_000)
    expect(log.get_by_text("tabs-err", exact=True)).to_have_count(1)
    expect(log.locator(".logline-system").first).to_contain_text("lux: ", timeout=10_000)
    expect(tab("All")).to_have_attribute("aria-selected", "true")
    assert "output=" not in page.url, page.url

    # Each tab counts its lines: All is Output plus Lux.
    def counts_add_up() -> bool:
        all_count, output_count, lux_count = (
            int(tab(name).locator(".tab-count").inner_text()) for name in ("All", "Output", "Lux")
        )
        return all_count == output_count + lux_count and output_count == 2 and lux_count >= 1
    wait_until(counts_add_up, 10, 0.5, "the tab counts do not add up")

    # Output: only stdout and stderr, numbered from 1.
    tab("Output").click()
    expect(tab("Output")).to_have_attribute("aria-selected", "true")
    expect(page).to_have_url(re.compile(r"[?&]output=output\b"))
    expect(log.locator(".logline-system")).to_have_count(0)
    assert texts("stdout") == ["tabs-out"] and texts("stderr") == ["tabs-err"], (texts("stdout"), texts("stderr"))
    expect(log.locator(".logline-no").first).to_have_text("1")
    expect(log.locator(".logline")).to_have_count(2)

    # A line written after the switch streams into the Output tab as it stands.
    lux.run("steer", run_id, "later")
    expect(log.get_by_text("tabs-later", exact=True)).to_have_count(1, timeout=20_000)
    expect(log.locator(".logline")).to_have_count(3)
    expect(log.locator(".logline-system")).to_have_count(0)

    # Lux: only Lux's lines.
    tab("Lux").click()
    expect(page).to_have_url(re.compile(r"[?&]output=lux\b"))
    expect(log.locator(".logline-stdout, .logline-stderr")).to_have_count(0)
    assert texts("system") and all(t.startswith(("lux: ", "[", "---")) for t in texts("system")), texts("system")

    # The choice round-trips through the URL; All drops the parameter.
    page.goto(env.luxd_url + f"/runs/{run_id}?output=output")
    expect(tab("Output")).to_have_attribute("aria-selected", "true", timeout=15_000)
    expect(log.get_by_text("tabs-out", exact=True)).to_have_count(1, timeout=20_000)
    expect(log.locator(".logline-system")).to_have_count(0)
    tab("All").click()
    expect(log.locator(".logline-system").first).to_be_visible()
    assert "output=" not in page.url, page.url
    assert not page.errors, page.errors
    lux.run("terminate", run_id)


def test_hosts_live_runs_shows_the_count_and_the_cap_only_near_it(page, lux, runners, hosts):
    runners.start(hosts[0], "--max-runs", "4")
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == hosts[0].name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    page.sign_in(lux.api_key, "/hosts")
    # The header's live summary, from GET /v1/hosts/summary (the same figures).
    summary = lux.api("/v1/hosts/summary").json()
    rows = lux.api("/v1/hosts").json()["hosts"]
    up = [h for h in rows if h["state"] in ("ready", "draining")]
    assert summary["live"] == len(rows), (summary, rows)
    for part in ("capacity", "allocated"):
        assert summary[part]["memory"] == sum(h[part].get("memory", 0) for h in up), (part, summary, rows)
        assert summary[part]["cpus"] == pytest.approx(sum(h[part].get("cpus", 0) for h in up)), (part, summary, rows)
    alloc, cap = summary["allocated"]["cpus"], summary["capacity"]["cpus"]
    assert summary["live"] >= 1 and cap >= 1, summary
    expect(page.get_by_text(re.compile(rf"\b{summary['live']} live · ready and draining: {re.escape(_cores(alloc))} of {re.escape(_cores(cap))} CPU, .+ memory allocated"))).to_be_visible(timeout=15_000)
    row = page.get_by_role("row").filter(has=page.locator(f'a[href^="/hosts/{host_id}"]'))
    link = row.locator(f'a[href^="/runs?host={host_id}"]')
    cell = row.locator("td", has=page.locator(f'a[href^="/runs?host={host_id}"]'))
    # No Runs: just the count, and the cap on hover.
    expect(cell).to_have_text(re.compile(r"^\s*0\s*$"), timeout=15_000)
    expect(cell.locator(".badge-warn")).to_have_count(0)
    link.hover()
    tip = page.get_by_role("tooltip")
    expect(tip).to_have_text("0 live · max 4 runs on this host")
    _assert_unclipped(tip)
    page.mouse.move(0, 0)
    # Half the cap is still just the count; three quarters is "N / M" in the warn tone.
    # Small reservations: the cap, not CPU or memory, is what fills first.
    small = {"cpus": 0.1, "memory": 64 * 1024 * 1024}
    runs = [lux.submit(generic(ALPINE_IMAGE, "sleep", "300", resources=small)) for _ in range(2)]
    for r in runs:
        lux.wait_state(r, "running")
    expect(cell).to_have_text(re.compile(r"^\s*2\s*$"), timeout=15_000)
    expect(cell.locator(".badge-warn")).to_have_count(0)
    runs.append(lux.submit(generic(ALPINE_IMAGE, "sleep", "300", resources=small)))
    lux.wait_state(runs[-1], "running")
    expect(cell.locator(".badge-warn")).to_have_text("3 / 4", timeout=15_000)
    assert not page.errors, page.errors
    for r in runs:
        lux.run("terminate", r)


def test_pages_update_live_from_events(page, operator, tenant_factory):
    """A new Run shows up on the Runs page within a moment, pushed by its
    event: while the stream is live, the page's own poll is a minute."""
    a = tenant_factory()
    page.sign_in(operator.api_key, "/runs")
    page.get_by_text("live", exact=True).first.wait_for(timeout=15_000)
    name = f"pushed-{a.tenant_id[-6:]}"
    run_id = _parked(a, name)
    page.get_by_text(name, exact=True).wait_for(timeout=5_000)
    assert not page.errors, page.errors
    a.run("terminate", run_id)


def test_host_page_charts_its_runner_process(page, operator, lux, runners, hosts):
    runners.start(hosts[0])
    # By the runners' tenant: other tests register their own host-a under
    # theirs, which stay listed (ready, then lost) after they stop.
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == hosts[0].name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    # A heartbeat has carried the runner's process before the page loads
    # (the page reads history every 30s).
    wait_until(lambda: any("runner" in s for s in operator.json("history", "--host", host_id, "--since", "5m")["samples"]),
               60, 1, "no runner sample")
    page.sign_in(operator.api_key, f"/hosts/{host_id}?tab=metrics&range=1h")
    for card in ("Runner CPU", "Runner memory", "Runner goroutines"):
        expect(page.get_by_role("heading", name=card, exact=True)).to_have_count(1, timeout=15_000)
    expect(page.get_by_text(re.compile(r"^the lux-runner process, not podman or its containers · (\d+ processes, a line each.* · the newest )?started "))).to_have_count(1, timeout=15_000)
    assert not page.errors, page.errors


def test_control_host_row_is_the_operators_whole_system_view(page, env, operator, tenant_factory):
    a = tenant_factory()
    # An hour's range reads raw samples; the default day's reads minute
    # rollups, which a freshly started luxd may not have written yet.
    page.sign_in(operator.api_key, "/?range=1h")
    expect(page.get_by_role("heading", name="Control host", exact=True)).to_have_count(1, timeout=15_000)
    expect(page.get_by_role("heading", name="Disk /", exact=True)).to_have_count(1, timeout=45_000)
    expect(page.get_by_role("heading", name="Postgres size", exact=True)).to_have_count(1, timeout=5_000)
    # The subtitle carries the latest sampled size once history has loaded.
    expect(page.get_by_text(re.compile(r"^lux's database · \d"))).to_have_count(1, timeout=5_000)
    # luxd's own process (a line per process, if a test restarted luxd), with when it started.
    for card in ("luxd CPU", "luxd memory", "luxd goroutines"):
        expect(page.get_by_role("heading", name=card, exact=True)).to_have_count(1, timeout=5_000)
    expect(page.get_by_text(re.compile(r"^the luxd process · (\d+ processes, a line each.* · the newest )?started "))).to_have_count(1, timeout=5_000)
    # Narrowed to a tenant, the row is gone.
    page.goto(env.luxd_url + f"/?tenant={a.tenant_id}")
    page.get_by_text(re.compile(rf"^tenant {re.escape(a.tenant_id)} · charts over")).wait_for(timeout=15_000)
    # Anchor on Activity, where Control host would render: the absences below are not vacuous.
    expect(page.get_by_role("heading", name="Trends", exact=True)).to_have_count(1)
    expect(page.get_by_role("heading", name="Control host", exact=True)).to_have_count(0)
    expect(page.get_by_role("heading", name="Postgres size", exact=True)).to_have_count(0)
    expect(page.get_by_role("heading", name="luxd CPU", exact=True)).to_have_count(0)
    assert not page.errors, page.errors


def test_a_tenant_key_overview_has_no_control_host(page, tenant_factory):
    a = tenant_factory()
    page.sign_in(a.api_key, "/")
    page.get_by_text(re.compile(r"^your runs and hosts · charts over")).wait_for(timeout=15_000)
    expect(page.get_by_role("heading", name="Trends", exact=True)).to_have_count(1)
    expect(page.get_by_role("heading", name="Control host", exact=True)).to_have_count(0)
    expect(page.get_by_role("heading", name="Postgres size", exact=True)).to_have_count(0)
    assert not page.errors, page.errors


def _priced_host(lux, runners, host, price: str = "4") -> str:
    """A ready static host of the tenant's, priced before any Run is placed
    on it, so its Runs' compute lines have a rate from their start. A few
    seconds of a small share of it must still show as whole cents, not
    "<$0.0001", so the price is high."""
    runners.start(host)
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == host.name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    lux.run("hosts", "price", host_id, "--hourly-price", price, "--currency", "USD")
    return host_id


def _costed_run(lux, text: str, name: str = "", labels: dict | None = None) -> str:
    """A Run that ends on its own, with its compute line written: the line
    comes at a state change (or the 2m tick), and the end is one."""
    extra = {"name": name} if name else {}
    if labels:
        extra["labels"] = labels
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", f"echo {text}; sleep 3", **extra))
    lux.wait_state(run_id, "succeeded")
    wait_until(lambda: lux.api(f"/v1/runs/{run_id}/cost").json()["totals"], 60, 1, "no compute line was written")
    return run_id


def test_runs_list_shows_runtime_and_placements(page, env, lux, runners, hosts):
    runners.start(hosts[0])
    name = f"runtime-{lux.tenant_id[-6:]}"
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", "sleep 2", name=name))
    lux.wait_state(run_id, "succeeded")
    listed = next(r for r in lux.json("ls") if r["id"] == run_id)
    assert listed["runtimeSeconds"] >= 1 and "runtimeSince" not in listed, listed
    never_name = f"runtime-never-{lux.tenant_id[-6:]}"
    never = _parked(lux, never_name)
    # Wide enough that the optional Placements column is shown.
    page.sign_in(lux.api_key, "/runs")
    page.set_viewport_size({"width": 1800, "height": 900})
    headers = page.locator("table thead th")
    runtime = page.get_by_role("columnheader", name="Runtime", exact=True)
    expect(runtime).to_have_count(1, timeout=15_000)
    placements = page.get_by_role("columnheader", name="Placements", exact=True)
    expect(placements).to_have_count(1)
    expect(placements.locator("[title]")).to_have_attribute("title", "Times this Run has been placed on a host")
    placement_time = page.get_by_role("columnheader", name="Placement time", exact=True)
    expect(placement_time).to_have_count(1)
    expect(page.get_by_role("columnheader", name="Epoch", exact=True)).to_have_count(0)
    # Every column sorts, on the server; the list opens newest first.
    for h in (runtime, placements, placement_time):
        expect(h).to_have_attribute("aria-sort", "none")
    expect(page.get_by_role("columnheader", name="Created", exact=True)).to_have_attribute("aria-sort", "descending")
    # The sort arrow is aria-hidden: a header's text is its name and the arrow.
    texts = [re.sub(r"\s*[↑↓↕]$", "", t.strip()) for t in headers.all_inner_texts()]
    runtime_col, placements_col, placement_time_col = texts.index("Runtime"), texts.index("Placements"), texts.index("Placement time")

    def cell(run_name, col):
        row = page.get_by_role("row").filter(has=page.get_by_role("link", name=run_name, exact=True))
        return row.locator("td").nth(col)
    expect(cell(name, runtime_col)).to_have_text(re.compile(r"^\d+(\.\d+)?(ms|s)$|^\d+[mhd]( \d+[smh])?$"))
    expect(cell(name, placements_col)).to_have_text("1")
    expect(cell(name, placement_time_col)).to_have_text(re.compile(r"^\d+(\.\d+)?(ms|s)$|^\d+[mhd]( \d+[smh])?$"))
    # Never placed: no runtime, no placement; it is still waiting for one.
    expect(cell(never_name, runtime_col)).to_have_text("–")
    expect(cell(never_name, placements_col)).to_have_text("0")
    expect(cell(never_name, placement_time_col)).to_have_text(re.compile(r"…$"))
    # Sorting by Runtime asks the server and marks the header.
    runtime.click()
    expect(runtime).to_have_attribute("aria-sort", "descending")
    expect(page.get_by_text("Runtime, largest first")).to_have_count(1, timeout=15_000)
    assert not page.errors, page.errors
    lux.run("terminate", never)


def _both_themes(page, env, path: str, check):
    """Run `check` on `path` in the light and then the dark theme."""
    for theme in ("light", "dark"):
        page.evaluate(f"localStorage.setItem('lux.theme', {theme!r})")
        page.goto(env.luxd_url + path)
        check(theme)
        assert not page.errors, (theme, page.errors)


def test_run_cost_card_shows_the_total_and_list_price(page, env, lux, runners, hosts):
    _priced_host(lux, runners, hosts[0])
    run_id = _costed_run(lux, "costed")
    total = lux.api(f"/v1/runs/{run_id}/cost").json()["totals"][0]
    assert total["currency"] == "USD" and float(total["amount"]) > 0, total
    page.sign_in(lux.api_key, f"/runs/{run_id}")

    def check(theme):
        page.get_by_role("tab", name="Resources & cost").click()
        card = page.locator("section.card.run-cost")
        expect(card.get_by_role("heading", name="Cost", exact=True)).to_have_count(1, timeout=15_000)
        # A dollar total, the status badge, a Compute family row and the item line.
        expect(card.locator(".money-list-lg")).to_have_text(re.compile(r"^\$\d"), timeout=15_000)
        expect(card.locator("[data-cost-status]")).to_have_count(1)
        expect(card.get_by_text("Compute", exact=True).first).to_be_visible()
        expect(card.get_by_role("cell", name="static", exact=True)).to_have_count(1)
        # "list price", explained on hover, inside the viewport at the card's right edge.
        card.locator(".list-price").hover()
        tip = page.get_by_role("tooltip")
        expect(tip).to_have_text(re.compile("list prices", re.I))
        box, width = tip.bounding_box(), page.viewport_size["width"]
        assert box["x"] >= 0 and box["x"] + box["width"] <= width, (box, width)
    _both_themes(page, env, f"/runs/{run_id}", check)


def test_a_pending_run_shows_no_cost_yet_not_zero(page, env, lux):
    run_id = _parked(lux, f"pending-cost-{lux.tenant_id[-6:]}")
    assert lux.api(f"/v1/runs/{run_id}/cost").json()["status"] == "pending"
    page.sign_in(lux.api_key, f"/runs/{run_id}")

    def check(theme):
        page.get_by_role("tab", name="Resources & cost").click()
        card = page.locator("section.card.run-cost")
        expect(card.get_by_text("No cost reported yet", exact=True)).to_have_count(1, timeout=15_000)
        expect(card.get_by_text(re.compile(r"\$0(\.0+)?\b"))).to_have_count(0)
        expect(card.locator(".money-list-lg")).to_have_count(0)
    _both_themes(page, env, f"/runs/{run_id}", check)
    lux.run("terminate", run_id)


def _dollars(amount: str, exact: bool = False) -> str:
    """formatMoney's figure for USD: half-even to 4 decimals, trimmed down
    to cents, a bound for a non-zero amount below that. exact: every digit,
    as formatMoneyExact."""
    d = Decimal(amount)
    q = d if exact else d.quantize(Decimal("0.0001"), rounding=ROUND_HALF_EVEN)
    if q == 0 and d != 0:
        return "<$0.0001" if d > 0 else ">-$0.0001"
    whole, _, frac = f"{abs(q):f}".partition(".")
    return f"{'-' if q < 0 else ''}${int(whole):,}.{frac.rstrip('0').ljust(2, '0')}"


def _listed_cost(lux, run_id: str) -> dict:
    return next(r for r in lux.json("ls") if r["id"] == run_id)["cost"]


def _cost_cell(cost: dict) -> str:
    """The Runs list's Cost cell for a one-currency USD cost, as lux ls has it."""
    total = cost["totals"][0]
    may_change = cost["status"] == "incomplete" or Decimal(total["estimate"]) != 0
    return ("~" if may_change else "") + _dollars(total["amount"])


def test_runs_list_cost_column_matches_the_run_page(page, env, lux, operator, runners, hosts):
    _priced_host(lux, runners, hosts[0], price="36")
    name = f"listed-cost-{lux.tenant_id[-6:]}"
    run_id = _costed_run(lux, "listed-cost", name=name)
    pending_name = f"listed-pending-{lux.tenant_id[-6:]}"
    pending = _parked(lux, pending_name)
    listed = _listed_cost(lux, run_id)
    assert listed["status"] != "pending" and len(listed["totals"]) == 1, listed
    total = listed["totals"][0]
    assert total["currency"] == "USD" and Decimal(total["amount"]) > 0, total
    # The Run has ended: its amount no longer changes (its status may still settle).
    shown = _dollars(total["amount"])
    assert not shown.startswith("<"), shown
    page.sign_in(lux.api_key, "/runs")

    def check(theme):
        row = page.get_by_role("row").filter(has=page.get_by_role("link", name=name, exact=True))
        cell = row.locator("[data-cost-figure]")
        expect(cell).to_have_text(re.compile(rf"^~?{re.escape(shown)}$"), timeout=15_000)
        # The ~ follows the status and estimate part luxd has for the Run now.
        wait_until(lambda: cell.inner_text() == _cost_cell(_listed_cost(lux, run_id)), 20, 1,
                   "the Cost cell's ~ does not match the Run's status")
        # Pending is a dash, never $0.
        pending_row = page.get_by_role("row").filter(has=page.get_by_role("link", name=pending_name, exact=True))
        expect(pending_row.locator("[data-cost-figure]")).to_have_text("–")
        expect(pending_row.get_by_text(re.compile(r"\$"))).to_have_count(0)
        # The status in words and the exact amount, on hover.
        cell.hover()
        tip = page.get_by_role("tooltip")
        expect(tip).to_contain_text(re.compile(r"^(Final|Estimate|Incomplete) · "))
        expect(tip).to_contain_text(_dollars(total["amount"], exact=True))
        _assert_unclipped(tip)
    _both_themes(page, env, "/runs", check)

    # The Run page shows the same figure.
    page.goto(env.luxd_url + f"/runs/{run_id}")
    page.get_by_role("tab", name="Resources & cost").click()
    expect(page.locator("section.card.run-cost .money-list-lg")).to_have_text(shown, timeout=15_000)
    assert not page.errors, page.errors

    # Beside the Cost column the State pill reads whole (a reason may still
    # ellipsize): at 1200px the table is under its 1100px breakpoint; at 1400px
    # it is just over it, where every column left State under 100px. For a
    # tenant, and for an operator, who also has a Tenant column.
    for key in (lux.api_key, operator.api_key):
        page.sign_in(key, "/runs")
        for width in (1200, 1300, 1400, 1500):
            page.set_viewport_size({"width": width, "height": 900})
            page.goto(env.luxd_url + "/runs")
            row = page.get_by_role("row").filter(has=page.get_by_role("link", name=name, exact=True))
            expect(row.locator(".pill")).to_have_text("Succeeded", timeout=15_000)
            state = row.locator("td", has=page.locator(".pill"))
            overflow = state.evaluate("""td => [td, ...td.querySelectorAll('*')]
                .filter(e => !e.closest('.state-reason') && getComputedStyle(e).display !== 'inline')
                .filter(e => e.scrollWidth > e.clientWidth)
                .map(e => `${e.tagName}.${e.className} ${e.scrollWidth}>${e.clientWidth}`)""")
            assert not overflow, (width, overflow)
    assert not page.errors, page.errors
    lux.run("terminate", pending)


@pytest.fixture
def cost_plugin(env):
    """luxd restarted with a fake cost plugin configured, then back."""
    plugin = FakeCostPlugin(env.gateway)
    env.stop_luxd()
    env.start_luxd(LUX_COSTS_PLUGINS=plugin.config(), LUX_COSTS_EVERY="30s")
    yield plugin
    env.stop_luxd()
    env.start_luxd()
    plugin.close()


def _cents(amount) -> str:
    return f"${Decimal(amount).quantize(Decimal('0.01'), ROUND_HALF_EVEN)}"


def _legend_label(scope, label):
    """A chart legend entry's label, by its exact text (the entry also holds its total)."""
    return scope.locator(".tschart-legend-label", has_text=re.compile(rf"^{re.escape(label)}$"))


def _legend_item(legend, label):
    # has= is matched inside each item, so it starts from the page, not from legend.
    return legend.locator(".tschart-legend-item", has=_legend_label(legend.page, label))


def _ui_matches_api(page, locator, api_cents, what):
    """The UI's figure equals the API's at cents, both read again until they
    agree: the fake plugin's open AI line grows between two reads. A figure
    not rendered yet after the reload is a retry, not a failure."""
    def same():
        page.reload()
        try:
            ui = locator.inner_text(timeout=15_000)
        except PlaywrightTimeoutError:
            return False
        return ui == api_cents()
    wait_until(same, 120, 2, f"{what}: the panel never matched the API")


def test_overview_cost_panel(page, env, lux, runners, hosts, cost_plugin):
    """The Cost panel: KPIs over stacked bars by family, Top Runs with their
    labels, By family; then Show External, Every Day, Break down by the app
    label, and a label filter, each in the URL and in the figures."""
    _priced_host(lux, runners, hosts[0])
    name = f"overview-cost-{lux.tenant_id[-6:]}"
    run_id = _costed_run(lux, "overview-cost", name=name, labels={"app": "e2e-app", "phase": "check"})
    # The summary reads cost_hourly, written with each source's lines.
    wait_until(lambda: {r["group"]["family"] for r in lux.api("/v1/costs?since=24h&group=family&interval=hour").json().get("series", [])} >= {"compute", "ai"},
               90, 1, "no hourly cost for both families")
    panel = page.locator(".cost-panel")
    card = panel.locator("section.card.cost-card")

    def check(theme):
        expect(card.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
        # Families read as on the Run page: the describe's displayName, not the key.
        # A legend entry is its label, then its range total: the label has its own element.
        legend = card.locator(".tschart-legend")
        expect(_legend_label(legend, "Compute")).to_have_count(1)
        expect(_legend_label(legend, "AI models")).to_have_count(1)
        expect(_legend_label(legend, "ai")).to_have_count(0)
        expect(legend.get_by_role("button", name=re.compile(r"^Compute\s*<?\$\d"))).to_have_count(1)
        # Each family has its own colour: Compute's swatch is not AI models'.
        swatch = lambda label: _legend_item(legend, label).locator(".tschart-key").evaluate("e => getComputedStyle(e).backgroundColor")
        compute, ai = swatch("Compute"), swatch("AI models")
        assert compute != ai and "0, 0, 0, 0" not in compute + ai, (theme, compute, ai)
        # The KPI strip: a dollar total, Compute, Block storage and External, the peak hour.
        expect(card.locator(".kpi").first.locator(".money-list-lg")).to_have_text(re.compile(r"^\$\d"))
        expect(card.locator(".kpi-label")).to_contain_text(["Total", "Compute", "Block storage", "External", "Peak hour"])
        # Top Runs leads with the Run's name, its id under it, its labels as chips.
        top = panel.locator("section.card", has=page.get_by_role("heading", name="Top Runs", exact=True))
        row = top.get_by_role("row").filter(has=page.get_by_role("link", name=name, exact=True))
        expect(row).to_have_count(1, timeout=15_000)
        expect(row.get_by_text(run_id)).to_have_count(1)
        expect(row.locator(".label-chip").first).to_have_text("app=e2e-app")
        expect(row.get_by_text(re.compile(r"^\$\d"))).to_have_count(1)
        expect(panel.get_by_role("heading", name="By family", exact=True)).to_have_count(1)
        # A tenant: no Tenant breakdown, no tenant table.
        expect(panel.get_by_role("radio", name="Tenant", exact=True)).to_have_count(0)
        expect(page.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(0)
        # Its host is in its own pool (a tenant's host token joins its default pool), so the
        # host time no Run reserved is its own: compute, and a dash for block storage (a static host has no disk).
        unallocated = panel.locator(".cost-unallocated")
        expect(unallocated.locator('[data-unallocated="compute"]')).to_have_text(re.compile(r"^Compute <?\$\d"), timeout=15_000)
        expect(unallocated.locator('[data-unallocated="block-storage"]')).to_have_text("Block storage –")
    # The host-hour rows come from their own refresh, after the Run's lines.
    wait_until(lambda: lux.api("/v1/costs?since=24h").json().get("unallocated"), 120, 2, "no unallocated host time for the tenant's own host")
    page.sign_in(lux.api_key, "/?tab=cost")
    _both_themes(page, env, "/?tab=cost", check)

    # Show External: the Compute KPI is muted, the chart has no Compute series.
    page.goto(env.luxd_url + "/?tab=cost")
    expect(card.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
    panel.get_by_role("radio", name="External", exact=True).click()
    expect(page).to_have_url(re.compile(r"[?&]cost=external(&|$)"))
    expect(card.locator(".kpi.is-muted")).to_contain_text("Compute")
    expect(_legend_label(card.locator(".tschart-legend"), "Compute")).to_have_count(0)
    expect(_legend_label(card.locator(".tschart-legend"), "AI models")).to_have_count(1)

    # Every Day: the top bar's step, carried on links; the panel and the trends are daily.
    page.goto(env.luxd_url + "/?tab=cost&range=7d&cost=external")
    page.locator(".topbar-wide .step-picker .select-trigger").click()
    page.get_by_role("option", name=re.compile(r"^Day")).click()
    expect(page).to_have_url(re.compile(r"[?&]every=day(&|$)"))
    expect(panel.locator(".section-note")).to_contain_text("per day")
    expect(card.locator(".kpi-label")).to_contain_text(["Peak day"])
    page.get_by_role("tab", name="Activity", exact=True).click()
    expect(page.get_by_role("tab", name="Activity", exact=True)).to_have_attribute("aria-selected", "true")
    expect(page).to_have_url(re.compile(r"[?&]every=day(&|$)"))
    expect(page.locator(".section-head", has_text="Trends")).to_contain_text("per day")
    expect(page.get_by_role("link", name="Runs", exact=True).first).to_have_attribute("href", re.compile(r"every=day"))
    # Day is too coarse for 24h: the menu says so and does not take it.
    page.goto(env.luxd_url + "/?every=day")
    expect(page.locator(".topbar-wide .step-picker .select-value")).to_have_text(re.compile(r"^Auto"))
    page.locator(".topbar-wide .step-picker .select-trigger").click()
    day = page.get_by_role("option", name=re.compile(r"^Day"))
    expect(day).to_have_attribute("aria-disabled", "true")
    expect(day).to_contain_text("too coarse")
    page.keyboard.press("Escape")

    # Break down by Label app: the chart stacks by app, the costed Run's app leads the table.
    page.goto(env.luxd_url + "/?tab=cost")
    expect(card.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
    panel.get_by_role("radio", name="Label", exact=True).click()
    expect(page).to_have_url(re.compile(r"[?&]by=label%3Aapp(&|$)"))
    by_app = panel.locator("section.card", has=page.get_by_role("heading", name="By app", exact=True))
    app_row = by_app.get_by_role("row").filter(has_text="e2e-app")
    expect(app_row).to_have_count(1, timeout=15_000)
    expect(_legend_label(card.locator(".tschart-legend"), "e2e-app")).to_have_count(1)

    def app_usd():
        totals = lux.api("/v1/costs?since=24h&group=label:app").json()["totals"]
        return _cents(next(r["amount"] for r in totals if r["group"]["label:app"] == "e2e-app" and r["currency"] == "USD"))
    # Columns: app, Runs, Compute, Block storage, External, Total, Share.
    _ui_matches_api(page, app_row.locator("td").nth(5), app_usd, "By app e2e-app Total")

    # A row is a filter: app = e2e-app, in the URL and on every figure.
    app_row.click()
    expect(page).to_have_url(re.compile(r"[?&]label=app%3De2e-app(&|$)"))
    expect(panel.locator("[data-filter-chip=app]")).to_contain_text("e2e-app")
    expect(card.locator(".kpi-label").first).to_contain_text("filtered")

    def filtered_usd():
        totals = lux.api("/v1/costs?since=24h&group=family&label=app%3De2e-app").json()["totals"]
        return _cents(sum(Decimal(r["amount"]) for r in totals if r["currency"] == "USD"))
    _ui_matches_api(page, card.locator(".kpi").first.locator(".money-list-lg"), filtered_usd, "filtered Total")
    # All Runs → the Runs list filtered by the same label.
    expect(panel.get_by_role("link", name="All Runs →")).to_have_attribute("href", re.compile(r"^/runs\?label=app%3De2e-app"))
    # A second filter from the picker: with app filtered, the picker opens on the only key left, phase.
    panel.locator(".filter-add").click()
    pop = panel.locator(".label-filter-pop")
    expect(pop.locator(".select-value")).to_have_text("phase")
    pop.locator(".label-filter-value", has_text="check").click()
    pop.get_by_role("button", name="Apply").click()
    expect(page).to_have_url(re.compile(r"[?&]label=phase%3Dcheck(&|$)"))
    expect(panel.locator("[data-filter-chip=phase]")).to_contain_text("check")
    expect(panel.locator(".filter-bar-note")).to_contain_text("not per-label")
    assert not page.errors, page.errors


MIB = 1024 * 1024
HISTORY = re.compile(r"/v1/history(\?|$)")


def test_overview_stored_chart_stacks_bytes_by_kind(page, env, lux):
    """The Stored card stacks the bytes in S3 by kind: the tooltip lists each
    kind's bytes and their Total, and a kind hidden from the legend leaves
    the Total. storedContext is sampled but not charted: no legend entry, no
    tooltip row, not in the Total."""
    now = time.time()
    at = lambda s: time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - s))
    body = {"from": at(600), "to": at(0), "resolution": 0, "samples": [
        {"at": at(120), "storedVolume": MIB, "storedOutput": 0, "storedArtifact": 0, "storedContext": 0},
        {"at": at(60), "storedVolume": 2 * MIB, "storedOutput": MIB, "storedArtifact": MIB, "storedContext": 3 * MIB},
    ]}
    page.route(HISTORY, lambda route: route.fulfill(status=200, content_type="application/json", body=json.dumps(body)))
    try:
        page.sign_in(lux.api_key, "/?tab=storage")
        card = page.locator("section.card", has=page.get_by_role("heading", name="Stored", exact=True))
        expect(card.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
        expect(card.locator(".tschart-legend").get_by_role("button")).to_have_text(["Snapshots", "Output", "Artifacts"])
        tip = card.locator(".tschart-tip")

        def hover_last():
            # The last sample is the plot's right edge; uPlot snaps the cursor to it.
            over = card.locator(".u-over")
            box = over.bounding_box()
            over.hover(position={"x": box["width"] - 2, "y": box["height"] / 2})
            expect(tip).to_have_count(1)

        hover_last()
        expect(tip.locator(".tschart-tip-label")).to_have_text(["Snapshots", "Output", "Artifacts", "Total"])
        expect(tip.locator(".tschart-tip-value")).to_have_text(["2 MiB", "1 MiB", "1 MiB", "4 MiB"])

        card.locator(".tschart-legend").get_by_role("button", name="Output", exact=True).click()
        hover_last()
        expect(tip.locator(".tschart-tip-label")).to_have_text(["Snapshots", "Artifacts", "Total"])
        expect(tip.locator(".tschart-tip-value")).to_have_text(["2 MiB", "1 MiB", "3 MiB"])
        assert not page.errors, page.errors
    finally:
        page.unroute(HISTORY)


def _memory_history(route):
    """An hour of minute samples near 1 TB allocated: every y label but 0 B
    is a long one such as "931.3 GiB" (the scale steps in decimal bytes)."""
    now = datetime.now(timezone.utc).replace(microsecond=0)
    at = lambda m: (now - timedelta(minutes=60 - m)).isoformat().replace("+00:00", "Z")
    samples = [{"at": at(m), "allocatedMemory": (900 + m) * 10**9, "capacityMemory": 1000 * 10**9} for m in range(61)]
    route.fulfill(status=200, content_type="application/json",
                  body=json.dumps({"from": at(0), "to": at(60), "resolution": 0, "samples": samples}))


def test_overview_memory_chart_fits_its_y_labels_and_tooltip_follows_the_cursor(page, browser, env, lux):
    """The y axis widens past its 56px floor to fit long byte labels, and the
    tooltip stays on the crosshair to either side of it, at 1x and 2x pixels."""
    # Capacity, 10**12 B, is a whole number of any nice step: always the top label.
    top_label = "931.3 GiB"
    # .tschart-tip.is-left sits 16px left of its anchor (components.css); the other one starts on it.
    flipped_gap = 16

    def check(pg, dpr):
        pg.route(HISTORY, _memory_history)
        pg.goto(env.luxd_url + "/?range=1h")
        chart = pg.locator("section.card", has=pg.get_by_role("heading", name="Memory", exact=True)).locator(".tschart")
        expect(chart.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
        assert pg.evaluate("() => devicePixelRatio") == dpr
        over = chart.locator(".u-over")
        gutter = lambda: over.evaluate("o => o.getBoundingClientRect().left - o.closest('.tschart').querySelector('canvas').getBoundingClientRect().left")
        label_w = pg.evaluate("""label => {
            const ctx = document.createElement('canvas').getContext('2d');
            ctx.font = '11px ' + getComputedStyle(document.documentElement).getPropertyValue('--font-sans');
            return ctx.measureText(label).width;
        }""", top_label)
        # 5px: uPlot's gap between a label and the plot. The label must need more than the 56px floor.
        need = math.ceil(label_w) + 5
        assert need > 56, label_w
        wait_until(lambda: gutter() >= need, 10, 0.2, f"y axis gutter under {need}px for {top_label!r} at {dpr}x")
        over.scroll_into_view_if_needed()
        box = over.bounding_box()
        for frac, flipped in ((0.25, False), (0.85, True)):
            pg.mouse.move(box["x"] + box["width"] * frac, box["y"] + box["height"] / 2)
            tip = chart.locator(".tschart-tip.is-left" if flipped else ".tschart-tip:not(.is-left)")
            expect(tip).to_be_visible(timeout=5_000)

            def offset():
                return pg.evaluate("""([tip, cur, flipped]) => {
                    const t = tip.getBoundingClientRect(), c = cur.getBoundingClientRect().left;
                    return flipped ? c - t.right : t.left - c;
                }""", [tip.element_handle(), chart.locator(".u-cursor-x").element_handle(), flipped])
            want = flipped_gap if flipped else 0
            wait_until(lambda: abs(offset() - want) <= 2, 5, 0.2,
                       f"tooltip {'right' if flipped else 'left'} edge not {want}px from the cursor at {dpr}x ({offset()})")
        pg.mouse.move(0, 0)
        assert not pg.errors, pg.errors

    page.sign_in(lux.api_key, "/?range=1h")
    try:
        check(page, 1)
    finally:
        page.unroute(HISTORY)

    ctx = browser.new_context(viewport=VIEWPORT, device_scale_factor=2)
    try:
        ctx.add_init_script(f"sessionStorage.setItem('lux.key', {json.dumps(lux.api_key)})")
        hi = ctx.new_page()
        hi.errors = []
        hi.on("pageerror", lambda e: hi.errors.append(str(e)))
        check(hi, 2)
    finally:
        ctx.close()


def test_operator_overview_has_unallocated_and_top_tenants(page, env, operator, lux):
    """All tenants: the Cost panel's By family has the unallocated host time
    under it, Compute and Block storage apart, Top tenants beside it, and
    Tenant is a breakdown; narrowed to one tenant with no pool of its own,
    neither."""
    page.sign_in(operator.api_key, "/?tab=cost")
    panel = page.locator(".cost-panel")
    expect(panel.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(1, timeout=15_000)
    expect(panel.locator(".cost-unallocated")).to_contain_text("Unallocated host time")
    expect(panel.locator('.cost-unallocated [data-unallocated="compute"]')).to_contain_text("Compute")
    expect(panel.locator('.cost-unallocated [data-unallocated="block-storage"]')).to_contain_text("Block storage")
    expect(panel.get_by_role("radio", name="Tenant", exact=True)).to_have_count(1)
    panel.get_by_role("radio", name="Tenant", exact=True).click()
    expect(page).to_have_url(re.compile(r"[?&]by=tenant(&|$)"))
    expect(panel.get_by_role("heading", name="By tenant", exact=True)).to_have_count(1)
    # Top tenants would repeat the breakdown: it gives way to it.
    expect(panel.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(0)
    # Narrowed to one tenant: no Top tenants, no Tenant breakdown; the tenant has no pool of its own, so no unallocated host time.
    page.goto(env.luxd_url + f"/?tab=cost&tenant={lux.tenant_id}")
    expect(panel.get_by_role("heading", name="By family", exact=True)).to_have_count(1, timeout=15_000)
    expect(panel.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(0)
    expect(panel.get_by_role("radio", name="Tenant", exact=True)).to_have_count(0)
    expect(panel.locator(".cost-unallocated")).to_have_count(0)
    assert not page.errors, page.errors


def test_host_page_shows_cost_to_its_owner_and_rates_to_operators(page, env, lux, operator, runners, hosts):
    """A tenant's static host is in its own pool: its owner sees its host
    cost, unallocated included, and no rate periods; operators also see the
    rate periods, each with its family (a static host has compute only)."""
    # More than 4 decimals: shown rounded, the exact rate one hover away.
    host_id = _priced_host(lux, runners, hosts[0], price="0.041666667")
    page.sign_in(lux.api_key, f"/hosts/{host_id}?tab=cost")
    expect(page.get_by_text(re.compile(r"^charged to Runs vs unallocated, per hour"))).to_have_count(1, timeout=15_000)
    tiles = page.locator(".host-cost .stat-label")
    expect(tiles).to_have_text(["Host cost (24h)", "Compute", "Block storage", "Unallocated", "Utilisation"])
    # Rate periods are the operators'.
    expect(page.get_by_role("heading", name="Rate periods", exact=True)).to_have_count(0)
    assert not page.errors, page.errors
    # Init scripts run in order: the operator's key now wins on every load.
    page.sign_in(operator.api_key, f"/hosts/{host_id}?tab=cost")

    def check(theme):
        rates = page.locator("section.card", has=page.get_by_role("heading", name="Rate periods", exact=True))
        expect(rates.get_by_text("$0.0417/h", exact=True)).to_have_count(1, timeout=15_000)
        rates.locator(".money-rounded").hover()
        expect(page.get_by_role("tooltip")).to_have_text("Exactly $0.041666667")
        page.mouse.move(0, 0)
        # Each period names its family; a static host has compute only.
        expect(rates.locator("tbody tr").locator("td").first).to_have_text("Compute")
        expect(rates.get_by_text("Block storage")).to_have_count(0)
        # The source once: "static", not "static price (static)".
        expect(rates.get_by_text("static", exact=True)).to_have_count(1)
        expect(rates.get_by_text(re.compile(r"static price|\(static\)"))).to_have_count(0)
        expect(page.get_by_text(re.compile(r"^charged to Runs vs unallocated, per hour"))).to_have_count(1)
    _both_themes(page, env, f"/hosts/{host_id}?tab=cost", check)
    # Narrowed to the host's tenant, the operator reads its cost as that tenant
    # (?tenant= on the read): unallocated of its own pool, no rate periods.
    with page.expect_request(lambda r: f"/v1/hosts/{host_id}/cost" in r.url and f"tenant={lux.tenant_id}" in r.url, timeout=15_000):
        page.goto(env.luxd_url + f"/hosts/{host_id}?tab=cost&tenant={lux.tenant_id}")
    expect(tiles).to_have_text(["Host cost (24h)", "Compute", "Block storage", "Unallocated", "Utilisation"], timeout=15_000)
    expect(page.get_by_role("heading", name="Rate periods", exact=True)).to_have_count(0)
    assert not page.errors, page.errors


def test_rename_a_pool_through_the_dialog(page, lux, runners, hosts):
    """The Pools page's Rename: only the name changes, so the pool's host
    and a Run waiting for it stay with it under the new name."""
    old, new = f"lab-{lux.tenant_id[-6:]}", f"lab2-{lux.tenant_id[-6:]}"
    lux.run("pools", "set", old, "--provider", "static")
    runners.start(hosts[0], token=runners.token("--pool", old))
    waiting = lux.submit(generic(ALPINE_IMAGE, "true", placement={"pool": old, "requires": {"nowhere": "yes"}}))
    page.sign_in(lux.api_key, "/pools")
    page.get_by_role("row").filter(has_text=old).get_by_role("button", name="Rename", exact=True).click()
    dialog = page.get_by_role("dialog")
    dialog.get_by_label("New name").fill(new)
    dialog.get_by_role("button", name="Rename pool", exact=True).click()
    page.get_by_text(f"Renamed {old} to {new}").wait_for(timeout=15_000)
    expect(page.get_by_role("row").filter(has_text=new)).to_have_count(1, timeout=15_000)
    assert [h["pool"] for h in lux.json("hosts", "ls") if h["name"] == hosts[0].name] == [new]
    run = lux.get(waiting)
    assert run["pool"] == new and run["spec"]["placement"]["pool"] == old, run
    assert not page.errors, page.errors
    lux.run("terminate", waiting)


def test_pools_page_makes_a_pool_the_default(page, lux):
    """A tenant marks one of its pools as its default through the dialog,
    which names the current default; the badge moves with the mark."""
    tag = lux.tenant_id[-6:]
    old, new = f"old-{tag}", f"new-{tag}"
    try:
        lux.run("pools", "set", old, "--provider", "static", "--default")
        lux.run("pools", "set", new, "--provider", "static")
        page.sign_in(lux.api_key, "/pools")
        old_row = page.get_by_role("row").filter(has_text=old)
        new_row = page.get_by_role("row").filter(has_text=new)
        expect(old_row.get_by_text("Default", exact=True)).to_be_visible(timeout=15_000)
        # Settings carries the provider (there is no Provider column).
        expect(old_row.get_by_text("static hosts", exact=True)).to_have_count(1)
        assert old_row.get_by_role("button", name="Make default").count() == 0
        new_row.get_by_role("button", name="Make default").click()
        dialog = page.get_by_role("dialog")
        expect(dialog).to_contain_text(f"{old} is your default pool now")
        expect(dialog).to_contain_text("Runs already submitted keep their pool")
        dialog.get_by_role("button", name="Make default").click()
        expect(new_row.get_by_text("Default", exact=True)).to_be_visible(timeout=15_000)
        expect(old_row.get_by_text("Default", exact=True)).to_have_count(0)
        assert [p["name"] for p in lux.json("pools", "ls") if p.get("isDefault")] == [new]
        assert not page.errors, page.errors
    finally:
        for pool in (old, new):
            lux.run("pools", "rm", pool, check=False)


def test_pool_page_shows_its_settings_and_events(page, tenant_factory):
    """The Pools list links each pool to its page: its settings, its hosts
    and what happened to it, newest first."""
    a = tenant_factory()
    name = f"evpool-{a.tenant_id[-6:]}"
    a.run("pools", "set", name, "--provider", "static", "--max", "2")
    a.run("pools", "set", name, "--provider", "static", "--max", "3")
    page.sign_in(a.api_key, "/pools")
    page.get_by_role("link", name=name, exact=True).click()
    page.wait_for_url(re.compile(rf"/pools/{name}$"), timeout=10_000)
    # Settings are on the Hosts tab, events on the Events tab.
    page.get_by_role("tab", name="Hosts").click()
    page.wait_for_url(re.compile(rf"/pools/{name}\?tab=hosts$"), timeout=10_000)
    expect(page.get_by_role("heading", name="Settings", exact=True)).to_have_count(1, timeout=15_000)
    expect(page.get_by_text("min 0 · warm 0 · max 3")).to_have_count(1, timeout=15_000)
    page.get_by_role("tab", name="Events").click()
    rows = page.locator("tr", has_text="pool.config_changed")
    expect(rows).to_have_count(2, timeout=15_000)
    # Newest first: the second change leads.
    expect(rows.first).to_contain_text("maxHosts 2→3")
    expect(rows.last).to_contain_text("created:")
    # Live: a change made now appears without a reload (the page polls).
    a.run("pools", "set", name, "--provider", "static", "--max", "4")
    expect(rows).to_have_count(3, timeout=30_000)
    expect(rows.first).to_contain_text("maxHosts 3→4")
    assert not page.errors, page.errors


def test_host_page_shows_its_events(page, env, lux, runners, hosts):
    """A host's events are server pages, newest first, like a pool's: its
    own registration, and with more than a page of events, Next to the
    older ones."""
    runners.start(hosts[0])
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == hosts[0].name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    page.sign_in(lux.api_key, f"/hosts/{host_id}?tab=events")
    card = page.locator(".card", has=page.get_by_role("heading", name="Events", exact=True))
    expect(card).to_have_count(1, timeout=15_000)
    expect(card.locator("tr", has_text="host.registered")).to_have_count(1, timeout=15_000)
    pager = card.get_by_role("navigation", name="Pages")
    expect(pager).to_contain_text("Time, newest first")
    expect(pager.get_by_role("button", name="Next ›")).to_be_disabled()

    # 60 more, newer than its registration: two pages of 50.
    with psycopg.connect(env.owner_dsn) as conn:
        conn.execute("""INSERT INTO host_events (tenant_id, host_id, type, data)
            SELECT h.tenant_id, h.id, 'host.placement_assigned', jsonb_build_object('run', 'hev-' || i, 'epoch', 1, 'host', 'h')
            FROM hosts h, generate_series(1, 60) i WHERE h.id = %s ORDER BY i""", (host_id,))
    tags = lambda: card.evaluate("""c => [...c.querySelectorAll('tbody tr')]
        .map(tr => (/(hev-\\d+) epoch/.exec(tr.textContent) || [])[1]).filter(Boolean)""")
    # The next poll (5s) reads the new page 1.
    expect(pager).to_contain_text("Page 1 · events 1–50", timeout=15_000)
    expect(card.locator("tbody tr").first).to_contain_text("hev-60 epoch", timeout=15_000)
    assert tags() == [f"hev-{i}" for i in range(60, 10, -1)], tags()
    expect(card.locator("tr", has_text="host.registered")).to_have_count(0)
    pager.get_by_role("button", name="Next ›").click()
    expect(pager).to_contain_text("Page 2 · events 51–", timeout=15_000)
    expect(card.locator("tbody tr").first).to_contain_text("hev-10 epoch", timeout=15_000)
    assert tags() == [f"hev-{i}" for i in range(10, 0, -1)], tags()
    expect(card.locator("tr", has_text="host.registered")).to_have_count(1)
    pager.get_by_role("button", name="‹ Previous").click()
    expect(card.locator("tbody tr").first).to_contain_text("hev-60 epoch", timeout=15_000)
    assert not page.errors, page.errors


def test_platform_pool_page_is_reachable_beside_a_tenant_pool_of_its_name(page, env, operator, tenant_factory):
    """A tenant's pool and the platform's may share a name: from the Pools
    list, the platform row opens the platform's pool and its events, and
    the tenant row that tenant's."""
    a = tenant_factory()
    name = f"twin-{a.tenant_id[-6:]}"
    a.run("pools", "set", name, "--provider", "static", "--max", "2")
    env.luxd_admin("create-pool", "--name", name, "--provider", "static", "--max", "7")
    # The CLI's view first: each is reachable, the bare name is ambiguous.
    assert [e["data"]["changes"]["maxHosts"]["new"] for e in operator.json("pools", "events", name, "--platform")] == [7]
    assert [e["data"]["changes"]["maxHosts"]["new"] for e in operator.json("pools", "events", name, "--tenant", a.tenant_id)] == [2]
    page.sign_in(operator.api_key, "/pools")
    rows = page.locator("tr", has_text=name)
    expect(rows).to_have_count(2, timeout=15_000)
    rows.filter(has_text="platform").get_by_role("link", name=name, exact=True).click()
    page.wait_for_url(re.compile(rf"/pools/{name}\?owner=platform$"), timeout=10_000)
    page.goto(env.luxd_url + f"/pools/{name}?owner=platform&tab=hosts")
    expect(page.get_by_text("min 0 · warm 0 · max 7")).to_have_count(1, timeout=15_000)
    page.goto(env.luxd_url + f"/pools/{name}?owner=platform&tab=events")
    events = page.locator("tr", has_text="pool.config_changed")
    expect(events).to_have_count(1, timeout=15_000)
    expect(events.first).to_contain_text("maxHosts –→7")
    # Back to the list: the tenant's row opens the tenant's.
    page.goto(env.luxd_url + "/pools")
    tenant_row = page.locator("tr", has_text=name).filter(has_not_text="platform")
    expect(tenant_row).to_have_count(1, timeout=15_000)
    tenant_row.get_by_role("link", name=name, exact=True).click()
    page.wait_for_url(re.compile(rf"/pools/{name}\?tenant="), timeout=10_000)
    tenant_url = page.url
    page.goto(tenant_url + "&tab=hosts")
    expect(page.get_by_text("min 0 · warm 0 · max 2")).to_have_count(1, timeout=15_000)
    page.goto(tenant_url + "&tab=events")
    expect(page.locator("tr", has_text="pool.config_changed").first).to_contain_text("maxHosts –→2", timeout=15_000)
    assert not page.errors, page.errors


def _add_pool_events(env, tenant_id: str, pool: str, n: int, tag: str):
    with psycopg.connect(env.owner_dsn) as conn:
        conn.execute("""INSERT INTO pool_events (tenant_id, pool_id, type, data)
            SELECT %s, p.id, 'pool.placement', jsonb_build_object('run', %s || '-' || i, 'epoch', 1, 'host', 'h')
            FROM pools p, generate_series(1, %s) i WHERE p.tenant_id = %s AND p.name = %s ORDER BY i""",
                     (tenant_id, tag, n, tenant_id, pool))


def _event_tags(card) -> list[str]:
    """The tags of the placement events the card lists, top to bottom."""
    return card.evaluate("""c => [...c.querySelectorAll('tbody tr')]
        .map(tr => (/((?:old|new)-\\d+) epoch/.exec(tr.textContent) || [])[1])
        .filter(Boolean)""")


def test_pool_events_pages_stay_put_as_new_events_arrive(page, env, tenant_factory):
    """Pool events are server pages in the sort chosen (newest first). A
    page past the first keeps its rows while new events arrive above it (its
    refresh re-reads it in place), and Next / Previous from there cover every
    event exactly once."""
    a = tenant_factory()
    name = f"busy-{a.tenant_id[-6:]}"
    a.run("pools", "set", name, "--provider", "static")
    _add_pool_events(env, a.tenant_id, name, 120, "old")
    page.sign_in(a.api_key, f"/pools/{name}?tab=events")
    card = page.locator(".card", has=page.get_by_role("heading", name="Events", exact=True))
    pager = card.get_by_role("navigation", name="Pages")
    # The pool's own created event is the oldest: 121 in all.
    expect(pager).to_contain_text("Page 1 · events 1–50", timeout=15_000)
    expect(pager).to_contain_text("Time, newest first")
    assert _event_tags(card) == [f"old-{i}" for i in range(120, 70, -1)]
    pager.get_by_role("button", name="Next ›").click()
    expect(pager).to_contain_text("Page 2 · events 51–100", timeout=15_000)
    page2 = [f"old-{i}" for i in range(70, 20, -1)]
    expect(card.locator("tbody tr").first).to_contain_text("old-70 epoch", timeout=15_000)
    assert _event_tags(card) == page2
    _add_pool_events(env, a.tenant_id, name, 30, "new")
    # One poll (15s) re-reads page 2 in place, from its own cursor.
    with page.expect_response(lambda r: f"/pools/{name}/events" in r.url and "at=" in r.url, timeout=25_000):
        pass
    expect(pager).to_contain_text("Page 2 · events 51–100")
    expect(card.locator("tbody tr").first).to_contain_text("old-70 epoch")
    expect(card.locator("tbody tr").last).to_contain_text("old-21 epoch")
    assert _event_tags(card) == page2
    # Previous from page 3 reads the same page 2 back.
    pager.get_by_role("button", name="Next ›").click()
    expect(pager).to_contain_text("Page 3", timeout=15_000)
    expect(card.locator("tbody tr").first).to_contain_text("old-20 epoch", timeout=15_000)
    pager.get_by_role("button", name="‹ Previous").click()
    expect(pager).to_contain_text("Page 2 · events 51–100", timeout=15_000)
    expect(card.locator("tbody tr").first).to_contain_text("old-70 epoch", timeout=15_000)
    assert _event_tags(card) == page2

    def walk(button: str, n: int) -> list[list[str]]:
        """This page and the n - 1 that button leads to, each one's tags."""
        pages = []
        for i in range(n):
            if i:
                before = _event_tags(card)
                pager.get_by_role("button", name=button).click()
                wait_until(lambda: _event_tags(card) != before, 15, 0.1, f"{button} did not change the page")
            pages.append(_event_tags(card))
        return pages
    # From the top (the new events lead it), Next to the end and Previous
    # back: every event exactly once, in order, the same pages both ways.
    pager.get_by_role("button", name="« First").click()
    expect(pager).to_contain_text("Page 1 · events 1–50", timeout=15_000)
    expect(card.locator("tbody tr").first).to_contain_text("new-30 epoch", timeout=15_000)
    forward = walk("Next ›", 4)
    expect(pager).to_contain_text("Page 4 · events 151–151")
    expect(pager.get_by_role("button", name="Next ›")).to_be_disabled()
    expect(card.locator("tbody tr").first).to_contain_text("created:")
    tags = [t for p in forward for t in p]
    assert tags == [f"new-{i}" for i in range(30, 0, -1)] + [f"old-{i}" for i in range(120, 0, -1)], tags
    backward = walk("‹ Previous", 4)
    expect(pager).to_contain_text("Page 1 · events 1–50")
    assert backward == forward[::-1], backward
    assert not page.errors, page.errors


@pytest.fixture
def priced_ec2(env, ec2, lux):
    """luxd with the fake EC2 and its Pricing API (on-demand and gp3 list
    prices), second-scale cost ticks; the tenant's pool burst is removed
    after the test, and its instances with it."""
    fake_only(ec2)
    env.stop_luxd()
    env.start_luxd(LUX_EC2_ENDPOINT=ec2.url, LUX_PRICING_ENDPOINT=ec2.url, AWS_ACCESS_KEY_ID="fake",
                   AWS_SECRET_ACCESS_KEY="fake", AWS_REGION="us-east-1", LUX_COSTS_EVERY="5s",
                   LUX_COSTS_PRICES_REFRESH="5s", **FAKE_EC2_TIMERS)
    yield ec2
    lux.run("pools", "rm", "burst", check=False)
    wait_until(lambda: not ec2.running(), 90, 0.3, "the removed pool's instances were not terminated")


def _cents_shown(amount) -> str:
    """formatMoney to the cent: a non-zero amount under half a cent is the bound "<$0.01", never $0.00."""
    d = Decimal(amount)
    return "<$0.01" if d > 0 and d.quantize(Decimal("0.01"), ROUND_HALF_EVEN) == 0 else _cents(d)


@pytest.mark.ec2
def test_pool_cost_tab_and_run_placements_with_block_storage(page, env, lux, priced_ec2):
    """A launched host has a disk: its pool's Cost tab shows compute and block
    storage apart, each charged to Runs or unallocated, as the API reads
    them, and the Run's cost lists its placement with both."""
    ec2 = priced_ec2
    lux.run("pools", "set", "burst", "--provider", "ec2", "--template", json.dumps({**ec2.template, "region": "eu-north-1"}), "--max", "1", "--min", "1")
    host = wait_until(lambda: next((h for h in lux.json("hosts", "ls") if h["pool"] == "burst" and h["state"] == "ready" and h.get("volumes")), None),
                      120, 0.5, "the pool's host never registered with its volumes")
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "3", placement={"pool": "burst"}))
    lux.wait_state(run_id, "succeeded", timeout=120)
    seen = {}

    def final_cost():
        seen["cost"] = c = lux.json("cost", run_id)
        return c if c["status"] == "final" else None
    try:
        cost = wait_until(final_cost, 120, 1, "the Run's cost never became final")
    except AssertionError as e:
        raise AssertionError(f"{e}: {seen.get('cost')}") from None
    lines = {l["family"]: l for l in cost["lines"]}
    assert set(lines) == {"compute", "block-storage"}, cost["lines"]

    # The host's hours are refreshed apart from the Run's lines: wait for both families' rows.
    def pool_cost():
        c = lux.api("/v1/pools/burst/cost?since=24h&interval=hour").json()
        fams = {(h["hostId"], h["family"]) for h in c.get("hosts") or []}
        return c if {(host["id"], "compute"), (host["id"], "block-storage")} <= fams else None
    api = wait_until(pool_cost, 120, 2, "the pool's host time never split by family")

    page.sign_in(lux.api_key, "/pools/burst?tab=cost")
    tab = page.locator(".pool-cost")
    # The tenant owns the pool: every tile, unallocated included.
    expect(tab.locator(".stat-label")).to_have_text(["Host cost (24h)", "Compute", "Block storage", "Unallocated", "Utilisation"], timeout=15_000)
    # Block storage's line names the host's disk, as luxd recorded it.
    expect(tab.locator(".stat", has_text="Block storage").locator(".stat-unit")).to_contain_text("100 GiB gp3")
    legend = tab.locator(".host-cost-chart .tschart-legend-label")
    expect(legend).to_have_text(["Compute · runs", "Compute · unallocated", "Block storage · runs", "Block storage · unallocated"])

    # Who paid and Cost by host match the API, per family. Host rows grow each refresh (the hour is open):
    # read both until they agree.
    hosts_card = page.locator(".card", has=page.locator(".card-title", has_text="Cost by host"))
    row = hosts_card.locator("tbody tr", has_text=host["name"])

    def who_paid_matches():
        page.reload()
        c = lux.api("/v1/pools/burst/cost?since=24h&interval=hour").json()
        series = c.get("hostSeries") or []
        card = page.locator(".card", has=page.locator(".card-title", has_text="Who paid"))
        try:
            card.locator("tbody tr").nth(2).wait_for(timeout=15_000)
            row.wait_for(timeout=15_000)
        except PlaywrightTimeoutError:
            return False
        rows = card.locator("tbody tr").evaluate_all("rs => rs.map(r => [...r.querySelectorAll('td')].map(td => td.textContent))")
        want = []
        for fam in ("compute", "block-storage"):
            alloc = sum(Decimal(r["allocated"]) for r in series if r["family"] == fam)
            idle = sum(Decimal(r["unallocated"]) for r in series if r["family"] == fam)
            want.append((_cents_shown(alloc), _cents_shown(idle), _cents_shown(alloc + idle)))
        got = [tuple(r[1:4]) for r in rows[:2]]
        # Cost by host: Host, Up, Compute, Block storage, Total, Unallocated, Utilisation; each family its own figure.
        mine = {h["family"]: h for h in c.get("hosts") or [] if h["hostId"] == host["id"]}
        fam_total = lambda f: str(Decimal(mine[f]["allocated"]) + Decimal(mine[f]["unallocated"]))
        want_host = [_dollars(fam_total("compute")), _dollars(fam_total("block-storage")),
                     _dollars(str(Decimal(fam_total("compute")) + Decimal(fam_total("block-storage")))),
                     _dollars(str(Decimal(mine["compute"]["unallocated"]) + Decimal(mine["block-storage"]["unallocated"])))]
        got_host = row.locator("td").all_inner_texts()[2:6]
        seen["who"] = (got, want, got_host, want_host)
        return (got == want and got_host == want_host) or None
    try:
        wait_until(who_paid_matches, 60, 2, "Who paid or Cost by host does not match the pool's host time per family")
    except AssertionError as e:
        raise AssertionError(f"{e}: {seen.get('who')}") from None
    cells = row.locator("td").all_inner_texts()
    assert re.fullmatch(r"\d+\.\d h", cells[1]) and re.fullmatch(r"(<1|\d+)%|–", cells[6]), cells
    assert api["idle"] and {i["family"] for i in api["idle"]} == {"compute", "block-storage"}, api["idle"]
    expect(tab.get_by_text("AI models")).to_have_count(0)
    expect(tab.locator(".callout")).to_have_text("Pool cost is the machines only — instance and disk. AI and other external costs belong to Runs.")
    # A host opens on its Cost tab: its rate periods say which family each is.
    row.get_by_role("link").click()
    page.wait_for_url(re.compile(rf"/hosts/{host['id']}\?tab=cost"))
    expect(page.locator(".host-cost .stat-label")).to_contain_text(["Block storage"], timeout=15_000)

    # The Run's placement: compute and block storage side by side, as the lines' details have them.
    page.goto(env.luxd_url + f"/runs/{run_id}?tab=resources")
    card = page.locator(".run-placements-cost")
    expect(card.locator("tbody tr")).to_have_count(1, timeout=15_000)
    [pc] = lines["compute"]["details"]["placements"]
    [pb] = lines["block-storage"]["details"]["placements"]
    cells = card.locator("tbody tr td").all_inner_texts()
    assert cells[0] == str(pc["epoch"]) and cells[1] == host["name"], cells
    assert cells[4] == _dollars(pc["amount"]) and cells[5] == _dollars(pb["amount"]), (cells, pc["amount"], pb["amount"])
    assert cells[6] == _dollars(str(Decimal(pc["amount"]) + Decimal(pb["amount"]))), cells
    # The lines table names the family as luxd does, with its own swatch.
    expect(page.locator("section.card.run-cost").get_by_role("cell", name="Block storage")).to_have_count(1)
    assert not page.errors, page.errors
