package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func controlHistoryRequest(t *testing.T, s *Server, key, query string) History {
	t.Helper()
	return historyRequest(t, s, key, "/v1/history?"+query)
}

func historyRequest(t *testing.T, s *Server, key, target string) History {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, w.Code, w.Body)
	}
	var h History
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	return h
}

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
			execSQL(t, s, ctx, `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds) VALUES ($1, $1, $2, $3, 1)`, inst, x.res, now.Add(-age))
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
				if _, err := tx.Exec(ctx, `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections)
					VALUES ($1, $1, 0, $2, $3, 4, $4, 1000, $5, $6)`, inst, hour.Add(at), base+float64(i*10), int64(100*(i+1)), int64(10*(i+1)), i+1); err != nil {
					return err
				}
				// /data's total grows, so its maximum differs from its mean.
				n := int64(i + 1)
				if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
					VALUES ($1, '/', 0, $2, $3, $4, 1000), ($1, '/data', 0, $2, $5, $6, $7)`,
					inst, hour.Add(at), 100*n, 900-100*n, 5000+10*n, 4000-10*n, 9000+n); err != nil {
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
		instance      string
		res           int
		at            time.Time
		cpu           float64
		cpus, conns   int
		mem, memT, db int64
	}
	type diskRow struct {
		instance          string
		res               int
		at                time.Time
		path              string
		used, free, total int64
	}
	var got []row
	var gotDisks []diskRow
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT instance, res, at, cpu_seconds, cpus, db_connections, mem_bytes, mem_total, db_bytes
			FROM control_samples WHERE res > 0 ORDER BY instance, res, at`)
		if err != nil {
			return err
		}
		var x row
		if _, err = pgx.ForEachRow(r, []any{&x.instance, &x.res, &x.at, &x.cpu, &x.cpus, &x.conns, &x.mem, &x.memT, &x.db}, func() error {
			got = append(got, x)
			return nil
		}); err != nil {
			return err
		}
		r, err = tx.Query(ctx, `SELECT instance, res, at, path, used_bytes, free_bytes, total_bytes
			FROM control_disk_samples WHERE res > 0 ORDER BY instance, res, at, path`)
		if err != nil {
			return err
		}
		var d diskRow
		_, err = pgx.ForEachRow(r, []any{&d.instance, &d.res, &d.at, &d.path, &d.used, &d.free, &d.total}, func() error {
			gotDisks = append(gotDisks, d)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	m1 := hour.Add(time.Minute)
	want := []row{
		{"a", 60, hour, 10, 4, 2, 150, 1000, 15}, // round(1.5) = 2
		{"a", 60, m1, 30, 4, 4, 350, 1000, 35},   // round(3.5) = 4
		{"a", 3600, hour, 30, 4, 3, 250, 1000, 25},
		{"b", 60, hour, 100010, 4, 2, 150, 1000, 15},
		{"b", 60, m1, 100030, 4, 4, 350, 1000, 35},
		{"b", 3600, hour, 100030, 4, 3, 250, 1000, 25},
	}
	var wantDisks []diskRow
	for _, inst := range []string{"a", "b"} {
		wantDisks = append(wantDisks,
			diskRow{inst, 60, hour, "/", 150, 750, 1000}, diskRow{inst, 60, hour, "/data", 5015, 3985, 9002},
			diskRow{inst, 60, m1, "/", 350, 550, 1000}, diskRow{inst, 60, m1, "/data", 5035, 3965, 9004},
			diskRow{inst, 3600, hour, "/", 250, 650, 1000}, diskRow{inst, 3600, hour, "/data", 5025, 3975, 9004})
	}
	// Timestamps are compared as instants: the driver's carry a location.
	if !slices.EqualFunc(got, want, func(a, b row) bool { a.at, b.at = a.at.UTC(), b.at.UTC(); return a == b }) {
		t.Errorf("rollups:\n got %+v\nwant %+v", got, want)
	}
	if !slices.EqualFunc(gotDisks, wantDisks, func(a, b diskRow) bool { a.at, b.at = a.at.UTC(), b.at.UTC(); return a == b }) {
		t.Errorf("disk rollups:\n got %+v\nwant %+v", gotDisks, wantDisks)
	}
}

// Two machines on one database, at very different CPU counter baselines,
// sample alternately. On machine a, luxd restarts (a1, then a2); machine b
// reboots (its counter drops). /v1/history serves a series per machine,
// its CPU rate across luxd restarts but never across machines, none (not a
// negative or huge one) across the reboot; and a series per luxd process.
func TestControlCPURatePerInstance(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	opKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	type pt struct {
		inst, host string
		off        time.Duration
		cpu        float64
	}
	pts := []pt{
		{"luxd_a1", "a", 0, 1000}, {"luxd_b", "b", 10 * time.Second, 100000},
		{"luxd_a1", "a", 20 * time.Second, 1040}, {"luxd_b", "b", 30 * time.Second, 100100},
		{"luxd_a2", "a", 40 * time.Second, 1080}, {"luxd_b", "b", 50 * time.Second, 50}, // b rebooted
		{"luxd_b", "b", 70 * time.Second, 150},
	}
	for _, p := range pts {
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at) VALUES ('', 0, $1)`, base.Add(p.off))
		execSQL(t, s, ctx, `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus, proc_started, proc_cpu_seconds)
			VALUES ($1, $2, 0, $3, $4, 8, $5, 1)`, p.inst, p.host, base.Add(p.off), p.cpu, base.Add(-time.Hour))
	}
	q := url.Values{"res": {"0"}, "from": {base.Add(-time.Second).Format(time.RFC3339Nano)}, "to": {base.Add(80 * time.Second).Format(time.RFC3339Nano)}}
	c := controlHistoryRequest(t, s, opKey, q.Encode()).Control
	two, five := 2.0, 5.0
	type point struct {
		off   time.Duration
		cores *float64
	}
	want := map[string][]point{
		"a": {{0, nil}, {20 * time.Second, &two}, {40 * time.Second, &two}},
		"b": {{10 * time.Second, nil}, {30 * time.Second, &five}, {50 * time.Second, nil}, {70 * time.Second, &five}},
	}
	if c == nil || len(c.Machines) != 2 || c.Machines[0].Hostname != "a" || c.Machines[1].Hostname != "b" || len(c.Postgres) != len(pts) {
		t.Fatalf("control: %+v", c)
	}
	for _, m := range c.Machines {
		w := want[m.Hostname]
		if len(m.Samples) != len(w) {
			t.Fatalf("%s: %+v", m.Hostname, m)
		}
		for i, p := range m.Samples {
			if !p.At.Equal(base.Add(w[i].off)) {
				t.Errorf("%s sample %d at +%v, want +%v", m.Hostname, i, p.At.Sub(base), w[i].off)
			}
			if (w[i].cores == nil) != (p.CPUCores == nil) || w[i].cores != nil && math.Abs(*w[i].cores-*p.CPUCores) > 1e-9 {
				t.Errorf("%s +%v: cpuCores %v, want %v", m.Hostname, w[i].off, deref(p.CPUCores), deref(w[i].cores))
			}
		}
	}
	var luxd []string
	for _, l := range c.Luxd {
		luxd = append(luxd, fmt.Sprintf("%s@%s:%d", l.Instance, l.Hostname, len(l.Samples)))
	}
	if want := []string{"luxd_a1@a:2", "luxd_b@b:4", "luxd_a2@a:1"}; !slices.Equal(luxd, want) {
		t.Fatalf("luxd series %v, want %v", luxd, want)
	}
}

// Rolled-up control samples reach /v1/history at their resolution, with
// CPU rates between consecutive buckets and each path's disk figures. The
// rows predate per-process ids: the instance stands in for the hostname.
func TestRolledUpControlHistory(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	opKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(opKey))
	hour := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	// Two minute buckets in the first hour, one in the second. No raw sample
	// is at a bucket's start, so raw control samples cannot pass for rolled ones.
	offs := []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second, time.Hour, time.Hour + 30*time.Second}
	cpu := []float64{1000, 1030, 1060, 1120, 8170, 8200}
	for i, off := range offs {
		at := hour.Add(off + 5*time.Second)
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, queued) VALUES ('', 0, $1, 1)`, at)
		execSQL(t, s, ctx, `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus) VALUES ('ctl', 'ctl', 0, $1, $2, 8)`, at, cpu[i])
		n := int64(i + 1)
		execSQL(t, s, ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
			VALUES ('ctl', '/', 0, $1, $2, $3, 1000), ('ctl', '/data', 0, $1, $4, $5, 9000)`, at, 100*n, 1000-100*n, 5000+10*n, 4000-10*n)
	}
	if err := s.rollupHistory(ctx); err != nil {
		t.Fatal(err)
	}
	get := func(res string) History {
		t.Helper()
		q := url.Values{"res": {res}, "from": {hour.Add(-time.Second).Format(time.RFC3339Nano)}, "to": {hour.Add(2 * time.Hour).Format(time.RFC3339Nano)}}
		return controlHistoryRequest(t, s, opKey, q.Encode())
	}
	disks := func(rootUsed, dataUsed int64) []DiskSample {
		return []DiskSample{{"/", rootUsed, 1000 - rootUsed, 1000}, {"/data", dataUsed, 9000 - dataUsed, 9000}}
	}
	f := func(v float64) *float64 { return &v }
	type point struct {
		off   time.Duration
		cores *float64
		disks []DiskSample
	}
	for _, c := range []struct {
		res  string
		want []point
	}{
		// Minute buckets: CPU the bucket's maximum, disks its mean.
		{"60", []point{
			{0, nil, disks(150, 5015)},
			{time.Minute, f((1120 - 1030) / 60.0), disks(350, 5035)},
			{time.Hour, f((8200 - 1120) / (59 * 60.0)), disks(550, 5055)},
		}},
		// Hour buckets, from the minute ones.
		{"3600", []point{
			{0, nil, disks(250, 5025)},
			{time.Hour, f((8200 - 1120) / 3600.0), disks(550, 5055)},
		}},
	} {
		h := get(c.res)
		if h.Control == nil || len(h.Control.Machines) != 1 || h.Control.Machines[0].Hostname != "ctl" || len(h.Control.Machines[0].Samples) != len(c.want) {
			t.Fatalf("res %s: control %+v", c.res, h.Control)
		}
		for i, w := range c.want {
			ctl := h.Control.Machines[0].Samples[i]
			if !ctl.At.Equal(hour.Add(w.off)) || ctl.CPUs == nil || *ctl.CPUs != 8 {
				t.Fatalf("res %s sample %d: %+v, want at +%v", c.res, i, ctl, w.off)
			}
			if (w.cores == nil) != (ctl.CPUCores == nil) || w.cores != nil && math.Abs(*w.cores-*ctl.CPUCores) > 1e-9 {
				t.Errorf("res %s +%v: cpuCores %v, want %v", c.res, w.off, deref(ctl.CPUCores), deref(w.cores))
			}
			if !slices.Equal(ctl.Disks, w.disks) {
				t.Errorf("res %s +%v: disks %+v, want %+v", c.res, w.off, ctl.Disks, w.disks)
			}
		}
	}
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
		return controlHistoryRequest(t, s, key, "res=0&since=1h"+query)
	}
	for _, c := range []struct{ key, query string }{{tenantKey, ""}, {tenantKey, "&tenant="}, {opKey, "&tenant=acme"}, {opKey, "&tenant=t1"}} {
		h := get(c.key, c.query)
		if len(h.Samples) != 2 {
			t.Errorf("%q: want the tenant's 2 samples, got %d", c.query, len(h.Samples))
		}
		if h.Control != nil {
			t.Errorf("key %s query %q: control %+v", map[string]string{tenantKey: "tenant", opKey: "operator"}[c.key], c.query, h.Control)
		}
	}
	h := get(opKey, "")
	c := h.Control
	if len(h.Samples) != 2 || c == nil || len(c.Machines) != 1 || c.Machines[0].Hostname != s.hostname || len(c.Machines[0].Samples) != 2 ||
		len(c.Postgres) != 2 || len(c.Luxd) != 1 || c.Luxd[0].Instance != s.id || !strings.HasPrefix(s.id, "luxd_") || len(c.Luxd[0].Samples) != 2 {
		t.Fatalf("operator: %+v", h)
	}
	for i, m := range c.Machines[0].Samples {
		pg := c.Postgres[i]
		if !m.At.Equal(h.Samples[i].At) || m.CPUs == nil || *m.CPUs <= 0 || m.MemoryTotal == nil || pg.Bytes == nil || pg.Connections == nil ||
			len(m.Disks) != 1 || m.Disks[0].Path != "/" || m.Disks[0].TotalBytes <= 0 {
			t.Fatalf("operator sample %d: %+v %+v", i, m, pg)
		}
		// CPU is a rate: none for the first point.
		if (m.CPUCores == nil) != (i == 0) {
			t.Errorf("sample %d cpuCores %v", i, m.CPUCores)
		}
	}
}
