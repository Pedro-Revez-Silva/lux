package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// testServer is a Server on a fresh database, for what e2e tests cannot
// wait for (rollups close minute and hour buckets). Needs LUX_TEST_PG, as
// the store tests do; skipped otherwise.
func testServer(t *testing.T) *Server {
	t.Helper()
	return testServerWith(t, "")
}

// testServerWith is testServer on a database created with opts (CREATE
// DATABASE's clauses, e.g. a collation).
func testServerWith(t *testing.T, opts string) *Server {
	t.Helper()
	admin := os.Getenv("LUX_TEST_PG")
	if admin == "" {
		t.Skip("LUX_TEST_PG not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	name := "lux_unit_" + ids.New("")[1:]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name+" "+opts); err != nil {
		t.Fatal(err)
	}
	cfg := conn.Config()
	owner := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, fmt.Sprintf("postgres://lux_app:lux_app@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		conn.Exec(ctx, "DROP DATABASE "+name)
		conn.Close(ctx)
	})
	return New(Config{}, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRollupHistory(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`); err != nil {
			return err
		}
		// Two raw samples in each of two minutes, and system samples
		// counting starts.
		for i, at := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
			if _, err := tx.Exec(ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds, mem_bytes, placements)
				VALUES ('h1', 0, $1, $2, $3, 1)`, hour.Add(at), float64(i*10), int64(100*(i+1))); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO system_samples (tenant_id, res, at, runs, started) VALUES ('', 0, $1, $2, 1)`,
				hour.Add(at), map[string]int{"running": i}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Twice: a second pass must not add or change anything.
	for range 2 {
		if err := s.rollupHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		at    time.Time
		cpu   float64
		mem   int64
		start int
		runs  map[string]int
	}
	var mins []row
	var hours []row
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT h.at, h.cpu_seconds, h.mem_bytes, s.started, s.runs FROM host_samples h
			JOIN system_samples s ON s.at = h.at AND s.res = h.res AND s.tenant_id = '' WHERE h.res = 60 ORDER BY h.at`)
		mins, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.at, &x.cpu, &x.mem, &x.start, &x.runs)
		})
		if err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, `SELECT h.at, h.cpu_seconds, h.mem_bytes, s.started, s.runs FROM host_samples h
			JOIN system_samples s ON s.at = h.at AND s.res = h.res AND s.tenant_id = '' WHERE h.res = 3600 ORDER BY h.at`)
		hours, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.at, &x.cpu, &x.mem, &x.start, &x.runs)
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mins) != 2 {
		t.Fatalf("minute rollups: %+v", mins)
	}
	// Counters: the last (max); levels: the mean; flows: the sum; states: the last.
	if m := mins[0]; !m.at.Equal(hour) || m.cpu != 10 || m.mem != 150 || m.start != 2 || m.runs["running"] != 1 {
		t.Fatalf("first minute: %+v", m)
	}
	if m := mins[1]; m.cpu != 30 || m.mem != 350 || m.runs["running"] != 3 {
		t.Fatalf("second minute: %+v", m)
	}
	if len(hours) != 1 || !hours[0].at.Equal(hour) || hours[0].start != 4 || hours[0].cpu != 30 || hours[0].mem != 250 {
		t.Fatalf("hour rollup: %+v", hours)
	}
}

// GET /v1/history carries the stored bytes: a tenant key its own, an
// operator the whole system's; zero bytes are sent, not left out.
func TestSystemHistoryStored(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'acme')`)
	tenantKey, opKey := ids.Secret("luxk"), ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('kt', 't1', 'k', $1, ARRAY['read'])`, ids.Hash(tenantKey))
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, stored_volume, stored_output, stored_artifact, stored_context) VALUES
		('', 0, now() - interval '1 minute', 10, 20, 30, 40), ('t1', 0, now() - interval '1 minute', 1, 0, 3, 0)`)
	for key, want := range map[string]string{tenantKey: "1 0 3 0", opKey: "10 20 30 40"} {
		h := historyRequest(t, s, key, "/v1/history?res=0&since=1h")
		if len(h.Samples) != 1 {
			t.Fatalf("samples: %+v", h.Samples)
		}
		sm := h.Samples[0]
		if sm.StoredVolume == nil || sm.StoredOutput == nil || sm.StoredArtifact == nil || sm.StoredContext == nil {
			t.Fatalf("missing stored bytes: %+v", sm)
		}
		if got := fmt.Sprint(*sm.StoredVolume, *sm.StoredOutput, *sm.StoredArtifact, *sm.StoredContext); got != want {
			t.Errorf("stored %s, want %s", got, want)
		}
	}
}

// The rollups average stored bytes per bucket, each kind and tenant apart.
func TestRollupStored(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i, off := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
		v := int64(100 * (i + 1))
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, stored_volume, stored_output, stored_artifact, stored_context)
			VALUES ('', 0, $1, $2::bigint, $2::bigint + 1, $2::bigint + 2, $2::bigint + 3), ('ta', 0, $1, $2::bigint * 2, 0, 0, 10)`, hour.Add(off), v)
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string][]int64{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT tenant_id || '/' || res, stored_volume, stored_output, stored_artifact, stored_context
			FROM system_samples WHERE res > 0 ORDER BY tenant_id, res, at`)
		var key string
		var v [4]int64
		_, err := pgx.ForEachRow(rows, []any{&key, &v[0], &v[1], &v[2], &v[3]}, func() error {
			got[key] = append(got[key], v[:]...)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int64{
		"/60":     {150, 151, 152, 153, 350, 351, 352, 353},
		"/3600":   {250, 251, 252, 253},
		"ta/60":   {300, 0, 0, 10, 700, 0, 0, 10},
		"ta/3600": {500, 0, 0, 10},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rollups:\n got %v\nwant %v", got, want)
	}
}

// The rollup's table-wide lower bound skips nothing: a key whose newest
// bucket lags another key's is still rolled up, in the rollup that follows
// and in the next hour's; a sample committed late into the newest rolled-up
// minute is still read (pb's hour h0, rolled up before it, keeps its value).
func TestRollupAcrossKeys(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	h0 := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	h1 := h0.Add(time.Hour)
	insert := func(rows ...any) {
		t.Helper()
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			for i := 0; i < len(rows); i += 3 {
				if _, err := tx.Exec(ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, running, started) VALUES ($1, '', 0, $2, $3, 1)`,
					rows[i], rows[i+1], rows[i+2]); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	type bucket struct {
		pool    string
		res     int
		at      time.Time
		running int
		started int
	}
	read := func() []bucket {
		t.Helper()
		var got []bucket
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT pool_id, res, at, running, started FROM pool_samples WHERE res > 0 ORDER BY res, pool_id, at`)
			var err error
			got, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (bucket, error) {
				var b bucket
				return b, r.Scan(&b.pool, &b.res, &b.at, &b.running, &b.started)
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO pools (id, name, provider) VALUES ('pa', 'a', 'static'), ('pb', 'b', 'static')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := func(h time.Time, min int, sec int) time.Time {
		return h.Add(time.Duration(min)*time.Minute + time.Duration(sec)*time.Second)
	}
	// pb's samples stop after its first minute; pa's go on for two more.
	insert("pa", m(h0, 0, 0), 2, "pa", m(h0, 0, 30), 4, "pa", m(h0, 1, 0), 6, "pa", m(h0, 2, 0), 8,
		"pb", m(h0, 0, 0), 10)
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	// A sample of pb committed late, at the start of pa's newest rolled-up
	// minute: the table-wide bound is that instant, inclusive, so it is read.
	insert("pb", m(h0, 2, 0), 40)
	// Next hour pb has more minutes than pa.
	insert("pa", m(h1, 0, 0), 1,
		"pb", m(h1, 0, 0), 20, "pb", m(h1, 1, 0), 30)
	for range 2 {
		if err := s.rollupHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := []bucket{
		{"pa", 60, m(h0, 0, 0), 3, 2}, {"pa", 60, m(h0, 1, 0), 6, 1}, {"pa", 60, m(h0, 2, 0), 8, 1}, {"pa", 60, m(h1, 0, 0), 1, 1},
		{"pb", 60, m(h0, 0, 0), 10, 1}, {"pb", 60, m(h0, 2, 0), 40, 1}, {"pb", 60, m(h1, 0, 0), 20, 1}, {"pb", 60, m(h1, 1, 0), 30, 1},
		{"pa", 3600, h0, 6, 4}, {"pa", 3600, h1, 1, 1},
		{"pb", 3600, h0, 10, 1}, {"pb", 3600, h1, 25, 2},
	}
	got := read()
	if len(got) != len(want) {
		t.Fatalf("buckets:\n got %v\nwant %v", got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.pool != w.pool || g.res != w.res || !g.at.Equal(w.at) || g.running != w.running || g.started != w.started {
			t.Fatalf("bucket %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// sampleSystem counts each tenant's Runs and the whole system's; a tenant's
// hosts are its own and the platform's; a start is counted once its window
// reaches it.
func TestSampleSystem(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('tb', 'b')`)
	exec(`INSERT INTO hosts (id, name, state, tenant_id, capacity) VALUES
		('ha', 'ha', 'ready', 'ta', '{"cpus": 4}'), ('hp', 'hp', 'ready', NULL, '{"cpus": 8}')`)
	exec(`INSERT INTO runs (id, tenant_id, spec, state, activity, created_at, first_started_at) VALUES
		('r1', 'ta', '{}', 'running', 'busy', now() - interval '3 minutes', now() - interval '2 minutes'),
		('r2', 'ta', '{}', 'submitted', '', now(), NULL),
		('r3', 'tb', '{}', 'running', 'idle', now() - interval '10 minutes', now() - interval '9 minutes')`)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Runs                     map[string]int
		Busy, Idle, Queued, Strt int
		CapCPUs                  float64
	}
	read := func() map[string]row {
		t.Helper()
		got := map[string]row{}
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT DISTINCT ON (tenant_id) tenant_id, runs, busy, idle, queued, started, cap_cpus
				FROM system_samples ORDER BY tenant_id, at DESC`)
			var id string
			var r row
			_, err := pgx.ForEachRow(rows, []any{&id, &r.Runs, &r.Busy, &r.Idle, &r.Queued, &r.Strt, &r.CapCPUs}, func() error {
				got[id] = r
				return nil
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The first window is SampleEvery long, ending a minute ago: no starts.
	want := map[string]row{
		"":   {map[string]int{"running": 2, "submitted": 1}, 1, 1, 1, 0, 12},
		"ta": {map[string]int{"running": 1, "submitted": 1}, 1, 0, 1, 0, 12},
		"tb": {map[string]int{"running": 1}, 0, 1, 0, 0, 8},
	}
	if got := read(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("first sample:\n got %v\nwant %v", got, want)
	}
	// A window from 5 minutes ago takes in r1's start, once.
	exec(`UPDATE system_samples SET window_end = now() - interval '5 minutes', at = now() - interval '5 minutes'`)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); got["ta"].Strt != 1 || got[""].Strt != 1 || got["tb"].Strt != 0 {
		t.Fatalf("second sample: %v", got)
	}
}

// sampleSystem records each tenant's and the whole system's bytes in S3 by
// kind: blobs still on a host or deleted are not counted, and a tenant with
// no live Run, flow or host but bytes in S3 still gets its row.
func TestSampleSystemStored(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('tc', 'c'), ('td', 'd')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, activity) VALUES
		('ra', 'ta', '{}', 'running', 'busy'), ('rc', 'tc', '{}', 'succeeded', ''), ('rd', 'td', '{}', 'failed', '')`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, location) VALUES
		('a1', 'ta', 'ra', 1, 'volume', 'v', 100, 's3'), ('a2', 'ta', 'ra', 1, 'volume', 'w', 20, 's3'),
		('a3', 'ta', 'ra', 1, 'output', 'o', 7, 's3'), ('a4', 'ta', 'ra', 1, 'volume', 'x', 1000, 'host'),
		('a5', 'ta', 'ra', 1, 'artifact', 'y', 5000, 'deleted'),
		('c1', 'tc', 'rc', 1, 'artifact', 'a', 3, 's3'), ('c2', 'tc', 'rc', 1, 'context', 'c', 40, 's3'),
		('c3', 'tc', 'rc', 1, 'output', 'o', 9, 's3'),
		('d1', 'td', 'rd', 1, 'volume', 'v', 50, 'deleted')`)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string][4]int64{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT tenant_id, stored_volume, stored_output, stored_artifact, stored_context FROM system_samples`)
		var id string
		var v [4]int64
		_, err := pgx.ForEachRow(rows, []any{&id, &v[0], &v[1], &v[2], &v[3]}, func() error {
			got[id] = v
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// td has a finished Run and only deleted bytes: no row.
	want := map[string][4]int64{
		"":   {120, 16, 3, 40},
		"ta": {120, 7, 0, 0},
		"tc": {0, 9, 3, 40},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored:\n got %v\nwant %v", got, want)
	}
}

// sampleSystem's hosts and capacity: a tenant counts its own non-terminated
// hosts and the platform's, never another tenant's; the whole system (the
// empty tenant id) counts all.
// Capacity is the ready and draining hosts' cpus and memory. A tenant with
// only bytes in S3 gets the platform's.
func TestSampleSystemHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('ts', 's'), ('tz', 'z')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, tenant_id, capacity) VALUES
		('ha1', 'ha1', 'ready', 'ta', '{"cpus": 4, "memory": 1000}'),
		('ha2', 'ha2', 'lost', 'ta', '{"cpus": 16, "memory": 9}'),
		('ha3', 'ha3', 'terminated', 'ta', '{"cpus": 128, "memory": 3}'),
		('hz', 'hz', 'ready', 'tz', '{"cpus": 64, "memory": 64000}'),
		('hp1', 'hp1', 'ready', NULL, '{"cpus": 8, "memory": 4000}'),
		('hp2', 'hp2', 'draining', NULL, '{"cpus": 1, "memory": 100}'),
		('hp3', 'hp3', 'lost', NULL, '{"cpus": 32, "memory": 7}'),
		('hp4', 'hp4', 'terminated', NULL, '{"cpus": 256, "memory": 5}')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('rs', 'ts', '{}', 'succeeded')`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, location) VALUES
		('s1', 'ts', 'rs', 1, 'output', 'o', 11, 's3')`)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT tenant_id, hosts, cap_cpus, cap_mem FROM system_samples`)
		var id string
		var hosts map[string]int
		var cpus float64
		var mem int64
		_, err := pgx.ForEachRow(rows, []any{&id, &hosts, &cpus, &mem}, func() error {
			got[id] = fmt.Sprint(hosts, cpus, mem)
			hosts = nil
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"":   fmt.Sprint(map[string]int{"ready": 3, "draining": 1, "lost": 2}, 77.0, int64(69100)),
		"ta": fmt.Sprint(map[string]int{"ready": 2, "draining": 1, "lost": 2}, 13.0, int64(5100)),
		"ts": fmt.Sprint(map[string]int{"ready": 1, "draining": 1, "lost": 1}, 9.0, int64(4100)),
		"tz": fmt.Sprint(map[string]int{"ready": 2, "draining": 1, "lost": 1}, 73.0, int64(68100)),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("hosts, cpus, memory:\n got %v\nwant %v", got, want)
	}
}
