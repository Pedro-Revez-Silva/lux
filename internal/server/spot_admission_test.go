package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
)

func TestSpotAgedAdmissionAndNormalPriority(t *testing.T) {
	s, _, _ := planningFixture(t, 1)
	ctx := context.Background()
	at := time.Now()
	s.now = func() time.Time { return at }
	execSQL(t, s, ctx, `UPDATE runs SET state='submitted', spec=spec||'{"spot":true,"resumePolicy":"never"}'`)
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 1, Memory: 10, Disk: 10, Runs: 1}, map[string]string{})
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities=$1 WHERE id='h'`, []string{proto.CapSpotReclaim})
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	at = at.Add(61 * time.Second)
	for i := 0; i < 25; i++ {
		execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,updated_at) VALUES($1,'t1','pool1','{"resources":{"cpus":2},"placement":{"pool":"burst"}}','submitted',now())`, "blocked"+string(rune('a'+i)))
	}
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,updated_at) VALUES('normal','t1','pool1','{"resources":{"cpus":1,"memory":10,"disk":10},"placement":{"pool":"burst"}}','resuming',now()+interval '1 second')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id='r0'`); n != 0 {
		t.Fatal("spot stole normal capacity")
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id='normal'`); state != StateScheduled {
		t.Fatal(state)
	}
}

func TestSpotReclaimPledgeConcurrencyAndRelease(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	pledgeRace(t, s, s)
}

// The same race between two luxd processes (distinct Servers on one
// database): one pledge, no steal.
func TestSpotReclaimPledgeConcurrencyAcrossLuxd(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	other := New(s.cfg, s.db, nil, s.log)
	other.id = s.id + "-other"
	other.hub.polled("h")
	pledgeRace(t, s, other)
}

// pledgeRace: host h is full of one spot placement and two normal Runs
// each need all of it; a and b schedule at once. Exactly one pledge is
// made; once the victim releases, its requester, and only it, is placed:
// the other normal Run stays queued with no placement.
func pledgeRace(t *testing.T, a, b *Server) {
	t.Helper()
	s := a
	ctx := context.Background()
	observePlanningHost(t, s, "h", "ready", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	b.hub.polled("h")
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,current_epoch) VALUES
 ('spot','t1','pool1','{"spot":true,"resumePolicy":"never","resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','running',1),
 ('n1','t1','pool1','{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','submitted',0),
 ('n2','t1','pool1','{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','submitted',0)`)
	execSQL(t, s, ctx, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,resources) VALUES('p','t1','spot','h',1,'running','{"cpus":2,"memory":20,"disk":20}')`)
	var wg sync.WaitGroup
	for _, srv := range []*Server{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.scheduleOnce(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 1 {
		t.Fatalf("pledges=%d", n)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements`); n != 1 {
		t.Fatal("started before release")
	}
	var requester string
	systemScan(t, s, `SELECT reclaim_for FROM placements WHERE id='p'`, nil, &requester)
	if reason := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id='p'`); reason != stopSpotReclaim {
		t.Fatal(reason)
	}
	// The other normal Run goes first in the queue from now on: only the
	// pledge keeps it from the released capacity.
	other := map[string]string{"n1": "n2", "n2": "n1"}[requester]
	execSQL(t, s, ctx, `UPDATE runs SET updated_at = now() - interval '1 hour' WHERE id=$1`, other)
	withHostLock(t, s, "h", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE placements SET state='exited' WHERE id='p'`)
		return err
	})
	for _, srv := range []*Server{b, a} {
		if err := srv.scheduleOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if state := queryOne[string](t, s, `SELECT state FROM runs WHERE id=$1`, requester); state != StateScheduled {
		t.Fatalf("requester=%s", state)
	}
	if st := queryOne[string](t, s, `SELECT state FROM runs WHERE id=$1`, other); st == StateScheduled {
		t.Fatalf("non-requester %s took the pledged capacity: %s", other, st)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id=$1`, other); n != 0 {
		t.Fatalf("non-requester %s has %d placements: the pledge was stolen", other, n)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE state IN `+livePlacementStates); n != 1 {
		t.Fatalf("%d live placements, want only the requester's", n)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatal("pledge leaked")
	}
}

// A normal Run that fits on a currently free host goes there: no spot
// placement on the full host is claimed and nothing is pledged.
func TestNormalRunUsesFreeHostBeforeReclaiming(t *testing.T) {
	s, _, _ := planningFixture(t, 0)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		observePlanningHost(t, s, id, "ready", proto.Capacity{CPUs: 2, Memory: 20, Disk: 20, Runs: 2}, map[string]string{})
	}
	execSQL(t, s, ctx, `INSERT INTO runs(id,tenant_id,pool_id,spec,state,current_epoch) VALUES
 ('spot','t1','pool1','{"spot":true,"resumePolicy":"never","resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','running',1),
 ('n','t1','pool1','{"resources":{"cpus":2,"memory":20,"disk":20},"placement":{"pool":"burst"}}','submitted',0)`)
	execSQL(t, s, ctx, `INSERT INTO placements(id,tenant_id,run_id,host_id,epoch,state,resources) VALUES('p','t1','spot','b',1,'running','{"cpus":2,"memory":20,"disk":20}')`)
	if err := s.scheduleOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if host := queryOne[string](t, s, `SELECT host_id FROM placements WHERE run_id='n'`); host != "a" {
		t.Fatalf("normal Run placed on %q, want the free host a", host)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE reclaim_for IS NOT NULL`); n != 0 {
		t.Fatalf("%d placements claimed", n)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM spot_pledges`); n != 0 {
		t.Fatalf("%d pledges", n)
	}
	if reason := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id='p'`); reason != "" {
		t.Fatalf("spot placement told to stop: %q", reason)
	}
}
