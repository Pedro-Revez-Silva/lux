package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
)

// sessionTimeZone makes every new connection of s's database use tz as its
// session time zone, so a test pins behaviour that must not depend on it.
func sessionTimeZone(t *testing.T, s *Server, tz string) {
	t.Helper()
	ctx := context.Background()
	var db string
	if err := s.db.Pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, os.Getenv("LUX_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// Identifiers cannot be bind parameters; db is the test's own generated name.
	if _, err := admin.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s SET timezone TO %s`, pgx.Identifier{db}.Sanitize(), pgQuote(tz))); err != nil {
		t.Fatal(err)
	}
	s.db.Pool.Reset()
	var got string
	if err := s.db.Pool.QueryRow(ctx, `SHOW timezone`).Scan(&got); err != nil || got != tz {
		t.Fatalf("session time zone %q, want %q (%v)", got, tz, err)
	}
}

func pgQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Days are UTC days whatever the database session's time zone is.
var dailyTimeZones = []string{"UTC", "Asia/Tokyo"}

// res=86400 folds the hourly samples into UTC days as the stored rollups
// fold theirs: a counter keeps its maximum (read as a rate), a level its
// mean, a p95 its maximum and a p50 its mean, a flow its sum, a state its
// last. A day without hours is absent, not zero.
func TestHistoryDaily(t *testing.T) {
	for _, tz := range dailyTimeZones {
		t.Run(tz, func(t *testing.T) { testHistoryDaily(t, tz) })
	}
}

func testHistoryDaily(t *testing.T, tz string) {
	s := testServer(t)
	sessionTimeZone(t, s, tz)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, state) VALUES ('h1', 'h1', 't1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running')`)
	key, tenantKey := ids.Secret("luxk"), ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator']), ('k1', 't1', 'k', $2, ARRAY['read'])`,
		ids.Hash(key), ids.Hash(tenantKey))
	day0 := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -5)
	day1, day3 := day0.AddDate(0, 0, 1), day0.AddDate(0, 0, 3)
	// Day 0: 22:00 and 23:00. Day 1: 00:00 and 01:00 (across the boundary).
	// Day 2: nothing. Day 3: 12:00.
	type hour struct {
		at       time.Time
		cpu      float64
		mem      int64
		started  int
		p50, p95 float64
		running  int
	}
	hours := []hour{
		{day0.Add(22 * time.Hour), 100, 100, 1, 1, 10, 1},
		{day0.Add(23 * time.Hour), 200, 300, 2, 3, 20, 2},
		{day1, 300, 1000, 4, 5, 7, 3},
		{day1.Add(time.Hour), 3900, 2000, 8, 7, 9, 4},
		{day3.Add(12 * time.Hour), 90300, 50, 0, 2, 2, 5},
	}
	for _, h := range hours {
		execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds, mem_bytes) VALUES ('h1', 3600, $1, $2, $3)`, h.at, h.cpu, h.mem)
		execSQL(t, s, ctx, `INSERT INTO system_samples (tenant_id, res, at, runs, started, start_p50, start_p95) VALUES ('', 3600, $1, $2, $3, $4, $5)`,
			h.at, map[string]int{"running": h.running}, h.started, h.p50, h.p95)
		// The Run: net_rx a counter like CPU, pids a level.
		execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds, mem_bytes, pids, net_rx) VALUES ('r1', 1, 't1', 3600, $1, $2, $3, $4, $5)`,
			h.at, h.cpu, h.mem, h.running*2, int64(h.cpu)/10)
		// The control host: CPU a counter, disk use a mean, disk size a maximum.
		execSQL(t, s, ctx, `INSERT INTO control_samples (instance, hostname, res, at, cpu_seconds, cpus, mem_bytes) VALUES ('ctl', 'ctl', 3600, $1, $2, 8, $3)`, h.at, h.cpu, h.mem)
		execSQL(t, s, ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes) VALUES ('ctl', '/', 3600, $1, $2, $3, 10000)`, h.at, h.mem, 10000-h.mem)
	}
	q := "?res=86400&from=" + url.QueryEscape(day0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(day3.Add(23*time.Hour).Format(time.RFC3339))

	sys := historyRequest(t, s, key, "/v1/history"+q)
	if sys.Resolution != 86400 {
		t.Errorf("resolution %d", sys.Resolution)
	}
	var got []string
	for _, sm := range sys.Samples {
		got = append(got, fmt.Sprintf("%s started=%d p50=%g p95=%g running=%d", sm.At.UTC().Format("01-02"),
			*sm.Started, *sm.StartP50, *sm.StartP95, sm.Runs["running"]))
	}
	want := []string{
		day0.Format("01-02") + " started=3 p50=2 p95=20 running=2",
		day1.Format("01-02") + " started=12 p50=6 p95=9 running=4",
		day3.Format("01-02") + " started=0 p50=2 p95=2 running=5",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("system days:\n got %v\nwant %v", got, want)
	}

	host := historyRequest(t, s, key, "/v1/hosts/h1/history"+q)
	got = nil
	for _, sm := range host.Samples {
		cores := "-"
		if sm.CPUCores != nil {
			cores = fmt.Sprintf("%g", *sm.CPUCores)
		}
		got = append(got, fmt.Sprintf("%s mem=%d cores=%s", sm.At.UTC().Format("01-02"), *sm.MemoryBytes, cores))
	}
	// Day 0's counter is 200, day 1's 3900: 3700 CPU-seconds over a day;
	// day 3's 90300, over two days (day 2 is a gap): 86400 / 172800.
	want = []string{
		day0.Format("01-02") + " mem=200 cores=-",
		day1.Format("01-02") + fmt.Sprintf(" mem=1500 cores=%g", 3700.0/86400),
		day3.Format("01-02") + " mem=50 cores=0.5",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("host days:\n got %v\nwant %v", got, want)
	}

	// The Run: day 0 mem (100+300)/2, pids round((2+4)/2), day 1 net_rx 390 - 20 over a day.
	run := historyRequest(t, s, tenantKey, "/v1/runs/r1/history"+q)
	got = nil
	for _, sm := range run.Samples {
		rx := "-"
		if sm.NetRxRate != nil {
			rx = fmt.Sprintf("%g", *sm.NetRxRate)
		}
		got = append(got, fmt.Sprintf("%s mem=%d pids=%d rx=%s", sm.At.UTC().Format("01-02"), *sm.MemoryBytes, *sm.Pids, rx))
	}
	want = []string{
		day0.Format("01-02") + " mem=200 pids=3 rx=-",
		day1.Format("01-02") + fmt.Sprintf(" mem=1500 pids=7 rx=%g", 370.0/86400),
		day3.Format("01-02") + fmt.Sprintf(" mem=50 pids=10 rx=%g", (9030.0-390)/172800),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("run days:\n got %v\nwant %v", got, want)
	}

	// The control host, its disk folded per day with the day itself.
	if sys.Control == nil || len(sys.Control.Machines) != 1 {
		t.Fatalf("control: %+v", sys.Control)
	}
	got = nil
	for _, sm := range sys.Control.Machines[0].Samples {
		cores := "-"
		if sm.CPUCores != nil {
			cores = fmt.Sprintf("%g", *sm.CPUCores)
		}
		got = append(got, fmt.Sprintf("%s cores=%s disks=%v", sm.At.UTC().Format("01-02"), cores, sm.Disks))
	}
	want = []string{
		day0.Format("01-02") + " cores=- disks=[{/ 200 9800 10000}]",
		day1.Format("01-02") + fmt.Sprintf(" cores=%g disks=[{/ 1500 8500 10000}]", 3700.0/86400),
		day3.Format("01-02") + " cores=0.5 disks=[{/ 50 9950 10000}]",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("control days:\n got %v\nwant %v", got, want)
	}

	// A range starting mid-day gets the days that start in it, as a stored
	// resolution does: day 0 starts before 12:00 and is left out.
	mid := "?res=86400&from=" + url.QueryEscape(day0.Add(12*time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(day1.Add(23*time.Hour).Format(time.RFC3339))
	if h := historyRequest(t, s, key, "/v1/history"+mid); len(h.Samples) != 1 || !h.Samples[0].At.Equal(day1) {
		t.Errorf("mid-day range: %+v", h.Samples)
	}

	var bad struct{}
	if code := getJSON(t, s, key, "/v1/history?res=604800", &bad); code != http.StatusBadRequest {
		t.Errorf("res=604800: %d", code)
	}
}

// A pool's metrics read at res=86400 are its hours folded into days, and
// historyFrom is the first day with an hour.
func TestPoolMetricsDaily(t *testing.T) {
	for _, tz := range dailyTimeZones {
		t.Run(tz, func(t *testing.T) { testPoolMetricsDaily(t, tz) })
	}
}

func testPoolMetricsDaily(t *testing.T, tz string) {
	s := testServer(t)
	sessionTimeZone(t, s, tz)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(key))
	namedPools(t, s, "p", "blue")
	day0 := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	for i, at := range []time.Time{day0.Add(20 * time.Hour), day0.Add(23 * time.Hour), day0.AddDate(0, 0, 1).Add(5 * time.Hour)} {
		execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, running, started, cap_cpus) VALUES ('blue', '', 3600, $1, $2, $3, $4)`,
			at, 2*(i+1), i+1, float64(4*(i+1)))
	}
	var got struct {
		Resolution  int          `json:"resolution"`
		HistoryFrom *time.Time   `json:"historyFrom"`
		Samples     []PoolSample `json:"samples"`
	}
	path := "/v1/pools/blue/metrics?res=86400&from=" + url.QueryEscape(day0.Format(time.RFC3339)) + "&to=" + url.QueryEscape(day0.AddDate(0, 0, 3).Format(time.RFC3339))
	if code := getJSON(t, s, key, path, &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.Resolution != 86400 || got.HistoryFrom == nil || !got.HistoryFrom.Equal(day0) || len(got.Samples) != 2 {
		t.Fatalf("pool days: %+v", got)
	}
	if s0 := got.Samples[0]; s0.Running != 3 || s0.Started != 3 || s0.CapacityCPUs != 6 {
		t.Errorf("day 0: %+v", s0)
	}
	if s1 := got.Samples[1]; s1.Running != 6 || s1.Started != 3 {
		t.Errorf("day 1: %+v", s1)
	}
}
