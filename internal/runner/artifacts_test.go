package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"/workspace/out/**", "/workspace/out/a.txt", true},
		{"/workspace/out/**", "/workspace/out/x/y/z.bin", true},
		{"/workspace/out/**", "/workspace/other/a.txt", false},
		{"/workspace/*.log", "/workspace/run.log", true},
		{"/workspace/*.log", "/workspace/sub/run.log", false},
		{"/workspace/**/*.xml", "/workspace/a/b/report.xml", true},
		{"/workspace/**/*.xml", "/workspace/report.xml", true},
		{"/workspace/report.xml", "/workspace/report.xml", true},
	} {
		if got := globMatch(splitPath(c.pattern), splitPath(c.name)); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlobPrefix(t *testing.T) {
	for _, c := range []struct {
		pattern, dir string
		want         bool
	}{
		{"/workspace/out/*.bin", "/workspace", true},
		{"/workspace/out/*.bin", "/workspace/out", true},
		{"/workspace/out/*.bin", "/workspace/node_modules", false},
		{"/workspace/out/*.bin", "/workspace/out/deeper", false},
		{"/workspace/**/*.xml", "/workspace/a/b/c", true},
		{"/workspace/*/reports/*", "/workspace/app", true},
		{"/workspace/*/reports/*", "/workspace/app/src", false},
	} {
		if got := globPrefix(splitPath(c.pattern), splitPath(c.dir)); got != c.want {
			t.Errorf("globPrefix(%q, %q) = %v, want %v", c.pattern, c.dir, got, c.want)
		}
	}
}

// boundedFIFO runs fn, failing the test if it has not returned within
// 5 s. On timeout it opens fifo read-write (never blocks on Linux), which
// releases an open blocked on it, so fn's goroutine ends with the test.
func boundedFIFO(t *testing.T, fifo string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Retried: a reader blocked on a later open of the FIFO
		// (tailRecords polls) needs another writer.
		for {
			if w, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
				w.Close()
			}
			select {
			case <-done:
				t.Fatal("blocked on a FIFO")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A FIFO swapped in for a matched artifact after the walk saw a regular
// file is refused at once; the other artifacts are still collected with
// their content and hash unchanged.
func TestCollectArtifactsRefusesFIFO(t *testing.T) {
	f := newFinishFixture(t)
	f.p.assign.Spec.Artifacts.Paths = []string{"/data/out/*"}
	out := filepath.Join(f.data, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"a.txt": "alpha\n", "b.txt": "bravo\n"}
	for name, content := range files {
		writeFile(t, filepath.Join(out, name), content)
	}
	first, err := f.p.collectArtifacts(context.Background())
	if err != nil || len(first) != 2 {
		t.Fatalf("first collection: %+v %v", first, err)
	}
	for _, a := range first {
		want := files[filepath.Base(a.Path)]
		if a.FileSize != int64(len(want)) || a.FileSHA256 != sha256Hex(want) {
			t.Fatalf("artifact %+v, want %q", a, want)
		}
		if s := blobContent(t, f.r.blobPath(a.BlobID)); s != want {
			t.Fatalf("%s: blob %q, want %q", a.Path, s, want)
		}
	}

	fifo := filepath.Join(out, "b.txt")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// The whole collection: the walk sees the FIFO and leaves it.
	var arts []proto.Artifact
	boundedFIFO(t, fifo, func() { arts, err = f.p.collectArtifacts(context.Background()) })
	if err != nil || len(arts) != 1 || arts[0].Path != "/data/out/a.txt" || arts[0].FileSHA256 != sha256Hex(files["a.txt"]) {
		t.Fatalf("collection with a FIFO: %+v %v", arts, err)
	}
	// The swap after the walk: the open itself meets the FIFO.
	root, err := os.OpenRoot(f.data)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	c := &collector{p: f.p}
	boundedFIFO(t, fifo, func() {
		c.add(root, "out/a.txt", "/data/out/a.txt")
		c.add(root, "out/b.txt", "/data/out/b.txt")
	})
	if len(c.out) != 1 || c.out[0].Path != "/data/out/a.txt" || c.out[0].FileSHA256 != sha256Hex(files["a.txt"]) {
		t.Fatalf("collected %+v", c.out)
	}
	if len(c.errs) != 1 || !strings.Contains(c.errs[0].Error(), "/data/out/b.txt: not a regular file") {
		t.Fatalf("errors %v", c.errs)
	}
}

// A FIFO left at a published artifact's staged path is refused at once:
// nothing is reported, one artifacts.failed event says why.
func TestPublishedArtifactRefusesFIFO(t *testing.T) {
	f := newFinishFixture(t)
	a := stagePublished(t, f, "x.txt", "x")
	fifo := filepath.Join(f.r.cfg.DataDir, "rt", a.File)
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	boundedFIFO(t, fifo, func() { tailToEnd(f) })
	if got := f.published(); len(got) != 0 {
		t.Fatalf("reports %+v", got)
	}
	waitFor(t, "artifacts.failed", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, fr := range f.reports {
			var ev proto.RunEvent
			if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == "artifacts.failed" {
				msg, _ := ev.Data["error"].(string)
				return strings.Contains(msg, "x.txt: not a regular file")
			}
		}
		return false
	})
}
