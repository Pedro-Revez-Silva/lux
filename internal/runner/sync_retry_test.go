package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// syncFixture is a running placement of run_x with repository app (a bare
// repository at bare), whose podman is script (given podman's arguments),
// and whose reports to luxd are acked; syncs are the git.sync events
// reported so far. The script sees the bare repository as $BARE.
type syncFixture struct {
	p     *placement
	sp    spec.RunSpec
	bare  string
	mu    sync.Mutex
	syncs []proto.SyncResult
}

func newSyncFixture(t *testing.T, script string) *syncFixture {
	t.Helper()
	root := t.TempDir()
	work, bare := filepath.Join(root, "w"), filepath.Join(root, "app.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	os.MkdirAll(work, 0o755)
	git(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644)
	git(work, "add", "-A")
	git(work, "commit", "-qm", "one")
	git(root, "clone", "-q", "--bare", work, bare)

	bin := filepath.Join(root, "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nBARE="+bare+"\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := filepath.Join(root, "rt")
	os.MkdirAll(rt, 0o755)
	r := &Runner{cfg: Config{DataDir: root}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, git: gitws.New(filepath.Join(root, "data"))}
	r.mounts.Store(runtimeVolume("run_x"), rt)
	r.conn = newConn(r)
	r.conn.polling = true
	sp := spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "app", URL: bare, Path: "/w/app"}}}}
	f := &syncFixture{sp: sp, bare: bare}
	f.p = &placement{r: r, runID: "run_x", tenantID: "t1", epoch: 1, dir: filepath.Join(root, "run"), phase: "running",
		assign: &proto.Assign{}, state: &runState{User: "1000:1000", Spec: &sp}}
	os.MkdirAll(f.p.dir, 0o755)

	// luxd: ack every report, keep the git.sync events.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			r.conn.mu.Lock()
			reports := r.conn.pollReports
			r.conn.pollReports = nil
			r.conn.mu.Unlock()
			for _, fr := range reports {
				var ev proto.RunEvent
				if json.Unmarshal(fr.Data, &ev) == nil && ev.Type == proto.EvGitSync {
					var res proto.SyncResult
					b, _ := json.Marshal(ev.Data)
					_ = json.Unmarshal(b, &res)
					f.mu.Lock()
					f.syncs = append(f.syncs, res)
					f.mu.Unlock()
				}
				r.conn.dispatch(ctx, proto.Frame{Type: proto.MsgAck, ID: fr.ID})
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

func (f *syncFixture) events() []proto.SyncResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.SyncResult(nil), f.syncs...)
}

// missingBaseOnce answers the first `lux-shim sync` with app failed for
// want of its base; the script's $retry part answers the second.
func missingBaseOnce(retry string) string {
	return `n=$(cat "$0.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0.n"
if [ $n = 1 ]; then
  echo '[{"repo":"app","ref":"main","status":"failed","error":"no base","missingBase":true}]'
  exit 0
fi
` + retry
}

// A running sync whose whole-history retry fails reports each repository
// once: the retry's failure, not also the first try's missingBase.
func TestFailedSyncRetryReportsARepositoryOnce(t *testing.T) {
	for _, c := range []struct {
		name, script, wantErr string
	}{
		// The retry's bundle cannot be made: the repository is gone from
		// where the runner fetches it.
		{"retry bundle fails", `rm -rf "$BARE"
echo '[{"repo":"app","ref":"main","status":"failed","error":"no base","missingBase":true}]'`, "app.git"},
		// The retry's lux-shim sync cannot run.
		{"retry exec fails", missingBaseOnce(`echo "container gone" >&2; exit 125`), "lux-shim sync"},
		// The retry runs and fails for its own reason.
		{"retry fails", missingBaseOnce(`echo '[{"repo":"app","ref":"main","status":"failed","error":"disk full"}]'`), "disk full"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSyncFixture(t, c.script)
			f.p.syncRunning(context.Background(), proto.Sync{RequestID: "rq1", Repos: []proto.SyncRef{{Repo: "app", Ref: "main"}}})
			evs := f.events()
			if len(evs) != 1 {
				t.Fatalf("git.sync events: %+v", evs)
			}
			e := evs[0]
			if e.Repo != "app" || e.Status != "failed" || e.MissingBase || !e.FullBundle || e.Error == "no base" ||
				!strings.Contains(e.Error, c.wantErr) {
				t.Fatalf("event %+v, want the retry's failure (%q)", e, c.wantErr)
			}
		})
	}
}

// Before init, a whole-history retry that cannot be bundled is reported
// once, with the shim's results, in place of its missingBase.
func TestFailedFallbackBundleReportsARepositoryOnce(t *testing.T) {
	f := newSyncFixture(t, `true`)
	ctx := context.Background()
	f.p.sync, _ = f.p.prepareSync(ctx, f.sp, []proto.SyncRef{{Repo: "app", Ref: "main"}}, "", false)
	if f.p.sync == nil {
		t.Fatal("no first bundle")
	}
	msgs := shimPipe(t, f.p)
	os.RemoveAll(f.bare)
	f.p.onSyncFallback(ctx, json.RawMessage(`{"repos":["app"]}`))
	if m := recvShim(t, msgs); m.Sync == nil || len(m.Sync.Repos) != 0 {
		t.Fatalf("answer: %+v", m)
	}
	if evs := f.events(); len(evs) != 0 {
		t.Fatalf("reported before the shim's record: %+v", evs)
	}
	f.p.onSyncRecord(ctx, json.RawMessage(`{"results":[{"repo":"app","ref":"main","status":"failed","error":"no base","missingBase":true}]}`))
	evs := f.events()
	if len(evs) != 1 || evs[0].MissingBase || !evs[0].FullBundle || !strings.Contains(evs[0].Error, "app.git") {
		t.Fatalf("git.sync events: %+v", evs)
	}
}

// shimPipe connects a fake shim to p: the ShimMsgs the runner sends it
// arrive on the returned channel.
func shimPipe(t *testing.T, p *placement) <-chan proto.ShimMsg {
	t.Helper()
	runnerEnd, shimEnd := net.Pipe()
	t.Cleanup(func() { runnerEnd.Close(); shimEnd.Close() })
	p.mu.Lock()
	p.shimConn, p.shimEnc = runnerEnd, json.NewEncoder(runnerEnd)
	p.mu.Unlock()
	msgs := make(chan proto.ShimMsg, 4)
	go func() {
		dec := json.NewDecoder(shimEnd)
		for {
			var m proto.ShimMsg
			if dec.Decode(&m) != nil {
				return
			}
			msgs <- m
		}
	}()
	return msgs
}

func recvShim(t *testing.T, msgs <-chan proto.ShimMsg) proto.ShimMsg {
	t.Helper()
	select {
	case m := <-msgs:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message to the shim")
		return proto.ShimMsg{}
	}
}
