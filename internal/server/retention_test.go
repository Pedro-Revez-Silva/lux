package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// retentionFixture: supersededFixture with every snapshot uploaded, and
// ra (t1, retention 30 days) and rb (t2, retention 5 days) ended days ago
// in state (and, terminated, terminated then).
func retentionFixture(t *testing.T, raState, rbState string, raDays, rbDays float64) (*Server, context.Context, *fakeS3) {
	t.Helper()
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	execSQL(t, s, ctx, `UPDATE tenants SET retention_days = 5 WHERE id = 't2'`)
	for _, r := range []struct {
		id, state string
		days      float64
	}{{"ra", raState, raDays}, {"rb", rbState, rbDays}} {
		execSQL(t, s, ctx, `UPDATE runs SET state = $2, finished_at = now() - make_interval(secs => $3 * 86400),
			terminated_at = CASE WHEN $2 = 'terminated' THEN now() - make_interval(secs => $3 * 86400) END WHERE id = $1`, r.id, r.state, r.days)
	}
	return s, ctx, f
}

// A terminated Run past its tenant's retention loses its snapshot volumes
// and output (rows deleted, S3 objects deleted) and every snapshot becomes
// unavailable; its artifacts stay. One within retention keeps everything.
func TestReapRetentionTerminal(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 29.9, 5.01)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"bB-vol-snapB": "deleted", "bB-out-snapB": "deleted", "bB-art-snapB": "s3",
		"bB-vol-snapB2": "deleted", "bB-out-snapB2": "deleted", "bB-art-snapB2": "s3"} {
		if got := blobLocations(t, s, "rb")[id]; got != want {
			t.Errorf("rb %s: %s, want %s", id, got, want)
		}
	}
	if snapshotAvailable(t, s, "snapB") || snapshotAvailable(t, s, "snapB2") {
		t.Error("rb's snapshots still available")
	}
	want := []string{"rb/bB-out-snapB", "rb/bB-out-snapB2", "rb/bB-vol-snapB", "rb/bB-vol-snapB2"}
	if got := f.Deleted(); !slices.Equal(got, want) {
		t.Errorf("S3 deletes %v, want %v", got, want)
	}
	// ra: within its tenant's 30 days.
	for id, loc := range blobLocations(t, s, "ra") {
		if loc != "s3" {
			t.Errorf("ra %s: %s, want s3", id, loc)
		}
	}
	if !snapshotAvailable(t, s, "snapA") {
		t.Error("snapA unavailable within retention")
	}
}

// A failed or succeeded Run is resumable: retention deletes nothing of it,
// however long ago it ended. Nor of a stopped Run with an old finished_at.
// Once it expires (terminated), its retention counts from the terminate,
// not from its 400-day-old finished_at.
func TestReapRetentionSparesResumable(t *testing.T) {
	for _, ended := range []string{StateFailed, StateSucceeded} {
		t.Run(ended, func(t *testing.T) { testReapRetentionSparesResumable(t, ended) })
	}
}

func testReapRetentionSparesResumable(t *testing.T, ended string) {
	s, ctx, f := retentionFixture(t, StateStopped, ended, 400, 400)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"ra", "rb"} {
		for id, loc := range blobLocations(t, s, run) {
			if loc != "s3" {
				t.Errorf("%s %s: %s, want s3", run, id, loc)
			}
		}
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("S3 deletes %v", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state_changed_at = now() - interval '91 days' WHERE id = 'rb'`)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'rb'`, nil, &state)
	if state != StateTerminated {
		t.Fatalf("rb after expiry: %s", state)
	}
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("deleted on expiry, before retention: %v", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET terminated_at = now() - interval '6 days' WHERE id = 'rb'`)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 4 {
		t.Fatalf("expired Run past retention: deleted %v, want its volumes and outputs", got)
	}
}

// A Run terminated by request long after it succeeded keeps its whole
// retention from the terminate: its old finished_at does not count.
func TestReapRetentionCountsFromTermination(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateStopped, StateSucceeded, 1, 400)
	key := apiKey(t, s, new("t2"), "run")
	if code, body := call(t, s, key, http.MethodPost, "/v1/runs/rb/terminate", nil); code != http.StatusAccepted {
		t.Fatalf("terminate: %d %s", code, body)
	}
	var finishedOld, terminatedNow bool
	systemScan(t, s, `SELECT finished_at < now() - interval '399 days', terminated_at > now() - interval '1 minute' FROM runs WHERE id = 'rb'`,
		nil, &finishedOld, &terminatedNow)
	if !finishedOld || !terminatedNow {
		t.Fatalf("finished_at kept %v, terminated_at now %v", finishedOld, terminatedNow)
	}
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("deleted at once on a late terminate: %v", got)
	}
}

// A due Run is claimed even when more already-reaped Runs than a pass's
// batch finished before it; a Run whose only S3 data is artifacts is not a
// candidate, however old: its snapshot stays available.
func TestReapRetentionPastReapedHistory(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 30)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, finished_at, terminated_at)
		SELECT 'old' || lpad(i::text, 3, '0'), 't2', '{}', 'terminated', 1, now() - make_interval(days => 100 + i), now() - make_interval(days => 100 + i)
		FROM generate_series(1, 50) i`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key, deleted_at)
		SELECT r.id || '-' || k, 't2', r.id, 1, k, 'work', 'deleted', r.id || '/' || k, now()
		FROM runs r, unnest(ARRAY['volume', 'output']) k WHERE r.id LIKE 'old___'`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, finished_at, terminated_at)
		VALUES ('rart', 't2', '{}', 'terminated', 1, now() - interval '400 days', now() - interval '400 days')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('part', 't2', 'rart', 'hb', 1, 'exited')`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key, deleted_at) VALUES
		('rart-vol', 't2', 'rart', 1, 'volume', 'work', 'deleted', 'rart/rart-vol', now()),
		('rart-art', 't2', 'rart', 1, 'artifact', 'a.txt', 's3', 'rart/rart-art', NULL)`)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snapArt', 't2', 'rart', 'part', 1, '{"volumes":[]}', 'hb', true)`)

	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"rb/bB-out-snapB", "rb/bB-out-snapB2", "rb/bB-vol-snapB", "rb/bB-vol-snapB2"}
	if got := f.Deleted(); !slices.Equal(got, want) {
		t.Errorf("S3 deletes %v, want rb's %v", got, want)
	}
	if !snapshotAvailable(t, s, "snapArt") {
		t.Error("an artifact-only Run's snapshot made unavailable")
	}
	if got := blobLocations(t, s, "rart")["rart-art"]; got != "s3" {
		t.Errorf("rart's artifact: %s, want s3", got)
	}
}

// The reaper's first tick reaps retention; a tick within the next minute
// leaves a Run that became due meanwhile alone, but still loses a
// placement whose lease expired meanwhile; one a minute after the last
// reaps it.
func TestReapOnceStorageCadence(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateStopped, StateTerminated, 1, 30)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('rl', 't1', '{"placement":{"pool":"default"}}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, lease_expires_at)
		VALUES ('pl1', 't1', 'rl', 'ha', 1, 'running', now() + interval '1 hour')`)
	var last time.Time
	s.reapOnce(ctx, &last)
	if got := f.Deleted(); len(got) != 4 {
		t.Fatalf("first tick deleted %v, want rb's volumes and outputs", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'terminated', terminated_at = now() - interval '40 days' WHERE id = 'ra'`)
	execSQL(t, s, ctx, `UPDATE placements SET lease_expires_at = now() - interval '1 minute' WHERE id = 'pl1'`)
	s.reapOnce(ctx, &last)
	if got := f.Deleted(); len(got) != 4 {
		t.Fatalf("a tick within the minute reaped retention: %v", got)
	}
	var state string
	systemScan(t, s, `SELECT state FROM runs WHERE id = 'rl'`, nil, &state)
	if state != StateLost {
		t.Fatalf("rl after its lease expired, a tick within the minute: %q, want lost", state)
	}
	last = last.Add(-storageReapEvery)
	s.reapOnce(ctx, &last)
	if got := f.Deleted(); len(got) <= 4 || !slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "ra/") }) {
		t.Fatalf("a tick a minute later did not reap ra: %v", got)
	}
}

// An S3 delete that fails leaves an orphan, logged; the claim stands.
func TestReapRetentionS3Failure(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 30)
	log := captureLog(s)
	f.failPrefix = "rb/bB-vol"
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "retention: S3 delete failed; object orphaned") || !strings.Contains(log.String(), "rb/bB-vol-snapB2") {
		t.Fatalf("no orphan logged: %s", log.String())
	}
	if blobLocations(t, s, "rb")["bB-vol-snapB2"] != "deleted" || snapshotAvailable(t, s, "snapB2") {
		t.Fatal("claim did not stand")
	}
	if got := f.Deleted(); !slices.Equal(got, []string{"rb/bB-out-snapB", "rb/bB-out-snapB2"}) {
		t.Fatalf("S3 deletes %v", got)
	}
}
