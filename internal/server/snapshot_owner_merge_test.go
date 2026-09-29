package server

import (
	"context"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// A refused move of a Run bound to the platform default stays stopped,
// records its host placement end, and a later manual resume keeps its
// platform owner despite a more attractive same-named tenant host.
func TestSnapshotReportRefusedMoveKeepsPoolOwner(t *testing.T) {
	s := testServer(t)
	s.cfg.LeaseDuration = time.Minute
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, shared, is_default) VALUES ('pp', NULL, 'burst', 'static', true, true)`)
	mustPut(t, s, "t1", Pool{Name: "burst", Provider: "static"})
	readyHost(t, s, "h-plat", "burst", "", false)
	readyHost(t, s, "h-t1", "burst", "t1", true)
	id := submitAs(t, s, "t1", "", "")
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, stop_reason)
		VALUES ('p-plat', 't1', $1, 'h-plat', 1, 'stopping', 'migrate')`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping', current_epoch = 1 WHERE id = $1`, id)
	execSQL(t, s, ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, command, state, epoch)
		VALUES ('t1', $1, 'web', 3000, '["serve"]', 'ready', 1)`, id)

	report := func(snapID string) proto.Frame {
		sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: snapID, RunID: id, Epoch: 1, Volumes: []proto.VolumeSnapshot{
			{Name: "work", Path: "/work", BlobID: "vol-" + snapID, Size: 1, SHA256: "vol"},
		}}, Output: &proto.BlobInfo{BlobID: "out-A", Size: 2, SHA256: "out"}}
		return s.handleReport(ctx, "h-plat", proto.Frame{Type: proto.MsgSnapshotDone, ID: 1, RunID: id, Epoch: 1, Data: proto.Marshal(sd)})
	}
	if f := report("snap-A"); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report A: %s %s", f.Type, f.Data)
	}
	if f := report("snap-B"); f.Type != proto.MsgAck || !ackRefused(t, f) {
		t.Fatalf("report B: %s %s, want refused", f.Type, f.Data)
	}
	code := 0
	if f := s.handleReport(ctx, "h-plat", proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: id, Epoch: 1,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}); f.Type != proto.MsgAck {
		t.Fatalf("exit: %s %s", f.Type, f.Data)
	}

	var state, owner, serverState, serverStop string
	systemScan(t, s, `SELECT r.state, coalesce((SELECT coalesce(p.tenant_id, '') FROM pools p WHERE p.id = r.pool_id), '<nil>'), sv.state, coalesce(sv.stop_reason, '')
		FROM runs r JOIN run_servers sv ON sv.run_id = r.id WHERE r.id = $1`, []any{id}, &state, &owner, &serverState, &serverStop)
	if state != StateStopped || owner != "" || serverState != ServerStopped || serverStop != "run stopped" {
		t.Fatalf("after refused move: Run %q owner %q server %q %q", state, owner, serverState, serverStop)
	}
	ended := events(t, s, evPlacementEnded)
	if len(ended) != 1 || ended[0].Owner != "h-plat" || ended[0].Data["outcome"] != StateStopped || ended[0].Data["stopReason"] != "migrate" {
		t.Fatalf("placement_ended events %+v", ended)
	}
	schedule(t, s)
	if n := queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id = $1`, id); n != 1 {
		t.Fatalf("%d placements after scheduling a refused move, want 1", n)
	}
	// The platform host is unavailable: the platform-bound Run waits
	// rather than using the tenant's same-named pool.
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true WHERE run_id = $1`, id)
	execSQL(t, s, ctx, `UPDATE hosts SET draining = true WHERE id = 'h-plat'`)
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}}); err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, state, _ := placedOn(t, s, id); host != "h-plat" || state != StateResuming {
		t.Fatalf("manual resume on %q in %q, want waiting with its previous h-plat placement", host, state)
	}
}
