package server

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// wakeups wake followers of Run events (the feed, output streams) when one
// is written, by any luxd: Postgres notifies lux_events with the Run's id
// after each insert into run_events (migration 012; 'srv:<id>' for a
// server with no Run, which wakes only followers of every event), and one connection
// per luxd listens. Followers re-read under their own scope; a wake-up says
// only "look".
//
// A follower takes its channel before it reads (next), then waits on it:
// an event committed while it reads closes that channel, so no wake-up is
// lost.
type wakeups struct {
	mu   sync.Mutex
	all  chan struct{}            // closed at any event
	runs map[string]chan struct{} // closed at an event of that Run
}

func newWakeups() *wakeups {
	return &wakeups{all: make(chan struct{}), runs: map[string]chan struct{}{}}
}

// next is closed at the next event (of runID, if given).
func (w *wakeups) next(runID string) <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if runID == "" {
		return w.all
	}
	ch, ok := w.runs[runID]
	if !ok {
		ch = make(chan struct{})
		w.runs[runID] = ch
	}
	return ch
}

// notify wakes followers of every Run (runID "") or of one.
func (w *wakeups) notify(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.all)
	w.all = make(chan struct{})
	if runID == "" {
		for id, ch := range w.runs {
			close(ch)
			delete(w.runs, id)
		}
	} else if ch, ok := w.runs[runID]; ok {
		close(ch)
		delete(w.runs, runID)
	}
}

// wait returns when ch closes, after fallback (in case notifications are
// missed: the listener reconnecting), or when ctx ends.
func wait(ctx context.Context, ch <-chan struct{}, fallback time.Duration) {
	t := time.NewTimer(fallback)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-ch:
	case <-t.C:
	}
}

// listenLoop holds the LISTEN connection, reconnecting as needed. On every
// (re)connect it wakes every follower, for what they may have missed.
func (s *Server) listenLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("events: listen", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// listen uses a connection of its own, outside the pool: it is held for
// good, and closed (not returned listening) when done. A connection that
// went quiet is pinged, so a dead one is found and replaced.
func (s *Server) listen(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, s.db.Pool.Config().ConnConfig)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN lux_events"); err != nil {
		return err
	}
	s.wakeups.notify("")
	for {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			s.wakeups.notify(n.Payload)
		case ctx.Err() != nil:
			return ctx.Err()
		case wctx.Err() != nil:
			if err := conn.Ping(ctx); err != nil {
				return err
			}
		default:
			return err
		}
	}
}
