package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// ackRunner is a Runner with a data dir, polling transport and an API
// server that records the uploads it receives.
func ackRunner(t *testing.T) (*Runner, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	uploads := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uploads = append(uploads, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(srv.Close)
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.api = newAPI(srv.URL, "tok", "host")
	r.conn = newConn(r)
	r.conn.polling = true
	r.uploads = newUploader(r)
	return r, &uploads
}

// reportWithReply sends a snapshot.done through the connection and answers
// it with reply, as luxd's poll response would.
func reportWithReply(t *testing.T, r *Runner, reply proto.Frame) proto.Ack {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		ack proto.Ack
		err error
	}
	done := make(chan result, 1)
	go func() {
		ack, err := r.conn.ReportAck(ctx, proto.Frame{Type: proto.MsgSnapshotDone, RunID: "run1", Epoch: 1})
		done <- result{ack, err}
	}()
	var id int64
	for id == 0 {
		r.conn.mu.Lock()
		if len(r.conn.pollReports) > 0 {
			id = r.conn.pollReports[0].ID
		}
		r.conn.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	reply.ID = id
	r.conn.dispatch(ctx, reply)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	return res.ack
}

// A snapshot whose report luxd refused is never uploaded, and its record
// and blob files are deleted; an accepted one (also from a luxd whose ack
// carries no data) is uploaded.
func TestSnapshotAckRefusedIsNotUploaded(t *testing.T) {
	for _, c := range []struct {
		name       string
		reply      proto.Frame
		wantUpload bool
	}{
		{"refused", proto.Frame{Type: proto.MsgAck, Data: proto.Marshal(proto.Ack{Refused: true})}, false},
		{"accepted", proto.Frame{Type: proto.MsgAck, Data: proto.Marshal(proto.Ack{})}, true},
		{"accepted, no data", proto.Frame{Type: proto.MsgAck}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, uploads := ackRunner(t)
			blob := r.blobPath("blob1")
			if err := os.WriteFile(blob, []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := r.saveSnapshotRecord("snap1", &snapshotRecord{RunID: "run1", Epoch: 1,
				Uploads: []pendingUpload{{BlobID: "blob1", Path: blob, Size: 4}}}); err != nil {
				t.Fatal(err)
			}
			ack := reportWithReply(t, r, c.reply)
			r.snapshotAcked("snap1", ack)
			r.uploads.pass(context.Background())

			if got := len(*uploads) == 1; got != c.wantUpload {
				t.Errorf("uploads %q, want uploaded: %v", *uploads, c.wantUpload)
			}
			_, blobErr := os.Stat(blob)
			_, recErr := os.Stat(r.recordPath("snap1"))
			if c.wantUpload {
				if blobErr != nil || recErr != nil {
					t.Errorf("accepted snapshot's files: blob %v, record %v", blobErr, recErr)
				}
			} else if !os.IsNotExist(blobErr) || !os.IsNotExist(recErr) {
				t.Errorf("refused snapshot's files kept: blob %v, record %v", blobErr, recErr)
			}
		})
	}
}

// A restored volume blob must have the size and sha256 the assignment
// carries, each checked when given.
func TestImportBlobChecksSizeAndSHA256(t *testing.T) {
	r, _ := ackRunner(t)
	size, sum, err := r.writeBlob("vol", func(w io.Writer) error { _, err := w.Write([]byte("volume contents")); return err })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		snap    proto.VolumeSnapshot
		wantErr bool
	}{
		{"matching", proto.VolumeSnapshot{Size: size, SHA256: sum}, false},
		{"other size", proto.VolumeSnapshot{Size: size + 1, SHA256: sum}, true},
		{"other size, no sha256", proto.VolumeSnapshot{Size: size + 1}, true},
		{"other sha256", proto.VolumeSnapshot{Size: size, SHA256: "0000"}, true},
		{"unchecked", proto.VolumeSnapshot{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := os.Open(r.blobPath("vol"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var got []byte
			err = importBlob(f, &c.snap, func(rd io.Reader) error {
				got, err = io.ReadAll(rd)
				return err
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v, want error: %v", err, c.wantErr)
			}
			if string(got) != "volume contents" {
				t.Fatalf("imported %q", got)
			}
		})
	}
}
