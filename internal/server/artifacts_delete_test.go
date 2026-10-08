package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/blob"
)

// callJSON is call with no request body and the response decoded.
func callJSON(t *testing.T, s *Server, key, method, path string) (int, map[string]any) {
	t.Helper()
	code, raw := call(t, s, key, method, path, nil)
	var body map[string]any
	_ = json.Unmarshal([]byte(raw), &body)
	return code, body
}

func artifactID(t *testing.T, s *Server, blobID string) string {
	t.Helper()
	var id string
	systemScan(t, s, `SELECT id FROM artifacts WHERE blob_id = $1`, []any{blobID}, &id)
	return id
}

// DELETE /v1/runs/{id}/artifacts deletes only that Run's artifacts (rows
// marked, S3 objects deleted, listed unavailable, downloads 410), even
// after retention deleted the rest; again, it is a no-op. A resumable or
// live Run is 409; another tenant's Run is 404.
func TestDeleteArtifacts(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 400)
	if err := s.reapRetention(ctx); err != nil {
		t.Fatal(err)
	}
	t1, t2 := apiKey(t, s, new("t1"), "run", "read"), apiKey(t, s, new("t2"), "run", "read")
	// Another terminated Run of rb's tenant, with an artifact of its own.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, finished_at) VALUES ('rc', 't2', '{}', 'terminated', 1, now())`)
	execSQL(t, s, ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, location, s3_key) VALUES ('bC-art', 't2', 'rc', 1, 'artifact', '/c', 's3', 'rc/bC-art')`)
	execSQL(t, s, ctx, `INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id) VALUES ('aC', 't2', 'rc', 1, '/c', 'bC-art')`)
	before := len(f.Deleted())

	if code, _ := callJSON(t, s, t1, http.MethodDelete, "/v1/runs/rb/artifacts"); code != http.StatusNotFound {
		t.Fatalf("another tenant's Run: %d, want 404", code)
	}
	code, body := callJSON(t, s, t2, http.MethodDelete, "/v1/runs/rb/artifacts")
	if code != http.StatusOK || body["deleted"] != float64(2) {
		t.Fatalf("delete: %d %v, want 200 deleted 2", code, body)
	}
	got := f.Deleted()
	if added := len(got) - before; added != 2 || !slices.Contains(got, "rb/bB-art-snapB") || !slices.Contains(got, "rb/bB-art-snapB2") {
		t.Fatalf("S3 deletes %v", got)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "s3" {
		t.Fatalf("ra's artifact: %s, want s3", loc)
	}
	if loc := blobLocations(t, s, "rc")["bC-art"]; loc != "s3" {
		t.Fatalf("rc's artifact (same tenant): %s, want s3", loc)
	}
	if code, _ := callJSON(t, s, t2, http.MethodGet, "/v1/artifacts/"+artifactID(t, s, "bB-art-snapB")); code != http.StatusGone {
		t.Fatalf("download after delete: %d, want 410", code)
	}
	_, list := callJSON(t, s, t2, http.MethodGet, "/v1/runs/rb/artifacts")
	for _, a := range list["artifacts"].([]any) {
		if a.(map[string]any)["available"] != false {
			t.Fatalf("listed available after delete: %v", a)
		}
	}
	// Idempotent.
	code, body = callJSON(t, s, t2, http.MethodDelete, "/v1/runs/rb/artifacts")
	if code != http.StatusOK || body["deleted"] != float64(0) || len(f.Deleted()) != before+2 {
		t.Fatalf("again: %d %v, deletes %v", code, body, f.Deleted())
	}
	// Resumable (a succeeded one included) and live Runs keep theirs.
	for _, state := range []string{StateStopped, StateLost, StateFailed, StateSucceeded, StateRunning, StateResuming} {
		execSQL(t, s, ctx, `UPDATE runs SET state = $1 WHERE id = 'ra'`, state)
		if code, _ := callJSON(t, s, t1, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusConflict {
			t.Fatalf("%s Run: %d, want 409", state, code)
		}
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "s3" {
		t.Fatalf("ra's artifact after refusals: %s", loc)
	}
}

// An artifact still on its host is deleted too; when its upload arrives,
// the object is dropped and the blob stays deleted.
func TestDeleteArtifactsBeforeUpload(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host', s3_key = NULL WHERE id = 'bA-art'`)
	key := apiKey(t, s, new("t1"), "run")
	if code, body := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("delete: %d %v", code, body)
	}
	if got := f.Deleted(); len(got) != 0 {
		t.Fatalf("S3 deletes for a blob never uploaded: %v", got)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "deleted" {
		t.Fatalf("bA-art: %s", loc)
	}
}

// An artifact deleted while its upload is under way: the upload answers
// 410, its S3 object is deleted, and the blob stays deleted.
func TestDeleteArtifactsDuringUpload(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	body := []byte("artifact bytes")
	sum := sha256.Sum256(body)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host', s3_key = NULL, sha256 = $1 WHERE id = 'bA-art'`, hex.EncodeToString(sum[:]))
	key := apiKey(t, s, new("t1"), "run")
	f.onPut = func(string) {
		if code, _ := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK {
			t.Errorf("delete during upload: %d", code)
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/runner/v1/blobs/bA-art?host=ha", bytes.NewReader(body))
	req.SetPathValue("id", "bA-art")
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.wrap(s.serveBlobUpload)(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("upload: %d %s, want 410", rec.Code, rec.Body)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "deleted" {
		t.Fatalf("bA-art: %s, want deleted", loc)
	}
	if got := f.Deleted(); !slices.Equal(got, []string{blob.Key("t1", "ra", "bA-art")}) {
		t.Fatalf("S3 deletes %v, want the uploaded object", got)
	}
}

// An artifact whose upload committed before the delete: the delete claims
// it and deletes the uploaded object.
func TestDeleteArtifactsAfterUpload(t *testing.T) {
	s, ctx, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	body := []byte("artifact bytes")
	sum := sha256.Sum256(body)
	execSQL(t, s, ctx, `UPDATE blobs SET location = 'host', s3_key = NULL, sha256 = $1 WHERE id = 'bA-art'`, hex.EncodeToString(sum[:]))
	req := httptest.NewRequest(http.MethodPut, "/runner/v1/blobs/bA-art?host=ha", bytes.NewReader(body))
	req.SetPathValue("id", "bA-art")
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.wrap(s.serveBlobUpload)(rec, req)
	if rec.Code/100 != 2 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "s3" {
		t.Fatalf("bA-art after upload: %s, want s3", loc)
	}
	key := apiKey(t, s, new("t1"), "run")
	if code, body := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("delete: %d %v", code, body)
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "deleted" {
		t.Fatalf("bA-art: %s, want deleted", loc)
	}
	if got := f.Deleted(); !slices.Equal(got, []string{blob.Key("t1", "ra", "bA-art")}) {
		t.Fatalf("S3 deletes %v, want the uploaded object", got)
	}
}

// An S3 delete that fails leaves an orphan, logged with its key; the claim
// stands: the blob stays deleted and the request succeeds.
func TestDeleteArtifactsS3Failure(t *testing.T) {
	s, _, f := retentionFixture(t, StateTerminated, StateTerminated, 1, 1)
	log := captureLog(s)
	f.failPrefix = "ra/"
	key := apiKey(t, s, new("t1"), "run")
	if code, body := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("delete: %d %v", code, body)
	}
	if !strings.Contains(log.String(), "object orphaned") || !strings.Contains(log.String(), "ra/bA-art") {
		t.Fatalf("no orphan logged: %s", log.String())
	}
	if loc := blobLocations(t, s, "ra")["bA-art"]; loc != "deleted" {
		t.Fatalf("bA-art: %s, want deleted (the claim stands)", loc)
	}
	if len(f.Deleted()) != 0 {
		t.Fatalf("S3 deletes %v", f.Deleted())
	}
	code, body := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts")
	if code != http.StatusOK || body["deleted"] != float64(0) {
		t.Fatalf("again: %d %v, want 200 deleted 0", code, body)
	}
}
