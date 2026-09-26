package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// The system tick also records this machine and its Postgres, at the whole
// system sample's instant; a tracked path that does not exist is skipped
// and logged once, and does not fail the tick.
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
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
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
	if len(rows) != 2 {
		t.Fatalf("want 2 control samples, got %+v", rows)
	}
	for _, x := range rows {
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
		if len(x.paths) != 1 || x.paths[0] != "/" || x.total <= 0 || x.used <= 0 || x.used+x.free > x.total {
			t.Fatalf("disks: %+v", x)
		}
	}
}

// Control samples roll up like the rest: CPU (a counter) and totals their
// maximum, levels their mean, per disk path.
func TestRollupControl(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		for i, at := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second} {
			if _, err := tx.Exec(ctx, `INSERT INTO control_samples (res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections)
				VALUES (0, $1, $2, 4, $3, 1000, $4, $5)`, hour.Add(at), float64(i*10), int64(100*(i+1)), int64(10*(i+1)), i+1); err != nil {
				return err
			}
			for _, p := range []string{"/", "/data"} {
				if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (path, res, at, used_bytes, free_bytes, total_bytes)
					VALUES ($1, 0, $2, $3, $4, 1000)`, p, hour.Add(at), int64(100*(i+1)), int64(900-100*(i+1))); err != nil {
					return err
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
		res               int
		cpu               float64
		cpus, conns       int
		mem, memT, db     int64
		used, free, total int64
		disks             int
	}
	var got []row
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT c.res, c.cpu_seconds, c.cpus, c.db_connections, c.mem_bytes, c.mem_total, c.db_bytes,
				d.used_bytes, d.free_bytes, d.total_bytes,
				(SELECT count(*) FROM control_disk_samples x WHERE x.res = c.res AND x.at = c.at)
			FROM control_samples c JOIN control_disk_samples d ON d.res = c.res AND d.at = c.at AND d.path = '/data'
			WHERE c.res > 0 ORDER BY c.res, c.at`)
		if err != nil {
			return err
		}
		var x row
		_, err = pgx.ForEachRow(r, []any{&x.res, &x.cpu, &x.cpus, &x.conns, &x.mem, &x.memT, &x.db, &x.used, &x.free, &x.total, &x.disks}, func() error {
			got = append(got, x)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []row{
		{60, 10, 4, 2, 150, 1000, 15, 150, 750, 1000, 2}, // round(1.5) = 2
		{60, 30, 4, 4, 350, 1000, 35, 350, 550, 1000, 2}, // round(3.5) = 4
		{3600, 30, 4, 3, 250, 1000, 25, 250, 650, 1000, 2},
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
