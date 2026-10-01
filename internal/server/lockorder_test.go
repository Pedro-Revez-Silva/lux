package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// runThenServers holds what a runner's report of the Run's end holds when
// it ends the Run's servers (applyReport, then endServers): the Run's row,
// then its servers' rows. It takes the Run, waits for release, then takes
// the servers and commits.
func runThenServers(t *testing.T, s *Server, ctx context.Context, runID string, locked chan<- struct{}, release <-chan struct{}) error {
	t.Helper()
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
			return err
		}
		close(locked)
		<-release
		_, err := tx.Exec(ctx, `SELECT 1 FROM run_servers WHERE run_id = $1 FOR UPDATE`, runID)
		return err
	})
}

// waitBlocked returns once some other backend waits on a lock.
func waitBlocked(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		systemScan(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`, nil, &n)
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing blocked within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdRunEnd starts runThenServers on runID and returns once it holds the
// Run; finishReport lets it take the servers and commit, and returns its
// error. A hold not finished is let go when the test ends.
func holdRunEnd(t *testing.T, s *Server, ctx context.Context, runID string) (finishReport func() error) {
	t.Helper()
	locked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	report := make(chan error, 1)
	go func() { report <- runThenServers(t, s, ctx, runID, locked, release) }()
	<-locked
	return func() error {
		let()
		return <-report
	}
}

// Deleting a server while its Run's end is being recorded: both take the
// Run first, then the server, so one waits for the other and neither
// deadlocks. The same for expiry.
func TestServerChangesLockTheRunFirst(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, s *Server, ctx context.Context, key, id string) error
	}{
		{"delete", func(t *testing.T, s *Server, ctx context.Context, key, id string) error {
			if w := apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+id, nil); w.Code != http.StatusNoContent {
				return fmt.Errorf("%d %s", w.Code, w.Body)
			}
			return nil
		}},
		{"expiry", func(_ *testing.T, s *Server, ctx context.Context, _, _ string) error { return s.expireServers(ctx) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"lifetime": "owner", "expireAfter": "1h", "hostname": "web.lock.lux.example.com"})
			execSQL(t, s, ctx, `UPDATE run_servers SET created_at = now() - interval '2 hours' WHERE id = $1`, sv.ID)
			finishReport := holdRunEnd(t, s, ctx, r1)
			change := make(chan error, 1)
			go func() { change <- c.change(t, s, ctx, key, sv.ID) }()
			waitBlocked(t, s)
			rerr := finishReport()
			cerr := <-change
			if rerr != nil || cerr != nil {
				t.Fatalf("report: %v; %s: %v", rerr, c.name, cerr)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusNotFound {
				t.Fatalf("after the %s: %d %s", c.name, w.Code, w.Body)
			}
		})
	}
}

// An expiry pass waiting for the Run's lock acts on the server as it is
// once it has the lock: one requested meanwhile is no longer due and
// stays; one detached meanwhile is left for the next pass, which expires
// it.
func TestExpiryRechecksUnderLock(t *testing.T) {
	for _, c := range []struct {
		name, meanwhile string
		nextPass        int
	}{
		{"requested meanwhile", `UPDATE run_servers SET last_request_at = now() WHERE id = $1`, http.StatusOK},
		{"detached meanwhile", `UPDATE run_servers SET run_id = NULL WHERE id = $1`, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"lifetime": "owner", "expireAfter": "1h", "hostname": "web.race.lux.example.com"})
			execSQL(t, s, ctx, `UPDATE run_servers SET created_at = now() - interval '2 hours' WHERE id = $1`, sv.ID)
			finishReport := holdRunEnd(t, s, ctx, r1)
			expired := make(chan error, 1)
			go func() { expired <- s.expireServers(context.Background()) }()
			waitBlocked(t, s)
			execSQL(t, s, ctx, c.meanwhile, sv.ID)
			if err := finishReport(); err != nil {
				t.Fatal(err)
			}
			if err := <-expired; err != nil {
				t.Fatal(err)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusOK {
				t.Fatalf("%s, yet expired: %d %s", c.name, w.Code, w.Body)
			}
			if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.expired")); n != 0 {
				t.Fatalf("server.expired events: %d", n)
			}
			if err := s.expireServers(ctx); err != nil {
				t.Fatal(err)
			}
			if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != c.nextPass {
				t.Fatalf("after the next pass: %d, want %d", w.Code, c.nextPass)
			}
		})
	}
}

// The idle check and a wake write a server's row and an event of its Run
// while its Run's end is being recorded: they take the Run (KEY SHARE, as
// the event's foreign key does) before the server, so neither deadlocks
// with the report, and each still writes its one event.
func TestIdleAndWakeLockTheRunFirst(t *testing.T) {
	for _, c := range []struct {
		name, event string
		setup       string
		write       func(s *Server, id string) error
	}{
		{"idle", "server.idle",
			`UPDATE run_servers SET state = 'ready', ready_since = now() - interval '5 minutes', last_request_at = now() - interval '2 minutes' WHERE id = $1`,
			func(s *Server, _ string) error { return s.checkIdle(context.Background()) }},
		{"wake", "server.wake_requested", `SELECT $1::text`,
			func(s *Server, id string) error {
				asked, err := s.requestWake(context.Background(), id, "ada@example.com", "/")
				if err == nil && !asked {
					err = errors.New("did not ask")
				}
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := wakeFixture(t)
			sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
				"wake": "request", "idleAfter": "1m"})
			execSQL(t, s, ctx, c.setup, sv.ID)
			finishReport := holdRunEnd(t, s, ctx, r1)
			wrote := make(chan error, 1)
			go func() { wrote <- c.write(s, sv.ID) }()
			waitBlocked(t, s)
			rerr := finishReport()
			werr := <-wrote
			if rerr != nil || werr != nil {
				t.Fatalf("report: %v; %s: %v", rerr, c.name, werr)
			}
			if n := len(serverEventsOf(t, s, ctx, sv.ID, c.event)); n != 1 {
				t.Fatalf("%s events: %d", c.event, n)
			}
		})
	}
}

// An idle pass waiting for the Run's lock acts on the server as it is once
// it has the lock: one requested meanwhile is no longer due and is not
// reported idle.
func TestIdleRechecksUnderLock(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "runId": r1,
		"wake": "request", "idleAfter": "1m"})
	execSQL(t, s, ctx, `UPDATE run_servers SET state = 'ready', ready_since = now() - interval '5 minutes', last_request_at = now() - interval '2 minutes' WHERE id = $1`, sv.ID)
	finishReport := holdRunEnd(t, s, ctx, r1)
	idle := make(chan error, 1)
	go func() { idle <- s.checkIdle(context.Background()) }()
	waitBlocked(t, s)
	execSQL(t, s, ctx, `UPDATE run_servers SET last_request_at = now() WHERE id = $1`, sv.ID)
	if err := finishReport(); err != nil {
		t.Fatal(err)
	}
	if err := <-idle; err != nil {
		t.Fatal(err)
	}
	if n := len(serverEventsOf(t, s, ctx, sv.ID, "server.idle")); n != 0 {
		t.Fatalf("requested meanwhile, yet idle: %d", n)
	}
}

// hold is holdTx, released (if it is not yet) when the test ends, so a
// failure leaves no transaction waiting.
func hold(t *testing.T, ctx context.Context, s *Server, write func(pgx.Tx) error) *heldTx {
	h := holdTx(ctx, s, write)
	t.Cleanup(func() {
		select {
		case h.release <- nil:
		default:
		}
	})
	return h
}

// xidOf is the id of h's transaction (it holds one once it has locked).
func xidOf(t *testing.T, ctx context.Context, s *Server, h *heldTx) int64 {
	t.Helper()
	if !h.settle(t, ctx, s) {
		t.Fatal("the holding transaction is blocked")
	}
	var xid int64
	systemScan(t, s, `SELECT backend_xid::text::bigint FROM pg_stat_activity WHERE pid = $1`, []any{h.backend}, &xid)
	return xid
}

// waitWaitingOn returns once another backend waits for transaction xid.
func waitWaitingOn(t *testing.T, s *Server, xid int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		systemScan(t, s, `SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text::bigint = $1`,
			[]any{xid}, &n)
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited for transaction %d within 10s", xid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockRow locks one row (mode: FOR UPDATE, FOR NO KEY UPDATE, ...).
func lockRow(table, id, mode string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT 1 FROM `+table+` WHERE id = $1 `+mode, id)
		return err
	}
}

// setRunID sets a server's run_id (nil: detached) before its holder commits.
func setRunID(id string, runID *string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE run_servers SET run_id = $2 WHERE id = $1`, id, runID)
		return err
	}
}

func release(t *testing.T, h *heldTx, then func(pgx.Tx) error) {
	t.Helper()
	h.release <- then
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
}

// A change of a server reads its Run before locking either; when the
// server is attached meanwhile, the change locks that Run in a second try
// and succeeds. Attached or detached again before that try has it, it is
// refused: 409 conflict, the server untouched.
func TestServerChangeRetriesWhenTheServerMoves(t *testing.T) {
	run := r1
	t.Run("attached meanwhile: retried, deleted", func(t *testing.T) {
		s, ctx, key, _ := wakeFixture(t)
		sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}})
		srv := hold(t, ctx, s, lockRow("run_servers", sv.ID, "FOR UPDATE"))
		xid := xidOf(t, ctx, s, srv)
		del := make(chan *httptest.ResponseRecorder, 1)
		go func() { del <- apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+sv.ID, nil) }()
		waitWaitingOn(t, s, xid) // the delete read run_id NULL and waits for the server
		release(t, srv, setRunID(sv.ID, &run))
		if w := <-del; w.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		if w := apiCall(t, s, key, http.MethodGet, "/v1/servers/"+sv.ID, nil); w.Code != http.StatusNotFound {
			t.Fatalf("after the delete: %d %s", w.Code, w.Body)
		}
	})
	t.Run("moved before each try has it: 409", func(t *testing.T) {
		s, ctx, key, _ := wakeFixture(t)
		sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}})
		// The first try waits for the server, which is attached to r1.
		srv1 := hold(t, ctx, s, lockRow("run_servers", sv.ID, "FOR UPDATE"))
		xid1 := xidOf(t, ctx, s, srv1)
		del := make(chan *httptest.ResponseRecorder, 1)
		go func() { del <- apiCall(t, s, key, http.MethodDelete, "/v1/servers/"+sv.ID, nil) }()
		waitWaitingOn(t, s, xid1)
		// r1 is held (NO KEY UPDATE: the attach's foreign key check still
		// gets its KEY SHARE): the second try, having read r1, waits for it.
		runLock := hold(t, ctx, s, lockRow("runs", r1, "FOR NO KEY UPDATE"))
		xidRun := xidOf(t, ctx, s, runLock)
		release(t, srv1, setRunID(sv.ID, &run))
		waitWaitingOn(t, s, xidRun)
		// The server is held again, and detached before the second try,
		// once it has r1, gets it.
		srv2 := hold(t, ctx, s, lockRow("run_servers", sv.ID, "FOR UPDATE"))
		xid2 := xidOf(t, ctx, s, srv2)
		release(t, runLock, nil)
		waitWaitingOn(t, s, xid2)
		release(t, srv2, setRunID(sv.ID, nil))
		w := <-del
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"conflict"`) {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		if got := getSrv(t, s, key, sv.ID); got.RunID != nil {
			t.Fatalf("after the refused delete: %+v", got.RunID)
		}
	})
}
