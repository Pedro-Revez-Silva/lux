package server

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// retentionFixture: supersededFixture with every snapshot uploaded, and
// ra (t1, retention 30 days) and rb (t2, retention 5 days) finished days
// ago in state.
func retentionFixture(t *testing.T, raState, rbState string, raDays, rbDays float64) (*Server, context.Context, *fakeS3) {
	t.Helper()
	s, ctx, f := supersededFixture(t)
	execSQL(t, s, ctx, `UPDATE snapshots SET uploaded = true`)
	execSQL(t, s, ctx, `UPDATE tenants SET retention_days = 5 WHERE id = 't2'`)
	for _, r := range []struct {
		id, state string
		days      float64
	}{{"ra", raState, raDays}, {"rb", rbState, rbDays}} {
		execSQL(t, s, ctx, `UPDATE runs SET state = $2, finished_at = now() - make_interval(secs => $3 * 86400) WHERE id = $1`, r.id, r.state, r.days)
	}
	return s, ctx, f
}

// A succeeded or cancelled Run past its tenant's retention loses its
// snapshot volumes and output (rows deleted, S3 objects deleted) and every
// snapshot becomes unavailable; its artifacts stay. One within retention
// keeps everything.
func TestReapRetentionTerminal(t *testing.T) {
	for _, state := range []string{StateSucceeded, StateCancelled} {
		t.Run(state, func(t *testing.T) {
			s, ctx, f := retentionFixture(t, state, state, 29.9, 5.01)
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
		})
	}
}

// A failed Run is resumable: retention deletes nothing of it, however long
// ago it failed. Nor of a stopped Run with an old finished_at.
func TestReapRetentionSparesResumable(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateStopped, StateFailed, 400, 400)
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
	// Once it expires (cancelled now), its retention counts from then.
	execSQL(t, s, ctx, `UPDATE runs SET state_changed_at = now() - interval '91 days' WHERE id = 'rb'`)
	if err := s.reapExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("deleted on expiry, before retention: %v", got)
	}
	execSQL(t, s, ctx, `UPDATE runs SET finished_at = now() - interval '6 days' WHERE id = 'rb'`)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Deleted(); len(got) != 4 {
		t.Fatalf("expired Run past retention: deleted %v, want its volumes and outputs", got)
	}
}

// A due Run is claimed even when more already-reaped Runs than a pass's
// batch finished before it; a Run whose only S3 data is artifacts is not a
// candidate, however old: its snapshot stays available.
func TestReapRetentionPastReapedHistory(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateSucceeded, StateSucceeded, 1, 30)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, finished_at)
		SELECT 'old' || lpad(i::text, 3, '0'), 't2', '{}', 'succeeded', 1, now() - make_interval(days => 100 + i)
		FROM generate_series(1, 50) i`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key, deleted_at)
		SELECT r.id || '-' || k, 't2', r.id, 1, k, 'work', 'deleted', r.id || '/' || k, now()
		FROM runs r, unnest(ARRAY['volume', 'output']) k WHERE r.id LIKE 'old___'`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, finished_at)
		VALUES ('rart', 't2', '{}', 'succeeded', 1, now() - interval '400 days')`)
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

// An S3 delete that fails leaves an orphan, logged; the claim stands.
func TestReapRetentionS3Failure(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateSucceeded, StateSucceeded, 1, 30)
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
