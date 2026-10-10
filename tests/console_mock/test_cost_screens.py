"""The cost screens on the mock console: the pool Cost tab splits host cost
into compute and block storage, each charged to Runs or unallocated, and
the Run's cost lists what each placement paid for its host."""

from __future__ import annotations

from decimal import Decimal

import pytest
import requests


@pytest.fixture()
def page(browser, mock_console):
    ctx = browser.new_context(viewport={"width": 1440, "height": 900})
    ctx.add_init_script("sessionStorage.setItem('lux.key', 'luxk_mock'); localStorage.setItem('lux.theme', 'light');")
    p = ctx.new_page()
    p.errors = []
    p.on("pageerror", lambda e: p.errors.append(str(e)))
    yield p
    ctx.close()


def _card(page, title: str):
    return page.locator(".card", has=page.locator(".card-title", has_text=title))


def _rows(card) -> list[list[str]]:
    return [[c.strip() for c in r] for r in card.locator("tbody tr").evaluate_all("rs => rs.map(r => [...r.querySelectorAll('td')].map(td => td.textContent))")]


def _dollars(amount: str) -> str:
    """formatMoney to cents, as the tiles show a headline figure."""
    return f"${Decimal(amount).quantize(Decimal('0.01')):,}"


def test_pool_cost_tab_splits_host_cost_by_family_and_who_paid(page, mock_console):
    api = requests.get(f"{mock_console}/v1/pools/default/cost?owner=platform&since=7d&interval=day", headers={"Authorization": "Bearer x"}, timeout=10).json()
    page.goto(f"{mock_console}/pools/default?owner=platform&tab=cost&range=7d")
    tab = page.locator(".pool-cost")
    tab.locator(".host-cost-chart canvas").first.wait_for(timeout=20_000)

    # The tiles read what the API says, per family, summed exactly.
    idle = sum(Decimal(i["amount"]) for i in api["idle"])
    alloc = {f: sum(Decimal(r["allocated"]) for r in api["hostSeries"] if r["family"] == f) for f in ("compute", "block-storage")}
    unalloc = {f: sum(Decimal(r["unallocated"]) for r in api["hostSeries"] if r["family"] == f) for f in ("compute", "block-storage")}
    tiles = {t.locator(".stat-label").inner_text(): t.locator(".stat-value").inner_text() for t in tab.locator(".stat").all()}
    assert tiles["Host cost (7d)"] == _dollars(str(sum(alloc.values()) + sum(unalloc.values()))), tiles
    assert tiles["Compute"] == _dollars(str(alloc["compute"] + unalloc["compute"])), tiles
    assert tiles["Block storage"] == _dollars(str(alloc["block-storage"] + unalloc["block-storage"])), tiles
    assert tiles["Unallocated"] == _dollars(str(idle)), tiles

    # The chart: four series, compute and block storage each charged to Runs and unallocated.
    legend = tab.locator(".host-cost-chart .tschart-legend-label").all_inner_texts()
    assert legend == ["Compute · runs", "Compute · unallocated", "Block storage · runs", "Block storage · unallocated"], legend

    # Who paid: Compute and Block storage rows apart, then their total.
    paid = _rows(_card(page, "Who paid"))
    assert [r[0] for r in paid] == ["Compute", "Block storage", "Total"], paid
    assert paid[1][2] == _dollars(str(unalloc["block-storage"])), paid

    # Cost by host: costliest first; a host whose volumes are not known has no block storage (a dash, not $0).
    hosts = _card(page, "Cost by host")
    rows = _rows(hosts)
    totals = [Decimal(r[4].lstrip("$").replace(",", "")) for r in rows]
    assert totals == sorted(totals, reverse=True), rows
    unknown = next(r for r in rows if r[0] == "gp-eu-west-1-b2")
    assert unknown[3] == "–", unknown
    # A host opens on its Cost tab.
    hosts.get_by_role("link", name="gp-eu-west-1-a7").click()
    page.wait_for_url("**/hosts/host_3k8wq2n5vz?tab=cost**")
    assert not page.errors, page.errors


def test_run_cost_placements_join_compute_and_block_storage(page, mock_console):
    api = requests.get(f"{mock_console}/v1/runs/run_k3jq7x2mfa9vbn4z/cost", headers={"Authorization": "Bearer x"}, timeout=10).json()
    by = {(l["family"]): {p["epoch"]: p for p in l["details"]["placements"]} for l in api["lines"] if l["source"] == "compute"}
    page.goto(f"{mock_console}/runs/run_k3jq7x2mfa9vbn4z?tab=resources")
    card = page.locator(".run-placements-cost")
    card.locator("tbody tr").first.wait_for(timeout=20_000)
    rows = _rows(card)
    assert [r[0] for r in rows] == ["1", "2", "3"], rows
    for r in rows:
        e = int(r[0])
        compute = by["compute"][e]["amount"]
        assert Decimal(r[4].lstrip("$")) == Decimal(compute).quantize(Decimal("0.0001")), (r, compute)
        if e in by["block-storage"]:
            assert r[5] != "–", r
            total = Decimal(compute) + Decimal(by["block-storage"][e]["amount"])
        else:
            # Epoch 1's host never recorded its volumes: compute only, an en dash, not $0.
            assert r[5] == "–", r
            total = Decimal(compute)
        assert Decimal(r[6].lstrip("$")) == total.quantize(Decimal("0.0001")), (r, total)
    # The host links to its page.
    card.get_by_role("link", name="gp-eu-west-1-b2").click()
    page.wait_for_url("**/hosts/host_9p4tz6c1mh**")
    assert not page.errors, page.errors
