package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// FeedEvent is an event in the feed of every Run and server the caller
// sees: a Run's (runId), or a server's (serverId, and runId its attached
// Run's, null while it has none).
type FeedEvent struct {
	Event
	RunID  *string `json:"runId" nullable:"true" doc:"The Run's id. Null only for an event of a server attached to no Run."`
	Tenant string  `json:"tenant"`
}

type feedInput struct {
	TenantQuery
	After       string `query:"after" doc:"From after this event id (default: from now)."`
	Last        int    `query:"last" doc:"Start with the last N events (at most 500), instead of from now."`
	Follow      string `query:"follow" doc:"false: end after what there is now."`
	LastEventID string `header:"Last-Event-ID" doc:"A reconnecting client's last event id: resumes after it."`
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

// serveFeed is GET /v1/events: every event of every Run and server the caller sees
// (an operator's: all tenants, or ?tenant=), as SSE, in id order. It reads
// when an event is written (wakeups.go), not on a timer.
func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request, in *feedInput) error {
	ctx := r.Context()
	p := principal(ctx)
	low := int64(-1)
	for _, v := range []string{in.LastEventID, in.After} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			low = n
			break
		}
	}
	if low < 0 {
		last := min(max(in.Last, 0), feedPage)
		err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce(min(id) - 1, (SELECT coalesce(max(id), 0) FROM run_events))
				FROM (SELECT id FROM run_events ORDER BY id DESC LIMIT $1) x`, last).Scan(&low)
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
	// low: every event at or below it has been sent or will never be seen.
	// sent: the ids above low already sent.
	sent := map[int64]bool{}
	keepalive := time.Now()
	for {
		woken := s.wakeups.next("") // before the read: see wakeups
		evs, err := s.feedAfter(ctx, p, low)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b, _ := json.Marshal(outputError{Error: err.Error()})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			return nil
		}
		wrote := false
		settled := true
		for _, e := range evs {
			if !sent[e.ID] {
				b, _ := json.Marshal(e)
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
		if len(evs) == feedPage && wrote {
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

func (s *Server) feedAfter(ctx context.Context, p Principal, after int64) ([]FeedEvent, error) {
	var evs []FeedEvent
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.id, coalesce(e.server_id, ''), e.epoch, e.type, e.data, e.created_at, e.run_id, t.name
			FROM run_events e JOIN tenants t ON t.id = e.tenant_id WHERE e.id > $1 ORDER BY e.id LIMIT $2`, after, feedPage)
		if err != nil {
			return err
		}
		evs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FeedEvent, error) {
			var e FeedEvent
			err := row.Scan(&e.ID, &e.ServerID, &e.Epoch, &e.Type, &e.Data, &e.Time, &e.RunID, &e.Tenant)
			s.eventDetail(&e.Event)
			return e, err
		})
		return err
	})
	return evs, err
}
