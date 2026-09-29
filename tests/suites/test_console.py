"""The web console, in a headless browser: it signs in with a key, shows
what that key sees, and acts through the same API as the CLI. Uses the
system's Chrome, or Playwright's own Chromium if installed
(`uv run playwright install chromium`); skipped when neither is there."""

from __future__ import annotations

import re
from decimal import ROUND_HALF_EVEN, Decimal

import psycopg
import pytest
from playwright.sync_api import expect

from conftest import generic
from env import ALPINE_IMAGE, wait_until
from fake_cost_plugin import FakeCostPlugin

pytestmark = pytest.mark.console


@pytest.fixture
def page(browser, env):
    """A page with a console error collector; sign in with page.sign_in(key)."""
    ctx = browser.new_context(viewport={"width": 1400, "height": 900})
    pg = ctx.new_page()
    pg.errors = []
    pg.on("pageerror", lambda e: pg.errors.append(str(e)))
    # The console first asks whoami without a key (is luxd behind Cloudflare
    # Access?): its 401 is expected, and browsers log it.
    pg.on("console", lambda m: m.type == "error" and "401" not in m.text and pg.errors.append(m.text))

    def sign_in(key: str, path: str = "/"):
        ctx.add_init_script(f"sessionStorage.setItem('lux.key', {key!r})")
        pg.goto(env.luxd_url + path)
    pg.sign_in = sign_in
    yield pg
    ctx.close()


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
    a.run("cancel", ra)
    b.run("cancel", rb)


def test_a_tenant_key_sees_only_its_own(page, tenant_factory):
    a, b = tenant_factory(), tenant_factory()
    na, nb = f"mine-{a.tenant_id[-6:]}", f"theirs-{b.tenant_id[-6:]}"
    ra, rb = _parked(a, na), _parked(b, nb)
    page.sign_in(a.api_key, "/runs")
    page.get_by_text(na, exact=True).wait_for(timeout=15_000)
    assert page.get_by_text(nb, exact=True).count() == 0
    # No tenants page for a tenant.
    assert page.get_by_role("link", name="Tenants").count() == 0
    a.run("cancel", ra)
    b.run("cancel", rb)


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
    lux.run("cancel", run_id)


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
    a.run("cancel", run_id)


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
    page.sign_in(operator.api_key, f"/hosts/{host_id}?range=1h")
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


def _priced_host(lux, runners, host, price: str = "0.40") -> str:
    """A ready static host of the tenant's, priced before any Run is placed
    on it, so its Runs' compute lines have a rate from their start."""
    runners.start(host)
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == host.name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    lux.run("hosts", "price", host_id, "--hourly-price", price, "--currency", "USD")
    return host_id


def _costed_run(lux, text: str, name: str = "") -> str:
    """A Run that ends on its own, with its compute line written: the line
    comes at a state change (or the 2m tick), and the end is one."""
    extra = {"name": name} if name else {}
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", f"echo {text}; sleep 3", **extra))
    lux.wait_state(run_id, "succeeded")
    wait_until(lambda: lux.api(f"/v1/runs/{run_id}/cost").json()["totals"], 60, 1, "no compute line was written")
    return run_id


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
    lux.run("cancel", run_id)


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
        # Not clipped: every ancestor that clips its overflow contains the whole tooltip.
        clipped = tip.evaluate("""t => {
            const r = t.getBoundingClientRect();
            for (let e = t.parentElement; e; e = e.parentElement) {
                if (getComputedStyle(e).overflow === 'visible') continue;
                const c = e.getBoundingClientRect();
                if (r.left < c.left || r.right > c.right || r.top < c.top || r.bottom > c.bottom) return e.tagName + '.' + e.className;
            }
            return null;
        }""")
        assert clipped is None, clipped
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
    lux.run("cancel", pending)


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


def test_overview_charts_cost_by_family(page, env, lux, runners, hosts, cost_plugin):
    _priced_host(lux, runners, hosts[0])
    name = f"overview-cost-{lux.tenant_id[-6:]}"
    run_id = _costed_run(lux, "overview-cost", name=name)
    # The summary reads cost_hourly, written with each source's lines.
    wait_until(lambda: {r["group"]["family"] for r in lux.api("/v1/costs?since=24h&group=family&interval=hour").json().get("series", [])} >= {"compute", "ai"},
               90, 1, "no hourly cost for both families")

    def check(theme):
        expect(page.get_by_role("heading", name="Cost by family", exact=True)).to_have_count(1, timeout=15_000)
        chart = page.locator("section.card", has=page.get_by_role("heading", name="Cost by family", exact=True))
        expect(chart.locator(".tschart-plot canvas")).to_have_count(1, timeout=15_000)
        # Families read as on the Run page: the describe's displayName, not the key.
        legend = chart.locator(".tschart-legend")
        expect(legend.get_by_text("Compute", exact=True)).to_have_count(1)
        expect(legend.get_by_text("AI models", exact=True)).to_have_count(1)
        expect(legend.get_by_text("ai", exact=True)).to_have_count(0)
        # Each family has its own colour: Compute's swatch is not AI models'.
        swatch = lambda label: legend.get_by_role("button", name=label, exact=True).locator(".tschart-key").evaluate("e => getComputedStyle(e).backgroundColor")
        compute, ai = swatch("Compute"), swatch("AI models")
        assert compute != ai and "0, 0, 0, 0" not in compute + ai, (theme, compute, ai)
        # Top Runs leads with the Run's name, its id beside it; a tenant gets no tenant table and no Unallocated tile.
        top = page.locator("section.card", has=page.get_by_role("heading", name="Top Runs", exact=True))
        row = top.get_by_role("row").filter(has=page.get_by_role("link", name=name, exact=True))
        expect(row).to_have_count(1, timeout=15_000)
        expect(row.get_by_text(run_id)).to_have_count(1)
        expect(row.get_by_text(re.compile(r"^\$\d"))).to_have_count(1)
        expect(page.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(0)
        expect(page.get_by_text(re.compile(r"^Unallocated"))).to_have_count(0)
    page.sign_in(lux.api_key, "/")
    _both_themes(page, env, "/", check)


def test_operator_overview_has_unallocated_and_top_tenants(page, env, operator):
    page.sign_in(operator.api_key, "/")
    expect(page.get_by_role("heading", name="Top tenants", exact=True)).to_have_count(1, timeout=15_000)
    expect(page.get_by_text(re.compile(r"^Unallocated \("))).to_have_count(1)
    assert not page.errors, page.errors


def test_host_page_shows_cost_to_its_owner_and_rates_to_operators(page, env, lux, operator, runners, hosts):
    # More than 4 decimals: shown rounded, the exact rate one hover away.
    host_id = _priced_host(lux, runners, hosts[0], price="0.041666667")
    page.sign_in(lux.api_key, f"/hosts/{host_id}")
    expect(page.get_by_text(re.compile(r"^allocated to your Runs, per hour"))).to_have_count(1, timeout=15_000)
    # Rate periods and unallocated are the operators'.
    expect(page.get_by_role("heading", name="Rate periods", exact=True)).to_have_count(0)
    assert not page.errors, page.errors
    # Init scripts run in order: the operator's key now wins on every load.
    page.sign_in(operator.api_key, f"/hosts/{host_id}")

    def check(theme):
        rates = page.locator("section.card", has=page.get_by_role("heading", name="Rate periods", exact=True))
        expect(rates.get_by_text("$0.0417/h", exact=True)).to_have_count(1, timeout=15_000)
        rates.locator(".money-rounded").hover()
        expect(page.get_by_role("tooltip")).to_have_text("Exactly $0.041666667")
        page.mouse.move(0, 0)
        # The source once: "static", not "static price (static)".
        expect(rates.get_by_text("static", exact=True)).to_have_count(1)
        expect(rates.get_by_text(re.compile(r"static price|\(static\)"))).to_have_count(0)
        expect(page.get_by_text(re.compile(r"^allocated to Runs vs unallocated, per hour"))).to_have_count(1)
    _both_themes(page, env, f"/hosts/{host_id}", check)


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
    expect(page.get_by_role("heading", name="Settings", exact=True)).to_have_count(1, timeout=15_000)
    expect(page.get_by_text("min 0 · warm 0 · max 3")).to_have_count(1, timeout=15_000)
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


def test_host_page_shows_its_events(page, lux, runners, hosts):
    runners.start(hosts[0])
    host_id = wait_until(lambda: next((h["id"] for h in lux.json("hosts", "ls")
                                       if h["name"] == hosts[0].name and h["state"] == "ready"), None),
                         30, 1, "the host never registered")
    page.sign_in(lux.api_key, f"/hosts/{host_id}")
    expect(page.get_by_role("heading", name="Events", exact=True)).to_have_count(1, timeout=15_000)
    expect(page.locator("tr", has_text="host.registered")).to_have_count(1, timeout=15_000)
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
    expect(page.get_by_text("min 0 · warm 0 · max 7")).to_have_count(1, timeout=15_000)
    events = page.locator("tr", has_text="pool.config_changed")
    expect(events).to_have_count(1, timeout=15_000)
    expect(events.first).to_contain_text("maxHosts –→7")
    # Back to the list: the tenant's row opens the tenant's.
    page.goto(env.luxd_url + "/pools")
    tenant_row = page.locator("tr", has_text=name).filter(has_not_text="platform")
    expect(tenant_row).to_have_count(1, timeout=15_000)
    tenant_row.get_by_role("link", name=name, exact=True).click()
    page.wait_for_url(re.compile(rf"/pools/{name}\?tenant="), timeout=10_000)
    expect(page.get_by_text("min 0 · warm 0 · max 2")).to_have_count(1, timeout=15_000)
    expect(page.locator("tr", has_text="pool.config_changed").first).to_contain_text("maxHosts –→2", timeout=15_000)
    assert not page.errors, page.errors


def _add_pool_events(env, tenant_id: str, pool: str, n: int, tag: str):
    with psycopg.connect(env.owner_dsn) as conn:
        conn.execute("""INSERT INTO pool_events (tenant_id, pool_id, type, data)
            SELECT %s, p.id, 'pool.placement', jsonb_build_object('run', %s || '-' || i, 'epoch', 1, 'host', 'h')
            FROM pools p, generate_series(1, %s) i WHERE p.tenant_id = %s AND p.name = %s ORDER BY i""",
                     (tenant_id, tag, n, tenant_id, pool))


def test_pool_events_keep_every_event_as_new_ones_arrive_above_older_pages(page, env, tenant_factory):
    """With older pages loaded, new events push some off the polled newest
    page: the page reads them back, so none goes missing between the two."""
    a = tenant_factory()
    name = f"busy-{a.tenant_id[-6:]}"
    a.run("pools", "set", name, "--provider", "static")
    _add_pool_events(env, a.tenant_id, name, 1100, "old")
    page.sign_in(a.api_key, f"/pools/{name}")
    card = page.locator(".card", has=page.get_by_role("heading", name="Events", exact=True))
    expect(card.get_by_text(re.compile(r"^1000 events"))).to_have_count(1, timeout=15_000)
    card.get_by_role("button", name="Load older events").click()
    expect(card.get_by_text(re.compile(r"^1101 events"))).to_have_count(1, timeout=15_000)
    _add_pool_events(env, a.tenant_id, name, 30, "new")
    card.get_by_role("button", name="Refresh").click()
    expect(card.get_by_text(re.compile(r"^1131 events"))).to_have_count(1, timeout=15_000)
    # The ones pushed off the newest page are there: old-101 .. old-130.
    for i in (101, 115, 130):
        expect(card.get_by_text(f"old-{i} epoch 1 on h", exact=True)).to_have_count(1)
    assert not page.errors, page.errors
