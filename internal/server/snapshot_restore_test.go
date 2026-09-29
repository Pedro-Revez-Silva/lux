package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// resumeFixture leaves rb stopped at epoch 1 with snapshot snapB (uploaded)
// and queued to resume; host hb is connected.
func resumeFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s, ctx := reportFixture(t)
	s.cfg.LeaseDuration = time.Minute
	if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); f.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", f.Type, f.Data)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE run_id IN ('ra', 'rb')`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped' WHERE id = 'ra'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming' WHERE id = 'rb'`)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	s.hub.polled("hb")
	return s, ctx
}

// assignedManifest is the snapshot in rb's queued epoch-2 assignment, if any.
func assignedManifest(t *testing.T, s *Server) *proto.Manifest {
	t.Helper()
	ctx := context.Background()
	var payload []byte
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload FROM host_messages WHERE run_id = 'rb' AND epoch = 2 AND type = $1`, proto.MsgAssign).Scan(&payload)
	})
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var a proto.Assign
	if err := json.Unmarshal(payload, &a); err != nil {
		t.Fatal(err)
	}
	if a.Resume == nil || a.Resume.Snapshot == nil {
		t.Fatalf("assignment without a snapshot: %s", payload)
	}
	return a.Resume.Snapshot
}

// The assignment carries each volume's size and sha256 from its blob row,
// whatever the stored manifest says.
func TestAssignSendsBlobChecksums(t *testing.T) {
	s, ctx := resumeFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(jsonb_set(manifest, '{volumes,0,sha256}', '"edited"'), '{volumes,0,size}', '1')
		WHERE id = 'snapB'`)
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	m := assignedManifest(t, s)
	if m == nil {
		t.Fatal("rb was not assigned")
	}
	if len(m.Volumes) != 1 || m.Volumes[0].BlobID != "bB-vol-snapB" || m.Volumes[0].SHA256 != "b-vol" || m.Volumes[0].Size != 11 {
		t.Fatalf("assigned volumes %+v, want bB-vol-snapB with sha256 b-vol, size 11", m.Volumes)
	}
}

// A stored manifest naming a blob that is not one of the Run's volumes
// (another Run's, or the Run's own output) is not assigned: the Run fails
// with a reason and nothing is placed.
func TestAssignRefusesSnapshotWithOtherBlobs(t *testing.T) {
	for name, blobID := range map[string]string{"another run's volume": "bA-vol", "own output": "bB-out-snapB", "unknown": "b-missing"} {
		t.Run(name, func(t *testing.T) {
			s, ctx := resumeFixture(t)
			execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes,0,blobId}', to_jsonb($1::text)) WHERE id = 'snapB'`, blobID)
			if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
				t.Fatal(err)
			}
			if m := assignedManifest(t, s); m != nil {
				t.Fatalf("assigned %+v", m)
			}
			var state, reason string
			var epoch, placements int
			systemScan(t, s, `SELECT state, state_reason, current_epoch, (SELECT count(*) FROM placements WHERE run_id = 'rb')
				FROM runs WHERE id = 'rb'`, nil, &state, &reason, &epoch, &placements)
			if state != StateFailed || reason != foreignSnapshotReason || epoch != 1 || placements != 1 {
				t.Fatalf("rb: state %q reason %q epoch %d placements %d", state, reason, epoch, placements)
			}
		})
	}
}

// A snapshot is marked uploaded only when every volume in its manifest is
// one of its Run's blobs and in S3.
func TestMarkUploadedCountsOnlyOwnBlobs(t *testing.T) {
	s, ctx := reportFixture(t)
	if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); f.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", f.Type, f.Data)
	}
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3'`)
	execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes}',
		manifest->'volumes' || '[{"name":"x","blobId":"bA-vol"}]') WHERE id = 'snapB'`)
	mark := func() bool { return uploadedAfterMark(t, s, "snapB", 1) }
	if mark() {
		t.Fatal("snapB marked uploaded from ra's blob")
	}
	execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes}', (manifest->'volumes') - 1) WHERE id = 'snapB'`)
	if !mark() {
		t.Fatal("snapB not marked uploaded with all its own volumes in S3")
	}
}

// A runner may download only the volumes of the snapshot its placement
// restores: not the Run's other blobs, older snapshots' volumes, or another
// Run's.
func TestRunnerBlobDownloadOnlyCurrentSnapshot(t *testing.T) {
	s, ctx := resumeFixture(t)
	useTestBlobs(t, s)
	// rb's newer snapshot, which it is now resuming from, on hb at epoch 2;
	// ra's current placement is on hb too.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopping' WHERE id = 'rb'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb2', 't2', 'rb', 'hb', 2, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'rb'`)
	if f := reportSnapshot(t, s, "hb", "rb", 2, snapshotB("snapB2", 2)); f.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", f.Type, f.Data)
	}
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('pb3', 't2', 'rb', 'hb', 3, 'assigned'), ('pa2', 't1', 'ra', 'hb', 2, 'assigned')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = current_epoch + 1`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)

	for _, c := range []struct {
		id, host string
		want     int
	}{
		{"bB-vol-snapB2", "hb", http.StatusFound},
		{"bB-vol-snapB2", "ha", http.StatusNotFound},
		{"bB-vol-snapB", "hb", http.StatusNotFound},  // an older snapshot's volume
		{"bB-out-snapB2", "hb", http.StatusNotFound}, // the Run's output
		{"bB-art-snapB2", "hb", http.StatusNotFound}, // an artifact
		{"bA-out", "hb", http.StatusNotFound},        // ra's, though ra is placed on hb
		{"bA-vol", "hb", http.StatusFound},           // ra's snapshot, for ra's placement on hb
	} {
		if got := runnerGet(s, c.id, c.host); got != c.want {
			t.Errorf("GET %s from %s: %d, want %d", c.id, c.host, got, c.want)
		}
	}
}

// twoEpochFixture: rb has snapB (epoch 1) and snapB2 (epoch 2), both
// uploaded, and is queued to resume from snapB2, whose manifest names
// snapB's volume instead of its own.
func twoEpochFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s, ctx := resumeFixture(t)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb2', 't2', 'rb', 'hb', 2, 'stopping')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2, state = 'stopping' WHERE id = 'rb'`)
	if f := reportSnapshot(t, s, "hb", "rb", 2, snapshotB("snapB2", 2)); f.Type != proto.MsgAck {
		t.Fatalf("rb's report: %s %s", f.Type, f.Data)
	}
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pb2'`)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'resuming' WHERE id = 'rb'`)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 's3', s3_key = run_id || '/' || id`)
	execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes,0,blobId}', '"bB-vol-snapB"') WHERE id = 'snapB2'`)
	return s, ctx
}

// A snapshot naming its Run's own volume from another placement (epoch) is
// not restored: the Run fails at assignment, the snapshot never counts as
// uploaded, and the volume is not served for download.
func TestSnapshotVolumeFromAnotherEpoch(t *testing.T) {
	t.Run("assign", func(t *testing.T) {
		s, ctx := twoEpochFixture(t)
		if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
			t.Fatal(err)
		}
		var state, reason string
		var placements int
		systemScan(t, s, `SELECT state, state_reason, (SELECT count(*) FROM placements WHERE run_id = 'rb')
			FROM runs WHERE id = 'rb'`, nil, &state, &reason, &placements)
		if state != StateFailed || reason != foreignSnapshotReason || placements != 2 {
			t.Fatalf("rb: state %q reason %q placements %d", state, reason, placements)
		}
	})
	t.Run("uploaded", func(t *testing.T) {
		s, ctx := twoEpochFixture(t)
		execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = false WHERE id = 'snapB2'`)
		if uploadedAfterMark(t, s, "snapB2", 2) {
			t.Fatal("snapB2 marked uploaded from epoch 1's volume")
		}
	})
	t.Run("download", func(t *testing.T) {
		s, ctx := twoEpochFixture(t)
		useTestBlobs(t, s)
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pb3', 't2', 'rb', 'hb', 3, 'assigned')`)
		execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3, state = 'scheduled' WHERE id = 'rb'`)
		if got := runnerGet(s, "bB-vol-snapB", "hb"); got != http.StatusNotFound {
			t.Fatalf("GET bB-vol-snapB: %d, want 404", got)
		}
		// The same volume, named by its own snapshot: served.
		execSQL(t, s, ctx, `UPDATE runs SET snapshot_id = 'snapB' WHERE id = 'rb'`)
		if got := runnerGet(s, "bB-vol-snapB", "hb"); got != http.StatusFound {
			t.Fatalf("GET bB-vol-snapB for snapB: %d, want 302", got)
		}
	})
}

// A snapshot counts as uploaded only when each volume is a volume blob of
// its Run and placement, in S3.
func TestMarkUploadedRequiresEachVolume(t *testing.T) {
	for _, c := range []struct {
		name, setup string
		want        bool
	}{
		{"in s3", ``, true},
		{"still on the host", `UPDATE blobs SET location = 'host' WHERE id = 'bB-vol-snapB'`, false},
		{"missing row", `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes,0,blobId}', '"b-missing"') WHERE id = 'snapB'`, false},
		{"output blob", `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes,0,blobId}', '"bB-out-snapB"') WHERE id = 'snapB'`, false},
		{"wrong kind", `UPDATE blobs SET kind = 'artifact' WHERE id = 'bB-vol-snapB'`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := reportFixture(t)
			if f := reportSnapshot(t, s, "hb", "rb", 1, snapshotB("snapB", 1)); f.Type != proto.MsgAck {
				t.Fatalf("rb's report: %s %s", f.Type, f.Data)
			}
			execSQL(t, s, ctx, `UPDATE blobs SET location = 's3'`)
			if c.setup != "" {
				execSQL(t, s, ctx, c.setup)
			}
			if got := uploadedAfterMark(t, s, "snapB", 1); got != c.want {
				t.Fatalf("uploaded %v, want %v", got, c.want)
			}
		})
	}
}

// A Run whose snapshot cannot be restored fails before a host is chosen,
// rather than waiting for an upload (here: moving away from hb, the only
// host with the copy, which is not uploaded).
func TestUnrestorableSnapshotFailsBeforeWaiting(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid waits", false: "invalid fails"}[valid], func(t *testing.T) {
			s, ctx := resumeFixture(t)
			s.hub.polled("ha")
			execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = false WHERE id = 'snapB'`)
			execSQL(t, s, ctx, `UPDATE runs SET avoid_host = 'hb' WHERE id = 'rb'`)
			if !valid {
				execSQL(t, s, ctx, `UPDATE snapshots SET manifest = jsonb_set(manifest, '{volumes,0,blobId}', '"bA-vol"') WHERE id = 'snapB'`)
			}
			if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
				t.Fatal(err)
			}
			var state, reason string
			systemScan(t, s, `SELECT state, state_reason FROM runs WHERE id = 'rb'`, nil, &state, &reason)
			if valid && (state != StateResuming || reason != "waiting for snapshot upload") {
				t.Fatalf("rb: state %q reason %q, want waiting for its upload", state, reason)
			}
			if !valid && (state != StateFailed || reason != foreignSnapshotReason) {
				t.Fatalf("rb: state %q reason %q, want failed", state, reason)
			}
		})
	}
}

func uploadedAfterMark(t *testing.T, s *Server, snapID string, epoch int) bool {
	t.Helper()
	ctx := context.Background()
	var up bool
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := markUploaded(ctx, tx, "rb", epoch); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT uploaded FROM snapshots WHERE id = $1`, snapID).Scan(&up)
	}); err != nil {
		t.Fatal(err)
	}
	return up
}

func useTestBlobs(t *testing.T, s *Server) {
	t.Helper()
	bs, err := blob.New(context.Background(), blob.Config{Endpoint: "http://s3.invalid:9000", Bucket: "b", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.blobs = bs
}

func runnerGet(s *Server, id, host string) int {
	req := httptest.NewRequest(http.MethodGet, "/runner/v1/blobs/"+id+"?host="+host, nil)
	req.SetPathValue("id", id)
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.wrap(s.serveRunnerBlobDownload)(rec, req)
	return rec.Code
}
