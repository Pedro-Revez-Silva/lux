package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// gitRemote is a smart-HTTP git server (git http-backend) over the
// repositories in root, which records each request's Authorization header
// by repository ("app", "lib").
type gitRemote struct {
	url  string
	mu   sync.Mutex
	auth map[string][]string
}

func newGitRemote(t *testing.T, root string) *gitRemote {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git http-backend: %v", err)
	}
	g := &gitRemote{auth: map[string][]string{}}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ".git")
		g.mu.Lock()
		g.auth[repo] = append(g.auth[repo], r.Header.Get("Authorization"))
		g.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.url = srv.URL
	return g
}

// took returns the Authorization headers repo's requests carried since the
// last call, and forgets them.
func (g *gitRemote) took(repo string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	got := g.auth[repo]
	delete(g.auth, repo)
	return got
}

func basicFor(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// onlyAuth fails unless every one of headers (at least one) is want.
func onlyAuth(t *testing.T, what string, headers []string, want string) {
	t.Helper()
	if len(headers) == 0 || slices.ContainsFunc(headers, func(h string) bool { return h != want }) {
		t.Fatalf("%s: Authorization %q, want only %q", what, headers, want)
	}
}

// A push's credentials reach the remote for that repository, in place of
// the assigned secret, for the mirror's fetch and for the push; a later
// push and a sync without credentials use the rotated value. A repository
// not named keeps its own (here none). The values reach no event, log
// line or file the runner writes.
func TestPushCredentialsRotate(t *testing.T) {
	pf := newPushFixture(t, containerExec)
	remote := newGitRemote(t, pf.root)
	var logs syncBuf
	pf.p.r.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pf.sp.Secrets = []spec.Secret{{Name: "GIT_TOKEN"}}
	pf.sp.Git.Repositories[0].URL = remote.url + "/app.git"
	pf.sp.Git.Repositories[0].Credential = "GIT_TOKEN"
	pf.sp.Git.Repositories[1].URL = remote.url + "/lib.git"
	pf.p.assign.Spec = pf.sp
	pf.p.assign.Secrets = map[string]string{"GIT_TOKEN": "tok-submit"}
	pf.p.state.Spec = &pf.sp
	head := pf.commitTwo(t)

	const fresh, newer = "tok-fresh-1", "tok-fresh-2"
	res := pf.pushWith(t, proto.Push{RequestID: "p1", Credentials: map[string]string{"app": fresh}})
	if res["app"]["status"] != "pushed" || res["app"]["commit"] != head || pf.branch("app") != head {
		t.Fatalf("app: %v", res["app"])
	}
	onlyAuth(t, "push with credentials", remote.took("app"), basicFor(fresh))
	if lib := remote.took("lib"); len(lib) == 0 || slices.ContainsFunc(lib, func(h string) bool { return h != "" }) {
		t.Fatalf("lib, not named and without a credential: Authorization %q", lib)
	}

	app := pf.checkout("app")
	os.WriteFile(filepath.Join(app, "third"), []byte("3"), 0o644)
	run(t, app, "git", "add", "third")
	run(t, app, "git", "commit", "-qm", "third")
	head = run(t, app, "git", "rev-parse", "HEAD")
	res = pf.pushWith(t, proto.Push{RequestID: "p2", Leases: map[string]string{"app": pf.branch("app")}})
	if res["app"]["status"] != "pushed" || pf.branch("app") != head {
		t.Fatalf("second push: %v", res["app"])
	}
	onlyAuth(t, "push without credentials", remote.took("app"), basicFor(fresh))

	pf.syncWith(t, proto.Sync{RequestID: "s1", Repos: []proto.SyncRef{{Repo: "app", Ref: "main"}}})
	onlyAuth(t, "sync without credentials", remote.took("app"), basicFor(fresh))
	pf.syncWith(t, proto.Sync{RequestID: "s2", Repos: []proto.SyncRef{{Repo: "app", Ref: "main"}},
		Credentials: map[string]string{"app": newer}})
	onlyAuth(t, "sync with credentials", remote.took("app"), basicFor(newer))
	pf.pushWith(t, proto.Push{RequestID: "p3", Leases: map[string]string{"app": head}})
	onlyAuth(t, "push after a sync's credentials", remote.took("app"), basicFor(newer))

	pf.mu.Lock()
	events := string(proto.Marshal([]any{pf.pushes, pf.syncs, pf.done}))
	pf.mu.Unlock()
	for _, tok := range []string{fresh, newer} {
		if strings.Contains(events, tok) || strings.Contains(logs.String(), tok) {
			t.Fatalf("%s in events or logs:\n%s\n%s", tok, events, logs.String())
		}
		if err := filepath.WalkDir(pf.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			b, err := os.ReadFile(path)
			if err == nil && bytes.Contains(b, []byte(tok)) {
				t.Errorf("%s written to %s", tok, path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// control hands req to the runner as luxd's message of type typ for the
// fixture's placement, as delivered on the connection.
func (pf *pushFixture) control(typ string, req any) {
	r := pf.p.r
	r.mu.Lock()
	if r.placements == nil {
		r.placements = map[string]*placement{}
	}
	r.placements[pf.p.runID] = pf.p
	r.mu.Unlock()
	r.handleControl(context.Background(), proto.Frame{Type: typ, RunID: pf.p.runID, Epoch: pf.p.epoch, Data: proto.Marshal(req)})
}

// pushWith runs one push, as luxd's message, and returns its git.push
// results by repository.
func (pf *pushFixture) pushWith(t *testing.T, req proto.Push) map[string]map[string]any {
	t.Helper()
	pf.mu.Lock()
	n := len(pf.pushes)
	pf.mu.Unlock()
	pf.control(proto.MsgPush, req)
	var ev map[string]any
	waitUntil(t, func() bool {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		if len(pf.pushes) > n {
			ev = pf.pushes[n]
		}
		return ev != nil
	})
	out := map[string]map[string]any{}
	for _, x := range ev["results"].([]any) {
		r := x.(map[string]any)
		out[r["repo"].(string)] = r
	}
	return out
}

// syncWith runs one sync, as luxd's message, and waits for its sync.done.
func (pf *pushFixture) syncWith(t *testing.T, req proto.Sync) {
	t.Helper()
	pf.mu.Lock()
	n := len(pf.done)
	pf.mu.Unlock()
	pf.control(proto.MsgSync, req)
	waitUntil(t, func() bool {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		return len(pf.done) > n
	})
}

// The Hello of every runner from this tree says it takes credentials on a
// push or a sync, with or without sync modes.
func TestCapabilitiesTakeGitCredentials(t *testing.T) {
	for _, modes := range []bool{false, true} {
		if r := (&Runner{shimSyncModes: modes}); !slices.Contains(r.capabilities(), proto.CapGitCredentials) {
			t.Fatalf("sync modes %v: %v", modes, r.capabilities())
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
