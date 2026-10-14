package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// flow is a server with tenants ta and tb with read keys (a, b) and an
// operator key (op); platform pools pa, pb and pc with hosts ha (pa), hb
// (pb), hc (pc), and hn with no pool. The sampler's next window is
// (now - 20m, now - 1m].
type flow struct {
	t    *testing.T
	s    *Server
	keys map[string]string
	now  time.Time
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('ta', 'a'), ('tb', 'b')`)
	keys := map[string]string{"a": ids.Secret("luxk"), "b": ids.Secret("luxk"), "op": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('ka', 'ta', 'k', $1, ARRAY['read']), ('kb', 'tb', 'k', $2, ARRAY['read']), ('ko', NULL, 'o', $3, ARRAY['operator'])`,
		ids.Hash(keys["a"]), ids.Hash(keys["b"]), ids.Hash(keys["op"]))
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared) VALUES
		('pa', NULL, 'a', 'static', true), ('pb', NULL, 'b', 'static', true), ('pc', NULL, 'c', 'static', true)`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, capacity) VALUES
		('ha', NULL, 'ha', 'pa', 'ready', '{"cpus": 4}'), ('hb', NULL, 'hb', 'pb', 'ready', '{"cpus": 4}'),
		('hc', NULL, 'hc', 'pc', 'ready', '{"cpus": 4}'), ('hn', NULL, 'hn', NULL, 'ready', '{"cpus": 4}')`)
	openSampleWindow(t, s)
	f := &flow{t: t, s: s, keys: keys}
	systemScan(t, s, `SELECT now()`, nil, &f.now)
	return f
}

// openSampleWindow makes the sampler's next window (now - 20m, now - 1m].
func openSampleWindow(t *testing.T, s *Server) {
	t.Helper()
	execSQL(t, s, context.Background(), `INSERT INTO system_samples (tenant_id, res, at, window_end) VALUES ('', 0, now() - interval '1 hour', now() - interval '20 minutes')`)
}

func (f *flow) ago(minutes time.Duration) time.Time { return f.now.Add(-minutes * time.Minute) }

// flowPlacement is a placement on host that reached running at started
// (zero: never).
type flowPlacement struct {
	host    string
	started time.Time
}

// run adds a Run of tenant bound to pool (runs.pool_id) with its
// placements as epochs 1, 2, ...; started and finished zero are NULL. A
// finished Run is succeeded, an unfinished one running on its last epoch.
func (f *flow) run(id, tenant, pool string, started, finished time.Time, pls ...flowPlacement) {
	f.t.Helper()
	ctx := context.Background()
	at := func(v time.Time) any {
		if v.IsZero() {
			return nil
		}
		return v
	}
	state := "running"
	if !finished.IsZero() {
		state = "succeeded"
	}
	execSQL(f.t, f.s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, first_started_at, finished_at, name)
		VALUES ($1, $2, '{}', $3, $4, $5, $6, $7, $1)`, id, tenant, state, pool, len(pls), at(started), at(finished))
	for i, pl := range pls {
		plState := "exited"
		if state == "running" && i == len(pls)-1 {
			plState = "running"
		}
		execSQL(f.t, f.s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at, resources)
			VALUES ($1 || '-' || $4::int, $2, $1, $3, $4::int, $5, $6, '{"cpus": 1}')`, id, tenant, pl.host, i+1, plState, at(pl.started))
	}
}

func (f *flow) exec(q string) {
	f.t.Helper()
	execSQL(f.t, f.s, context.Background(), q)
}

func (f *flow) sample() {
	f.t.Helper()
	sampleNow(f.t, f.s)
}

func (f *flow) wantSamples(want map[string]string) {
	f.t.Helper()
	wantMap(f.t, "samples", flowSamples(f.t, f.s), want)
}

// wantStats checks runsStarted per pool from GET /v1/pools/stats?since=2h
// with key who (a, b or op).
func (f *flow) wantStats(who string, want map[string]int) {
	f.t.Helper()
	wantMap(f.t, who+" stats", runsStarted(f.statsList(who)), want)
}

func (f *flow) statsList(who string) []PoolStats {
	f.t.Helper()
	var list struct{ Pools []PoolStats }
	if code := getJSON(f.t, f.s, f.keys[who], "/v1/pools/stats?since=2h", &list); code != http.StatusOK {
		f.t.Fatalf("GET /v1/pools/stats: %d", code)
	}
	return list.Pools
}

// flowSamples is every raw pool sample's started/finished, by pool and
// tenant ("" for the pool's own row): "pa/": "5/1".
func flowSamples(t *testing.T, s *Server) map[string]string {
	t.Helper()
	got := map[string]string{}
	rows := queryOne[[]string](t, s, `SELECT coalesce(array_agg(pool_id || '/' || tenant_id || '=' || started || '/' || finished ORDER BY 1), '{}')
		FROM pool_samples WHERE res = 0 AND (started > 0 OR finished > 0)`)
	for _, r := range rows {
		k, v, _ := strings.Cut(r, "=")
		got[k] = v
	}
	return got
}

// runsStarted is each pool's non-zero RunsStarted.
func runsStarted(pools []PoolStats) map[string]int {
	got := map[string]int{}
	for _, p := range pools {
		if p.RunsStarted != 0 {
			got[p.ID] = p.RunsStarted
		}
	}
	return got
}

func operatorPoolStats(ctx context.Context, s *Server) (*poolStatsOutput, error) {
	return s.poolStats(context.WithValue(ctx, principalKey, Principal{Operator: true, Scopes: []string{"read"}}), &poolStatsInput{Since: "2h"})
}

func sampleNow(t *testing.T, s *Server) {
	t.Helper()
	if err := s.sampleSystem(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func wantMap[V comparable](t *testing.T, what string, got, want map[string]V) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

// applyStatusT1 applies a runner's status for t1's Run id at epoch.
func applyStatusT1(t *testing.T, s *Server, id string, epoch int, st proto.Status) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.applyStatus(ctx, tx, "t1", id, epoch, st)
	}); err != nil {
		t.Fatal(err)
	}
}

// resumeIntoBig resumes movedRun's Run into big with resources only big fits.
func resumeIntoBig(t *testing.T, s *Server, id, big string) {
	t.Helper()
	if _, err := resumeTo(s, id, big, &resumeResources{CPUs: f64(8), Memory: bytesPtr(32 * gib)}); err != nil {
		t.Fatal(err)
	}
}

// startedAgo sets movedRun's first start, on epoch 1, to now() - ago.
func startedAgo(t *testing.T, s *Server, id, ago string) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE placements SET started_at = now() - $2::interval WHERE run_id = $1 AND epoch = 1`, id, ago)
	execSQL(t, s, ctx, `UPDATE runs SET first_started_at = now() - $2::interval WHERE id = $1`, id, ago)
}

// A Run first started on pa's host, moved by a resume to pb (runs.pool_id
// now pb) and finished on pb's host counts its start under pa and its finish
// under pb, once each, in the pool rows and its tenant's rows of the samples
// and in GET /v1/pools/stats; it is running and queued nowhere.
func TestPoolFlowMovedRun(t *testing.T) {
	f := newFlow(t)
	f.run("moved", "ta", "pb", f.ago(15), f.ago(5), flowPlacement{"ha", f.ago(15)}, flowPlacement{"hb", f.ago(10)})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "1/0", "pa/ta": "1/0", "pb/": "0/1", "pb/ta": "0/1"})
	f.wantStats("op", map[string]int{"pa": 1})
	f.wantStats("a", map[string]int{"pa": 1})
	f.wantStats("b", map[string]int{})
}

// The start is the placement that first ran: the one whose started_at is
// first_started_at even when another started earlier, else the earliest;
// running stays on runs.pool_id, where the Run is now.
func TestPoolFlowFirstRunningPlacement(t *testing.T) {
	f := newFlow(t)
	// started_at of epoch 2 is first_started_at: pa, not hb's earlier epoch 1.
	f.run("match", "ta", "pb", f.ago(9), time.Time{}, flowPlacement{"hb", f.ago(40)}, flowPlacement{"ha", f.ago(9)}, flowPlacement{"hb", f.ago(3)})
	// No placement's started_at is first_started_at: the earliest, pa's.
	f.run("earliest", "tb", "pb", f.ago(8), time.Time{}, flowPlacement{"ha", f.ago(7)}, flowPlacement{"hb", f.ago(6)})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "2/0", "pa/ta": "1/0", "pa/tb": "1/0"})
	if got := queryOne[string](t, f.s, `SELECT string_agg(pool_id || '/' || tenant_id || '=' || running, ' ' ORDER BY 1)
		FROM pool_samples WHERE res = 0 AND running > 0`); got != "pb/=2 pb/ta=1 pb/tb=1" {
		t.Errorf("running %q, want both on pb", got)
	}
	f.wantStats("op", map[string]int{"pa": 2})
	f.wantStats("b", map[string]int{"pa": 1})
}

// The same through lux's own paths: a Run that first ran on default's host
// ha, resumed into t1's pool big, placed on hb by the scheduler, reported
// running and then exited 0 there, counts its start under default and its
// finish under big, for the pool and for t1.
func TestPoolFlowResumedIntoAnotherPool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id, big := movedRun(t, s)
	startedAgo(t, s, id, "10 minutes")
	resumeIntoBig(t, s, id, big)
	schedule(t, s)
	if host, _, reason := placedOn(t, s, id); host != "hb" {
		t.Fatalf("placed on %q (%s), want hb", host, reason)
	}
	code := 0
	applyStatusT1(t, s, id, 2, proto.Status{State: "running"})
	applyStatusT1(t, s, id, 2, proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})
	if state := storedState(t, s, id); state != StateSucceeded {
		t.Fatalf("state %s, want succeeded", state)
	}
	// Into the sampler's window, which ends a minute ago.
	execSQL(t, s, ctx, `UPDATE runs SET finished_at = finished_at - interval '2 minutes' WHERE id = $1`, id)
	openSampleWindow(t, s)
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"default/": "1/0", "default/t1": "1/0", big + "/": "0/1", big + "/t1": "0/1"})
	out, err := operatorPoolStats(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	wantMap(t, "operator stats", runsStarted(out.Body.Pools), map[string]int{"default": 1})
}

// A Run moved twice, first run on pa's ha, then on pb's hb, then placed on
// pc's hc where it never reached running and finished bound to pc: its
// start counts under pa and its finish under pc, the last placement's
// pool, not the last one that ran.
func TestPoolFlowMovedTwice(t *testing.T) {
	f := newFlow(t)
	f.run("twice", "ta", "pc", f.ago(15), f.ago(3), flowPlacement{"ha", f.ago(15)}, flowPlacement{"hb", f.ago(10)}, flowPlacement{"hc", time.Time{}})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "1/0", "pa/ta": "1/0", "pc/": "0/1", "pc/ta": "0/1"})
	f.wantStats("op", map[string]int{"pa": 1})
	f.wantStats("a", map[string]int{"pa": 1})
}

// A Run that last ran on default's ha, resumed into big and terminated
// before it was placed again finishes bound to big; its finish counts
// under default, its last placement's pool, not runs.pool_id.
func TestPoolFlowTerminatedBeforePlacement(t *testing.T) {
	s := testServer(t)
	id, big := movedRun(t, s)
	// Started before the sampler's window: only the finish is in it.
	startedAgo(t, s, id, "30 minutes")
	resumeIntoBig(t, s, id, big)
	if _, err := s.terminateRun(tenantCtx("t1"), &RunPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	if b := binding(t, s, id); b.State != StateTerminated || b.PoolID != big {
		t.Fatalf("state %s bound to %s, want terminated bound to %s", b.State, b.PoolID, big)
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET finished_at = finished_at - interval '2 minutes' WHERE id = $1`, id)
	openSampleWindow(t, s)
	sampleNow(t, s)
	wantMap(t, "samples", flowSamples(t, s), map[string]string{"default/": "0/1", "default/t1": "0/1"})
}

// runs.first_pool_id is written by the first running report only: the pool
// of the host it ran on (default's ha). A resume into big and a running
// report on hb leave it, and first_started_at, as they were.
func TestFirstPoolIDSetOnceAtFirstStart(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	id, big := movedRun(t, s)
	first := func() (pool string, at time.Time) {
		t.Helper()
		systemScan(t, s, `SELECT coalesce(first_pool_id, ''), first_started_at FROM runs WHERE id = $1`, []any{id}, &pool, &at)
		return pool, at
	}
	// Placement 1 on ha, not yet running; the Run never started.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'starting', started_at = NULL WHERE run_id = $1 AND epoch = 1`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'starting', first_started_at = NULL WHERE id = $1`, id)
	applyStatusT1(t, s, id, 1, proto.Status{State: "running"})
	pool, at := first()
	if pool != "default" {
		t.Fatalf("after the first start: first_pool_id %q, want default", pool)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id = $1 AND epoch = 1`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	resumeIntoBig(t, s, id, big)
	schedule(t, s)
	if host, _, reason := placedOn(t, s, id); host != "hb" {
		t.Fatalf("placed on %q (%s), want hb", host, reason)
	}
	applyStatusT1(t, s, id, 2, proto.Status{State: "running"})
	if state := storedState(t, s, id); state != StateRunning {
		t.Fatalf("state %s after the second start, want running", state)
	}
	if pool2, at2 := first(); pool2 != "default" || !at2.Equal(at) {
		t.Errorf("after a start in big: first_pool_id %q at %v, want default at %v", pool2, at2, at)
	}
}

// A first start on a host outside the Run's pool (its chosen host, place_on,
// as a resume to a host or a migration sets it) records the host's pool; on
// a chosen host with no pool, the pool the Run is bound to.
func TestFirstPoolIDOfChosenHost(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.LeaseDuration = time.Minute
	elsewhere := submitOnHA(t, s, resizeBase)
	poolless := submitOnHA(t, s, resizeBase)
	execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'ha'`)
	readyHost(t, s, "hb", "big", "t1", false)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, tenant_id, state, capacity, last_heartbeat)
		VALUES ('hn', 'hn', NULL, 't1', 'ready', '{"runs":10}', now())`)
	s.hub.polled("hn")
	execSQL(t, s, ctx, `UPDATE hosts SET capacity = '{"cpus":16,"memory":68719476736,"runs":10}', registered_at = now() WHERE id IN ('hb', 'hn')`)
	execSQL(t, s, ctx, `UPDATE runs SET place_on = CASE id WHEN $1 THEN 'hb' ELSE 'hn' END WHERE id IN ($1, $2)`, elsewhere, poolless)
	schedule(t, s)
	for id, want := range map[string]struct{ host, pool string }{elsewhere: {"hb", poolID(t, s, "big", "t1")}, poolless: {"hn", "default"}} {
		host, _, reason := placedOn(t, s, id)
		if host != want.host {
			t.Fatalf("%s placed on %q (%s), want %s", id, host, reason, want.host)
		}
		applyStatusT1(t, s, id, 1, proto.Status{State: "running"})
		var bound, first string
		systemScan(t, s, `SELECT pool_id, coalesce(first_pool_id, '') FROM runs WHERE id = $1`, []any{id}, &bound, &first)
		if bound != "default" || first != want.pool {
			t.Errorf("first start on %s: bound to %s, first_pool_id %q; want bound to default, first_pool_id %q", want.host, bound, first, want.pool)
		}
	}
}

// A Run first started on pa's ha and now on pb, with pa retired and ha
// terminated before the sample: pa's start is in its history (its rows
// come from that start alone), and GET /v1/pools/stats, which lists live
// pools only, does not list pa.
func TestPoolFlowRetiredPool(t *testing.T) {
	f := newFlow(t)
	f.run("leftpa", "ta", "pb", f.ago(12), time.Time{}, flowPlacement{"ha", f.ago(12)}, flowPlacement{"hb", f.ago(8)})
	f.exec(`UPDATE pools SET retired = true WHERE id = 'pa'`)
	f.exec(`UPDATE hosts SET state = 'terminated' WHERE id = 'ha'`)
	f.sample()
	f.wantSamples(map[string]string{"pa/": "1/0", "pa/ta": "1/0"})
	var listed []string
	for _, p := range f.statsList("op") {
		listed = append(listed, fmt.Sprintf("%s=%d", p.ID, p.RunsStarted))
	}
	if got := strings.Join(listed, " "); got != "pb=0 pc=0" {
		t.Errorf("stats pools %q, want pb=0 pc=0", got)
	}
}

// A Run's recorded first_pool_id is its start's pool, ahead of its
// placements' hosts and runs.pool_id: started under pc by the record,
// though its placement ran on pa's host and it is bound to pb.
func TestPoolFlowReadsFirstPoolID(t *testing.T) {
	f := newFlow(t)
	f.run("recorded", "ta", "pb", f.ago(10), time.Time{}, flowPlacement{"ha", f.ago(10)})
	f.exec(`UPDATE runs SET first_pool_id = 'pc' WHERE id = 'recorded'`)
	f.sample()
	f.wantSamples(map[string]string{"pc/": "1/0", "pc/ta": "1/0"})
	f.wantStats("op", map[string]int{"pc": 1})
	f.wantStats("a", map[string]int{"pc": 1})
}

// A Run that never moved counts in its pool for both.
func TestPoolFlowSamePool(t *testing.T) {
	f := newFlow(t)
	f.run("stay", "tb", "pa", f.ago(14), f.ago(4), flowPlacement{"ha", f.ago(14)}, flowPlacement{"ha", f.ago(9)})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "1/1", "pa/tb": "1/1"})
	f.wantStats("b", map[string]int{"pa": 1})
	f.wantStats("a", map[string]int{})
}

// No placement, or a placement whose host has no pool: runs.pool_id.
func TestPoolFlowFallbacks(t *testing.T) {
	f := newFlow(t)
	f.run("none", "ta", "pc", f.ago(13), f.ago(3))
	f.run("nopool", "tb", "pc", f.ago(12), f.ago(2), flowPlacement{"hn", f.ago(12)})
	f.sample()
	f.wantSamples(map[string]string{"pc/": "2/2", "pc/ta": "1/1", "pc/tb": "1/1"})
	f.wantStats("op", map[string]int{"pc": 2})
	f.wantStats("a", map[string]int{"pc": 1})
}

// A Run whose epoch 1 on pc's hc never started and whose epoch 2 ran on
// pa's ha finishes under pa, its last placement's pool, even though the
// unstarted placement has no started_at.
func TestPoolFlowFinishAfterUnstartedPlacement(t *testing.T) {
	f := newFlow(t)
	f.run("lateran", "ta", "pa", f.ago(15), f.ago(4), flowPlacement{"hc", time.Time{}}, flowPlacement{"ha", f.ago(15)})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "1/1", "pa/ta": "1/1"})
}

// A moved Run whose start is in the sample's window and finish after it
// counts its start only; one started before the window and finished in it,
// its finish only; one started before the stats range counts there nowhere.
// The window is (since, until]: a start or finish at exactly since, the
// previous sample's window_end, was the previous sample's and is not
// counted again; one a microsecond after it is. A Run started and finished
// 30 s ago, after until (a minute before the sample), is the next sample's,
// though within the stats range; one started 30 s from now, after the
// stats' to, counts nowhere.
func TestPoolFlowWindowEdges(t *testing.T) {
	f := newFlow(t)
	var since time.Time
	systemScan(t, f.s, `SELECT window_end FROM system_samples WHERE res = 0 AND tenant_id = ''`, nil, &since)
	in := since.Add(time.Microsecond)
	f.run("startin", "ta", "pb", f.ago(11), f.ago(-1), flowPlacement{"ha", f.ago(11)}, flowPlacement{"hb", f.ago(5)})
	f.run("finishin", "ta", "pb", f.ago(30), f.ago(6), flowPlacement{"ha", f.ago(30)}, flowPlacement{"hb", f.ago(25)})
	f.run("old", "tb", "pb", f.ago(180), f.ago(90), flowPlacement{"ha", f.ago(180)}, flowPlacement{"hb", f.ago(150)})
	f.run("startatsince", "ta", "pb", since, time.Time{}, flowPlacement{"ha", since})
	f.run("startjustin", "ta", "pb", in, time.Time{}, flowPlacement{"ha", in})
	f.run("finishatsince", "ta", "pa", f.ago(60), since, flowPlacement{"ha", f.ago(60)}, flowPlacement{"hb", f.ago(50)})
	f.run("finishjustin", "ta", "pa", f.ago(60), in, flowPlacement{"ha", f.ago(60)}, flowPlacement{"hb", f.ago(50)})
	recent := f.now.Add(-30 * time.Second)
	f.run("afteruntil", "ta", "pb", recent, recent, flowPlacement{"hb", recent})
	future := f.now.Add(30 * time.Second)
	f.run("afterto", "ta", "pc", future, future, flowPlacement{"hc", future})
	f.sample()
	f.wantSamples(map[string]string{"pa/": "2/0", "pa/ta": "2/0", "pb/": "0/2", "pb/ta": "0/2"})
	f.wantStats("op", map[string]int{"pa": 6, "pb": 1})
	f.wantStats("b", map[string]int{})
	wantMap(t, "operator stats with starts at from", statsWithStartsAtFrom(t, f.s), map[string]int{"pa": 6, "pb": 1, "pc": 2})
}

// statsWithStartsAtFrom is runsStarted per pool from GET /v1/pools/stats
// (as an operator, since=2h) with four Runs on pc's hc added at its from,
// now() - since in the read's first transaction, and a microsecond after
// it: one of each with first_pool_id recorded and one resolved at read
// time. That transaction is held at its pools read while they are written;
// its start is read from pg_stat_activity.
func statsWithStartsAtFrom(t *testing.T, s *Server) map[string]int {
	t.Helper()
	ctx := context.Background()
	type result struct {
		out *poolStatsOutput
		err error
	}
	done := make(chan result, 1)
	var from time.Time
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE pools IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
		go func() {
			out, err := operatorPoolStats(ctx, s)
			done <- result{out, err}
		}()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var found bool
			systemScan(t, s, `SELECT count(*) = 1, coalesce(max(xact_start), 'epoch') - interval '2 hours' FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
				AND query LIKE 'SELECT p.id FROM pools p WHERE % AND NOT p.retired ORDER BY p.id'`, nil, &found, &from)
			if found {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the stats read never waited on pools")
			}
		}
		in := from.Add(time.Microsecond)
		for _, r := range []struct {
			id    string
			at    time.Time
			first any
		}{{"recordedat", from, "pc"}, {"recordedin", in, "pc"}, {"resolvedat", from, nil}, {"resolvedin", in, nil}} {
			if _, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, spec, state, pool_id, current_epoch, first_started_at, first_pool_id)
				VALUES ($1, 'ta', '{}', 'running', 'pb', 1, $2, $3)`, r.id, r.at, r.first); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, started_at, resources)
				VALUES ($1, 'ta', $1, 'hc', 1, 'running', $2, '{"cpus": 1}')`, r.id, r.at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !res.out.Body.From.Equal(from) {
		t.Fatalf("from %v, want the held transaction's %v", res.out.Body.From, from)
	}
	return runsStarted(res.out.Body.Pools)
}
