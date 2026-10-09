"""Cost by label and by who submitted the Run, end to end: a Run records the
key it was submitted with, `/v1/costs` filters by its labels (label and
nolabel) and groups by submitter (group=key), `/v1/costs/labels` lists the
keys on costed Runs, and `lux costs` passes --label/--no-label/--by key.

Each Run is costed as it ends (its state change queues it), as in
test_costs_cli."""

from __future__ import annotations

import re
from urllib.parse import quote

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def _costed(lux, labels: dict) -> str:
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "2", labels=labels))
    lux.wait_state(run_id, "succeeded")
    wait_until(lambda: lux.api(f"/v1/runs/{run_id}/cost").json()["totals"], 60, 1, f"run {run_id} never got a cost line")
    return run_id


def _runs(lux, query: str) -> set[str]:
    body = lux.api(f"/v1/costs?since=1h&group=run{query}").json()
    return {r["group"]["run"] for r in body["totals"]}


def test_cost_labels_and_submitters(lux, tenant_factory, operator, runners, hosts):
    host = runners.start(hosts[0])
    lux.run("hosts", "price", host.name, "--hourly-price", "36", "--currency", "USD")
    # A value with "=", "," and non-ASCII is a value, never SQL text.
    odd = "a=b,c ünï"
    jer = _costed(lux, {"app": "jervasion", "repo": "absmartly/abs"})
    dude = _costed(lux, {"app": "dude", "note": odd})
    bare = _costed(lux, {})
    wait_until(lambda: _runs(lux, "") >= {jer, dude, bare}, 30, 1, "the summary never had all three Runs")

    # The Run says who submitted it: this tenant's key, by name.
    me = lux.api("/v1/whoami").json()
    sub = lux.api(f"/v1/runs/{jer}").json()["submittedBy"]
    assert sub["keyId"] == me["keyId"] and sub.get("keyName"), sub

    # Filters: one key's values OR together, different keys AND, nolabel is "not set".
    assert _runs(lux, "&label=app%3Djervasion") == {jer}
    assert _runs(lux, "&label=app%3Djervasion&label=app%3Ddude") == {jer, dude}
    assert _runs(lux, "&label=app%3Ddude&label=repo%3Dabsmartly%2Fabs") == set()
    assert _runs(lux, "&nolabel=app") == {bare}
    assert _runs(lux, "&label=" + quote(f"note={odd}")) == {dude}
    assert _runs(lux, "&label=" + quote("note=a=b")) == set()
    for bad in ("label=app", "label=" + quote("bad key=x"), "nolabel=" + quote("a b")):
        r = lux.api(f"/v1/costs?since=1h&{bad}")
        assert r.status_code == 400, (bad, r.status_code, r.text)

    # Grouped by a label: a Run without it is "(none)", never dropped.
    by_app = {r["group"]["label:app"] for r in lux.api("/v1/costs?since=1h&group=label:app").json()["totals"]}
    assert by_app >= {"jervasion", "dude", "(none)"}, by_app
    # Folded to the top value: the other app is the fold (other: true, read "(other)"), "(none)" is kept and not counted.
    # Either app can cost more (real runtimes), so exactly one of them is kept.
    top = lux.api("/v1/costs?since=1h&group=label:app&top=1").json()
    values = {r["group"]["label:app"] for r in top["totals"] if not r.get("other")}
    assert len(values & {"jervasion", "dude"}) == 1 and values - {"jervasion", "dude"} == {"(none)"}, top
    assert [r["group"]["label:app"] for r in top["totals"] if r.get("other")] == ["(other)"], top
    assert top["otherCount"] == {"USD": 1}, top
    assert lux.api("/v1/costs?since=1h&group=label:app&top=51").status_code == 422

    # The label keys on costed Runs, most Runs first; filtered like the summary.
    keys = {k["key"]: k["runs"] for k in lux.api("/v1/costs/labels?since=1h").json()["keys"]}
    assert keys.get("app", 0) >= 2 and keys.get("repo", 0) >= 1 and keys.get("note", 0) >= 1, keys
    keys = {k["key"] for k in lux.api("/v1/costs/labels?since=1h&label=app%3Djervasion").json()["keys"]}
    assert "note" not in keys and "repo" in keys, keys

    # By submitter: this tenant's key, named.
    body = lux.api("/v1/costs?since=1h&group=key").json()
    assert me["keyId"] in {r["group"]["key"] for r in body["totals"]}, body
    named = {k["id"]: k for k in body["keys"]}
    assert named[me["keyId"]].get("name"), named

    # The CLI: --label and --no-label filter, --by key names the key.
    out = lux.run("costs", "--since", "1h", "--by", "run", "--label", "app=dude").stdout
    assert dude in out and jer not in out and bare not in out, out
    out = lux.run("costs", "--since", "1h", "--by", "run", "--no-label", "app").stdout
    assert bare in out and jer not in out, out
    out = lux.run("costs", "--since", "1h", "--by", "key").stdout
    # The amount as the CLI prints it: digits, a decimal point, thousands separators, or "<0.0001".
    assert re.search(rf"^{re.escape(named[me['keyId']]['name'])}\s+[\d.,<]+ USD$", out, re.M), out

    # Another tenant sees none of it: no Runs, no label keys, no key names.
    other = tenant_factory()
    assert not other.api("/v1/costs?since=1h&group=key").json()["totals"]
    assert other.api("/v1/costs/labels?since=1h").json()["keys"] == []
    assert other.api(f"/v1/runs/{jer}").status_code == 404

    # A Run an operator submits for the tenant: the tenant sees an operator
    # key, unnamed; the operator sees its name.
    op_run = operator.submit(generic(ALPINE_IMAGE, "sleep", "2", labels={"app": "ops"}), "--tenant", lux.tenant_id)
    operator.wait_state(op_run, "succeeded")
    wait_until(lambda: op_run in _runs(lux, ""), 60, 1, "the operator's Run never had a cost")
    seen = lux.api(f"/v1/runs/{op_run}").json()["submittedBy"]
    assert seen.get("keyId") and "keyName" not in seen, seen
    # The operator names its key, also narrowed to the tenant.
    for q in ("", f"?tenant={lux.tenant_id}"):
        assert operator.api(f"/v1/runs/{op_run}{q}").json()["submittedBy"].get("keyName"), q
    tenant_keys = {k["id"]: k for k in lux.api("/v1/costs?since=1h&group=key").json()["keys"]}
    assert tenant_keys[seen["keyId"]].get("operator") and "name" not in tenant_keys[seen["keyId"]], tenant_keys
    op_keys = {k["id"]: k for k in operator.api(f"/v1/costs?since=1h&group=key&tenant={lux.tenant_id}").json()["keys"]}
    assert op_keys[seen["keyId"]].get("name"), op_keys
