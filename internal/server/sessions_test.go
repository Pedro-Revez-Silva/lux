package server

import (
	"context"
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
		rows, _ := tx.Query(ctx, `SELECT epoch, session_id, first_seen, last_seen FROM run_sessions
			WHERE run_id = $1 ORDER BY epoch, first_seen`, runID)
		var err error
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

// A resume that cannot load the old session starts a new one: both ids stay
// recorded, each at its epoch, while runs.session_id holds only the latest.
func TestRunSessionsResumeKeepsBothIDs(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	s.testAdapterEvent(t, ctx, 1, "old")
	s.testAdapterEvent(t, ctx, 2, "new")

	got := runSessions(t, s, ctx, "r1")
	if len(got) != 2 || got[0].Epoch != 1 || got[0].ID != "old" || got[1].Epoch != 2 || got[1].ID != "new" {
		t.Fatalf("run_sessions: %+v", got)
	}
	var latest string
	_ = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT session_id FROM runs WHERE id = 'r1'`).Scan(&latest)
	})
	if latest != "new" {
		t.Fatalf("runs.session_id = %q, want new", latest)
	}
}

// An id that only a snapshot manifest carries (no adapter event) is recorded
// at the snapshot's epoch, also for a late snapshot of an older placement.
func TestRunSessionsSnapshotOnly(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	sessionFixture(t, s, ctx)
	for _, sd := range []struct {
		epoch    int
		snap, id string
	}{{1, "snap1", "from-epoch-1"}, {2, "snap2", "from-manifest"}} {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applySnapshotDone(ctx, tx, "t1", "h1", "r1", sd.epoch, 2, proto.SnapshotDone{
				Manifest: proto.Manifest{SnapshotID: sd.snap, RunID: "r1", Epoch: sd.epoch, SessionID: sd.id, Volumes: []proto.VolumeSnapshot{}},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got := runSessions(t, s, ctx, "r1")
	if len(got) != 2 || got[0].Epoch != 1 || got[0].ID != "from-epoch-1" || got[1].Epoch != 2 || got[1].ID != "from-manifest" {
		t.Fatalf("run_sessions: %+v", got)
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
