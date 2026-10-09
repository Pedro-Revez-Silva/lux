package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// stagePublished writes what `lux-shim publish` leaves on the runtime
// volume for content: the staged copy, and its lux.artifact record
// appended to the placement's output file.
func stagePublished(t *testing.T, f *finishFixture, name, content string) proto.StagedArtifact {
	t.Helper()
	rt := filepath.Join(f.r.cfg.DataDir, "rt")
	id := ids.New(ids.Artifact)
	if err := os.MkdirAll(filepath.Join(rt, stagingDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt, stagingDir, id), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	a := proto.StagedArtifact{ID: id, Name: name, Description: "about " + name, ContentType: "text/plain",
		Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), File: stagingDir + "/" + id}
	appendEvent(t, f, proto.EvArtifact, a)
	return a
}

// storeStaged stores a's staged copy as onPublished would, without
// reporting it.
func storeStaged(t *testing.T, f *finishFixture, a proto.StagedArtifact) *snapshotRecord {
	t.Helper()
	rt, err := os.OpenRoot(filepath.Join(f.r.cfg.DataDir, "rt"))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rec, err := f.p.storePublished(rt, a.File, a)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func appendEvent(t *testing.T, f *finishFixture, typ string, data any) {
	t.Helper()
	path := filepath.Join(f.r.cfg.DataDir, "rt", proto.OutputFile(1))
	ev, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	seq := int64(1)
	if b, err := os.ReadFile(path); err == nil {
		seq += int64(bytes.Count(b, []byte("\n")))
	}
	line, _ := json.Marshal(proto.Record{Seq: seq, Ch: "event", Event: ev})
	out, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// tailToEnd reads the placement's whole output file, as tailEvents does
// once its container has exited, and returns when every record is handled.
func tailToEnd(f *finishFixture) {
	exited := make(chan struct{})
	close(exited)
	f.p.tailEvents(context.Background(), exited)
}

func (f *finishFixture) published() []proto.ArtifactPublished {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proto.ArtifactPublished
	for _, fr := range f.reports {
		if fr.Type == proto.MsgArtifactPublished {
			var a proto.ArtifactPublished
			_ = json.Unmarshal(fr.Data, &a)
			out = append(out, a)
		}
	}
	return out
}

func blobContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := zr.DecodeAll(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A published artifact is stored as a blob, reported once under the
// shim's id with its description, uploaded after the ack, and its staged
// copy removed. The same record read again, by this placement or by one
// re-adopted after a runner restart, reports and stores nothing more.
func TestPublishedArtifactIsReportedOnce(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	a := stagePublished(t, f, "design/notes.md", "# notes\n")
	appendEvent(t, f, proto.EvArtifact, a) // the record twice in one file
	tailToEnd(f)

	got := f.published()
	if len(got) != 1 {
		t.Fatalf("reports %+v", got)
	}
	rep := got[0]
	if rep.ID != a.ID || rep.Description != a.Description || rep.Path != "/.lux/artifacts/design/notes.md" ||
		rep.FileSize != a.Size || rep.FileSHA256 != a.SHA256 || rep.ContentType != "text/plain" {
		t.Fatalf("report %+v for %+v", rep, a)
	}
	if s := blobContent(t, f.r.blobPath(rep.BlobID)); s != "# notes\n" {
		t.Fatalf("blob %q", s)
	}
	if _, err := os.Stat(filepath.Join(f.r.cfg.DataDir, "rt", a.File)); !os.IsNotExist(err) {
		t.Fatalf("staged copy kept: %v", err)
	}
	f.r.uploads.pass(context.Background())
	if !slices.Equal(f.uploaded, []string{rep.BlobID}) {
		t.Fatalf("uploaded %v", f.uploaded)
	}

	// The runner restarts: a new placement value re-reads the output file.
	f.p = &placement{r: f.r, runID: "run1", tenantID: "t1", epoch: 1, dir: f.p.dir, state: f.p.state, assign: f.p.assign}
	tailToEnd(f)
	if n := len(f.published()); n != 1 {
		t.Fatalf("%d reports after re-reading the output", n)
	}
	entries, _ := os.ReadDir(filepath.Join(f.r.cfg.DataDir, "snapshots"))
	var blobs int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".zst" {
			blobs++
		}
	}
	if blobs != 1 {
		t.Fatalf("%d blob files", blobs)
	}
}

// A runner that stored an artifact but restarted before luxd acked its
// report reports it again, from its record (luxd treats it as the same
// artifact): the staged copy is gone by then, and the blob is the same.
func TestPublishedArtifactUnackedIsReportedAgain(t *testing.T) {
	f := newFinishFixture(t)
	a := stagePublished(t, f, "x.txt", "x")
	rec := storeStaged(t, f, a)
	_ = os.Remove(filepath.Join(f.r.cfg.DataDir, "rt", a.File))
	tailToEnd(f)
	got := f.published()
	if len(got) != 1 || got[0].ID != a.ID || got[0].BlobID != rec.Published.BlobID {
		t.Fatalf("reports %+v, record %+v", got, rec.Published)
	}
	if rec, _ := f.r.readRecord(a.ID); rec == nil || !rec.Reported {
		t.Fatalf("record after the ack: %+v", rec)
	}
}

// Past maxPublished, a publish is dropped (its staged copy removed) and
// an artifacts.failed event says so, once.
func TestPublishedArtifactsAreCapped(t *testing.T) {
	f := newFinishFixture(t)
	f.p.published = map[string]bool{}
	for i := range maxPublished {
		f.p.published[ids.New(ids.Artifact)+string(rune('a'+i%26))] = true
	}
	a := stagePublished(t, f, "over.txt", "x")
	appendEvent(t, f, proto.EvArtifact, a)
	tailToEnd(f)
	if got := f.published(); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.r.cfg.DataDir, "rt", a.File)); !os.IsNotExist(err) {
		t.Fatalf("staged copy kept: %v", err)
	}
	waitFor(t, "artifacts.failed", func() bool { return slices.Contains(f.types(), proto.MsgRunEvent) })
	if n := slices.Index(f.types(), proto.MsgRunEvent); slices.Contains(f.types()[n+1:], proto.MsgRunEvent) {
		t.Fatalf("reports %v", f.types())
	}
}

// A record whose id is not one the shim mints, or whose name leaves the
// staging directory, touches nothing: a root workload can write records.
func TestForgedPublishRecordsAreIgnored(t *testing.T) {
	f := newFinishFixture(t)
	rt := filepath.Join(f.r.cfg.DataDir, "rt")
	if err := os.WriteFile(filepath.Join(rt, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What art_/../x resolves to once joined: staged and well-formed but
	// for its id, so only the id check stops it.
	if err := os.MkdirAll(filepath.Join(rt, stagingDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt, stagingDir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, a := range []proto.StagedArtifact{
		{ID: "../config.json", Name: "x", Size: 2},
		{ID: "art_aaaaaaaaaaaaaaa/", Name: "x"},
		{ID: ids.New(ids.Artifact), Name: "../../etc/passwd"},
		{ID: "art_/../x", Name: "x", Size: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", File: "artifacts/x"},
	} {
		appendEvent(t, f, proto.EvArtifact, a)
	}
	tailToEnd(f)
	if got := f.published(); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if _, err := os.Stat(filepath.Join(rt, "config.json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(f.r.cfg.DataDir, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext == ".zst" || ext == ".json" {
			t.Errorf("snapshots/%s written", e.Name())
		}
	}
}

// Run B's record naming Run A's published artifact (a forged id; records
// are host-wide) leaves A's record and blob as they were, and reports
// nothing.
func TestPublishedRecordOfAnotherRunIsIgnored(t *testing.T) {
	f := newFinishFixture(t)
	a := stagePublished(t, f, "x.txt", "x")
	f.p.runID = "runA"
	recA := storeStaged(t, f, a)
	f.p.runID = "run1"
	tailToEnd(f)
	if got := f.published(); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	if rec, err := f.r.readRecord(a.ID); err != nil || rec.RunID != "runA" || rec.Reported {
		t.Fatalf("A's record: %+v %v", rec, err)
	}
	if s := blobContent(t, recA.Uploads[0].Path); s != "x" {
		t.Fatalf("A's blob %q", s)
	}
}

// The uploader takes snapshots' blobs before published artifacts' (a
// resume elsewhere waits for the snapshot), each group oldest first.
func TestUploaderTakesSnapshotsFirst(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	for _, c := range []struct {
		id        string
		created   int64
		published bool
	}{
		{"pub-b", 1, true}, {"snap-c", 4, false}, {"pub-a", 0, true},
		{"snap-a", 6, false}, {"pub-c", 2, true}, {"snap-b", 5, false},
	} {
		blob := f.r.blobPath(c.id)
		if err := os.WriteFile(blob, []byte(c.id), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := &snapshotRecord{RunID: "run1", Epoch: 1, Created: c.created, Reported: true,
			Uploads: []pendingUpload{{BlobID: c.id, Path: blob, Size: int64(len(c.id))}}}
		if c.published {
			rec.Published = &proto.ArtifactPublished{ID: c.id}
		}
		if err := f.r.saveSnapshotRecord(c.id, rec); err != nil {
			t.Fatal(err)
		}
	}
	f.r.uploads.pass(context.Background())
	if want := []string{"snap-c", "snap-b", "snap-a", "pub-a", "pub-b", "pub-c"}; !slices.Equal(f.uploaded, want) {
		t.Fatalf("uploaded %v, want %v", f.uploaded, want)
	}
}

// One pass uploads only what is pending for a live placement: a fenced-off
// placement's blob is not uploaded, a done record is kept, a discarded done
// record is removed. Stale is per Run: runA's fence does not touch runB.
func TestUploaderPassSkipsOnlyWhatIsDoneOrStale(t *testing.T) {
	f := newFinishFixture(t)
	f.serveUploads(t)
	if err := writeRunState(f.r.runDir("runA"), &runState{RunID: "runA", Epoch: 1, Stale: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, run       string
		done, discard bool
	}{
		{"a-stale", "runA", false, false}, {"b-pending", "runB", false, false},
		{"c-done", "runB", true, false}, {"d-discarded", "runB", true, true},
	} {
		blob := f.r.blobPath(c.id)
		if err := os.WriteFile(blob, []byte(c.id), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.r.saveSnapshotRecord(c.id, &snapshotRecord{RunID: c.run, Epoch: 1, Reported: true, Discard: c.discard,
			Uploads: []pendingUpload{{BlobID: c.id, Path: blob, Size: int64(len(c.id)), Done: c.done}}}); err != nil {
			t.Fatal(err)
		}
	}
	f.r.uploads.pass(context.Background())
	if want := []string{"b-pending"}; !slices.Equal(f.uploaded, want) {
		t.Fatalf("uploaded %v, want %v", f.uploaded, want)
	}
	if _, err := os.Stat(f.r.recordPath("d-discarded")); !os.IsNotExist(err) {
		t.Fatalf("discarded done record kept: %v", err)
	}
	if _, err := os.Stat(f.r.recordPath("c-done")); err != nil {
		t.Fatalf("done record removed: %v", err)
	}
}

// A staged copy that is not what its record says (rewritten by a root
// workload after the publish) is not reported: one artifacts.failed, the
// staged copy removed, no blob or record kept.
func TestPublishedArtifactNotAsRecorded(t *testing.T) {
	f := newFinishFixture(t)
	a := stagePublished(t, f, "x.txt", "x")
	rt := filepath.Join(f.r.cfg.DataDir, "rt")
	if err := os.WriteFile(filepath.Join(rt, a.File), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	tailToEnd(f)
	if got := f.published(); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	waitFor(t, "artifacts.failed", func() bool { return slices.Contains(f.types(), proto.MsgRunEvent) })
	var events []proto.RunEvent
	f.mu.Lock()
	for _, fr := range f.reports {
		if fr.Type == proto.MsgRunEvent {
			var ev proto.RunEvent
			_ = json.Unmarshal(fr.Data, &ev)
			events = append(events, ev)
		}
	}
	f.mu.Unlock()
	if len(events) != 1 || events[0].Type != "artifacts.failed" {
		t.Fatalf("events %+v", events)
	}
	if _, err := os.Stat(filepath.Join(rt, a.File)); !os.IsNotExist(err) {
		t.Fatalf("staged copy kept: %v", err)
	}
	if _, err := os.Stat(f.r.blobPath(publishedBlobID(a.ID))); !os.IsNotExist(err) {
		t.Fatalf("blob kept: %v", err)
	}
	if _, err := f.r.readRecord(a.ID); !os.IsNotExist(err) {
		t.Fatalf("record kept: %v", err)
	}
}

// A refused report deletes the artifact's blob and record: luxd never asks
// for it.
func TestPublishedArtifactRefused(t *testing.T) {
	f := newFinishFixture(t)
	a := stagePublished(t, f, "x.txt", "x")
	rec := storeStaged(t, f, a)
	f.r.publishedAcked(a.ID, proto.Ack{Refused: true})
	if _, err := os.Stat(rec.Uploads[0].Path); !os.IsNotExist(err) {
		t.Fatalf("blob kept: %v", err)
	}
	if _, err := f.r.readRecord(a.ID); !os.IsNotExist(err) {
		t.Fatalf("record kept: %v", err)
	}
}
