"""A luxd outage longer than the lease (a machine reboot, say) loses no Run
and no host: time no luxd could hear heartbeats is not held against them."""

from __future__ import annotations

import psycopg

from conftest import generic
from env import ALPINE_IMAGE, wait_until


def test_a_running_run_survives_a_luxd_outage(env, lux, runners, hosts):
    runners.start(hosts[0])
    run_id = lux.submit(generic(ALPINE_IMAGE, "sh", "-c", 'i=0; while true; do i=$((i+1)); echo "tick-$i"; sleep 1; done'))
    lux.wait_output(run_id, "tick-2")

    env.stop_luxd()
    # The outage, without waiting through it: an hour ago was the last any
    # luxd, host or lease was heard of. A luxd that held that against them
    # would lose them all at its first reap.
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        conn.execute("UPDATE luxd_alive SET at = now() - interval '1 hour'")
        conn.execute("UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE state IN ('ready', 'draining')")
        conn.execute("UPDATE placements SET lease_expires_at = now() - interval '1 hour' WHERE state = 'running'")
        restarted = conn.execute("SELECT now()").fetchone()[0]
    env.start_luxd()

    # The runner reaches the new luxd and renews: host and lease fresh again.
    with psycopg.connect(env.owner_dsn, autocommit=True) as conn:
        wait_until(lambda: conn.execute(
            """SELECT EXISTS (SELECT 1 FROM placements p JOIN hosts h ON h.id = p.host_id
                 WHERE p.run_id = %s AND p.state = 'running' AND p.lease_expires_at > now()
                   AND h.state = 'ready' AND h.last_heartbeat > %s)""",
            (run_id, restarted)).fetchone()[0], 60, 0.5, "the host never renewed its heartbeat and lease")

    run = lux.get(run_id)
    assert run["state"] == "running", run
    assert len(run["placements"]) == 1, run["placements"]
    # And the same container goes on.
    seen = lux.logs(run_id).count("tick-")
    lux.wait_output(run_id, f"tick-{seen + 1}", timeout=30)
    lux.run("cancel", run_id)
