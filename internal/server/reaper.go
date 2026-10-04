package server

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// reaperLoop enforces time: expired leases, lost hosts, Run timeouts, and
// retention of the blobs of finished Runs.
func (s *Server) reaperLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, f := range []func(context.Context) error{s.reapLeases, s.reapHosts, s.reapTimeouts, s.reapExpiry, s.reapSuperseded, s.reapRetention, s.reapOutdatedStaticHosts} {
			if err := f(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("reaper", "err", err)
			}
		}
	}
}

// aliveEvery is how often luxd records itself alive: a sixth of the lease,
// at most every 5s (the default 30s lease), at least every tick.
func (s *Server) aliveEvery() time.Duration {
	return max(min(s.cfg.LeaseDuration/6, 5*time.Second), s.cfg.Tick)
}

// aliveGap is the longest luxd may go without recording itself alive
// before the time counts as a gap: two records missed, a third of the
// lease. A shorter outage needs no allowance: runners reconnect within a
// second of a luxd that restarted, and renew within a heartbeat interval.
// Every luxd should share LUX_LEASE and LUX_TICK, or they judge gaps
// differently.
func (s *Server) aliveGap() time.Duration {
	return 2 * s.aliveEvery()
}

// aliveLoop records, every aliveEvery, that a luxd is running and reaches
// Postgres, and when one came back from a gap (luxd_alive). It is apart
// from reaperLoop, so a slow reap is not taken for a gap.
func (s *Server) aliveLoop(ctx context.Context) {
	// Rows of luxds gone a day, once per process: each start adds one.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// The newest row stays: it is the evidence of a gap longer than
		// the day, until recordAlive has ended that gap.
		_, err := tx.Exec(ctx, `DELETE FROM luxd_alive WHERE at < now() - interval '1 day'
			AND at < (SELECT max(at) FROM luxd_alive)`)
		return err
	}); err != nil && ctx.Err() == nil {
		s.log.Warn("pruning luxd_alive", "err", err)
	}
	// The first record at once: a luxd back from a gap says so before its
	// first reap.
	t := time.NewTicker(s.aliveEvery())
	defer t.Stop()
	for {
		gap, err := s.recordAlive(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.Warn("recording luxd alive", "err", err)
		case err == nil && gap != nil:
			s.log.Warn("no luxd heard heartbeats for a while: no host or lease is lost for it", "gap", gap.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// recordAlive records this luxd alive, and returns the gap it ends, if
// any: the time since any luxd last recorded itself. Each luxd writes only
// its own row, so none waits on another. A record that cannot be made
// within the gap gives up.
func (s *Server) recordAlive(ctx context.Context) (gap *time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.aliveGap())
	defer cancel()
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `WITH g AS (
				SELECT clock_timestamp() AS now, nullif(clock_timestamp() - max(at) > $2::interval, false) AS gapped,
					clock_timestamp() - max(at) AS gap FROM luxd_alive
			), up AS (
				INSERT INTO luxd_alive (instance, at, resumed_at)
				SELECT $1, now, CASE WHEN gapped THEN now END FROM g
				ON CONFLICT (instance) DO UPDATE SET at = EXCLUDED.at,
					resumed_at = coalesce(EXCLUDED.resumed_at, luxd_alive.resumed_at)
			)
			SELECT CASE WHEN gapped THEN gap END FROM g`, instanceID, interval(s.aliveGap())).Scan(&gap)
		return err
	})
	return gap, err
}

// heardSQL is a condition: a missing heartbeat, at this instant, is the
// runner's to answer for. Not during a gap no luxd recorded itself alive
// (every luxd stopped or hung, or Postgres unreachable), nor for a lease
// after one came back plus the runner's longest wait between reconnects,
// so its runners reach luxd again first. No record yet is no gap. It is
// part of every staleness test, and clock_timestamp() rather than now(),
// so a test re-run after lock waits sees a gap during them.
func (s *Server) heardSQL() string {
	return fmt.Sprintf(`coalesce((SELECT max(at) >= clock_timestamp() - '%s'::interval
			AND coalesce(max(resumed_at) < clock_timestamp() - '%s'::interval, true) FROM luxd_alive), true)`,
		interval(s.aliveGap()), interval(s.cfg.LeaseDuration+proto.MaxReconnectWait))
}

// reapLeases: a placement whose lease expired is lost.
func (s *Server) reapLeases(ctx context.Context) error {
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Discover candidates without locking placements; renewal may win before
		// the Run lock, so check expiry again after acquiring it.
		expired, err := livePlacements(ctx, tx, "p.lease_expires_at < now() AND "+s.heardSQL())
		if err != nil {
			return err
		}
		runs := make([]string, 0, len(expired))
		for _, p := range expired {
			runs = append(runs, p.RunID)
		}
		if err := lockReaperRuns(ctx, tx, runs); err != nil {
			return err
		}
		if err := lockCostHosts(ctx, tx, runs); err != nil {
			return err
		}
		expired, err = livePlacements(ctx, tx, "p.lease_expires_at < now() AND p.run_id = ANY($1) AND "+s.heardSQL(), runs)
		if err != nil {
			return err
		}
		var later laterEvents
		for _, p := range expired {
			if err := s.placementLost(ctx, tx, p.RunID, p.Epoch, "lease expired: host stopped heartbeating", &later); err != nil {
				return err
			}
		}
		n = len(expired)
		return later.write()
	})
	if err == nil && n > 0 {
		s.Kick()
	}
	return err
}

func lockReaperRuns(ctx context.Context, tx pgx.Tx, runs []string) error {
	if len(runs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = ANY($1) ORDER BY id FOR UPDATE`, runs)
	return err
}

// reapHosts: a host without heartbeats is lost, and with it its live
// placements and the snapshots only it held.
func (s *Server) reapHosts(ctx context.Context) error {
	var lost []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM hosts
			WHERE state IN ('ready', 'draining') AND last_heartbeat < now() - $1::interval AND `+s.heardSQL()+`
			ORDER BY id`, interval(s.cfg.LeaseDuration))
		if err != nil {
			return err
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(candidates) == 0 {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT DISTINCT run_id FROM placements
			WHERE host_id = ANY($1) AND state IN `+livePlacementStates+` ORDER BY run_id`, candidates)
		if err != nil {
			return err
		}
		runs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := lockReaperRuns(ctx, tx, runs); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT host_id FROM (
			SELECT unnest($1::text[]) AS host_id
			UNION SELECT host_id FROM placements WHERE run_id = ANY($2)
		) all_hosts ORDER BY host_id`, candidates, runs)
		if err != nil {
			return err
		}
		hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, host := range hosts {
			if err := lockCostHost(ctx, tx, host); err != nil {
				return err
			}
		}
		// A placement assigned after discovery has no Run lock here. Leave
		// these hosts for the next pass rather than lock a Run after a host.
		rows, err = tx.Query(ctx, `SELECT DISTINCT run_id FROM placements
			WHERE host_id = ANY($1) AND state IN `+livePlacementStates+` ORDER BY run_id`, candidates)
		if err != nil {
			return err
		}
		currentRuns, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		locked := make(map[string]bool, len(runs))
		for _, run := range runs {
			locked[run] = true
		}
		for _, run := range currentRuns {
			if !locked[run] {
				return nil
			}
		}
		// Heartbeats may have refreshed a candidate while the locks were
		// acquired. Only retire hosts still stale under the host advisory lock.
		for _, host := range candidates {
			if err := tx.QueryRow(ctx, `UPDATE hosts SET state = 'lost', lost_at = now(), state_reason = 'missed heartbeats'
				WHERE id = $1 AND state IN ('ready', 'draining')
				  AND last_heartbeat < now() - $2::interval AND `+s.heardSQL()+`
				RETURNING id`, host, interval(s.cfg.LeaseDuration)).Scan(&host); err != nil {
				if err == pgx.ErrNoRows {
					continue
				}
				return err
			}
			lost = append(lost, host)
		}
		if len(lost) == 0 {
			return nil
		}
		if err := hostsGone(ctx, tx, lost); err != nil {
			return err
		}
		// The events after every row: event streams come last (infraevents.go).
		var later laterEvents
		for _, host := range lost {
			later.host(ctx, tx, host, evLost, map[string]any{"reason": "missed heartbeats"})
		}
		// A fresh assignment's lease can outlast its host's heartbeat.
		live, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", lost)
		if err != nil {
			return err
		}
		for _, p := range live {
			if err := s.placementLost(ctx, tx, p.RunID, p.Epoch, "host lost: missed heartbeats", &later); err != nil {
				return err
			}
		}
		return later.write()
	})
	if err == nil && len(lost) > 0 {
		s.log.Warn("hosts lost", "hosts", lost)
		s.Kick()
	}
	return err
}

// reapTimeouts stops Runs that exceeded their timeout: time spent running,
// summed over placements (each from reaching running to its end, or now).
// Time stopped, lost or waiting for a host does not count, so a Run parked
// for days and resumed keeps what it had left. No timeout: no limit.
func (s *Server) reapTimeouts(ctx context.Context) error {
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		const dueQuery = `SELECT r.id FROM runs r
			WHERE r.state IN ('starting', 'running') AND r.first_started_at IS NOT NULL
			  AND coalesce(r.spec->>'timeout', '') NOT IN ('', '0s')
			  AND (SELECT coalesce(sum(coalesce(p.ended_at, now()) - p.started_at), interval '0')
			       FROM placements p WHERE p.run_id = r.id AND p.started_at IS NOT NULL) > (r.spec->>'timeout')::interval
			  AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.stop_requested_at IS NOT NULL)`
		rows, err := tx.Query(ctx, dueQuery+` ORDER BY r.id LIMIT 50`)
		if err != nil {
			return err
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := lockReaperRuns(ctx, tx, candidates); err != nil {
			return err
		}
		if err := lockCostHosts(ctx, tx, candidates); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT r.id, r.tenant_id FROM runs r
			WHERE r.id = ANY($1) AND r.state IN ('starting', 'running') AND r.first_started_at IS NOT NULL
			  AND coalesce(r.spec->>'timeout', '') NOT IN ('', '0s')
			  AND (SELECT coalesce(sum(coalesce(p.ended_at, now()) - p.started_at), interval '0')
			       FROM placements p WHERE p.run_id = r.id AND p.started_at IS NOT NULL) > (r.spec->>'timeout')::interval
			  AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.stop_requested_at IS NOT NULL)
			ORDER BY r.id`, candidates)
		if err != nil {
			return err
		}
		type rr struct {
			id, tenant string
		}
		due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rr, error) {
			var r rr
			err := row.Scan(&r.id, &r.tenant)
			return r, err
		})
		if err != nil {
			return err
		}
		for _, r := range due {
			h, err := s.requestStop(ctx, tx, r.tenant, r.id, "timeout")
			if err != nil {
				return err
			}
			if h != "" {
				hosts = append(hosts, h)
			}
		}
		return nil
	})
	for _, h := range hosts {
		s.hub.Notify(h)
	}
	return err
}

// reapOutdatedStaticHosts tells a static host drained for outdated
// binaries to exit, once it is idle and has nothing left to upload: its
// systemd unit restarts it, whose ExecStartPre re-downloads first. A
// provisioned host takes the existing drain→terminate→relaunch path
// instead (reconcilePool); this is only for hosts nothing else replaces.
// A host also carrying causeManual is skipped: the operator owns it
// (docs/operations.md). An exit still unanswered after 10 minutes (the
// runner ignored it, or its fetch failed) is re-sent.
func (s *Server) reapOutdatedStaticHosts(ctx context.Context) error {
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH candidates AS (
				SELECT id, exit_requested_at IS NOT NULL AS resend FROM hosts
				WHERE draining AND $1 = ANY(drain_causes) AND NOT ($2 = ANY(drain_causes)) AND provision_requested_at IS NULL
				  AND (exit_requested_at IS NULL OR exit_requested_at < now() - interval '10 minutes')
				  AND NOT EXISTS (SELECT 1 FROM placements p WHERE p.host_id = hosts.id AND p.state IN `+livePlacementStates+`)
				  AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.host_id = hosts.id AND b.location = 'host')
				FOR UPDATE OF hosts
			)
			UPDATE hosts SET exit_requested_at = now()
			FROM candidates WHERE hosts.id = candidates.id
			RETURNING hosts.id, candidates.resend`, causeOutdated, causeManual)
		if err != nil {
			return err
		}
		type candidate struct {
			ID     string
			Resend bool
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
		if err != nil {
			return err
		}
		var resent []string
		for _, c := range candidates {
			hosts = append(hosts, c.ID)
			if c.Resend {
				resent = append(resent, c.ID)
			}
		}
		if len(resent) > 0 {
			s.log.Warn("re-sending exit: the host is still outdated", "hosts", resent)
		}
		for _, h := range hosts {
			if err := enqueue(ctx, tx, h, "", 0, proto.MsgExit,
				proto.ExitHost{Reason: outdatedBinariesReason, Code: proto.ExitCodeOutdatedBinaries}); err != nil {
				return err
			}
		}
		return nil
	})
	s.notifyAll(hosts)
	return err
}

// requestStop asks the current placement to wind down. Returns the host to
// notify, or "" when there is no live placement.
func (s *Server) requestStop(ctx context.Context, tx pgx.Tx, tenantID, runID, reason string) (string, error) {
	var hostID string
	var epoch int
	err := tx.QueryRow(ctx, `UPDATE placements p SET stop_requested_at = coalesce(stop_requested_at, now()),
			stop_reason = CASE WHEN stop_reason = '' OR $2 = 'cancel' OR (stop_reason = 'migrate' AND $2 = 'stop') THEN $2 ELSE stop_reason END
		FROM runs r
		WHERE r.id = $1 AND p.run_id = r.id AND p.epoch = r.current_epoch
		  AND p.state IN `+livePlacementStates+`
		RETURNING p.host_id, p.epoch`, runID, reason).Scan(&hostID, &epoch)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	typ := proto.MsgStop
	if reason == "cancel" {
		typ = proto.MsgCancel
	}
	if err := enqueue(ctx, tx, hostID, runID, epoch, typ, proto.StopRequest{Reason: reason}); err != nil {
		return "", err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		return "", err
	}
	if state != StateStopping {
		if err := setRunState(ctx, tx, tenantID, runID, StateStopping, reason, epoch); err != nil {
			return "", err
		}
	}
	return hostID, nil
}

// reapExpiry cancels Runs that have rested (stopped, lost or failed) longer
// than their tenant's expire_after_days (0: never). The clock is
// state_changed_at, so a resume and a later stop restart it. The Run is
// locked and its state re-checked by FOR UPDATE (a resume that committed
// first fails the WHERE on the row's new version); one held by a resume or
// cancel in progress is skipped and seen on a later pass.
func (s *Server) reapExpiry(ctx context.Context) error {
	var expired []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// due: each expiring tenant's oldest due Runs, read from
		// runs_resting below that tenant's own cutoff, so a tenant with a
		// short limit does not make the pass read other tenants' resting
		// Runs that are not due; then the oldest 20 of those. The outer
		// query locks them and repeats the conditions.
		rows, err := tx.Query(ctx, `WITH due AS (
				SELECT d.id FROM tenants dt CROSS JOIN LATERAL (
					SELECT dr.id, dr.state_changed_at FROM runs dr
					WHERE dr.tenant_id = dt.id AND dr.state IN `+resumableRunStates+`
					  AND dr.state_changed_at < now() - make_interval(days => dt.expire_after_days)
					ORDER BY dr.state_changed_at LIMIT 20) d
				WHERE dt.expire_after_days > 0
				ORDER BY d.state_changed_at LIMIT 20
			)
			SELECT r.id, r.tenant_id, r.state, t.expire_after_days FROM runs r
			JOIN tenants t ON t.id = r.tenant_id
			WHERE r.id IN (SELECT id FROM due) AND r.state IN `+resumableRunStates+`
			  AND t.expire_after_days > 0 AND r.state_changed_at < now() - make_interval(days => t.expire_after_days)
			ORDER BY r.state_changed_at
			FOR UPDATE OF r SKIP LOCKED`)
		if err != nil {
			return err
		}
		type due struct {
			ID, Tenant, State string
			Days              int
		}
		runs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[due])
		if err != nil {
			return err
		}
		for _, r := range runs {
			reason := fmt.Sprintf("expired: %s for %d days", r.State, r.Days)
			if err := setRunState(ctx, tx, r.Tenant, r.ID, StateCancelled, reason, 0); err != nil {
				return err
			}
			expired = append(expired, r.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range expired {
		s.secrets.drop(id)
	}
	return nil
}

// supersededWindow: how many flagged Runs one reapSuperseded pass inspects.
const supersededWindow = 200

// reapSuperseded deletes the volumes of a Run's snapshots other than its
// current one (runs.snapshot_id), once the current one is uploaded: until
// then an older snapshot is the only copy that survives losing the host.
// Only Runs not succeeded or cancelled (reapRetention has those).
//
// The claim is reapRetention's: the Run is locked first, in a statement of
// its own, so the claim's statement reads its snapshot_id after any resume
// --from-snapshot or new snapshot report that committed before the lock,
// and none can commit during it. The snapshot a queued or starting Run
// restores is its snapshot_id (assign reads it under the same lock, and
// neither path moves it while the Run is queued or placed), so it is never
// claimed. A snapshot with a volume still on its host waits, available,
// for a later pass: never deleted from under an upload. A blob the current
// manifest also names is kept.
//
// Runs are found by runs.snapshots_superseded, cleared here once the Run
// has no other available snapshot left, or has ended (reapRetention's).
// A pass inspects at most supersededWindow flagged Runs after
// s.supersededCursor, in id order, and moves the cursor past them, so
// Runs whose older snapshots wait for an upload cost one window per pass
// however many there are, and every flagged Run is inspected once per
// rotation.
func (s *Server) reapSuperseded(ctx context.Context) error {
	var keys []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// One index probe per id: under RLS the planner estimates few
		// flagged rows survive and would seq-scan and sort all of runs for
		// a plain ORDER BY id LIMIT.
		rows, err := tx.Query(ctx, `WITH RECURSIVE w(id, n) AS (
				SELECT (SELECT min(id) FROM runs WHERE snapshots_superseded AND id > $1), 1
				UNION ALL
				SELECT (SELECT min(id) FROM runs WHERE snapshots_superseded AND id > w.id), w.n + 1
				FROM w WHERE w.id IS NOT NULL AND w.n < $2
			) SELECT id FROM w WHERE id IS NOT NULL ORDER BY id`, s.supersededCursor, supersededWindow)
		if err != nil {
			return err
		}
		window, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		// A short window reached the end: the next pass starts over. Each
		// luxd keeps its own cursor; the claim's lock keeps them apart.
		next := ""
		if len(window) == supersededWindow {
			next = window[len(window)-1]
		}
		s.supersededCursor = next
		if len(window) == 0 {
			return nil
		}
		// Candidates: an ended Run or a stale hint (only its flag to
		// clear), or a Run with an older snapshot the claim would take
		// now. One whose older snapshots all wait for an upload is left
		// out before the LIMIT, so it cannot hold a batch slot.
		rows, err = tx.Query(ctx, `SELECT r.id FROM runs r
			WHERE r.id = ANY($1) AND r.snapshots_superseded AND (r.state IN ('succeeded', 'cancelled')
				OR NOT EXISTS (SELECT 1 FROM snapshots o WHERE o.run_id = r.id AND o.available AND o.id IS DISTINCT FROM r.snapshot_id)
				OR (EXISTS (SELECT 1 FROM snapshots cur WHERE cur.id = r.snapshot_id AND cur.uploaded)
					AND EXISTS (SELECT 1 FROM snapshots o WHERE o.run_id = r.id AND o.available AND o.id <> r.snapshot_id
						AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(nullif(o.manifest->'volumes', 'null'), '[]')) v
							JOIN blobs b ON b.id = v->>'blobId' AND b.run_id = o.run_id WHERE b.location = 'host'))))
			ORDER BY r.id LIMIT 20
			FOR UPDATE OF r SKIP LOCKED`, window)
		if err != nil {
			return err
		}
		runs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(runs) == 0 {
			return err
		}
		// A full batch may leave ready Runs later in the window: the next
		// pass resumes after the last one claimed.
		if len(runs) == 20 {
			s.supersededCursor = runs[len(runs)-1]
		}
		// A new statement: it sees whatever committed before the locks.
		rows, err = tx.Query(ctx, `
			WITH cur AS (
				SELECT r.id AS run_id, r.snapshot_id, c.manifest FROM runs r JOIN snapshots c ON c.id = r.snapshot_id
				WHERE r.id = ANY($1) AND c.uploaded AND r.state NOT IN ('succeeded', 'cancelled')
			), old AS (
				SELECT o.id, o.run_id, o.manifest, cur.manifest AS keep FROM snapshots o JOIN cur ON cur.run_id = o.run_id
				WHERE o.available AND o.id <> cur.snapshot_id
				  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(nullif(o.manifest->'volumes', 'null'), '[]')) v
					JOIN blobs b ON b.id = v->>'blobId' AND b.run_id = o.run_id WHERE b.location = 'host')
			), gone AS (
				UPDATE snapshots SET available = false WHERE id IN (SELECT id FROM old)
			)
			UPDATE blobs b SET location = 'deleted', deleted_at = now()
			FROM old, jsonb_array_elements(coalesce(nullif(old.manifest->'volumes', 'null'), '[]')) v
			WHERE b.id = v->>'blobId' AND b.run_id = old.run_id AND b.kind = 'volume' AND b.location = 's3'
			  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(nullif(old.keep->'volumes', 'null'), '[]')) k
				WHERE k->>'blobId' = b.id)
			RETURNING b.s3_key`, runs)
		if err != nil {
			return err
		}
		if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		// After the claim's statement, so it sees the snapshots it took.
		_, err = tx.Exec(ctx, `UPDATE runs r SET snapshots_superseded = false
			WHERE r.id = ANY($1) AND (r.state IN ('succeeded', 'cancelled') OR NOT EXISTS (
				SELECT 1 FROM snapshots o WHERE o.run_id = r.id AND o.available AND o.id IS DISTINCT FROM r.snapshot_id))`, runs)
		return err
	})
	if err != nil {
		return err
	}
	s.deleteObjects(ctx, "superseded snapshot", keys)
	return nil
}

// deleteObjects deletes claimed S3 objects, after the claim committed. One
// that fails to delete is an orphan in S3, logged, never retried.
func (s *Server) deleteObjects(ctx context.Context, what string, keys []string) {
	for _, k := range keys {
		if err := s.blobs.Delete(ctx, k); err != nil {
			s.log.Warn(what+": S3 delete failed; object orphaned", "key", k, "err", err)
		}
	}
}

// reapRetention deletes the blobs of Runs that succeeded or were cancelled
// longer ago than their tenant's retention: snapshot volumes and output.
// Artifacts are kept until their owner deletes them (deleteArtifacts). A
// failed Run is resumable, so it is exempt from retention until it expires
// (reapExpiry) and its retention counts from then.
//
// The database is the claim: blobs are marked deleted, and the Run's
// snapshots unavailable, in one transaction that locks each Run and checks
// it is still terminal, so a Run cannot be resumed from a snapshot that is
// going. S3 objects are deleted after the claim; one that fails to delete
// is an orphan in S3, never a Run pointing at nothing.
func (s *Server) reapRetention(ctx context.Context) error {
	var keys []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT r.id FROM runs r JOIN tenants t ON t.id = r.tenant_id
				WHERE r.finished_at IS NOT NULL AND r.finished_at < now() - make_interval(days => t.retention_days)
				  AND r.state IN ('succeeded', 'cancelled')
				  AND EXISTS (SELECT 1 FROM blobs bl WHERE bl.run_id = r.id AND bl.location = 's3' AND bl.kind <> 'artifact')
				ORDER BY r.finished_at LIMIT 20
				FOR UPDATE OF r SKIP LOCKED
			), gone AS (
				UPDATE snapshots SET available = false WHERE run_id IN (SELECT id FROM due) AND available
			)
			-- Only blobs in S3: one still on its host is mid-upload; it is
			-- claimed on a later pass, once it has arrived.
			UPDATE blobs SET location = 'deleted', deleted_at = now()
			WHERE run_id IN (SELECT id FROM due) AND location = 's3' AND kind <> 'artifact'
			RETURNING s3_key`)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return err
	}
	s.deleteObjects(ctx, "retention", keys)
	return nil
}
