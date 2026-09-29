package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

type sessionRow struct {
	Epoch               int
	ID                  string
	FirstSeen, LastSeen time.Time
}

func runSessions(t *testing.T, s *Server, ctx context.Context, runID string) []sessionRow {
	t.Helper()
	var out []sessionRow
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT epoch, session_id, first_seen, last_seen FROM run_sessions
			WHERE run_id = $1 ORDER BY epoch, first_seen`, runID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[sessionRow])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sessionFixture(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 2)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'exited'), ('p2', 't1', 'r1', 'h1', 2, 'running')`)
}

func (s *Server) testAdapterEvent(t *testing.T, ctx context.Context, epoch int, sessionID string) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyAdapterEvent(ctx, tx, "t1", "r1", epoch, proto.AdapterEvent{SessionID: sessionID})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runSessionID reads the Run's current session and snapshot ids.
func runSessionID(t *testing.T, s *Server, ctx context.Context, runID string) (sessionID, snapshotID string) {
	t.Helper()
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT session_id, coalesce(snapshot_id, '') FROM runs WHERE id = $1`, runID).Scan(&sessionID, &snapshotID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return sessionID, snapshotID
}

// reportSession sends a runner's adapter event for r1 through the report
// path (fencing included) from host h1, and returns the reply.
func (s *Server) reportSession(t *testing.T, ctx context.Context, epoch int, sessionID string) proto.Frame {
	t.Helper()
	return s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgAdapterEvent, ID: 1, RunID: "r1", Epoch: epoch,
		Data: proto.Marshal(proto.AdapterEvent{SessionID: sessionID})})
}

// A resume that cannot load the old session starts a new one: both ids stay
// recorded, each at its epoch, while runs.session_id holds only the latest.
// Once the Run is at epoch 2, a report from epoch 1 is refused as stale and
// records nothing.
func TestRunSessionsResumeKeepsBothIDs(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)
	if r := s.reportSession(t, ctx, 1, "old"); r.Type != proto.MsgAck {
		t.Fatalf("epoch 1 report: %s %s", r.Type, r.Data)
	}

	// The resume: epoch 1's placement ends, epoch 2's starts.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'p1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p2', 't1', 'r1', 'h1', 2, 'running')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'r1'`)
	if r := s.reportSession(t, ctx, 2, "new"); r.Type != proto.MsgAck {
		t.Fatalf("epoch 2 report: %s %s", r.Type, r.Data)
	}

	got := runSessions(t, s, ctx, "r1")
	if len(got) != 2 || got[0].Epoch != 1 || got[0].ID != "old" || got[1].Epoch != 2 || got[1].ID != "new" {
		t.Fatalf("run_sessions: %+v", got)
	}
	if latest, _ := runSessionID(t, s, ctx, "r1"); latest != "new" {
		t.Fatalf("runs.session_id = %q, want new", latest)
	}

	r := s.reportSession(t, ctx, 1, "late")
	var nack proto.Nack
	if r.Type != proto.MsgNack || json.Unmarshal(r.Data, &nack) != nil || !nack.Stale {
		t.Fatalf("late epoch 1 report: %s %s, want a stale nack", r.Type, r.Data)
	}
	if after := runSessions(t, s, ctx, "r1"); len(after) != 2 || after[0] != got[0] || after[1] != got[1] {
		t.Fatalf("a stale report changed run_sessions: %+v", after)
	}
	if latest, _ := runSessionID(t, s, ctx, "r1"); latest != "new" {
		t.Fatalf("a stale report set runs.session_id = %q", latest)
	}
}

// An id that only a snapshot manifest carries (no adapter event) is recorded
// at the snapshot's epoch, also for a late snapshot of an older placement,
// which leaves the Run's session and snapshot at epoch 2's.
func TestRunSessionsSnapshotOnly(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	for _, sd := range []struct {
		epoch    int
		snap, id string
	}{{2, "snap2", "from-manifest"}, {1, "snap1", "from-epoch-1"}} {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := s.applySnapshotDone(ctx, tx, "t1", "h1", "r1", sd.epoch, 2, proto.SnapshotDone{
				Manifest: proto.Manifest{SnapshotID: sd.snap, RunID: "r1", Epoch: sd.epoch, SessionID: sd.id, Volumes: []proto.VolumeSnapshot{}},
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got := runSessions(t, s, ctx, "r1")
	if len(got) != 2 || got[0].Epoch != 1 || got[0].ID != "from-epoch-1" || got[1].Epoch != 2 || got[1].ID != "from-manifest" {
		t.Fatalf("run_sessions: %+v", got)
	}
	if session, snapshot := runSessionID(t, s, ctx, "r1"); session != "from-manifest" || snapshot != "snap2" {
		t.Fatalf("runs: session_id %q snapshot_id %q, want from-manifest, snap2", session, snapshot)
	}
}

// The same id reported again (every adapter message carries it) moves
// last_seen and adds no row.
func TestRunSessionsRepeatUpdatesLastSeen(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	s.testAdapterEvent(t, ctx, 2, "same")
	first := runSessions(t, s, ctx, "r1")
	time.Sleep(20 * time.Millisecond)
	s.testAdapterEvent(t, ctx, 2, "same")
	got := runSessions(t, s, ctx, "r1")
	if len(first) != 1 || len(got) != 1 {
		t.Fatalf("rows: before %+v after %+v", first, got)
	}
	if !got[0].FirstSeen.Equal(first[0].FirstSeen) || !got[0].LastSeen.After(first[0].LastSeen) {
		t.Fatalf("first/last seen: before %+v after %+v", first[0], got[0])
	}
}

// run_sessions is tenant scoped: t1's scope reads only t1's sessions and
// cannot write one for t2.
func TestRunSessionsTenantIsolation(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running'), ('r2', 't2', '{}', 'running')`)
	execSQL(t, s, ctx, `INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id) VALUES
		('t1', 'r1', 1, 'mine'), ('t2', 'r2', 1, 'theirs'), ('t2', 'r2', 2, 'theirs-too')`)

	var ids []string
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT session_id FROM run_sessions ORDER BY session_id`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil || len(ids) != 1 || ids[0] != "mine" {
		t.Errorf("t1 sees sessions %v (%v), want [mine]", ids, err)
	}
	err = s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_sessions (tenant_id, run_id, epoch, session_id) VALUES ('t2', 'r2', 3, 'evil')`)
		return err
	})
	if err == nil {
		t.Error("t1 wrote a session for t2")
	}
}
