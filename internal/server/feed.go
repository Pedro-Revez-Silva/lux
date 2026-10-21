package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type feedInput struct {
	TenantQuery
	After       string   `query:"after" doc:"From after this event id (default: from now)."`
	Last        int      `query:"last" doc:"Start with the last N events (at most 500), instead of from now. With label or type: the newest N matching events among the most recent 20,000 events; fewer if fewer match there."`
	Follow      string   `query:"follow" doc:"false: end after what there is now."`
	Label       []string `query:"label,explode" doc:"Only events of Runs with this label (key=value); repeat to require several. A server's event matches through the Run it is attached to (runId); one attached to no Run never matches."`
	Type        string   `query:"type" doc:"Only events of these types (comma-separated)." example:"state,exited,input.consumed"`
	LastEventID string   `header:"Last-Event-ID" doc:"A reconnecting client's last event id: resumes after it."`
}

// Resolve reads every label as given: huma drops them all when the first
// is empty.
func (in *feedInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Label = u.Query()["label"]
	return nil
}

// feedFilter is which events of the feed are sent: all, without labels or
// types.
type feedFilter struct {
	labels []string
	types  []string
}

func (in *feedInput) filter() feedFilter {
	f := feedFilter{labels: in.Label}
	if in.Type != "" {
		f.types = strings.Split(in.Type, ",")
	}
	return f
}

func (f feedFilter) none() bool { return len(f.labels) == 0 && len(f.types) == 0 }

// match is the filter as one condition on runs r and run_events e, false
// for an event of no Run when labels are given (r.labels is NULL).
// arg binds a value and returns its placeholder.
func (f feedFilter) match(arg func(any) string) string {
	var conditions []string
	for _, l := range f.labels {
		k, v, _ := strings.Cut(l, "=")
		conditions = append(conditions, "r.labels @> "+arg(map[string]string{k: v}))
	}
	if len(f.types) > 0 {
		conditions = append(conditions, "e.type = ANY("+arg(f.types)+")")
	}
	return "coalesce(" + strings.Join(conditions, " AND ") + ", false)"
}

// feedSettle: event ids are taken when an event is written, not when its
// transaction commits, so a lower id can become visible after a higher
// one. The feed re-reads events this young on every read (sending each
// once), so a late commit is still delivered.
const feedSettle = 10 * time.Second

// feedFallback: how long the feed waits without a notification before
// reading anyway (a luxd's LISTEN connection may be reconnecting, and
// young events are re-read until they settle).
const feedFallback = 5 * time.Second

const feedPage = 500

// feedLastScan bounds the work of last=N with a filter, done before the
// stream's headers are sent: it looks among this many newest events only.
// Without an index on labels or types, finding N rare matches (or none)
// would otherwise read the whole table.
const feedLastScan = 20000

// feedRead reads one window of the feed; a test replaces it to watch the
// watermark.
var feedRead = (*Server).feedWindow

// serveFeed is GET /v1/events: every event of every Run and server the caller sees
// (an operator's: all tenants, or ?tenant=), as SSE, in id order, or those
// matching ?label= and ?type=. It reads when an event is written
// (wakeups.go), not on a timer.
func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request, in *feedInput) error {
	ctx := r.Context()
	p := principal(ctx)
	f := in.filter()
	low := int64(-1)
	for _, v := range []string{in.LastEventID, in.After} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			low = n
			break
		}
	}
	if low < 0 {
		args := []any{min(max(in.Last, 0), feedPage)}
		q := feedLastSQL(f, p.TenantID, func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) })
		err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, q, args...).Scan(&low)
		})
		if err != nil {
			return err
		}
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Headers now, so a client knows it is connected before the first event.
	if flusher != nil {
		flusher.Flush()
	}
	// low: every event at or below it has been sent, does not match, or
	// will never be seen. sent: the ids above low already sent.
	sent := map[int64]bool{}
	keepalive := time.Now()
	for {
		woken := s.wakeups.next("") // before the read: see wakeups
		// The window is every event above low, matching or not, so low
		// passes the events the filter skips as it passes those it sends.
		evs, err := feedRead(s, ctx, p, low, f)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b, _ := json.Marshal(outputError{Error: err.Error()})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			return nil
		}
		wrote, skipped := false, false
		settled := true
		for _, e := range evs {
			if e.match && !sent[e.ID] {
				b, _ := json.Marshal(e.FeedEvent)
				if _, err := fmt.Fprintf(w, "id: %d\nevent: lux\ndata: %s\n\n", e.ID, b); err != nil {
					return nil
				}
				sent[e.ID], wrote = true, true
			}
			// The watermark passes old events only: nothing below them can
			// still be on its way.
			if settled && time.Since(e.Time) > feedSettle {
				low = e.ID
				delete(sent, e.ID)
				skipped = skipped || !e.match
			} else {
				settled = false
			}
		}
		if wrote || time.Since(keepalive) > 15*time.Second {
			if !wrote {
				fmt.Fprint(w, ": keepalive\n\n")
			}
			keepalive = time.Now()
			if flusher != nil {
				flusher.Flush()
			}
		}
		// A full window may have more after it: read on at once if this one
		// moved the stream (sent, or passed skipped events).
		if len(evs) == feedPage && (wrote || skipped) {
			continue
		}
		if in.Follow == "false" {
			return nil
		}
		// Young events still settling are re-read soon; otherwise at the
		// next event (or the fallback).
		fallback := feedFallback
		if len(sent) > 0 {
			fallback = time.Second
		}
		wait(ctx, woken, fallback)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// feedLastSQL is the watermark below the $1 newest events matching f: just
// before the oldest of them, or the newest event's id if none. With a
// filter it looks only among the feedLastScan newest events, of tenant
// only when it is not "" (the principal's tenant, as for RLS).
func feedLastSQL(f feedFilter, tenant string, arg func(any) string) string {
	if f.none() {
		return `SELECT coalesce(min(id) - 1, (SELECT coalesce(max(id), 0) FROM run_events))
				FROM (SELECT id FROM run_events ORDER BY id DESC LIMIT $1) x`
	}
	join := ""
	if len(f.labels) > 0 {
		// A primary-key lookup per scanned event, whatever the statistics:
		// OFFSET 0 keeps the planner from flattening it into a join that,
		// on an unanalyzed run_events, rescans a materialized runs. No Run:
		// r.labels is NULL, as with a LEFT JOIN.
		join = " LEFT JOIN LATERAL (SELECT r.labels FROM runs r WHERE r.id = e.run_id OFFSET 0) r ON true"
	}
	// RLS's tenant-or-system predicate is no index condition: a tenant's
	// scan would otherwise walk every other tenant's newer events.
	where := ""
	if tenant != "" {
		where = " WHERE tenant_id = " + arg(tenant)
	}
	return `SELECT coalesce(min(id) - 1, (SELECT coalesce(max(id), 0) FROM run_events` + where + `))
				FROM (SELECT e.id FROM (SELECT e.id, e.run_id, e.type FROM run_events e` + where + ` ORDER BY e.id DESC LIMIT ` + strconv.Itoa(feedLastScan) + `) e` + join + `
					WHERE ` + f.match(arg) + ` ORDER BY e.id DESC LIMIT $1) x`
}

func (s *Server) feedAfter(ctx context.Context, p Principal, after int64) ([]FeedEvent, error) {
	rows, err := s.feedWindow(ctx, p, after, feedFilter{})
	evs := make([]FeedEvent, len(rows))
	for i, e := range rows {
		evs[i] = e.FeedEvent
	}
	return evs, err
}

// feedRow is an event of a feed window, and whether it matches the
// filter. A row that does not match has no data.
type feedRow struct {
	FeedEvent
	match bool
}

// feedWindow reads the feedPage events after after, matching f or not.
func (s *Server) feedWindow(ctx context.Context, p Principal, after int64, f feedFilter) ([]feedRow, error) {
	args := []any{after, feedPage}
	q := feedWindowSQL(f, func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) })
	var evs []feedRow
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		evs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (feedRow, error) {
			var e feedRow
			err := row.Scan(&e.ID, &e.ServerID, &e.Epoch, &e.Type, &e.Data, &e.Time, &e.RunID, &e.Tenant, &e.match)
			if e.match {
				s.eventDetail(&e.Event)
			}
			return e, err
		})
		return err
	})
	return evs, err
}

// feedWindowSQL reads the $2 events after $1, each with whether it matches
// f; without a filter, the feed's read as it always was.
func feedWindowSQL(f feedFilter, arg func(any) string) string {
	join, data, match := "", "e.data", "true"
	if !f.none() {
		if len(f.labels) > 0 {
			// A server's event with no Run joins no row: r.labels is NULL.
			join = " LEFT JOIN runs r ON r.id = e.run_id"
		}
		// The payload of an event that is not sent is not read.
		join += " CROSS JOIN LATERAL (SELECT " + f.match(arg) + " AS match) m"
		data, match = "CASE WHEN m.match THEN e.data END", "m.match"
	}
	return `SELECT e.id, coalesce(e.server_id, ''), e.epoch, e.type, ` + data + `, e.created_at, e.run_id, t.name, ` + match + `
			FROM run_events e JOIN tenants t ON t.id = e.tenant_id` + join + ` WHERE e.id > $1 ORDER BY e.id LIMIT $2`
}
