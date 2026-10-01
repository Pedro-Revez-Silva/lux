package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Wakes and idleness: lux never starts or stops a Run for a server. It
// tells the server's owner, on the feed (GET /v1/events):
//
//   - server.wake_requested, when a signed-in request finds a server that
//     wakes on request with no running Run serving it: once per wake. A
//     wake is open from then until the server becomes ready (resolved) or
//     wakeTimeout passes (timed out); while it is open no request asks
//     again, so many requests and many tabs ask once.
//   - server.idle, when a ready server has had no request for idleAfter:
//     once per idle period (the next request, or a new ready, starts a new
//     one).
//
// Both are one conditional UPDATE of the server's row with the event in the
// same transaction: of concurrent requests (or luxds) only the one whose
// update matched writes the event, as Postgres re-checks the condition on
// the row it waited for.

// requestWake asks the server's owner to bring a Run up, unless a wake is
// open already. asked: this request asked (wrote the event).
func (s *Server) requestWake(ctx context.Context, id, by, path string) (asked bool, err error) {
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// The Run first (KEY SHARE: what the event's foreign key takes),
		// then the server, as every change of a server locks them.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = (SELECT run_id FROM run_servers WHERE id = $1) FOR KEY SHARE`, id); err != nil {
			return err
		}
		var tenantID string
		var runID *string
		var wakes int
		var ref srvRef
		err := tx.QueryRow(ctx, `UPDATE run_servers SET wake_requested_at = now(), wake_by = $2, wake_path = $3, wakes = wakes + 1
			WHERE id = $1 AND wake = 'request'
			  AND (wake_requested_at IS NULL OR wake_requested_at + make_interval(secs => wake_timeout_s) <= now())
			RETURNING tenant_id, run_id, wakes, name, host, labels`, id, by, truncate(path, 1000)).
			Scan(&tenantID, &runID, &wakes, &ref.name, &ref.host, &ref.labels)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		asked = true
		return serverEvent(ctx, tx, tenantID, runID, id, 0, "server.wake_requested", ref,
			map[string]any{"by": by, "path": path, "wake": wakes})
	})
	return asked, err
}

// idleLoop looks for idle and expired servers every preview idle check.
func (s *Server) idleLoop(ctx context.Context) {
	every := s.cfg.Preview.IdleCheck
	if every <= 0 {
		every = 5 * time.Second
	}
	for ctx.Err() == nil {
		// Idleness is measured by preview requests: without previews,
		// there are none to measure.
		if s.preview != nil {
			if err := s.checkIdle(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("servers: idle", "err", err)
			}
		}
		if err := s.expireServers(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("servers: expiry", "err", err)
		}
		wait(ctx, nil, every)
	}
}

// checkIdle emits server.idle for every ready server of a running Run
// with no request for its idleAfter, once per idle period. The due rows'
// Runs are locked (KEY SHARE) before their servers; the UPDATE repeats the
// conditions on the rows it then locks.
func (s *Server) checkIdle(ctx context.Context) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		const due = `r.id = sv.run_id AND r.state = 'running' AND sv.state = 'ready' AND sv.idle_after_s > 0
			  AND greatest(sv.last_request_at, sv.ready_since) + make_interval(secs => sv.idle_after_s) <= now()
			  AND (sv.idle_notified_at IS NULL OR sv.idle_notified_at < greatest(sv.last_request_at, sv.ready_since))`
		rows, err := tx.Query(ctx, `WITH locked AS (
				SELECT sv.id FROM run_servers sv JOIN runs r ON `+due+` FOR KEY SHARE OF r)
			UPDATE run_servers sv SET idle_notified_at = now()
			FROM runs r
			WHERE sv.id IN (SELECT id FROM locked) AND `+due+`
			RETURNING sv.id, sv.tenant_id, sv.run_id, sv.name, sv.host, sv.labels, sv.last_request_at, sv.idle_after_s, r.current_epoch`)
		if err != nil {
			return err
		}
		type idle struct {
			id, tenantID, runID string
			ref                 srvRef
			last                *time.Time
			after, epoch        int
		}
		var found []idle
		for rows.Next() {
			var i idle
			if err := rows.Scan(&i.id, &i.tenantID, &i.runID, &i.ref.name, &i.ref.host, &i.ref.labels, &i.last, &i.after, &i.epoch); err != nil {
				rows.Close()
				return err
			}
			found = append(found, i)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, i := range found {
			d := map[string]any{"idleAfter": secs(i.after).String(), "lastRequestAt": i.last}
			if err := serverEvent(ctx, tx, i.tenantID, &i.runID, i.id, i.epoch, "server.idle", i.ref, d); err != nil {
				return err
			}
		}
		return nil
	})
}

// expireServers deletes owner servers that went expireAfter without a
// request (since their creation, if never requested): detached first, then
// gone, with server.expired. The candidates are read unlocked; each is then
// locked as every change of a server is, its Run first, and checked again.
func (s *Server) expireServers(ctx context.Context) error {
	const due = `sv.lifetime = 'owner' AND sv.expire_after_s IS NOT NULL
		AND coalesce(sv.last_request_at, sv.created_at) + make_interval(secs => sv.expire_after_s) <= now()`
	var ids []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT sv.id FROM run_servers sv WHERE `+due+` ORDER BY sv.id LIMIT 100`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		var hosts []string
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			var runID *string
			if err := tx.QueryRow(ctx, `SELECT run_id FROM run_servers WHERE id = $1`, id).Scan(&runID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil
				}
				return err
			}
			if runID != nil {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, *runID); err != nil {
					return err
				}
			}
			v, err := scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.id = $1 AND `+due+` FOR UPDATE OF sv`, id))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // requested, or deleted, meanwhile
			}
			if err != nil {
				return err
			}
			if strOf(v.RunID) != strOf(runID) {
				return nil // attached or detached meanwhile: the next pass
			}
			if err := deleteServerTx(ctx, tx, v.TenantID, v, "expired", "lux", "server.expired"); err != nil {
				return err
			}
			if v.RunID != nil {
				hosts, err = syncServerSets(ctx, tx, *v.RunID)
			}
			return err
		})
		if err != nil {
			return err
		}
		for _, h := range hosts {
			s.hub.Notify(h)
		}
	}
	return nil
}

// ---- the waking page --------------------------------------------------------

// wakeStep is one line of the waking page.
type wakeStep struct {
	Label   string
	State   string // done | current | pending | failed
	Elapsed string
}

// wakingPage writes what a server that wakes on request shows until it is
// ready, from its state now: its steps with the time each took since the
// wake (or since its Run's placement began), and the variants: waiting for
// a host, moving, no answer, exited during start.
func (p *previews) wakingPage(ctx context.Context, w http.ResponseWriter, id, to string) error {
	var v serverRow
	var pl struct {
		created, restored, started, synced *time.Time
		syncFailed, syncWanted             bool
		hostname                           string
	}
	err := p.s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		if v, err = scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.id = $1`, id)); err != nil {
			return err
		}
		if v.RunID == nil {
			return nil
		}
		err = tx.QueryRow(ctx, `SELECT p.created_at, p.volumes_restored_at, p.container_started_at, coalesce(h.name, ''),
				(SELECT max(e.created_at) FROM run_events e WHERE e.run_id = r.id AND e.epoch = r.current_epoch AND e.type = 'git.sync'),
				coalesce((SELECT bool_or(e.data->>'status' = 'failed') FROM run_events e WHERE e.run_id = r.id AND e.epoch = r.current_epoch AND e.type = 'git.sync'), false),
				EXISTS (SELECT 1 FROM run_events e WHERE e.run_id = r.id AND e.epoch = r.current_epoch AND e.type = 'sync.requested')
					OR r.pending_sync IS NOT NULL
			FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch LEFT JOIN hosts h ON h.id = p.host_id
			WHERE r.id = $1`, *v.RunID).Scan(&pl.created, &pl.restored, &pl.started, &pl.hostname, &pl.synced, &pl.syncFailed, &pl.syncWanted)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		p.page(w, http.StatusNotFound, pageGone, nil)
		return nil
	}
	if err != nil {
		p.page(w, http.StatusBadGateway, pageError, nil)
		return err
	}
	now := time.Now()
	state := v.derive(now)
	data := map[string]any{"Name": v.Name, "To": to, "WaitURL": previewWaitPath + "?to=" + url.QueryEscape(to)}
	switch {
	case state == SrvReady:
		// Ready since the page was asked for: straight in.
		w.Header().Set("Refresh", "0;url="+previewWaitPath+"?to="+url.QueryEscape(to))
	case state == SrvExited && v.RunState == StateRunning:
		data["Code"] = exitCodeText(v.ExitCode)
		data["Error"] = strOf(v.Error)
		data["LogURL"] = p.consoleServerURL(v.ID)
		p.page(w, http.StatusServiceUnavailable, pageDidNotStart, data)
		return nil
	case state == SrvNoAnswer:
		data["Asked"] = ago(now, *v.WakeRequestedAt)
		data["AskAgain"] = previewWakePath
		data["ConsoleURL"] = p.consoleServerURL(v.ID)
		p.page(w, http.StatusServiceUnavailable, pageNoAnswer, data)
		return nil
	case state == SrvStopped:
		p.page(w, http.StatusServiceUnavailable, pageStopped, map[string]any{"Reason": strOf(v.StopReason)})
		return nil
	}
	// The steps, timed from the wake (or, without one, from its Run's
	// current placement).
	base := v.WakeRequestedAt
	if base == nil {
		base = pl.created
	}
	since := func(t *time.Time) string {
		if t == nil || base == nil {
			return ""
		}
		return elapsed(t.Sub(*base))
	}
	placed := pl.created != nil && (v.WakeRequestedAt == nil || !pl.created.Before(*v.WakeRequestedAt)) &&
		v.RunState != StateStopped && v.RunState != StateStopping
	if v.RunID != nil && v.RunState != "" && slices.Contains([]string{StateSubmitted, StateResuming, StateProvisioning}, v.RunState) {
		placed = false
	}
	steps := []wakeStep{{Label: "Asked the orchestrator to start it", State: "done", Elapsed: since(v.WakeRequestedAt)}}
	if v.WakeRequestedAt == nil {
		steps[0] = wakeStep{Label: "Its Run is starting", State: "done"}
	}
	host := wakeStep{Label: "Waiting for a host", State: "current"}
	if v.RunID != nil && (v.RunState == StateResuming || v.RunState == StateSubmitted || v.RunState == StateProvisioning) {
		host.Elapsed = since(&now)
	}
	if placed {
		host = wakeStep{Label: "Placed on a host", State: "done", Elapsed: since(pl.created)}
	}
	restore := wakeStep{Label: "Restoring its state", State: "pending"}
	if placed {
		restore.State = "current"
		if pl.restored != nil {
			restore.State, restore.Elapsed = "done", since(pl.restored)
		}
	}
	syncStep := wakeStep{Label: "Updating to the latest commit", State: "pending"}
	if restore.State == "done" {
		syncStep.State = "current"
		switch {
		case pl.synced != nil && pl.syncFailed:
			syncStep.State, syncStep.Elapsed, syncStep.Label = "failed", since(pl.synced), "Could not update to the latest commit (serving what it has)"
		case pl.synced != nil:
			syncStep.State, syncStep.Elapsed = "done", since(pl.synced)
		case !pl.syncWanted && pl.started != nil:
			syncStep.State, syncStep.Label = "done", "Kept its commit (no update asked for)"
		}
	}
	start := wakeStep{Label: "Starting the server", State: "pending"}
	if v.RunState == StateRunning && (v.State == ServerStarting || v.State == ServerUnreachable) {
		start.State, start.Elapsed = "current", since(&now)
		if syncStep.State == "current" {
			syncStep.State = "done"
		}
	}
	steps = append(steps, host, restore, syncStep, start)
	data["Steps"] = steps
	pg := pageWaking
	switch {
	case v.Moving:
		pg = pageWakingMoving
	case state == SrvAsleep || (v.RunID != nil && !placed):
		pg = pageWakingHost
	}
	if pl.hostname != "" && placed {
		data["Host"] = pl.hostname
	}
	p.page(w, http.StatusServiceUnavailable, pg, data)
	return nil
}

// elapsed is a duration as the waking page shows it: 6s, 1m 04s.
func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func ago(now, t time.Time) string {
	return elapsed(now.Sub(t)) + " ago"
}
