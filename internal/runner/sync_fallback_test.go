package runner

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// The shim's lux.sync.fallback before init is answered with one ShimSync:
// the named repositories bundled with the whole history (no base), or
// empty when the placement has no sync of its own (re-adopted after a
// runner restart), so the shim never waits out its limit.
func TestSyncFallbackAnswersTheShim(t *testing.T) {
	t.Run("with the placement's sync", func(t *testing.T) {
		f := newSyncFixture(t, `true`)
		ctx := context.Background()
		// The checkout's known base is the bare repository's first commit;
		// main has moved on, so the first bundle is incremental.
		base := gitOut(t, f.bare, "rev-parse", "main")
		tree := gitOut(t, f.bare, "rev-parse", "main^{tree}")
		next := gitOut(t, f.bare, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", tree, "-p", base, "-m", "two")
		gitOut(t, f.bare, "update-ref", "refs/heads/main", next)
		f.p.state.GitBases = map[string]string{"app": base}
		var failed []proto.SyncResult
		f.p.sync, failed = f.p.prepareSync(ctx, f.sp, []proto.SyncRef{{Repo: "app", Ref: "main"}}, "", false)
		if f.p.sync == nil || len(failed) != 0 || f.p.sync.Repos[0].Base != base {
			t.Fatalf("first: %+v %+v", f.p.sync, failed)
		}
		msgs := shimPipe(t, f.p)
		f.p.onSyncFallback(ctx, json.RawMessage(`{"repos":["app"]}`))
		m := recvShim(t, msgs)
		if m.Type != proto.ShimSync || m.Sync == nil || len(m.Sync.Repos) != 1 {
			t.Fatalf("answer: %+v", m)
		}
		got := m.Sync.Repos[0]
		if got.Name != "app" || got.Base != "" || got.Commit != f.p.sync.Repos[0].Commit || got.Bundle == "" {
			t.Fatalf("retry: %+v", got)
		}
		// The bundle holds the whole history: it verifies in an empty
		// repository, with no prerequisite.
		host := filepath.Join(f.p.r.cfg.DataDir, "rt", got.Bundle[len(proto.ShimRunDir)+1:])
		empty := t.TempDir()
		if out, err := exec.Command("git", "-C", empty, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if out, err := exec.Command("git", "-C", empty, "bundle", "verify", host).CombinedOutput(); err != nil {
			t.Fatalf("bundle verify: %v %s", err, out)
		}
		select {
		case extra := <-msgs:
			t.Fatalf("a second message: %+v", extra)
		case <-time.After(100 * time.Millisecond):
		}
	})
	t.Run("without one", func(t *testing.T) {
		f := newSyncFixture(t, `true`)
		msgs := shimPipe(t, f.p)
		f.p.onSyncFallback(context.Background(), json.RawMessage(`{"repos":["app"]}`))
		m := recvShim(t, msgs)
		if m.Type != proto.ShimSync || m.Sync == nil || len(m.Sync.Repos) != 0 {
			t.Fatalf("answer: %+v", m)
		}
	})
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
