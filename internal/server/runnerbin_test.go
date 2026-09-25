package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcioapm/lux/internal/ids"
)

func TestLoadRunnerBinaries(t *testing.T) {
	dir := t.TempDir()
	write := func(arch, name, content string) {
		t.Helper()
		d := filepath.Join(dir, "linux-"+arch)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := func(content string) string {
		h := sha256.Sum256([]byte(content))
		return hex.EncodeToString(h[:])
	}
	write("arm64", "lux-runner", "runner-arm64")
	write("arm64", "lux-shim", "shim-arm64")
	write("amd64", "lux-runner", "runner-amd64")
	// amd64's lux-shim is missing: the pair is incomplete, so amd64 is
	// hashed (a lookup for lux-runner alone still finds it) but never
	// offered in the manifest and never grounds for a drain decision.

	s := &Server{cfg: Config{RunnerBinDir: dir}}
	s.loadRunnerBinaries()

	if got := s.bins["arm64"]["lux-runner"].sha256; got != sum("runner-arm64") {
		t.Errorf("arm64 lux-runner: %q", got)
	}
	if _, ok := s.bins["amd64"]["lux-shim"]; ok {
		t.Error("amd64 lux-shim: found one that was never written")
	}

	m := s.runnerBinManifest()
	if m["linux-arm64"]["lux-runner"] != sum("runner-arm64") || m["linux-arm64"]["lux-shim"] != sum("shim-arm64") {
		t.Errorf("manifest linux-arm64: %+v", m["linux-arm64"])
	}
	if _, has := m["linux-amd64"]; has {
		t.Error("manifest offered amd64, which is missing lux-shim")
	}
}

// An empty runner_bin_dir (the default when nothing is configured to
// serve): every lookup misses, cleanly.
func TestLoadRunnerBinariesEmptyDir(t *testing.T) {
	s := &Server{cfg: Config{RunnerBinDir: ""}}
	s.loadRunnerBinaries()
	if len(s.runnerBinManifest()) != 0 {
		t.Errorf("manifest not empty: %+v", s.runnerBinManifest())
	}
	if len(s.bins) != 0 {
		t.Errorf("binaries loaded with no configured directory: %v", s.bins)
	}
}

// binariesOutdated only fires when luxd holds a binary for the arch that
// differs from what the runner reports; an older runner (no shas) or an
// arch luxd serves nothing for are never grounds to drain.
func TestBinariesOutdated(t *testing.T) {
	s := &Server{bins: map[string]map[string]runnerBin{
		"arm64": {"lux-runner": {sha256: "r1"}, "lux-shim": {sha256: "s1"}},
		"riscv": {"lux-runner": {sha256: "r1"}}, // only one of the pair
	}}
	cases := []struct {
		name               string
		arch, runner, shim string
		want               bool
	}{
		{"matches", "arm64", "r1", "s1", false},
		{"runner differs", "arm64", "r2", "s1", true},
		{"shim differs", "arm64", "r1", "s2", true},
		{"older runner: no shas", "arm64", "", "", false},
		{"unknown arch: luxd holds nothing", "amd64", "whatever", "whatever", false},
		{"only one of the pair: never grounds to drain", "riscv", "r2", "whatever", false},
	}
	for _, c := range cases {
		if got := s.binariesOutdated(c.arch, c.runner, c.shim); got != c.want {
			t.Errorf("%s: binariesOutdated = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBinariesMatch(t *testing.T) {
	s := &Server{bins: map[string]map[string]runnerBin{
		"arm64": {"lux-runner": {sha256: "r1"}, "lux-shim": {sha256: "s1"}},
		"riscv": {"lux-runner": {sha256: "r1"}}, // only one of the pair
	}}
	if !s.binariesMatch("arm64", "r1", "s1") {
		t.Error("matching shas did not match")
	}
	if s.binariesMatch("arm64", "r2", "s1") {
		t.Error("a differing runner sha matched")
	}
	if s.binariesMatch("arm64", "", "") {
		t.Error("empty shas (no report yet) matched")
	}
	if s.binariesMatch("riscv", "r1", "s1") {
		t.Error("matched an arch luxd only half serves")
	}
}

// serveRunnerBin serves the bytes read at startup, never reopening
// runner_bin_dir: a file replaced on disk in place (as a rolling deploy
// would, if it skipped a luxd restart) must not change what luxd serves
// or its X-Lux-Sha256, since the in-memory copy and its hash were taken
// together and never re-read. Exercised through s.Handler(), a real
// GET /runner/v1/bin/linux-arm64/lux-runner with a host token: a handler
// that reopened the path on every request would serve the swapped bytes
// and would still pass a test that only reads s.bins directly.
func TestServedBytesSurviveAnInPlaceFileSwap(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	sub := filepath.Join(dir, "linux-arm64")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("original runner bytes")
	if err := os.WriteFile(filepath.Join(sub, "lux-runner"), original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "lux-shim"), []byte("shim bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.RunnerBinDir = dir
	s.loadRunnerBinaries()
	wantSHA := sha256Hex(original)

	token := ids.Secret("luxh")
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, token_hash) VALUES ('tok1', $1)`, ids.Hash(token))

	// A release replaces the file on disk in place, without restarting
	// luxd: the served content and its advertised hash must still be the
	// original bytes.
	if err := os.WriteFile(filepath.Join(sub, "lux-runner"), []byte("swapped-in bytes, different content"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/runner/v1/bin/linux-arm64/lux-runner", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runner/v1/bin/linux-arm64/lux-runner: %d %s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Lux-Sha256"); got != wantSHA {
		t.Fatalf("X-Lux-Sha256 after an in-place swap: %q, want the original %q", got, wantSHA)
	}
	if !bytes.Equal(w.Body.Bytes(), original) {
		t.Fatalf("served body after an in-place file swap: %q, want the original %q", w.Body.Bytes(), original)
	}
}
