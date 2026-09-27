"""Cloudflare Access authentication with an operator allowlist and default tenant."""

from __future__ import annotations

import pytest
import requests

from conftest import Runners, generic
from env import ALPINE_IMAGE, wait_until
from fake_access import FakeAccess


@pytest.fixture(scope="module")
def access(env):
    """luxd restarted in cloudflare-access mode for this module, then back."""
    fake = FakeAccess(env.gateway)
    env.stop_luxd()
    tenant = env.luxd_admin("create-tenant", "--name", "access-default")
    env.start_luxd(LUX_CONSOLE_AUTH="cloudflare-access", LUX_CF_ACCESS_TEAM=fake.team, LUX_CF_ACCESS_AUD=fake.AUD,
                   LUX_CF_ACCESS_OPERATORS="ada@example.com,grace@example.com,mallory-victim@example.com",
                   LUX_CF_ACCESS_DEFAULT_TENANT=tenant["tenantId"])
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


def test_operator_allowlist_ignores_email_case(env, access, tenant_factory):
    other = tenant_factory()
    run_id = other.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    hdr = {"Cf-Access-Jwt-Assertion": access.token("ADA@EXAMPLE.COM")}
    me = _get(env, "/v1/whoami", headers=hdr)
    assert me.status_code == 200 and me.json()["operator"] is True, me.text
    assert me.json()["email"] == "ADA@EXAMPLE.COM"
    runs = _get(env, "/v1/runs", headers=hdr, params={"tenant": other.tenant_id, "limit": 1000})
    assert runs.status_code == 200 and run_id in {r["id"] for r in runs.json()["runs"]}, runs.text
    other.run("cancel", run_id)


def test_access_tenant_is_isolated(env, access, tenant_factory):
    other = tenant_factory()
    run_id = other.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    hdr = {"Cf-Access-Jwt-Assertion": access.token("ordinary@example.com", "Ordinary")}
    me = _get(env, "/v1/whoami", headers=hdr)
    assert me.status_code == 200, me.text
    assert me.json()["operator"] is False and me.json()["tenant"] == "access-default"
    assert me.json()["email"] == "ordinary@example.com" and me.json()["scopes"] == ["admin"]
    mine = requests.post(f"{env.luxd_url}/v1/runs", headers=hdr,
                         json=generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}), timeout=10)
    assert mine.status_code in (200, 201, 202), mine.text
    mine_id = mine.json()["id"]
    own = _get(env, "/v1/runs/" + mine_id, headers=hdr)
    assert own.status_code == 200 and own.json()["id"] == mine_id, own.text
    assert mine_id in {r["id"] for r in _get(env, "/v1/runs?limit=1000", headers=hdr).json()["runs"]}
    assert run_id not in {r["id"] for r in _get(env, "/v1/runs?limit=1000&tenant=" + other.tenant_id,
                                                   headers=hdr).json()["runs"]}
    assert _get(env, "/v1/runs/" + run_id, headers=hdr).status_code == 404
    assert _get(env, "/v1/tenants", headers=hdr).status_code == 403
    r = requests.post(f"{env.luxd_url}/v1/runs/{run_id}/cancel", headers=hdr, timeout=10)
    assert r.status_code == 404, r.text
    cancel = requests.post(f"{env.luxd_url}/v1/runs/{mine_id}/cancel", headers=hdr, timeout=10)
    assert cancel.status_code == 202, cancel.text
    events = _get(env, f"/v1/runs/{mine_id}/events", headers=hdr)
    assert events.status_code == 200, events.text
    assert [e["data"]["by"] for e in events.json()["events"] if e["type"] == "cancel.requested"] == ["ordinary@example.com"]
    other.run("cancel", run_id)


def test_access_status_and_history_are_tenant_scoped(env, access, tenant_factory):
    other = tenant_factory()
    hdr = {"Cf-Access-Jwt-Assertion": access.token("ordinary@example.com")}
    operator = {"Cf-Access-Jwt-Assertion": access.token("ada@example.com")}
    spec = generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}})
    mine = requests.post(f"{env.luxd_url}/v1/runs", headers=hdr, json=spec, timeout=10)
    assert mine.status_code in (200, 201, 202), mine.text
    own_id = mine.json()["id"]
    other_ids = [other.submit(spec) for _ in range(2)]
    try:
        for path in ("/v1/status", f"/v1/status?tenant={other.tenant_id}"):
            r = _get(env, path, headers=hdr)
            assert r.status_code == 200 and r.json()["queued"] == 1, r.text
        foreign = _get(env, "/v1/status", headers=operator, params={"tenant": other.tenant_id})
        assert foreign.status_code == 200 and foreign.json()["queued"] == 2, foreign.text

        def sampled(headers, tenant=None):
            r = _get(env, "/v1/history", headers=headers,
                     params={"since": "5m", "res": 0, **({"tenant": tenant} if tenant else {})})
            assert r.status_code == 200, r.text
            return r.json()["samples"]

        wait_until(lambda: any(s.get("queued") == 2 for s in sampled(operator, other.tenant_id)),
                   30, 1, "foreign tenant never had two queued Runs in history")
        own_samples = sampled(hdr)
        narrowed_samples = sampled(hdr, other.tenant_id)
        assert own_samples and any(s.get("queued") == 1 for s in own_samples), own_samples
        assert narrowed_samples and any(s.get("queued") == 1 for s in narrowed_samples), narrowed_samples
        assert all(s.get("queued", 0) < 2 for s in own_samples + narrowed_samples)
    finally:
        requests.post(f"{env.luxd_url}/v1/runs/{own_id}/cancel", headers=hdr, timeout=10)
        for run_id in other_ids:
            other.run("cancel", run_id)


def test_access_hosts_and_pools_are_tenant_scoped(env, access, tenant_factory, hosts):
    other = tenant_factory()
    hdr = {"Cf-Access-Jwt-Assertion": access.token("ordinary@example.com")}
    operator = {"Cf-Access-Jwt-Assertion": access.token("ada@example.com")}
    own_name, foreign_name = "access-own-pool", f"access-foreign-{other.tenant_id[-6:]}"
    own_host, foreign_host = "access-own-host", "access-foreign-host"
    own = Runners(env, other)
    foreign = Runners(env, other)
    tenants = _get(env, "/v1/tenants", headers=operator)
    assert tenants.status_code == 200, tenants.text
    default_id = next(t["id"] for t in tenants.json()["tenants"] if t["name"] == "access-default")
    own_token = env.luxd_admin("create-host-token", "--tenant", default_id)["token"]
    other_pool = {"name": foreign_name, "provider": "static", "minHosts": 0, "maxHosts": 0, "warmHosts": 0}
    created = requests.post(f"{env.luxd_url}/v1/pools", headers={"Authorization": f"Bearer {other.api_key}"},
                            json=other_pool, timeout=10)
    assert created.status_code == 200, created.text
    try:
        own.start(hosts[0], token=own_token, name=own_host, wait=False)
        wait_until(lambda: any(h["name"] == own_host and h["state"] == "ready"
                               for h in _get(env, "/v1/hosts", headers=hdr).json()["hosts"]),
                   30, 0.3, "Access tenant host never became ready")
        foreign.start(hosts[1], name=foreign_host)
        foreign_id = next(h["id"] for h in other.json("hosts", "ls") if h["name"] == foreign_host)
        for path in ("/v1/hosts", f"/v1/hosts?tenant={other.tenant_id}"):
            r = _get(env, path, headers=hdr)
            assert r.status_code == 200, r.text
            names = {h["name"] for h in r.json()["hosts"]}
            assert own_host in names and foreign_host not in names, r.text
        for suffix in ("", f"?tenant={other.tenant_id}"):
            assert _get(env, f"/v1/hosts/{foreign_id}{suffix}", headers=hdr).status_code == 404
            assert _get(env, f"/v1/hosts/{foreign_id}/history{suffix}", headers=hdr).status_code == 404
        visible = _get(env, f"/v1/hosts/{foreign_id}", headers=operator)
        assert visible.status_code == 200 and visible.json()["name"] == foreign_host, visible.text
        own_visible = _get(env, f"/v1/hosts/{own_host}/history", headers=hdr)
        assert own_visible.status_code == 200, own_visible.text

        pool = {"name": own_name, "provider": "static", "minHosts": 0, "maxHosts": 0, "warmHosts": 0}
        created = requests.post(f"{env.luxd_url}/v1/pools", headers=hdr, json=pool, timeout=10)
        assert created.status_code == 200, created.text
        pool["maxHosts"] = 2
        updated = requests.post(f"{env.luxd_url}/v1/pools", headers=hdr, json=pool, timeout=10)
        assert updated.status_code == 200, updated.text
        for path in ("/v1/pools", f"/v1/pools?tenant={other.tenant_id}"):
            r = _get(env, path, headers=hdr)
            assert r.status_code == 200, r.text
            pools = {p["name"]: p for p in r.json()["pools"]}
            assert pools[own_name]["maxHosts"] == 2 and foreign_name not in pools, r.text
        forbidden = requests.delete(f"{env.luxd_url}/v1/pools/{foreign_name}", headers=hdr,
                                    params={"tenant": other.tenant_id}, timeout=10)
        assert forbidden.status_code == 404, forbidden.text
        assert any(p["name"] == foreign_name for p in other.json("pools", "ls"))
        deleted = requests.delete(f"{env.luxd_url}/v1/pools/{own_name}", headers=hdr, timeout=10)
        assert deleted.status_code == 204, deleted.text
        assert own_name not in {p["name"] for p in _get(env, "/v1/pools", headers=hdr).json()["pools"]}
    finally:
        own.stop_all()
        foreign.stop_all()
        requests.delete(f"{env.luxd_url}/v1/pools/{own_name}", headers=hdr, timeout=10)
        requests.delete(f"{env.luxd_url}/v1/pools/{foreign_name}",
                        headers={"Authorization": f"Bearer {other.api_key}"}, timeout=10)


def test_missing_default_tenant_denies_nonoperators(env, access):
    env.stop_luxd()
    env.start_luxd(LUX_CONSOLE_AUTH="cloudflare-access", LUX_CF_ACCESS_TEAM=access.team, LUX_CF_ACCESS_AUD=access.AUD,
                   LUX_CF_ACCESS_OPERATORS="ada@example.com", LUX_CF_ACCESS_DEFAULT_TENANT="missing-access-tenant")
    try:
        tenant = _get(env, "/v1/whoami", headers={"Cf-Access-Jwt-Assertion": access.token("ordinary@example.com")})
        assert tenant.status_code == 403, tenant.text
        operator = _get(env, "/v1/whoami", headers={"Cf-Access-Jwt-Assertion": access.token("ada@example.com")})
        assert operator.status_code == 200 and operator.json()["operator"], operator.text
    finally:
        env.stop_luxd()
        env.start_luxd(LUX_CONSOLE_AUTH="cloudflare-access", LUX_CF_ACCESS_TEAM=access.team, LUX_CF_ACCESS_AUD=access.AUD,
                       LUX_CF_ACCESS_OPERATORS="ada@example.com,grace@example.com,mallory-victim@example.com",
                       LUX_CF_ACCESS_DEFAULT_TENANT="access-default")


@pytest.mark.parametrize("email", ["", " ADA@example.com", "Ada <ada@example.com>", "ada@example.com.evil"])
def test_access_does_not_promote_bad_or_unlisted_email(env, access, email):
    r = _get(env, "/v1/whoami", headers={"Cf-Access-Jwt-Assertion": access.token(email)})
    if not email or email != email.strip() or "<" in email:
        assert r.status_code == 401, r.text
    else:
        assert r.status_code == 200 and not r.json()["operator"], r.text



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
