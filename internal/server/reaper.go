package server

import (
	"context"
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
		reaps := []func(context.Context) error{s.reapLeases, s.reapHosts, s.reapTimeouts, s.reapRetention, s.reapOutdatedStaticHosts}
		if err := s.forgiveOutage(ctx); err != nil {
			// Unsure whether leases ran out while nobody listened: judge
			// none of them this tick.
			reaps = reaps[2:]
			if ctx.Err() == nil {
				s.log.Warn("reaper", "err", err)
			}
		}
		for _, f := range reaps {
			if err := f(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("reaper", "err", err)
			}
		}
	}
}

// forgiveOutage adds time no reaper ran (every luxd stopped or hung, or
// Postgres unreachable) back to host heartbeats and placement leases: no
// luxd could hear a runner then, so it is not the runner's to answer for.
// A gap shorter than a heartbeat interval (or a couple of ticks) is
// ordinary. Nothing is extended past what a heartbeat now would give it.
func (s *Server) forgiveOutage(ctx context.Context) error {
	ordinary := max(s.cfg.LeaseDuration/3, 2*s.cfg.Tick)
	var gap time.Duration
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT now() - at FROM reaper_alive FOR UPDATE`).Scan(&gap); err != nil {
			return err
		}
		if gap > ordinary {
			if _, err := tx.Exec(ctx, `UPDATE hosts SET last_heartbeat = least(last_heartbeat + $1::interval, now())
				WHERE state IN ('ready', 'draining')`, interval(gap)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE placements SET lease_expires_at = least(lease_expires_at + $1::interval, now() + $2::interval)
				WHERE state IN `+livePlacementStates, interval(gap), interval(s.cfg.LeaseDuration)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE reaper_alive SET at = now()`)
		return err
	})
	if err == nil && gap > ordinary {
		s.log.Warn("no reaper ran for a while: leases extended by the gap", "gap", gap.Round(time.Millisecond))
	}
	return err
}

// reapLeases: a placement whose lease expired is lost.
func (s *Server) reapLeases(ctx context.Context) error {
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Discover candidates without locking placements; renewal may win before
		// the Run lock, so check expiry again after acquiring it.
		expired, err := livePlacements(ctx, tx, "p.lease_expires_at < now()")
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
		expired, err = livePlacements(ctx, tx, "p.lease_expires_at < now() AND p.run_id = ANY($1)", runs)
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
			WHERE state IN ('ready', 'draining') AND last_heartbeat < now() - $1::interval
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
				  AND last_heartbeat < now() - $2::interval
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

// reapRetention deletes the blobs of Runs that finished longer ago than
// their tenant's retention.
//
// The database is the claim: blobs are marked deleted, and the snapshots
// they belong to unavailable, in one transaction that locks each Run and
// checks it is still finished, so a concurrent resume either sees the Run
// finished (and is refused a snapshot that is going) or clears finished_at
// first (and keeps everything). S3 objects are deleted after the claim; one
// that fails to delete is an orphan in S3, never a Run pointing at nothing.
func (s *Server) reapRetention(ctx context.Context) error {
	var keys []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT r.id FROM runs r JOIN tenants t ON t.id = r.tenant_id
				WHERE r.finished_at IS NOT NULL AND r.finished_at < now() - make_interval(days => t.retention_days)
				  AND EXISTS (SELECT 1 FROM blobs bl WHERE bl.run_id = r.id AND bl.location = 's3')
				ORDER BY r.finished_at LIMIT 20
				FOR UPDATE OF r SKIP LOCKED
			), gone AS (
				UPDATE snapshots SET available = false WHERE run_id IN (SELECT id FROM due)
			)
			-- Only blobs in S3: one still on its host is mid-upload; it is
			-- claimed on a later pass, once it has arrived.
			UPDATE blobs SET location = 'deleted', deleted_at = now()
			WHERE run_id IN (SELECT id FROM due) AND location = 's3'
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
	for _, k := range keys {
		if err := s.blobs.Delete(ctx, k); err != nil {
			s.log.Warn("retention: S3 delete failed; object orphaned", "key", k, "err", err)
		}
	}
	return nil
}
