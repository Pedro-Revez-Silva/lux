"""The terminal's colours are the terminal's: choosing Solarized light or
dark repaints that terminal in place, keeps its shell and scrollback, is
remembered across a reload, and leaves the console's theme alone."""

from __future__ import annotations

import re

import pytest

RUN = "run_k3jq7x2mfa9vbn4z"
LIGHT_BG = "rgb(253, 246, 227)"  # Solarized base3
DARK_BG = "rgb(0, 43, 54)"  # Solarized base03


@pytest.fixture()
def page(browser, mock_console):
    ctx = browser.new_context(viewport={"width": 1440, "height": 900})
    # A signed-in session and a console theme of light, set before any script.
    ctx.add_init_script("sessionStorage.setItem('lux.key', 'luxk_mock'); if (!localStorage.getItem('lux.theme')) localStorage.setItem('lux.theme', 'light');")
    # xterm's DOM renderer, so the screen's text can be read back (WebGL
    # draws it into a canvas); the scheme applies to either renderer.
    ctx.add_init_script("""const gc = HTMLCanvasElement.prototype.getContext;
        HTMLCanvasElement.prototype.getContext = function (t, ...a) { return String(t).startsWith('webgl') ? null : gc.call(this, t, ...a); };""")
    p = ctx.new_page()
    p.sockets = []
    p.on("websocket", lambda ws: "/exec" in ws.url and p.sockets.append(ws.url))
    yield p
    ctx.close()


def _screen_bg(page) -> str:
    return page.eval_on_selector(".term-screen", "e => getComputedStyle(e).backgroundColor")


def _xterm_bg(page) -> str:
    # xterm paints its scrollable area with theme.background.
    return page.eval_on_selector(".term-screen .xterm-scrollable-element", "e => getComputedStyle(e).backgroundColor")


def _console(page) -> dict:
    return page.evaluate("""() => ({
        dataTheme: document.documentElement.dataset.theme ?? null,
        stored: localStorage.getItem('lux.theme'),
        bodyBg: getComputedStyle(document.body).backgroundColor,
        topbarBg: getComputedStyle(document.querySelector('.topbar') ?? document.body).backgroundColor,
        surface: getComputedStyle(document.documentElement).getPropertyValue('--bg-surface').trim(),
    })""")


def _screen_text(page) -> str:
    return page.evaluate("""() => {
        const rows = document.querySelector('.term-screen .xterm-rows');
        return rows ? rows.innerText : '';
    }""")


def _pick(page, label: str):
    page.get_by_role("radio", name=label).click()


def test_terminal_scheme_is_the_terminals_own(page, mock_console):
    page.goto(f"{mock_console}/runs/{RUN}/terminal")
    page.wait_for_selector(".term-screen .xterm-scrollable-element")
    # The mock's shell types its script on open; the last command's output ends it.
    page.wait_for_function("() => (document.querySelector('.term-screen .xterm-rows')?.innerText ?? '').includes('timeout=5')", timeout=45_000)
    page.wait_for_timeout(500)
    before = _console(page)
    assert before["dataTheme"] == "light" and before["stored"] == "light"
    assert page.get_by_role("radio", name="Match console").get_attribute("aria-checked") == "true"
    assert _xterm_bg(page) == LIGHT_BG and _screen_bg(page) == LIGHT_BG
    scrollback = _screen_text(page)
    sockets = list(page.sockets)
    assert len(sockets) == 1, sockets

    _pick(page, "Solarized dark")
    page.wait_for_function(f"() => getComputedStyle(document.querySelector('.term-screen .xterm-scrollable-element')).backgroundColor === '{DARK_BG}'")
    assert _screen_bg(page) == DARK_BG
    assert page.eval_on_selector(".term", "e => e.dataset.termScheme") == "dark"
    # Only the terminal changed: the console's theme, its key and its tokens stay.
    assert _console(page) == before
    assert page.evaluate("localStorage.getItem('lux.terminal.theme')") == "dark"
    # Same shell: no new socket, the screen's text is still there.
    page.wait_for_timeout(500)
    assert page.sockets == sockets
    assert _screen_text(page) == scrollback
    assert re.search(r"Connected", page.inner_text(".page-head")), "the shell is still connected"

    # Remembered across a reload (a reload is a new shell, by design).
    page.reload()
    page.wait_for_selector(".term-screen .xterm-scrollable-element")
    page.wait_for_timeout(500)
    assert page.get_by_role("radio", name="Solarized dark").get_attribute("aria-checked") == "true"
    assert _xterm_bg(page) == DARK_BG
    assert _console(page)["dataTheme"] == "light"

    # Match console follows the console: flip the console to dark, then back.
    _pick(page, "Solarized light")
    page.wait_for_function(f"() => getComputedStyle(document.querySelector('.term-screen .xterm-scrollable-element')).backgroundColor === '{LIGHT_BG}'")
    _pick(page, "Match console")
    assert page.evaluate("localStorage.getItem('lux.terminal.theme')") is None
    assert _xterm_bg(page) == LIGHT_BG


def test_a_bad_stored_scheme_follows_the_console(page, mock_console):
    page.context.add_init_script("localStorage.setItem('lux.theme', 'dark'); localStorage.setItem('lux.terminal.theme', 'mauve');")
    page.goto(f"{mock_console}/runs/{RUN}/terminal")
    page.wait_for_selector(".term-screen .xterm-scrollable-element")
    page.wait_for_timeout(500)
    assert page.get_by_role("radio", name="Match console").get_attribute("aria-checked") == "true"
    assert _xterm_bg(page) == DARK_BG
    assert _console(page)["dataTheme"] == "dark"
