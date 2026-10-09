package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

func published(id, name, content, description string) proto.ArtifactPublished {
	return proto.ArtifactPublished{ID: id, Description: description, Artifact: proto.Artifact{
		BlobInfo: proto.BlobInfo{BlobID: "blob-" + id, Size: int64(len(content)), SHA256: sha(content)},
		Path:     proto.PublishedPrefix + name, ContentType: "text/markdown",
		FileSize: int64(len(content)), FileSHA256: sha(content)}}
}

// sha is the blob's sha256: in these tests a blob's bytes are its content.
func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// snapshotAAt is snapshotA as ra's placement at epoch reports it, with
// that placement's own blob ids.
func snapshotAAt(epoch int) proto.SnapshotDone {
	sd := snapshotA()
	e := strconv.Itoa(epoch)
	sd.Manifest.SnapshotID, sd.Manifest.Epoch = "snapA"+e, epoch
	sd.Manifest.Volumes[0].BlobID = "bA" + e + "-vol"
	sd.Output.BlobID = "bA" + e + "-out"
	sd.Artifacts[0].BlobID = "bA" + e + "-art"
	return sd
}

// laterPlacements ends ra's epoch 1 and adds its placements at epochs 2
// and 3 on ha.
func laterPlacements(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited' WHERE id = 'pa1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pa2', 't1', 'ra', 'ha', 2, 'stopping'), ('pa3', 't1', 'ra', 'ha', 3, 'stopping')`)
}

func reportPublished(t *testing.T, s *Server, hostID, runID string, epoch int, ap proto.ArtifactPublished) proto.Frame {
	t.Helper()
	return s.handleReport(context.Background(), hostID, proto.Frame{Type: proto.MsgArtifactPublished, ID: 1, RunID: runID, Epoch: epoch,
		Data: proto.Marshal(ap)})
}

func uploadBlob(t *testing.T, s *Server, hostName, blobID, content string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/runner/v1/blobs/"+blobID+"?host="+hostName, bytes.NewReader([]byte(content)))
	req.SetPathValue("id", blobID)
	req.Header.Set("Authorization", "Bearer host-secret")
	rec := httptest.NewRecorder()
	s.wrap(s.serveBlobUpload)(rec, req)
	return rec.Code
}

type listedArtifact struct {
	ID, Path, Description string
	Version               int
}

func listed(t *testing.T, s *Server, key, runID, query string) []listedArtifact {
	t.Helper()
	code, body := callJSON(t, s, key, http.MethodGet, "/v1/runs/"+runID+"/artifacts"+query)
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	var out []listedArtifact
	for _, a := range body["artifacts"].([]any) {
		m := a.(map[string]any)
		out = append(out, listedArtifact{m["id"].(string), m["path"].(string), m["description"].(string), int(m["version"].(float64))})
	}
	return out
}

func publishedEvents(t *testing.T, s *Server, runID string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	var out []map[string]any
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM run_events WHERE run_id = $1 AND type = 'artifact.published' ORDER BY id`, runID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[map[string]any])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A published artifact is recorded under the shim's id, with its
// description; publishing the name again is version 2. The listing has
// each path's latest version, versions=all every one. A report sent again
// is acked and records nothing more; one naming the same id with another
// blob is refused.
func TestPublishedArtifactVersions(t *testing.T) {
	s, _ := reportFixture(t)
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'running' WHERE id = 'ra'`)
	key := apiKey(t, s, new("t1"), "read")
	v1 := published("art_aaaaaaaaaaaaaaaa", "notes.md", "one", "first")
	v2 := published("art_bbbbbbbbbbbbbbbb", "notes.md", "two", "second")
	for _, ap := range []proto.ArtifactPublished{v1, v2, v1} {
		if f := reportPublished(t, s, "ha", "ra", 1, ap); f.Type != proto.MsgAck || ackRefused(t, f) {
			t.Fatalf("report %s: %s %s", ap.ID, f.Type, f.Data)
		}
	}
	got := listed(t, s, key, "ra", "")
	want := []listedArtifact{
		{v2.ID, "/.lux/artifacts/notes.md", "second", 2},
		{"", "/out/a.txt", "", 1},
	}
	if len(got) != 2 {
		t.Fatalf("listed %+v", got)
	}
	got[1].ID = ""
	if !slices.Equal(got, want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
	all := listed(t, s, key, "ra", "?versions=all")
	if len(all) != 3 || all[0].ID != v1.ID || all[0].Version != 1 || all[1].ID != v2.ID || all[1].Version != 2 {
		t.Fatalf("all versions %+v", all)
	}
	forged := v1
	forged.BlobID = "blob-other"
	if f := reportPublished(t, s, "ha", "ra", 1, forged); f.Type != proto.MsgAck || !ackRefused(t, f) {
		t.Fatalf("same id, other blob: %s %s, want a refused ack", f.Type, f.Data)
	}
	// rb's report naming ra's blob is refused too, and stores nothing.
	stolen := published("art_cccccccccccccccc", "x", "one", "")
	stolen.BlobID = v1.BlobID
	if f := reportPublished(t, s, "hb", "rb", 1, stolen); f.Type != proto.MsgAck || !ackRefused(t, f) {
		t.Fatalf("another Run's blob: %s %s, want a refused ack", f.Type, f.Data)
	}
	if n := len(listed(t, s, key, "ra", "?versions=all")); n != 3 {
		t.Fatalf("%d artifacts after refusals", n)
	}
	if got := eventErrors(t, s, "ra", "artifacts.failed"); len(got) != 1 || !strings.Contains(got[0], "notes.md: refused") {
		t.Fatalf("ra's artifacts.failed %q", got)
	}
	if got := eventErrors(t, s, "rb", "artifacts.failed"); len(got) != 1 || !strings.Contains(got[0], "x: refused") {
		t.Fatalf("rb's artifacts.failed %q", got)
	}
	for _, bad := range []proto.ArtifactPublished{published("nope", "x", "1", ""), published("art_dddddddddddddddd", "../x", "1", "")} {
		if f := reportPublished(t, s, "ha", "ra", 1, bad); f.Type != proto.MsgAck || !ackRefused(t, f) {
			t.Fatalf("malformed %+v: %s %s", bad, f.Type, f.Data)
		}
	}
}

// A forged report luxd could not store (a NUL in a quoted content type
// parameter, a NUL in the description, blob id or sha256) is a refused ack with
// artifacts.failed, not a failed report the runner would send forever.
func TestPublishedArtifactUnstorableIsRefused(t *testing.T) {
	s, _ := reportFixture(t)
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'running' WHERE id = 'ra'`)
	ctype := published("art_aaaaaaaaaaaaaaaa", "a.txt", "a", "")
	ctype.ContentType = "text/plain; a=\"x\x00y\""
	desc := published("art_bbbbbbbbbbbbbbbb", "b.txt", "b", "x\x00y")
	blob := published("art_cccccccccccccccc", "c.txt", "c", "")
	blob.BlobID = "blob_\x00"
	sum := published("art_dddddddddddddddd", "d.txt", "d", "")
	sum.FileSHA256 = "\x00"
	for i, ap := range []proto.ArtifactPublished{ctype, desc, blob, sum} {
		f := reportPublished(t, s, "ha", "ra", 1, ap)
		if f.Type != proto.MsgAck || !ackRefused(t, f) {
			t.Fatalf("%s: %s %s, want a refused ack", ap.ID, f.Type, f.Data)
		}
		if got := eventErrors(t, s, "ra", "artifacts.failed"); len(got) != i+1 {
			t.Fatalf("%s: artifacts.failed %q", ap.ID, got)
		}
	}
	if _, ok := blobLocations(t, s, "ra")[desc.BlobID]; ok {
		t.Fatal("a refused artifact's blob was recorded")
	}
}

// artifact.published is added once per artifact, when its blob reaches S3:
// not on the report, not on an upload sent again. A globbed artifact's
// upload adds it too.
func TestArtifactPublishedEventOnUpload(t *testing.T) {
	s, ctx := reportFixture(t)
	fs3 := useFakeS3(t, s)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running' WHERE id = 'ra'`)
	ap := published("art_aaaaaaaaaaaaaaaa", "design/notes.md", "hello", "the design")
	if f := reportPublished(t, s, "ha", "ra", 1, ap); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report: %s %s", f.Type, f.Data)
	}
	if n := len(publishedEvents(t, s, "ra")); n != 0 {
		t.Fatalf("%d events before the upload", n)
	}
	// A retried PUT that arrives while the first is still in S3: both read
	// location 'host'; the second to commit adds no event.
	nested := false
	fs3.onPut = func(string) {
		if !nested {
			nested = true
			if code := uploadBlob(t, s, "ha", ap.BlobID, "hello"); code/100 != 2 {
				t.Errorf("nested upload: %d", code)
			}
		}
	}
	for range 2 {
		if code := uploadBlob(t, s, "ha", ap.BlobID, "hello"); code/100 != 2 {
			t.Fatalf("upload: %d", code)
		}
	}
	fs3.onPut = nil
	evs := publishedEvents(t, s, "ra")
	if len(evs) != 1 {
		t.Fatalf("events %v", evs)
	}
	want := map[string]any{"artifactId": ap.ID, "path": "/.lux/artifacts/design/notes.md", "name": "design/notes.md", "version": float64(1),
		"description": "the design", "size": float64(5), "sha256": ap.FileSHA256, "contentType": "text/markdown"}
	for k, v := range want {
		if evs[0][k] != v {
			t.Errorf("event %s = %v, want %v", k, evs[0][k], v)
		}
	}
	// snapshotA's artifact (/out/a.txt), whose blob is "a-art"'s bytes in
	// this fixture: give it a real sha256 and upload it.
	execSQL(t, s, ctx, `UPDATE blobs SET sha256 = $1 WHERE id = 'bA-art'`, sha("glob"))
	if code := uploadBlob(t, s, "ha", "bA-art", "glob"); code/100 != 2 {
		t.Fatalf("upload: %d", code)
	}
	evs = publishedEvents(t, s, "ra")
	if len(evs) != 2 || evs[1]["path"] != "/out/a.txt" || evs[1]["name"] != "/out/a.txt" {
		t.Fatalf("events %v", evs)
	}
	// A published artifact's upload while the Run runs does not mark the
	// placement uploaded: it has not reported its snapshot.
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('pa2', 't1', 'ra', 'ha', 2, 'running')`)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'ra'`)
	ap2 := published("art_bbbbbbbbbbbbbbbb", "later.md", "later", "")
	if f := reportPublished(t, s, "ha", "ra", 2, ap2); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("report: %s %s", f.Type, f.Data)
	}
	if code := uploadBlob(t, s, "ha", ap2.BlobID, "later"); code/100 != 2 {
		t.Fatalf("upload: %d", code)
	}
	var uploaded bool
	systemScan(t, s, `SELECT uploaded_at IS NOT NULL FROM placements WHERE id = 'pa2'`, nil, &uploaded)
	if uploaded {
		t.Fatal("a running placement marked uploaded")
	}
}

// An exit collection of artifacts.paths whose file is the path's latest
// version records nothing; changed, it is the next version.
func TestCollectedArtifactNotDuplicated(t *testing.T) {
	s, ctx := reportFixture(t)
	key := apiKey(t, s, new("t1"), "read")
	laterPlacements(t, s)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = 'ra'`)
	same := snapshotAAt(2)
	if f := reportSnapshot(t, s, "ha", "ra", 2, same); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("epoch 2: %s %s", f.Type, f.Data)
	}
	// Sent again, it is the same report: not refused, not marked so.
	if f := reportSnapshot(t, s, "ha", "ra", 2, same); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("epoch 2 again: %s %s", f.Type, f.Data)
	}
	if got := eventErrors(t, s, "ra", "snapshot.failed"); len(got) != 0 {
		t.Fatalf("redelivery refused: %q", got)
	}
	var refused bool
	systemScan(t, s, `SELECT snapshot_refused FROM placements WHERE id = 'pa2'`, nil, &refused)
	if refused {
		t.Fatal("redelivery marked the placement snapshot_refused")
	}
	if all := listed(t, s, key, "ra", "?versions=all"); len(all) != 1 || all[0].Version != 1 {
		t.Fatalf("after the same file: %+v", all)
	}
	if _, ok := blobLocations(t, s, "ra")["bA2-art"]; ok {
		t.Fatal("the duplicate's blob was recorded")
	}
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3 WHERE id = 'ra'`)
	changed := snapshotAAt(3)
	changed.Artifacts[0].FileSHA256 = "a-file-changed"
	if f := reportSnapshot(t, s, "ha", "ra", 3, changed); f.Type != proto.MsgAck || ackRefused(t, f) {
		t.Fatalf("epoch 3: %s %s", f.Type, f.Data)
	}
	all := listed(t, s, key, "ra", "?versions=all")
	if len(all) != 2 || all[1].Version != 2 {
		t.Fatalf("after a changed file: %+v", all)
	}
	if latest := listed(t, s, key, "ra", ""); len(latest) != 1 || latest[0].Version != 2 {
		t.Fatalf("latest: %+v", latest)
	}
}

// A collected artifact's path can be up to PATH_MAX, past what a btree
// index entry holds: one of about 3300 bytes is recorded, listed, and is
// not a new version when collected again unchanged.
func TestCollectedArtifactLongPath(t *testing.T) {
	s, ctx := reportFixture(t)
	key := apiKey(t, s, new("t1"), "read")
	// Segments of hex digests: Postgres compresses a repetitive key to fit.
	var long string
	for i := range 103 {
		long += "/" + sha(strconv.Itoa(i))[:31]
	}
	long += "/f.txt"
	laterPlacements(t, s)
	for epoch := 2; epoch <= 3; epoch++ {
		execSQL(t, s, ctx, `UPDATE runs SET current_epoch = $1 WHERE id = 'ra'`, epoch)
		sd := snapshotAAt(epoch)
		sd.Artifacts[0].Path = long
		if f := reportSnapshot(t, s, "ha", "ra", epoch, sd); f.Type != proto.MsgAck || ackRefused(t, f) {
			t.Fatalf("epoch %d: %s %s", epoch, f.Type, f.Data)
		}
	}
	var got []string
	for _, a := range listed(t, s, key, "ra", "?versions=all") {
		got = append(got, fmt.Sprintf("%d %d", len(a.Path), a.Version))
	}
	want := []string{fmt.Sprintf("%d 1", len(long)), "10 1"}
	if len(long) < 3300 || !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
}

// DELETE /v1/runs/{id}/artifacts deletes every version.
func TestDeleteArtifactsEveryVersion(t *testing.T) {
	s, ctx := reportFixture(t)
	useFakeS3(t, s)
	for _, ap := range []proto.ArtifactPublished{published("art_aaaaaaaaaaaaaaaa", "n.md", "1", ""), published("art_bbbbbbbbbbbbbbbb", "n.md", "2", "")} {
		if f := reportPublished(t, s, "ha", "ra", 1, ap); f.Type != proto.MsgAck || ackRefused(t, f) {
			t.Fatalf("report: %s %s", f.Type, f.Data)
		}
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'terminated' WHERE id = 'ra'`)
	key := apiKey(t, s, new("t1"), "run", "read")
	if code, body := callJSON(t, s, key, http.MethodDelete, "/v1/runs/ra/artifacts"); code != http.StatusOK || body["deleted"] != float64(3) {
		t.Fatalf("delete: %d %v, want 3", code, body)
	}
	for id, loc := range blobLocations(t, s, "ra") {
		if (id == "blob-art_aaaaaaaaaaaaaaaa" || id == "blob-art_bbbbbbbbbbbbbbbb" || id == "bA-art") && loc != "deleted" {
			t.Errorf("%s: %s", id, loc)
		}
	}
}
