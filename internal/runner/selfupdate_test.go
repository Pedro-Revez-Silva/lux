package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// binariesMatchManifest fetches luxd's current manifest and reports a
// match only when this host's own arch is listed there with shas equal
// to what this runner already reports: the check MsgExit handling uses
// to ignore a stale, redelivered exit once the runner has already caught
// up (whichever restart or undrain got there first).
func TestBinariesMatchManifest(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	var manifest map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runner/v1/bin/manifest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(manifest)
	}))
	defer srv.Close()

	r := &Runner{runnerSHA256: "r1", shimSHA256: "s1"}
	r.api = newAPI(srv.URL, "tok", "host")
	ctx := context.Background()

	manifest = map[string]map[string]string{arch: {"lux-runner": "r1", "lux-shim": "s1"}}
	if !r.binariesMatchManifest(ctx) {
		t.Error("matching shas for this arch did not match")
	}

	manifest = map[string]map[string]string{arch: {"lux-runner": "r2", "lux-shim": "s1"}}
	if r.binariesMatchManifest(ctx) {
		t.Error("a differing runner sha matched")
	}

	manifest = map[string]map[string]string{"linux-riscv64": {"lux-runner": "r1", "lux-shim": "s1"}}
	if r.binariesMatchManifest(ctx) {
		t.Error("a manifest without this arch matched")
	}

	manifest = nil
	if r.binariesMatchManifest(ctx) {
		t.Error("an empty manifest matched")
	}
}

// A manifest luxd cannot be reached for never matches: MsgExit must still
// be acted on rather than silently swallowed by a network blip.
func TestBinariesMatchManifestUnreachable(t *testing.T) {
	r := &Runner{runnerSHA256: "r1", shimSHA256: "s1"}
	r.api = newAPI("http://127.0.0.1:1", "tok", "host") // nothing listens
	if r.binariesMatchManifest(context.Background()) {
		t.Error("an unreachable luxd matched")
	}
}

// shouldExit holds back while a placement is live or once this runner's
// binaries already match luxd's manifest; otherwise it proceeds.
func TestShouldExit(t *testing.T) {
	arch := "linux-" + runtime.GOARCH
	cases := []struct {
		name       string
		manifest   string // lux-runner and lux-shim sha in luxd's manifest
		placements map[string]*placement
		want       bool
	}{
		{"live placement", "old", map[string]*placement{"run1": {phase: "running"}}, false},
		{"binaries already match", "old", map[string]*placement{}, false},
		{"idle and outdated", "new", map[string]*placement{}, true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]map[string]string{arch: {"lux-runner": c.manifest, "lux-shim": c.manifest}})
		}))
		r := &Runner{
			runnerSHA256: "old", shimSHA256: "old",
			log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			placements: c.placements,
		}
		r.api = newAPI(srv.URL, "tok", "host")
		if got := r.shouldExit(context.Background(), proto.ExitHost{Code: 42}); got != c.want {
			t.Errorf("%s: shouldExit = %v, want %v", c.name, got, c.want)
		}
		srv.Close()
	}
}
