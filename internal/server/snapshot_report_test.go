package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// reportFixture: Run ra (tenant t1) on host ha, Run rb (tenant t2) on host
// hb, each at epoch 1 with a live placement. ra has reported a snapshot
// with volume, output and artifact blobs.
func reportFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, token_hash) VALUES ('tok', $1)`, ids.Hash("host-secret"))
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool, state, token_id, last_heartbeat) VALUES
		('ha', 'ha', 'default', 'ready', 'tok', now()), ('hb', 'hb', 'default', 'ready', 'tok', now())`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
		('ra', 't1', '{"placement":{"pool":"default"}}', 'stopping', 1),
		('rb', 't2', '{"placement":{"pool":"default"}}', 'stopping', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('pa1', 't1', 'ra', 'ha', 1, 'stopping'), ('pb1', 't2', 'rb', 'hb', 1, 'stopping')`)
	if f := reportSnapshot(t, s, "ha", "ra", 1, snapshotA()); f.Type != proto.MsgAck {
		t.Fatalf("ra's report: %s %s", f.Type, f.Data)
	}
	return s, ctx
}

func snapshotA() proto.SnapshotDone {
	return proto.SnapshotDone{
		Manifest: proto.Manifest{SnapshotID: "snapA", RunID: "ra", Epoch: 1, Volumes: []proto.VolumeSnapshot{
			{Name: "work", Path: "/work", BlobID: "bA-vol", Size: 10, SHA256: "a-vol"},
		}},
		Output: &proto.BlobInfo{BlobID: "bA-out", Size: 20, SHA256: "a-out"},
		Artifacts: []proto.Artifact{{BlobInfo: proto.BlobInfo{BlobID: "bA-art", Size: 30, SHA256: "a-art"},
			Path: "/out/a.txt", ContentType: "text/plain", FileSize: 31, FileSHA256: "a-file"}},
	}
}

// snapshotB is a well-formed report for rb with only its own new blobs.
func snapshotB(snapID string, epoch int) proto.SnapshotDone {
	return proto.SnapshotDone{
		Manifest: proto.Manifest{SnapshotID: snapID, RunID: "rb", Epoch: epoch, Volumes: []proto.VolumeSnapshot{
			{Name: "work", Path: "/work", BlobID: "bB-vol-" + snapID, Size: 11, SHA256: "b-vol"},
		}},
		Output: &proto.BlobInfo{BlobID: "bB-out-" + snapID, Size: 21, SHA256: "b-out"},
		Artifacts: []proto.Artifact{{BlobInfo: proto.BlobInfo{BlobID: "bB-art-" + snapID, Size: 31, SHA256: "b-art"},
			Path: "/out/b.txt", ContentType: "text/plain", FileSize: 32, FileSHA256: "b-file"}},
	}
}

func reportSnapshot(t *testing.T, s *Server, hostID, runID string, epoch int, sd proto.SnapshotDone) proto.Frame {
	t.Helper()
	return s.handleReport(context.Background(), hostID, proto.Frame{Type: proto.MsgSnapshotDone, ID: 1, RunID: runID, Epoch: epoch,
		Data: proto.Marshal(sd)})
}

// runRecords is everything a snapshot report can write for a Run.
type runRecords struct {
	Snapshots, Blobs, Artifacts []string
	OutputBlob, RunSnapshot     string
	SnapshotBytes               int64
}

func recordsOf(t *testing.T, s *Server, runID string) runRecords {
	t.Helper()
	ctx := context.Background()
	var r runRecords
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		collect := func(q string) ([]string, error) {
			rows, err := tx.Query(ctx, q, runID)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[string])
		}
		var err error
		if r.Snapshots, err = collect(`SELECT id || ' ' || manifest::text FROM snapshots WHERE run_id = $1 ORDER BY id`); err != nil {
			return err
		}
		if r.Blobs, err = collect(`SELECT concat_ws(' ', id, tenant_id, run_id, epoch, kind, name, size, sha256, location)
			FROM blobs WHERE run_id = $1 ORDER BY id`); err != nil {
			return err
		}
		if r.Artifacts, err = collect(`SELECT concat_ws(' ', path, blob_id, tenant_id, content_type, size, sha256) FROM artifacts WHERE run_id = $1 ORDER BY path, blob_id`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(r.snapshot_id, ''),
				coalesce((SELECT string_agg(coalesce(output_blob_id, '-'), ',' ORDER BY epoch) FROM placements WHERE run_id = r.id), ''),
				coalesce((SELECT sum(coalesce(snapshot_bytes, 0)) FROM placements WHERE run_id = r.id), 0)
			FROM runs r WHERE r.id = $1`, runID).Scan(&r.RunSnapshot, &r.OutputBlob, &r.SnapshotBytes)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func eventErrors(t *testing.T, s *Server, runID, typ string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT coalesce(data->>'error', '') FROM run_events WHERE run_id = $1 AND type = $2 ORDER BY id`, runID, typ)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A report from rb naming any of ra's blob ids (or ra's snapshot id) is
// refused whole: acknowledged, with a snapshot.failed event on rb, and
// nothing of it stored for rb, not even its own new blobs; ra's records
// are unchanged.
func TestSnapshotReportRefusesAnotherRunsBlobs(t *testing.T) {
	for name, corrupt := range map[string]func(*proto.SnapshotDone){
		"volume": func(sd *proto.SnapshotDone) {
			sd.Manifest.Volumes = append(sd.Manifest.Volumes, proto.VolumeSnapshot{Name: "home", Path: "/home", BlobID: "bA-vol", Size: 10, SHA256: "a-vol"})
		},
		"output": func(sd *proto.SnapshotDone) {
			sd.Output = &proto.BlobInfo{BlobID: "bA-out", Size: 20, SHA256: "a-out"}
		},
		"artifact": func(sd *proto.SnapshotDone) {
			sd.Artifacts = append(sd.Artifacts, proto.Artifact{BlobInfo: proto.BlobInfo{BlobID: "bA-art", Size: 30, SHA256: "a-art"},
				Path: "/out/x.txt", ContentType: "text/plain"})
		},
		"snapshot id": func(sd *proto.SnapshotDone) { sd.Manifest.SnapshotID = "snapA" },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := reportFixture(t)
			beforeA, beforeB := recordsOf(t, s, "ra"), recordsOf(t, s, "rb")
			sd := snapshotB("snapB", 1)
			corrupt(&sd)
			if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck || !ackRefused(t, f) {
				t.Fatalf("reply %s %s, want an ack with refused (a refusal is final)", f.Type, f.Data)
			}
			if got := recordsOf(t, s, "rb"); !reflect.DeepEqual(got, beforeB) {
				t.Errorf("rb's records changed:\nbefore %+v\nafter  %+v", beforeB, got)
			}
			if got := recordsOf(t, s, "ra"); !reflect.DeepEqual(got, beforeA) {
				t.Errorf("ra's records changed:\nbefore %+v\nafter  %+v", beforeA, got)
			}
			if got := eventErrors(t, s, "rb", "snapshot.failed"); !reflect.DeepEqual(got, []string{foreignBlobsReason}) {
				t.Errorf("rb's snapshot.failed events: %q", got)
			}
			if got := eventErrors(t, s, "rb", "snapshot"); len(got) != 0 {
				t.Errorf("rb has snapshot events: %q", got)
			}
		})
	}
}

// A Run naming its own blob from an earlier report is accepted only as that
// same blob of the same placement: same epoch, kind, size and sha256.
func TestSnapshotReportOwnBlobReuse(t *testing.T) {
	s, ctx := reportFixture(t)

	// The same placement naming its recorded blob again, under a new
	// snapshot id: accepted.
	same := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapA-again", RunID: "ra", Epoch: 1, Volumes: []proto.VolumeSnapshot{
		{Name: "work", Path: "/work", BlobID: "bA-vol", Size: 10, SHA256: "a-vol"}}}}
	if f := reportSnapshot(t, s, "ha", "ra", 1, same); f.Type != proto.MsgAck {
		t.Fatalf("reply %s %s", f.Type, f.Data)
	}
	if got := recordsOf(t, s, "ra"); got.RunSnapshot != "snapA-again" || len(got.Snapshots) != 2 {
		t.Fatalf("same-placement report not recorded: %+v", got)
	}
	if n := len(eventErrors(t, s, "ra", "snapshot.failed")); n != 0 {
		t.Fatalf("%d snapshot.failed events", n)
	}

	// Anything else is refused: a later placement (the runner names new
	// blobs in every placement's report), another kind, another size.
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pa1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pa2', 't1', 'ra', 'ha', 2, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'ra'`)
	before := recordsOf(t, s, "ra")
	laterEpoch := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapA-epoch", RunID: "ra", Epoch: 2, Volumes: []proto.VolumeSnapshot{
		{Name: "work", Path: "/work", BlobID: "bA-vol", Size: 10, SHA256: "a-vol"}}}}
	asArtifact := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapA-kind", RunID: "ra", Epoch: 1, Volumes: []proto.VolumeSnapshot{}},
		Artifacts: []proto.Artifact{{BlobInfo: proto.BlobInfo{BlobID: "bA-vol", Size: 10, SHA256: "a-vol"}, Path: "/x"}}}
	resized := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapA-size", RunID: "ra", Epoch: 1, Volumes: []proto.VolumeSnapshot{
		{Name: "work", Path: "/work", BlobID: "bA-vol", Size: 99, SHA256: "a-vol"}}}}
	for _, sd := range []proto.SnapshotDone{laterEpoch, asArtifact, resized} {
		if f := reportSnapshot(t, s, "ha", "ra", sd.Manifest.Epoch, sd); f.Type != proto.MsgAck {
			t.Fatalf("%s: reply %s %s", sd.Manifest.SnapshotID, f.Type, f.Data)
		}
		if got := recordsOf(t, s, "ra"); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s: stored\nbefore %+v\nafter  %+v", sd.Manifest.SnapshotID, before, got)
		}
	}
	if n := len(eventErrors(t, s, "ra", "snapshot.failed")); n != 3 {
		t.Fatalf("%d snapshot.failed events, want 3", n)
	}
}

// A report delivered twice is recorded once and acknowledged both times.
func TestSnapshotReportRedelivered(t *testing.T) {
	s, _ := reportFixture(t)
	sd := snapshotB("snapB", 1)
	for i := range 2 {
		if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck {
			t.Fatalf("delivery %d: %s %s", i, f.Type, f.Data)
		}
	}
	got := recordsOf(t, s, "rb")
	if got.RunSnapshot != "snapB" || len(got.Snapshots) != 1 || len(got.Blobs) != 3 || len(got.Artifacts) != 1 {
		t.Fatalf("records: %+v", got)
	}
	if n := len(eventErrors(t, s, "rb", "snapshot")); n != 1 {
		t.Fatalf("%d snapshot events, want 1", n)
	}
	if n := len(eventErrors(t, s, "rb", "snapshot.failed")); n != 0 {
		t.Fatalf("%d snapshot.failed events", n)
	}
}

// A placement's final report refused: the Run ends as its stop decides,
// with the refusal in its state_reason, keeps its previous snapshot, and is
// not resumed, a move included. None of the report is stored, not even the
// placement's snapshot_done_at. An accepted report of a move resumes it.
func TestSnapshotReportRefusedEndsRunWithoutResume(t *testing.T) {
	for _, c := range []struct {
		name, stop    string
		earlier       bool // rb has a snapshot from before
		refused       bool
		state, reason string
	}{
		{"migrate", "migrate", true, true, StateStopped, "migrate; " + refusedSnapshotReason},
		{"drain", "drain", true, true, StateStopped, "drain; " + refusedSnapshotReason},
		{"stop", "stop", true, true, StateStopped, "stop; " + refusedSnapshotReason},
		{"stop, no earlier snapshot", "stop", false, true, StateStopped, "stop; " + refusedNoSnapshotReason},
		{"timeout", "timeout", true, true, StateFailed, "timeout; " + refusedSnapshotReason},
		{"migrate, accepted", "migrate", true, false, StateResuming, "auto-resume after migrate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := reportFixture(t)
			wantSnap := ""
			if c.earlier {
				if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); f.Type != proto.MsgAck {
					t.Fatalf("rb's first report: %s %s", f.Type, f.Data)
				}
				wantSnap = "snapB"
			}
			execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pb1'`)
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, stop_reason)
				VALUES ('pb2', 't2', 'rb', 'hb', 2, 'stopping', $1)`, c.stop)
			execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'rb'`)
			execSQL(t, s, ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, command, state, epoch)
				VALUES ('t2', 'rb', 'web', 3000, '["serve"]', 'ready', 2)`)

			sd := snapshotB("snapB2", 2)
			if c.refused {
				sd.Output = &proto.BlobInfo{BlobID: "bA-out", Size: 20, SHA256: "a-out"}
			} else {
				wantSnap = "snapB2"
			}
			if f := reportSnapshot(t, s, "hb", "rb", 2, sd); f.Type != proto.MsgAck || ackRefused(t, f) != c.refused {
				t.Fatalf("reply %s %s, want an ack with refused %v", f.Type, f.Data, c.refused)
			}
			code := 0
			exited := proto.Frame{Type: proto.MsgStatus, ID: 2, RunID: "rb", Epoch: 2,
				Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})}
			if f := s.handleReport(ctx, "hb", exited); f.Type != proto.MsgAck {
				t.Fatalf("exit: %s %s", f.Type, f.Data)
			}

			var state, reason, snap string
			var epoch int
			var doneAt, refusedCol bool
			systemScan(t, s, `SELECT r.state, r.state_reason, coalesce(r.snapshot_id, ''), r.current_epoch,
					p.snapshot_done_at IS NOT NULL, p.snapshot_refused
				FROM runs r JOIN placements p ON p.id = 'pb2' WHERE r.id = 'rb'`, nil,
				&state, &reason, &snap, &epoch, &doneAt, &refusedCol)
			if state != c.state || reason != c.reason || snap != wantSnap || epoch != 2 {
				t.Errorf("rb: state %q reason %q snapshot %q epoch %d; want %q %q %q 2", state, reason, snap, epoch, c.state, c.reason, wantSnap)
			}
			if doneAt == c.refused || refusedCol != c.refused {
				t.Errorf("pb2: snapshot_done_at set %v, snapshot_refused %v", doneAt, refusedCol)
			}
			// The Run's servers stop with the placement, refused or not; only
			// a move that is resumed elsewhere counts as migrated.
			wantStop := "run stopped"
			if (c.stop == "migrate" || c.stop == "drain") && !c.refused {
				wantStop = "migrated"
			}
			var svState, svStop string
			var svEpoch int
			systemScan(t, s, `SELECT state, coalesce(stop_reason, ''), coalesce(stopped_epoch, 0) FROM run_servers WHERE run_id = 'rb'`, nil,
				&svState, &svStop, &svEpoch)
			if svState != ServerStopped || svStop != wantStop || svEpoch != 2 {
				t.Errorf("rb's server: state %q stop_reason %q stopped_epoch %d; want %q %q 2", svState, svStop, svEpoch, ServerStopped, wantStop)
			}
			// Nothing schedules it: no epoch-3 placement.
			if c.refused {
				if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
					t.Fatal(err)
				}
				var placements int
				systemScan(t, s, `SELECT count(*) FROM placements WHERE run_id = 'rb'`, nil, &placements)
				if placements != 2 {
					t.Errorf("rb has %d placements after scheduling, want 2", placements)
				}
			}
		})
	}
}

// A report redelivered under a recorded snapshot id is accepted only if it
// is the same report: the same placement, manifest, output and artifacts.
func TestSnapshotReportRedeliveryMustMatch(t *testing.T) {
	for name, change := range map[string]func(*proto.SnapshotDone){
		"volume sha256":  func(sd *proto.SnapshotDone) { sd.Manifest.Volumes[0].SHA256 = "changed" },
		"volume id":      func(sd *proto.SnapshotDone) { sd.Manifest.Volumes[0].BlobID = "bB-vol-other" },
		"session":        func(sd *proto.SnapshotDone) { sd.Manifest.SessionID = "other" },
		"output sha256":  func(sd *proto.SnapshotDone) { sd.Output.SHA256 = "changed" },
		"another output": func(sd *proto.SnapshotDone) { sd.Output.BlobID = "bB-out-other" },
		"another artifact": func(sd *proto.SnapshotDone) {
			sd.Artifacts = append(sd.Artifacts, proto.Artifact{BlobInfo: proto.BlobInfo{BlobID: "bB-art-other", Size: 1, SHA256: "x"}, Path: "/out/c"})
		},
		"output omitted":        func(sd *proto.SnapshotDone) { sd.Output = nil },
		"artifact omitted":      func(sd *proto.SnapshotDone) { sd.Artifacts = nil },
		"artifact path":         func(sd *proto.SnapshotDone) { sd.Artifacts[0].Path = "/out/other.txt" },
		"artifact content type": func(sd *proto.SnapshotDone) { sd.Artifacts[0].ContentType = "text/html" },
		"artifact file size":    func(sd *proto.SnapshotDone) { sd.Artifacts[0].FileSize++ },
		"artifact file sha256":  func(sd *proto.SnapshotDone) { sd.Artifacts[0].FileSHA256 = "changed" },
		"artifact twice": func(sd *proto.SnapshotDone) {
			sd.Artifacts = append(sd.Artifacts, sd.Artifacts[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := reportFixture(t)
			if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); f.Type != proto.MsgAck {
				t.Fatalf("first report: %s %s", f.Type, f.Data)
			}
			before := recordsOf(t, s, "rb")
			sd := snapshotB("snapB", 1)
			change(&sd)
			// Not refused to the runner: snapB is still recorded, and its
			// files are still to be uploaded.
			if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck || ackRefused(t, f) {
				t.Fatalf("changed report: %s %s, want an ack without refused", f.Type, f.Data)
			}
			if got := recordsOf(t, s, "rb"); !reflect.DeepEqual(got, before) {
				t.Errorf("stored\nbefore %+v\nafter  %+v", before, got)
			}
			if got := eventErrors(t, s, "rb", "snapshot.failed"); !reflect.DeepEqual(got, []string{foreignBlobsReason}) {
				t.Errorf("snapshot.failed events %q", got)
			}
			// The latest report was refused, whatever was recorded before.
			var refused bool
			systemScan(t, s, `SELECT snapshot_refused FROM placements WHERE id = 'pb1'`, nil, &refused)
			if !refused {
				t.Error("pb1 not marked snapshot_refused")
			}
		})
	}
}

// The same snapshot id reported again from a later placement is refused,
// with the same manifest too (here with no output or artifacts to differ).
func TestSnapshotReportRedeliveryFromAnotherPlacement(t *testing.T) {
	s, ctx := reportFixture(t)
	sd := snapshotB("snapB", 1)
	sd.Output, sd.Artifacts = nil, nil
	if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck {
		t.Fatalf("first report: %s %s", f.Type, f.Data)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pb1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb2', 't2', 'rb', 'hb', 2, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'rb'`)
	before := recordsOf(t, s, "rb")
	if f := reportSnapshot(t, s, "hb", "rb", 2, sd); f.Type != proto.MsgAck || !ackRefused(t, f) {
		t.Fatalf("reply %s %s, want an ack with refused", f.Type, f.Data)
	}
	if got := recordsOf(t, s, "rb"); !reflect.DeepEqual(got, before) {
		t.Errorf("stored\nbefore %+v\nafter  %+v", before, got)
	}
	var refused bool
	systemScan(t, s, `SELECT snapshot_refused FROM placements WHERE id = 'pb2'`, nil, &refused)
	if !refused {
		t.Error("pb2 not marked snapshot_refused")
	}
}

func ackRefused(t *testing.T, f proto.Frame) bool {
	t.Helper()
	var ack proto.Ack
	if len(f.Data) > 0 {
		if err := json.Unmarshal(f.Data, &ack); err != nil {
			t.Fatal(err)
		}
	}
	return ack.Refused
}

// exitPlacement reports rb's placement at epoch as exited with code 0.
func exitPlacement(t *testing.T, s *Server, epoch int) {
	t.Helper()
	code := 0
	f := s.handleReport(context.Background(), "hb", proto.Frame{Type: proto.MsgStatus, ID: 9, RunID: "rb", Epoch: epoch,
		Data: proto.Marshal(proto.Status{State: "exited", ExitCode: &code, Reason: "exited"})})
	if f.Type != proto.MsgAck {
		t.Fatalf("exit: %s %s", f.Type, f.Data)
	}
}

// A moving placement that reported snapshot A, then a refused report B,
// stays stopped rather than resuming from A, A redelivered unchanged
// included: a redelivery changes nothing. A later new report C clears the
// refusal, and the move then resumes from C.
func TestSnapshotReportRefusedAfterAcceptedInSamePlacement(t *testing.T) {
	for _, c := range []struct {
		name         string
		later        string // a report after the refused B: "", "snapB-A" again, or a new "snapB-C"
		state, snap  string
		reason       string
		wantResuming bool
	}{
		{"refused last", "", StateStopped, "snapB-A", "migrate; " + refusedSnapshotReason, false},
		{"A redelivered after refused", "snapB-A", StateStopped, "snapB-A", "migrate; " + refusedSnapshotReason, false},
		{"new accepted after refused", "snapB-C", StateResuming, "snapB-C", "auto-resume after migrate", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := reportFixture(t)
			execSQL(t, s, ctx, `UPDATE placements SET stop_reason = 'migrate' WHERE id = 'pb1'`)
			if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB-A", 1)); f.Type != proto.MsgAck || ackRefused(t, f) {
				t.Fatalf("report A: %s %s", f.Type, f.Data)
			}
			refusedB := snapshotB("snapB-B", 1)
			refusedB.Output = &proto.BlobInfo{BlobID: "bA-out", Size: 20, SHA256: "a-out"}
			// B's id is not recorded, so the runner may drop its files.
			if f := reportSnapshot(t, s, "hb", "rb", 1, refusedB); f.Type != proto.MsgAck || !ackRefused(t, f) {
				t.Fatalf("report B: %s %s, want an ack with refused", f.Type, f.Data)
			}
			before := recordsOf(t, s, "rb")
			snapshotEvents := len(eventErrors(t, s, "rb", "snapshot"))
			if c.later != "" {
				if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB(c.later, 1)); f.Type != proto.MsgAck || ackRefused(t, f) {
					t.Fatalf("report %s: %s %s", c.later, f.Type, f.Data)
				}
			}
			if c.later == "snapB-A" {
				if got := recordsOf(t, s, "rb"); !reflect.DeepEqual(got, before) {
					t.Errorf("redelivery stored\nbefore %+v\nafter  %+v", before, got)
				}
				if n := len(eventErrors(t, s, "rb", "snapshot")); n != snapshotEvents {
					t.Errorf("redelivery added snapshot events: %d, want %d", n, snapshotEvents)
				}
			}
			exitPlacement(t, s, 1)

			var state, reason, snap string
			var refused bool
			systemScan(t, s, `SELECT r.state, r.state_reason, coalesce(r.snapshot_id, ''), p.snapshot_refused
				FROM runs r JOIN placements p ON p.id = 'pb1' WHERE r.id = 'rb'`, nil, &state, &reason, &snap, &refused)
			if state != c.state || reason != c.reason || snap != c.snap || refused == c.wantResuming {
				t.Errorf("rb: state %q reason %q snapshot %q refused %v; want %q %q %q %v",
					state, reason, snap, refused, c.state, c.reason, c.snap, !c.wantResuming)
			}
			if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
				t.Fatal(err)
			}
			var placements int
			systemScan(t, s, `SELECT count(*) FROM placements WHERE run_id = 'rb'`, nil, &placements)
			if !c.wantResuming && placements != 1 {
				t.Errorf("rb has %d placements after scheduling, want 1", placements)
			}
		})
	}
}

// A Run whose first snapshot report was refused has nothing to restore:
// resume answers 409 no_snapshot, GET says why, and the resumable filter
// leaves it out. A Run stopped before any snapshot for another reason
// keeps resuming from scratch.
func TestResumeRefusedWithoutSnapshot(t *testing.T) {
	for _, c := range []struct {
		name    string
		refused bool
	}{
		{"refused first snapshot", true},
		{"ordinary stop without snapshot", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := reportFixture(t)
			execSQL(t, s, ctx, `UPDATE placements SET stop_reason = 'stop' WHERE id = 'pb1'`)
			if c.refused {
				sd := snapshotB("snapB", 1)
				sd.Output = &proto.BlobInfo{BlobID: "bA-out", Size: 20, SHA256: "a-out"}
				if f := reportSnapshot(t, s, "hb", "rb", 1, sd); f.Type != proto.MsgAck || !ackRefused(t, f) {
					t.Fatalf("report: %s %s", f.Type, f.Data)
				}
			} else if f := reportSnapshot(t, s, "hb", "rb", 1, proto.SnapshotDone{Error: "no volumes"}); f.Type != proto.MsgAck {
				t.Fatalf("report: %s %s", f.Type, f.Data)
			}
			exitPlacement(t, s, 1)
			actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t2", Scopes: []string{"run", "read"}})

			got, err := s.getRun(actx, &RunPath{ID: "rb"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Body.State != StateStopped || got.Body.SnapshotID != nil || got.Body.Resume == nil {
				t.Fatalf("rb: %+v", got.Body)
			}
			if blocked := slices.Contains(got.Body.Resume.Blockers, noSnapshotReason); blocked != c.refused {
				t.Errorf("resume blockers %q, want the no-snapshot one: %v", got.Body.Resume.Blockers, c.refused)
			}
			list, err := s.listRuns(actx, &listRunsInput{Resumable: true})
			if err != nil {
				t.Fatal(err)
			}
			listed := slices.ContainsFunc(list.Body.Runs, func(r *Run) bool { return r.ID == "rb" })
			if listed == c.refused {
				t.Errorf("rb in the resumable list: %v", listed)
			}

			_, err = s.resumeRun(actx, &resumeRunInput{RunPath: RunPath{ID: "rb"}})
			var he *HTTPError
			if c.refused {
				if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "no_snapshot" {
					t.Fatalf("resume: %v, want 409 no_snapshot", err)
				}
			} else if err != nil {
				t.Fatalf("resume: %v", err)
			}
			var state string
			systemScan(t, s, `SELECT state FROM runs WHERE id = 'rb'`, nil, &state)
			if want := map[bool]string{true: StateStopped, false: StateResuming}[c.refused]; state != want {
				t.Errorf("rb is %s after resume, want %s", state, want)
			}
		})
	}
}
