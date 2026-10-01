"""Typed keys are checked before storage; acceptance opens a new document
to drop password-manager UI. Uses a separate page without test_console.py's
key-on-every-document script, so tests exercise manual sign-in."""

from __future__ import annotations

import pytest
from playwright.sync_api import expect

pytestmark = pytest.mark.console


@pytest.fixture
def page(browser):
    ctx = browser.new_context(viewport={"width": 1400, "height": 900})
    pg = ctx.new_page()
    pg.errors = []
    pg.on("pageerror", lambda e: pg.errors.append(str(e)))
    # The keyless whoami (is luxd behind Access?) and a refused key both 401.
    pg.on("console", lambda m: m.type == "error" and "401" not in m.text and pg.errors.append(m.text))
    pg.documents = []
    pg.on("request", lambda r: r.is_navigation_request() and r.frame == pg.main_frame and pg.documents.append(r.url))
    yield pg
    ctx.close()


def _sign_in_screen(page, url: str):
    page.goto(url)
    page.get_by_placeholder("lux_", exact=False).wait_for(timeout=10_000)
    # Gone once anything replaces this document.
    page.evaluate("window.signInDocument = 'original'")
    page.documents.clear()


def _submit(page, key: str):
    page.get_by_placeholder("lux_", exact=False).fill(key)
    page.get_by_role("button", name="Sign in").click()


def _stored_key(page):
    return page.evaluate("sessionStorage.getItem('lux.key')")


def test_a_refused_key_stays_on_the_form(page, env, operator):
    _sign_in_screen(page, env.luxd_url + "/runs")
    _submit(page, "luxk_not-a-key")
    expect(page.get_by_role("alert")).to_contain_text("That key was not accepted")
    assert _stored_key(page) is None
    assert page.evaluate("window.signInDocument") == "original"
    assert page.documents == []

    # The right key: one new document, at the page it was headed for.
    _submit(page, operator.api_key)
    expect(page.locator(".sidebar-user")).to_contain_text("Operator key", timeout=15_000)
    assert page.evaluate("window.signInDocument") is None
    assert _stored_key(page) == operator.api_key
    assert page.documents == [env.luxd_url + "/runs"]
    assert page.errors == []


def test_a_403_is_a_refusal_too(page, env):
    _sign_in_screen(page, env.luxd_url + "/")
    page.route("**/v1/whoami", lambda route: route.fulfill(status=403, content_type="application/json",
                                                          body='{"error":{"code":"forbidden","message":"no"}}'))
    _submit(page, "luxk_forbidden")
    expect(page.get_by_role("alert")).to_contain_text("That key was not accepted")
    assert _stored_key(page) is None
    assert page.evaluate("window.signInDocument") == "original"
    assert page.documents == []


def test_a_failed_check_can_be_retried(page, env, operator):
    _sign_in_screen(page, env.luxd_url + "/")
    page.route("**/v1/whoami", lambda route: route.fulfill(status=503, content_type="text/html", body="<h1>Unavailable</h1>"))
    _submit(page, operator.api_key)
    expect(page.get_by_role("alert")).to_contain_text("Could not check that key")
    assert _stored_key(page) is None
    assert page.evaluate("window.signInDocument") == "original"
    expect(page.get_by_role("button", name="Sign in")).to_be_enabled()
    assert page.documents == []

    # Again, held mid-check: the key is on the request but kept nowhere yet.
    page.unroute("**/v1/whoami")
    held = []
    page.route("**/v1/whoami", lambda route: held.append(route))
    with page.expect_request("**/v1/whoami") as checking:
        page.get_by_role("button", name="Sign in").click()
    assert checking.value.headers["authorization"] == f"Bearer {operator.api_key}"
    expect(page.get_by_role("button", name="Checking…")).to_be_disabled()
    assert _stored_key(page) is None
    assert page.evaluate("window.signInDocument") == "original"
    assert page.documents == []

    assert len(held) == 1
    held[0].continue_()
    page.unroute("**/v1/whoami")
    expect(page.locator(".sidebar-user")).to_contain_text("Operator key", timeout=15_000)
    assert page.evaluate("window.signInDocument") is None
    assert page.documents == [env.luxd_url + "/"]
