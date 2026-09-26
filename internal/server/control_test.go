package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// The system tick also records this machine and its Postgres, at the whole
// system sample's instant; a tracked path that does not exist is skipped
// and logged once, does not fail the tick, and is sampled again once it
// exists.
func TestSampleControlHost(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	missing := t.TempDir() + "/gone"
	s.cfg.DiskPaths = []string{"/", missing}
	for range 2 {
		if err := s.sampleSystem(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "disk path skipped"); n != 1 || !strings.Contains(logs.String(), missing) {
		t.Fatalf("want one log line naming %s, got:\n%s", missing, logs.String())
	}
	// Once the path exists, the next ticks sample it again, and say so once.
	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.sampleSystem(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "disk path readable again"); n != 1 {
		t.Fatalf("want one recovery line, got %d:\n%s", n, logs.String())
	}
	var perTick []int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT (SELECT count(*) FROM control_disk_samples d WHERE d.res = 0 AND d.at = c.at AND d.path = $1)
			FROM control_samples c WHERE c.res = 0 ORDER BY c.at`, missing)
		if err != nil {
			return err
		}
		perTick, err = pgx.CollectRows(r, pgx.RowTo[int])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(perTick, []int{0, 0, 1, 1}) {
		t.Fatalf("samples of %s per tick: %v, want [0 0 1 1]", missing, perTick)
	}
	type row struct {
		at                      time.Time
		cpuS                    float64
		cpus, conns             int
		mem, memT, db           int64
		sysAt                   *time.Time
		paths                   []string
		used, free, total, disk int64
	}
	var rows []row
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT c.at, c.cpu_seconds, c.cpus, c.db_connections, c.mem_bytes, c.mem_total, c.db_bytes,
				(SELECT at FROM system_samples WHERE tenant_id = '' AND res = 0 AND at = c.at),
				(SELECT array_agg(path ORDER BY path) FROM control_disk_samples d WHERE d.res = 0 AND d.at = c.at),
				d.used_bytes, d.free_bytes, d.total_bytes
			FROM control_samples c JOIN control_disk_samples d ON d.at = c.at AND d.res = 0 AND d.path = '/'
			WHERE c.res = 0 ORDER BY c.at`)
		if err != nil {
			return err
		}
		var x row
		_, err = pgx.ForEachRow(r, []any{&x.at, &x.cpuS, &x.cpus, &x.conns, &x.mem, &x.memT, &x.db, &x.sysAt, &x.paths, &x.used, &x.free, &x.total}, func() error {
			rows = append(rows, x)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 control samples, got %+v", rows)
	}
	for i, x := range rows {
		if x.sysAt == nil || !x.sysAt.Equal(x.at) {
			t.Fatalf("control sample at %v has no whole-system sample at the same instant", x.at)
		}
		if x.cpuS <= 0 || x.cpus <= 0 || x.mem <= 0 || x.memT < x.mem {
			t.Fatalf("host figures: %+v", x)
		}
		// This test's own connection (and luxd's pool) is connected.
		if x.db <= 0 || x.conns < 1 {
			t.Fatalf("postgres figures: %+v", x)
		}
		if want := map[bool][]string{true: {"/"}, false: {"/", missing}}[i < 2]; !slices.Equal(x.paths, want) || x.total <= 0 || x.used <= 0 || x.used+x.free > x.total {
			t.Fatalf("disks: %+v", x)
		}
	}
}

// When the Postgres size probe fails, the tick still writes its samples,
// without the two Postgres fields; the failure is logged once, and so is
// the recovery.
func TestSampleControlPostgresProbeFails(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.cfg.DiskPaths = []string{"/"}
	fail := true
	s.readPostgres = func(ctx context.Context) (int64, int, error) {
		if fail {
			return 0, 0, errors.New("probe timed out")
		}
		return s.postgresFigures(ctx)
	}
	for i := range 4 {
		fail = i < 2
		if err := s.sampleSystem(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	type row struct {
		cpus  *int
		db    *int64
		conns *int
	}
	var rows []row
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT cpus, db_bytes, db_connections FROM control_samples WHERE res = 0 ORDER BY at`)
		if err != nil {
			return err
		}
		var x row
		_, err = pgx.ForEachRow(r, []any{&x.cpus, &x.db, &x.conns}, func() error {
			rows = append(rows, x)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 control samples, got %d", len(rows))
	}
	for i, x := range rows {
		if x.cpus == nil {
			t.Errorf("tick %d: host figures missing", i)
		}
		if failed := i < 2; failed != (x.db == nil) || failed != (x.conns == nil) {
			t.Errorf("tick %d: db %v conns %v", i, x.db, x.conns)
		}
	}
	if n := strings.Count(logs.String(), "postgres size and connections skipped"); n != 1 {
		t.Errorf("want one failure line, got %d:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "postgres size and connections readable again"); n != 1 {
		t.Errorf("want one recovery line, got %d:\n%s", n, logs.String())
	}
}

// Unset disk paths track "/"; an explicitly empty list tracks none, and
// the tick still writes the control sample.
func TestControlDiskPathsEmpty(t *testing.T) {
	base := testServer(t)
	ctx := context.Background()
	if got := New(Config{}, base.db, nil, base.log).cfg.DiskPaths; !slices.Equal(got, []string{"/"}) {
		t.Fatalf("unset: %q", got)
	}
	s := New(Config{DiskPaths: []string{}}, base.db, nil, base.log)
	if err := s.sampleSystem(ctx); err != nil {
		t.Fatal(err)
	}
	var samples, disks int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM control_samples), (SELECT count(*) FROM control_disk_samples)`).Scan(&samples, &disks)
	})
	if err != nil || samples != 1 || disks != 0 {
		t.Fatalf("control samples %d, disk samples %d, err %v", samples, disks, err)
	}
}

// Each resolution's control and control disk samples are deleted once
// older than that resolution's retention; younger ones stay. Each
// resolution's rows belong to their own instance, so the rollups the same
// pass writes (into coarser resolutions) are told apart from the fixture.
func TestControlRetention(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.HistoryRaw, s.cfg.HistoryMinutes, s.cfg.HistoryHours = time.Hour, 3*time.Hour, 10*time.Hour
	now := time.Now().UTC().Truncate(time.Second)
	seed := map[string]struct {
		res           int
		expired, live time.Duration
	}{
		"raw": {0, 2 * time.Hour, 10 * time.Minute},
		"min": {60, 4 * time.Hour, 2 * time.Hour},
		"hr":  {3600, 11 * time.Hour, 5 * time.Hour},
	}
	for inst, x := range seed {
		for _, age := range []time.Duration{x.expired, x.live} {
			execSQL(t, s, ctx, `INSERT INTO control_samples (instance, res, at, cpu_seconds) VALUES ($1, $2, $3, 1)`, inst, x.res, now.Add(-age))
			execSQL(t, s, ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
				VALUES ($1, '/', $2, $3, 1, 1, 2)`, inst, x.res, now.Add(-age))
		}
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"control_samples", "control_disk_samples"} {
		got := map[string][]time.Time{}
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			r, err := tx.Query(ctx, `SELECT instance, at FROM `+table+`
				WHERE (instance, res) IN (('raw', 0), ('min', 60), ('hr', 3600)) ORDER BY at`)
			if err != nil {
				return err
			}
			var inst string
			var at time.Time
			_, err = pgx.ForEachRow(r, []any{&inst, &at}, func() error {
				got[inst] = append(got[inst], at)
				return nil
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		for inst, x := range seed {
			if len(got[inst]) != 1 || !got[inst][0].Equal(now.Add(-x.live)) {
				t.Errorf("%s, %s (res %d): survivors %v, want only the one at -%v", table, inst, x.res, got[inst], x.live)
			}
		}
	}
}

// Control samples roll up like the rest, per luxd instance: CPU (a
// counter) and totals their maximum, levels their mean, per disk path.
// Instance b's counter has another baseline; its buckets never mix with a's.
func TestRollupControl(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for inst, base := range map[string]float64{"a": 0, "b": 100000} {
			for i, at := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
				if _, err := tx.Exec(ctx, `INSERT INTO control_samples (instance, res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections)
					VALUES ($1, 0, $2, $3, 4, $4, 1000, $5, $6)`, inst, hour.Add(at), base+float64(i*10), int64(100*(i+1)), int64(10*(i+1)), i+1); err != nil {
					return err
				}
				for _, p := range []string{"/", "/data"} {
					if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
						VALUES ($1, $2, 0, $3, $4, $5, 1000)`, inst, p, hour.Add(at), int64(100*(i+1)), int64(900-100*(i+1))); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.rollupHistory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		instance          string
		res               int
		cpu               float64
		cpus, conns       int
		mem, memT, db     int64
		used, free, total int64
		disks             int
	}
	var got []row
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT c.instance, c.res, c.cpu_seconds, c.cpus, c.db_connections, c.mem_bytes, c.mem_total, c.db_bytes,
				d.used_bytes, d.free_bytes, d.total_bytes,
				(SELECT count(*) FROM control_disk_samples x WHERE x.instance = c.instance AND x.res = c.res AND x.at = c.at)
			FROM control_samples c JOIN control_disk_samples d ON d.instance = c.instance AND d.res = c.res AND d.at = c.at AND d.path = '/data'
			WHERE c.res > 0 ORDER BY c.instance, c.res, c.at`)
		if err != nil {
			return err
		}
		var x row
		_, err = pgx.ForEachRow(r, []any{&x.instance, &x.res, &x.cpu, &x.cpus, &x.conns, &x.mem, &x.memT, &x.db, &x.used, &x.free, &x.total, &x.disks}, func() error {
			got = append(got, x)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"a", 60, 10, 4, 2, 150, 1000, 15, 150, 750, 1000, 2}, // round(1.5) = 2
		{"a", 60, 30, 4, 4, 350, 1000, 35, 350, 550, 1000, 2}, // round(3.5) = 4
		{"a", 3600, 30, 4, 3, 250, 1000, 25, 250, 650, 1000, 2},
		{"b", 60, 100010, 4, 2, 150, 1000, 15, 150, 750, 1000, 2},
		{"b", 60, 100030, 4, 4, 350, 1000, 35, 350, 550, 1000, 2},
		{"b", 3600, 100030, 4, 3, 250, 1000, 25, 250, 650, 1000, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("rollups:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rollup %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// Two luxd instances on one database, at very different CPU counter
// baselines, sample alternately; the second reboots (its counter drops).
// /v1/history serves the instance with the latest sample in the range, and
// its CPU rate only between its own consecutive samples: never a rate
// across instances, none (not a negative or huge one) across the reboot.
func TestControlCPURatePerInstance(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	opKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	type pt struct {
		inst string
		off  time.Duration
		cpu  float64
	}
	pts := []pt{
		{"a", 0, 1000}, {"b", 10 * time.Second, 100000},
		{"a", 20 * time.Second, 1040}, {"b", 30 * time.Second, 100100},
		{"a", 40 * time.Second, 1080}, {"b", 50 * time.Second, 50}, // b rebooted
		{"b", 70 * time.Second, 150},
	}
	for _, p := range pts {
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at) VALUES ('', 0, $1)`, base.Add(p.off))
		execSQL(t, s, ctx, `INSERT INTO control_samples (instance, res, at, cpu_seconds, cpus) VALUES ($1, 0, $2, $3, 8)`, p.inst, base.Add(p.off), p.cpu)
	}
	get := func(to time.Duration) History {
		t.Helper()
		q := url.Values{"res": {"0"}, "from": {base.Add(-time.Second).Format(time.RFC3339Nano)}, "to": {base.Add(to).Format(time.RFC3339Nano)}}
		req := httptest.NewRequest(http.MethodGet, "/v1/history?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+opKey)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var h History
		if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	check := func(h History, inst string, want map[time.Duration]*float64) {
		t.Helper()
		if len(h.Samples) == 0 {
			t.Fatal("no samples")
		}
		for _, sm := range h.Samples {
			off := sm.At.Sub(base)
			w, ours := want[off]
			if !ours {
				if sm.Control != nil {
					t.Errorf("+%v: another instance's sample served: %+v", off, sm.Control)
				}
				continue
			}
			c := sm.Control
			if c == nil || c.Instance != inst {
				t.Fatalf("+%v: want %s's control sample, got %+v", off, inst, c)
			}
			if (w == nil) != (c.CPUCores == nil) || w != nil && math.Abs(*w-*c.CPUCores) > 1e-9 {
				t.Errorf("+%v: cpuCores %v, want %v", off, deref(c.CPUCores), deref(w))
			}
		}
	}
	two, five := 2.0, 5.0
	// b's latest sample is the newest: b is served, a is not.
	check(get(80*time.Second), "b", map[time.Duration]*float64{
		10 * time.Second: nil, 30 * time.Second: &five, 50 * time.Second: nil, 70 * time.Second: &five,
	})
	// Up to +45s a has the newest sample.
	check(get(45*time.Second), "a", map[time.Duration]*float64{0: nil, 20 * time.Second: &two, 40 * time.Second: &two})
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// GET /v1/history carries the control host only for an operator key
// reading the whole system: a tenant key, and an operator narrowed to a
// tenant, get none of it, though the samples exist for their instants.
func TestSystemHistoryControlIsOperatorsOnly(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'acme')`)
	tenantKey, opKey := ids.Secret("luxk"), ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('kt', 't1', 'k', $1, ARRAY['read'])`, ids.Hash(tenantKey))
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	// t1's samples are stamped at the control samples' instants, so a
	// missing gate would attach the control host to them too.
	s.cfg.DiskPaths = []string{"/"}
	for range 2 {
		if err := s.sampleSystem(ctx); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at) SELECT 't1', 0, at FROM system_samples WHERE tenant_id = '' AND res = 0`)

	get := func(key, query string) History {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/history?res=0&since=1h"+query, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), `"control"`) != (key == opKey && query == "") {
			t.Errorf("key %s query %q: control in body = %v:\n%s", map[string]string{tenantKey: "tenant", opKey: "operator"}[key], query,
				strings.Contains(w.Body.String(), `"control"`), w.Body)
		}
		var h History
		if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, c := range []struct{ key, query string }{{tenantKey, ""}, {tenantKey, "&tenant="}, {opKey, "&tenant=acme"}, {opKey, "&tenant=t1"}} {
		if h := get(c.key, c.query); len(h.Samples) != 2 {
			t.Errorf("%q: want the tenant's 2 samples, got %d", c.query, len(h.Samples))
		}
	}
	h := get(opKey, "")
	if len(h.Samples) != 2 {
		t.Fatalf("operator: %+v", h)
	}
	for i, sm := range h.Samples {
		c := sm.Control
		if c == nil || c.CPUs == nil || *c.CPUs <= 0 || c.MemoryTotal == nil || c.DatabaseBytes == nil || c.DatabaseConnections == nil ||
			len(c.Disks) != 1 || c.Disks[0].Path != "/" || c.Disks[0].TotalBytes <= 0 {
			t.Fatalf("operator sample %d: %+v", i, c)
		}
		// CPU is a rate: none for the first point.
		if (c.CPUCores == nil) != (i == 0) {
			t.Errorf("sample %d cpuCores %v", i, c.CPUCores)
		}
	}
}
