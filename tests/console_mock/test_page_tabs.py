"""Pill tabs on the Host page and the Overview: a click switches what the
page shows and writes ?tab=; a ?tab= link opens on that tab; the first tab
has no ?tab= at all."""

from __future__ import annotations

from urllib.parse import parse_qs, urlparse

import pytest

HOST = "host_7f2cq9m1x0"


@pytest.fixture()
def page(browser, mock_console):
    ctx = browser.new_context(viewport={"width": 1440, "height": 900})
    ctx.add_init_script("sessionStorage.setItem('lux.key', 'luxk_mock'); localStorage.setItem('lux.theme', 'light');")
    p = ctx.new_page()
    yield p
    ctx.close()


def _tab_param(page) -> str | None:
    return parse_qs(urlparse(page.url).query).get("tab", [None])[0]


def _selected(page) -> str:
    return page.locator('[role="tab"][aria-selected="true"]').inner_text().split("\n")[0].strip()


def _card(page, title: str):
    return page.locator(".card", has=page.get_by_text(title, exact=True).and_(page.locator(".card-title")))


def _cards(page) -> list[str]:
    return page.locator(".card-title").all_inner_texts()


def test_host_tabs_switch_content_and_deep_link(page, mock_console):
    page.goto(f"{mock_console}/hosts/{HOST}")
    page.wait_for_selector(".card-title")
    assert _selected(page) == "Overview" and _tab_param(page) is None
    assert {"Details", "Lifecycle", "Live placements"} <= set(_cards(page))
    assert "agent-refactor-42" in _card(page, "Live placements").inner_text()

    page.get_by_role("tab", name="Metrics").click()
    _card(page, "Memory").wait_for()
    assert _tab_param(page) == "metrics"
    assert "Details" not in _cards(page)
    assert page.locator(".tschart-plot canvas").count() > 0

    page.get_by_role("tab", name="Events").click()
    _card(page, "Events").locator("tbody tr").first.wait_for()
    assert _tab_param(page) == "events"
    assert "host.placement_assigned" in _card(page, "Events").inner_text()
    assert "Memory" not in _cards(page)

    # A link straight to a tab opens on it, and back to the first drops ?tab=.
    page.goto(f"{mock_console}/hosts/{HOST}?tab=runs")
    _card(page, "Recent runs on this host").locator("tbody tr").first.wait_for()
    assert _selected(page) == "Runs"
    assert _cards(page) == ["Recent runs on this host"]
    page.get_by_role("tab", name="Cost").click()
    _card(page, "Rate periods").wait_for()
    assert _tab_param(page) == "cost"
    page.get_by_role("tab", name="Overview").click()
    _card(page, "Details").wait_for()
    assert _tab_param(page) is None


def test_overview_tabs_switch_content_and_deep_link(page, mock_console):
    page.goto(f"{mock_console}/")
    page.wait_for_selector(".stat")
    assert _selected(page) == "Activity" and _tab_param(page) is None
    assert page.locator(".stat-label", has_text="Running").count() == 1
    assert "Runs" in _cards(page) and "Stored" not in _cards(page)
    assert page.locator(".cost-panel").count() == 0

    page.get_by_role("tab", name="Storage").click()
    _card(page, "Stored").wait_for()
    assert _tab_param(page) == "storage"
    assert page.locator(".stat").count() == 0
    assert _cards(page) == ["Stored"]

    page.get_by_role("tab", name="Cost").click()
    page.locator(".cost-panel").wait_for()
    assert _tab_param(page) == "cost"
    assert "Stored" not in _cards(page)

    page.goto(f"{mock_console}/?tab=storage")
    _card(page, "Stored").wait_for()
    assert _selected(page) == "Storage"
    assert page.locator(".cost-panel").count() == 0
    page.get_by_role("tab", name="Activity").click()
    page.locator(".stat").first.wait_for()
    assert _tab_param(page) is None
