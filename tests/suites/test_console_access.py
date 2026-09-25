"""The console behind Cloudflare Access: luxd verifies Access's token and
takes whoever Access let in as an operator (docs/operators.md). A fake
Access (fake_access.py) signs tokens as Access does; luxd runs its real
verification against it."""

from __future__ import annotations

import pytest
import requests

from conftest import generic
from env import ALPINE_IMAGE
from fake_access import FakeAccess


@pytest.fixture(scope="module")
def access(env):
    """luxd restarted in cloudflare-access mode for this module, then back."""
    fake = FakeAccess(env.gateway)
    env.stop_luxd()
    env.start_luxd(LUX_CONSOLE_AUTH="cloudflare-access", LUX_CF_ACCESS_TEAM=fake.team, LUX_CF_ACCESS_AUD=fake.AUD)
    yield fake
    env.stop_luxd()
    env.start_luxd()
    fake.close()


def _get(env, path: str, **kw) -> requests.Response:
    return requests.get(env.luxd_url + path, timeout=10, **kw)


def test_an_access_user_is_an_operator(env, access, tenant_factory):
    t = tenant_factory()
    token = access.token("ada@example.com", "Ada Lovelace")
    # As Access forwards it: the header, or the browser's cookie.
    for kw in ({"headers": {"Cf-Access-Jwt-Assertion": token}}, {"cookies": {"CF_Authorization": token}}):
        me = _get(env, "/v1/whoami", **kw).json()
        assert me["operator"] and me["email"] == "ada@example.com" and me["name"] == "Ada Lovelace", me
        assert me["consoleAuth"] == "cloudflare-access"
    # Every tenant, and actions recorded as the person.
    run_id = t.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    hdr = {"Cf-Access-Jwt-Assertion": token}
    assert run_id in {r["id"] for r in _get(env, "/v1/runs?limit=1000", headers=hdr).json()["runs"]}
    r = requests.post(f"{env.luxd_url}/v1/runs/{run_id}/cancel", headers=hdr, timeout=10)
    assert r.status_code == 202, r.text
    assert [e["data"]["by"] for e in t.events(run_id, "cancel.requested")] == ["ada@example.com"]


@pytest.mark.parametrize("bad", ["expired", "wrong audience", "wrong issuer", "forged", "garbage"])
def test_invalid_access_tokens_are_refused(env, access, bad):
    from cryptography.hazmat.primitives.asymmetric import rsa
    token = {
        "expired": lambda: access.token("x@example.com", ttl=-60),
        "wrong audience": lambda: access.token("x@example.com", aud="another-app"),
        "wrong issuer": lambda: access.token("x@example.com", iss="https://evil.cloudflareaccess.com"),
        "forged": lambda: access.token("x@example.com", key=rsa.generate_private_key(public_exponent=65537, key_size=2048)),
        "garbage": lambda: "not.a.jwt",
    }[bad]()
    r = _get(env, "/v1/whoami", headers={"Cf-Access-Jwt-Assertion": token})
    assert r.status_code == 401, (bad, r.status_code, r.text)
    assert _get(env, "/v1/whoami").status_code == 401  # and none at all


def test_the_cookie_does_not_authorize_cross_site_actions(env, access, tenant_factory):
    """A browser sends the Access cookie with any request, a cross-site form
    post included: the cookie alone reads, but acts only from this origin."""
    t = tenant_factory()
    run_id = t.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    cookie = {"CF_Authorization": access.token("mallory-victim@example.com")}
    url = f"{env.luxd_url}/v1/runs/{run_id}/cancel"
    assert _get(env, "/v1/whoami", cookies=cookie).status_code == 200
    for site in (None, "cross-site", "same-site"):
        hdr = {"Sec-Fetch-Site": site} if site else {}
        r = requests.post(url, cookies=cookie, headers=hdr, timeout=10)
        assert r.status_code == 401, (site, r.status_code, r.text)
    assert t.get(run_id)["state"] != "cancelled"
    r = requests.post(url, cookies=cookie, headers={"Sec-Fetch-Site": "same-origin"}, timeout=10)
    assert r.status_code == 202, r.text


def test_keys_still_work_behind_access(env, access, lux):
    """The CLI and runners keep their keys: a key wins over a token."""
    me = lux.json("ls")
    assert me == [] or isinstance(me, list)
    r = _get(env, "/v1/whoami", headers={"Authorization": f"Bearer {lux.api_key}", "Cf-Access-Jwt-Assertion": "garbage"})
    assert r.status_code == 200 and not r.json()["operator"], r.text


def test_the_console_signs_in_through_access(env, access, browser):
    token = access.token("grace@example.com", "Grace Hopper")
    ctx = browser.new_context()
    ctx.add_cookies([{"name": "CF_Authorization", "value": token, "url": env.luxd_url}])
    page = ctx.new_page()
    errors = []
    page.on("pageerror", lambda e: errors.append(str(e)))
    page.goto(env.luxd_url + "/")
    # No key screen: the person's name, and operator pages.
    page.get_by_text("Grace Hopper", exact=True).wait_for(timeout=15_000)
    assert page.get_by_placeholder("lux_", exact=False).count() == 0
    assert page.get_by_role("link", name="Tenants").count() == 1
    assert not errors, errors
    ctx.close()
