package server

import (
	"net/http"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// A never Run's exit snapshot lists no volume (the runner exports none):
// luxd records it with its output, the Run ends terminated with its
// outcome, and a resume is still refused.
func TestNeverRunZeroVolumeSnapshotEndsTerminated(t *testing.T) {
	s, _ := policyFixture(t, "never")
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapR1", RunID: "r1", Epoch: 1, Volumes: []proto.VolumeSnapshot{}},
		Output: &proto.BlobInfo{BlobID: "b-r1-out", Size: 7, SHA256: "r1-out"}, OutputSeq: 3}
	if got := reportSnapshot(t, s, "h1", "r1", 1, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
		t.Fatalf("snapshot report: %s %s", got.Type, got.Data)
	}
	reportR1Exit(t, s, 0, "exited")

	var state, reason, snapshot, output, blobs string
	var volumes, bytes int64
	systemScan(t, s, `SELECT r.state, r.state_reason, coalesce(r.snapshot_id, ''), coalesce(p.output_blob_id, ''),
			coalesce(p.snapshot_bytes, -1),
			(SELECT string_agg(kind || ':' || id, ',' ORDER BY id) FROM blobs WHERE run_id = r.id),
			(SELECT (data->>'volumes')::bigint FROM run_events WHERE run_id = r.id AND type = 'snapshot')
		FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 1 WHERE r.id = 'r1'`, nil,
		&state, &reason, &snapshot, &output, &bytes, &blobs, &volumes)
	if state != StateTerminated || reason != "succeeded; resumePolicy never" {
		t.Errorf("state %q reason %q, want terminated, succeeded; resumePolicy never", state, reason)
	}
	if snapshot != "snapR1" || output != "b-r1-out" || bytes != 0 || volumes != 0 || blobs != "output:b-r1-out" {
		t.Errorf("recorded snapshot %q output %q bytes %d volumes %d blobs %q", snapshot, output, bytes, volumes, blobs)
	}
	_, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: "r1"}})
	refused(t, err, http.StatusConflict, "not_resumable", "resume of a never Run")
}

// The runner sends a snapshot.done again when its ack was lost: luxd acks
// the same zero-volume report both times, records one snapshot, does not
// mark the placement refused, and the Run still ends terminated.
func TestNeverRunZeroVolumeSnapshotRedelivered(t *testing.T) {
	s, _ := policyFixture(t, "never")
	sd := proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: "snapR1", RunID: "r1", Epoch: 1, Volumes: []proto.VolumeSnapshot{}},
		Output: &proto.BlobInfo{BlobID: "b-r1-out", Size: 7, SHA256: "r1-out"}, OutputSeq: 3}
	for i := range 2 {
		if got := reportSnapshot(t, s, "h1", "r1", 1, sd); got.Type != proto.MsgAck || ackRefused(t, got) {
			t.Fatalf("snapshot report %d: %s %s", i+1, got.Type, got.Data)
		}
	}
	reportR1Exit(t, s, 0, "exited")

	var state, reason string
	var events int64
	var refusedCol bool
	systemScan(t, s, `SELECT r.state, r.state_reason, p.snapshot_refused,
			(SELECT count(*) FROM run_events WHERE run_id = r.id AND type = 'snapshot')
		FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 1 WHERE r.id = 'r1'`, nil,
		&state, &reason, &refusedCol, &events)
	if events != 1 || refusedCol {
		t.Errorf("snapshot events %d, snapshot_refused %v: want 1, false", events, refusedCol)
	}
	if state != StateTerminated || reason != "succeeded; resumePolicy never" {
		t.Errorf("state %q reason %q, want terminated, succeeded; resumePolicy never", state, reason)
	}
}
