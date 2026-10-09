package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

type publishFixture struct {
	dir    string
	socket string
	mu     sync.Mutex
	events []proto.StagedArtifact
	fail   error
}

// newPublishFixture serves a publisher with a cap of max bytes on a socket
// in a temp dir, recording what it emits.
func newPublishFixture(t *testing.T, max int64) *publishFixture {
	t.Helper()
	base := t.TempDir()
	f := &publishFixture{dir: filepath.Join(base, "artifacts"), socket: filepath.Join(base, "publish.sock")}
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := &publisher{base: base, rel: "artifacts", max: max, emit: func(a proto.StagedArtifact) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail != nil {
			return f.fail
		}
		// The staged file is complete before its record is written.
		if b, err := os.ReadFile(filepath.Join(base, a.File)); err != nil || int64(len(b)) != a.Size {
			return errors.New("record written before the staged file was complete")
		}
		f.events = append(f.events, a)
		return nil
	}}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go p.accept(ln)
	return f
}

func (f *publishFixture) staged(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.md")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A publish stages the bytes under the id it answers with, and its record
// carries the name, description, size and sha256; the source can change
// straight after without touching the copy.
func TestPublishStagesACopy(t *testing.T) {
	f := newPublishFixture(t, 1<<20)
	content := []byte("# notes\nhello\n")
	src := writeTemp(t, content)
	rep, err := publishFile(f.socket, src, proto.PublishRequest{Name: "design/notes.md", Description: "the design"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if rep.ID == "" || !strings.HasPrefix(rep.ID, "art_") || rep.Name != "design/notes.md" || rep.Size != int64(len(content)) || rep.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("reply %+v", rep)
	}
	if len(f.events) != 1 {
		t.Fatalf("events %+v", f.events)
	}
	ev := f.events[0]
	want := proto.StagedArtifact{ID: rep.ID, Name: "design/notes.md", Description: "the design", ContentType: "text/markdown; charset=utf-8",
		Size: int64(len(content)), SHA256: rep.SHA256, File: "artifacts/" + rep.ID}
	if ev != want {
		t.Fatalf("event %+v, want %+v", ev, want)
	}
	if err := os.WriteFile(src, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.dir, rep.ID)); !bytes.Equal(got, content) {
		t.Fatalf("staged %q", got)
	}
	if names := f.staged(t); len(names) != 1 || names[0] != rep.ID {
		t.Fatalf("staging dir %v", names)
	}
}

// Refused publishes stage nothing and record nothing: over the cap (not
// truncated), a name that leaves the directory, a body shorter or longer
// than declared.
func TestPublishRefusals(t *testing.T) {
	f := newPublishFixture(t, 10)
	dial := func() *net.UnixConn {
		c, err := net.Dial("unix", f.socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.(*net.UnixConn)
	}
	for _, c := range []struct {
		name    string
		req     proto.PublishRequest
		body    string
		wantErr string
	}{
		{"over the cap", proto.PublishRequest{Name: "big", Size: 11}, "01234567890", "over the 10-byte limit"},
		{"dot-dot", proto.PublishRequest{Name: "../x", Size: 1}, "x", ".. segment"},
		{"absolute", proto.PublishRequest{Name: "/etc/x", Size: 1}, "x", "relative"},
		{"short body", proto.PublishRequest{Name: "short", Size: 8}, "abc", "short read: 3 of 8"},
		{"long body", proto.PublishRequest{Name: "long", Size: 3}, "abcdef", "more than the declared"},
		{"bad content type", proto.PublishRequest{Name: "x", ContentType: "nope nope", Size: 1}, "x", "content type"},
		{"NUL in a content type parameter", proto.PublishRequest{Name: "x", ContentType: "text/plain; a=\"x\x00y\"", Size: 1}, "x", "control character"},
		{"NUL in the description", proto.PublishRequest{Name: "x", Description: "a\x00b", Size: 1}, "x", "control character"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := dial()
			hdr, _ := json.Marshal(c.req)
			_, _ = conn.Write(append(hdr, '\n'))
			_, _ = conn.Write([]byte(c.body))
			_ = conn.CloseWrite()
			var rep proto.PublishReply
			if err := json.NewDecoder(conn).Decode(&rep); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(rep.Error, c.wantErr) || rep.ID != "" {
				t.Fatalf("reply %+v, want error with %q", rep, c.wantErr)
			}
		})
	}
	if len(f.events) != 0 || len(f.staged(t)) != 0 {
		t.Fatalf("events %v, staged %v", f.events, f.staged(t))
	}
}

// The client reports the shim's refusal of a file over the cap.
func TestPublishClientOverTheCap(t *testing.T) {
	f := newPublishFixture(t, 4)
	_, err := publishFile(f.socket, writeTemp(t, []byte("too long")), proto.PublishRequest{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "over the 4-byte limit") {
		t.Fatalf("err %v", err)
	}
}

// A record that cannot be written (the shim is ending) fails the publish
// and leaves nothing staged.
func TestPublishWithoutItsRecordFails(t *testing.T) {
	f := newPublishFixture(t, 1<<20)
	f.fail = errOutputClosed
	_, err := publishFile(f.socket, writeTemp(t, []byte("x")), proto.PublishRequest{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "ending") {
		t.Fatalf("err %v", err)
	}
	if names := f.staged(t); len(names) != 0 {
		t.Fatalf("staged %v", names)
	}
}

// A link in place of the staging directory that leads off the runtime
// volume fails the publish; nothing is written where it points.
func TestPublishRefusesALinkedStagingDir(t *testing.T) {
	f := newPublishFixture(t, 1<<20)
	elsewhere := t.TempDir()
	if err := os.Remove(f.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, f.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := publishFile(f.socket, writeTemp(t, []byte("x")), proto.PublishRequest{Name: "x"}); err == nil {
		t.Fatal("published through a link off the volume")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 || len(f.events) != 0 {
		t.Fatalf("written %v, events %v", entries, f.events)
	}
}

// A staging directory off the runtime volume is refused before anything
// is done to it: not emptied, no socket opened.
func TestStartPublishOffTheVolume(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Shim{cfg: proto.ShimConfig{ArtifactsDir: dir}}
	if err := s.startPublish(); err == nil || !strings.Contains(err.Error(), "not on the runtime volume") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("staging dir emptied: %v", err)
	}
}

func TestParsePublishArgs(t *testing.T) {
	for _, c := range []struct {
		args    []string
		file    string
		req     proto.PublishRequest
		wantErr bool
	}{
		{[]string{"/tmp/out/report.md"}, "/tmp/out/report.md", proto.PublishRequest{Name: "report.md"}, false},
		{[]string{"f", "--name", "a/b.md", "--description", "d", "--content-type", "text/plain"}, "f",
			proto.PublishRequest{Name: "a/b.md", Description: "d", ContentType: "text/plain"}, false},
		{[]string{"--name=x", "f"}, "f", proto.PublishRequest{Name: "x"}, false},
		{[]string{"--", "-f"}, "-f", proto.PublishRequest{Name: "-f"}, false},
		{[]string{}, "", proto.PublishRequest{}, true},
		{[]string{"a", "b"}, "", proto.PublishRequest{}, true},
		{[]string{"a", "--name"}, "", proto.PublishRequest{}, true},
		{[]string{"a", "--bogus", "x"}, "", proto.PublishRequest{}, true},
	} {
		file, req, err := parsePublishArgs(c.args)
		if (err != nil) != c.wantErr || (!c.wantErr && (file != c.file || req != c.req)) {
			t.Errorf("parsePublishArgs(%q) = %q %+v %v", c.args, file, req, err)
		}
	}
}
