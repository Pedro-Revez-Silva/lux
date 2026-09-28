"""Run costs through the CLI: a static host's price becomes the compute
line of a Run on it, which `lux cost`, `lux costs --by family` and the COST
column of `lux ls` show.

The Run is costed as it ends: its state change queues it, and the drainer
(every 2s, woken by the Run's events) writes the compute line and its
hourly row. No cost tick (every 2m) is waited for."""

from __future__ import annotations

import re
from decimal import ROUND_HALF_EVEN, Decimal

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_cost_cli(lux, tenant_factory, operator, runners, hosts):
    host = runners.start(hosts[0])
    # 36 USD an hour: a Run of a few seconds costs a few cents, never
    # rounded away at 4 decimals.
    lux.run("hosts", "price", host.name, "--hourly-price", "36", "--currency", "USD")
    run_id = lux.submit(generic(ALPINE_IMAGE, "sleep", "3"))
    lux.wait_state(run_id, "succeeded")

    cost = wait_until(lambda: (lambda c: c if c["status"] != "pending" and c["lines"] else None)(
        lux.json("cost", run_id)), 60, 1, f"run {run_id} never got a cost line")
    compute = [l for l in cost["lines"] if l["source"] == "compute"]
    assert compute and compute[0]["family"] == "compute" and compute[0]["currency"] == "USD", cost
    total = next(t for t in cost["totals"] if t["currency"] == "USD")
    assert float(total["amount"]) > 0, total

    out = lux.run("cost", run_id).stdout
    assert out.count("list price") == 1, out
    assert re.search(rf"^status:\s+{cost['status']}\b", out, re.M), out
    assert re.search(r"^\S+ USD\s+\S+ USD\s+\S+ USD$", out, re.M), out  # total, final, estimate
    assert re.search(r"^Compute \(compute\)\s+[\d.<]+ USD", out, re.M), out
    assert re.search(r"^SOURCE\s+STATUS\s+ANSWERED$", out, re.M), out
    assert re.search(r"^compute\s+(ok|final|incomplete)\s+\w{3} \d", out, re.M), out

    # The summary's hourly rows are written with the line.
    def by_family():
        s = lux.json("costs", "--since", "1h", "--by", "family")
        return s if any(r["group"].get("family") == "compute" for r in s["totals"]) else None
    summary = wait_until(by_family, 30, 1, "lux costs never showed compute")
    row = next(r for r in summary["totals"] if r["group"]["family"] == "compute")
    assert row["currency"] == "USD" and float(row["amount"]) > 0, summary
    out = lux.run("costs", "--since", "1h", "--by", "family").stdout
    assert out.count("list price") == 1, out
    assert re.search(r"^FAMILY\s+TOTAL$", out, re.M), out
    assert re.search(r"^compute\s+[\d.<]+ USD$", out, re.M), out
    # A tenant never sees the unallocated total.
    assert "unallocated" not in out and "unallocated" not in summary, out

    # The Runs list: one currency is its amount, as `lux cost` rounds it.
    # The Run has ended, so its compute line no longer changes.
    cost = lux.json("cost", run_id)
    listed = next(r for r in lux.json("ls") if r["id"] == run_id)["cost"]
    assert listed == {"status": cost["status"], "totals": cost["totals"]}, (listed, cost)
    ls = lux.run("ls").stdout
    shown = re.search(r"^TOTAL\s+FINAL\s+ESTIMATE\n(\S+) USD", lux.run("cost", run_id).stdout, re.M)
    assert shown, out
    assert re.search(rf"^{run_id}\s.*\s~?{re.escape(shown.group(1))} USD\s", ls, re.M), ls

    # A Run nothing has reported for is — (pending), never 0.
    pending = lux.submit(generic(ALPINE_IMAGE, "true", placement={"requires": {"nowhere": "yes"}}))
    ls = lux.run("ls").stdout
    assert re.search(rf"^{pending}\s.*\s—\s", ls, re.M), ls
    assert re.search(r"^total:\s+—$", lux.run("cost", pending).stdout, re.M)
    lux.run("cancel", pending)

    # Another tenant sees neither the Run's cost nor its summary.
    other = tenant_factory()
    assert other.run("cost", run_id, check=False).returncode == 3
    assert not other.json("costs", "--since", "1h")["totals"]

    # An operator narrowed to the tenant sees what it sees; without a
    # tenant, also the hosts' cost charged to no Run.
    narrowed = operator.json("costs", "--since", "1h", "--by", "run", "--tenant", lux.tenant_id)
    assert [r["group"]["run"] for r in narrowed["totals"]] == [run_id] and "unallocated" not in narrowed, narrowed
    # The database is the session's: other tests' hosts may add to these.
    assert any(r["currency"] == "USD" for r in operator.json("costs", "--since", "1h")["unallocated"])
    out = operator.run("costs", "--since", "1h", "--by", "host").stdout
    assert re.search(r"^unallocated \(hosts' cost charged to no Run\): .*[\d.<]+ USD", out, re.M), out
    assert re.search(r"^HOST\s+ALLOCATED\s+UNALLOCATED$", out, re.M), out

    # The priced host's row shows luxd's amounts as `lux costs` rounds them.
    # Its hours refresh on their own schedule, so the text is compared with
    # a response that did not change around it.
    host_id = lux.json("hosts", "get", host.name)["id"]

    def host_row():
        return next((h for h in operator.json("costs", "--since", "1h", "--by", "host")["hosts"]
                     if h["hostId"] == host_id and h["currency"] == "USD"), None)

    def shown_host():
        before = host_row()
        text = operator.run("costs", "--since", "1h", "--by", "host").stdout
        after = host_row()
        return (after, text) if before and before == after else None
    row, out = wait_until(shown_host, 30, 1, f"host {host_id} never had a stable allocation row")
    allocated, unallocated = money(row["allocated"]), money(row["unallocated"])
    assert (allocated, unallocated) != ("0", "0"), row
    want = rf"^{re.escape(host_id)}\s+{re.escape(allocated)} USD\s+{re.escape(unallocated)} USD$"
    assert re.search(want, out, re.M), (row, out)


def money(amount: str) -> str:
    """The CLI's display rounding: half-even to 4 decimals, trailing zeros
    trimmed, a non-zero amount below that shown as a bound."""
    d = Decimal(amount)
    q = d.quantize(Decimal("0.0001"), rounding=ROUND_HALF_EVEN)
    if q == 0 and d != 0:
        return "<0.0001" if d > 0 else ">-0.0001"
    s = format(q, "f")
    return s.rstrip("0").rstrip(".") if "." in s else s
