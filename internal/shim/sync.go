package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// `lux-shim sync <json>`: move each checkout to its commit, from a bundle
// the runner fetched through the host's mirror. The runner runs it as the
// workload user inside the Run's container (its .git is the workload's;
// no credential is involved), before init on a new placement, or while
// the Run runs. Prints one proto.SyncResult per repository as a JSON array.
//
// The rule, deterministic:
//   - HEAD is the commit: up-to-date.
//   - No tracked file changed and HEAD is an ancestor of the commit:
//     fast-forward (the branch moves to it, or HEAD detaches at a tag or sha).
//   - Otherwise (tracked files changed, or the histories diverged): reset.
//     What was there is saved first (a stash commit of the changes, or
//     HEAD) as refs/lux/pre-sync; then tracked files become the commit's.
//     Untracked and ignored files are kept (an untracked file the commit
//     tracks is replaced).
//   - Any error: failed, with git's message; the checkout as it was where
//     git left it. The Run goes on either way.

// Sync is `lux-shim sync`'s main.
func Sync(args []string) int {
	var a proto.SyncArgs
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &a) != nil {
		fmt.Fprintln(os.Stderr, "usage: lux-shim sync <json>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := json.NewEncoder(os.Stdout).Encode(SyncRepos(ctx, a)); err != nil {
		return 1
	}
	return 0
}

// SyncRepos moves each checkout, in order; one result each.
func SyncRepos(ctx context.Context, a proto.SyncArgs) []proto.SyncResult {
	out := make([]proto.SyncResult, 0, len(a.Repos))
	for _, r := range a.Repos {
		out = append(out, syncRepo(ctx, r))
	}
	return out
}

// gitIn runs git in a checkout: no hooks, no prompts.
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	// An identity for the stash commit of tracked changes (refs/lux/pre-sync).
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "advice.detachedHead=false",
		"-c", "user.name=lux", "-c", "user.email=lux@lux.invalid"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	b, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(b))
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", args[0], err, out)
	}
	return out, nil
}

func syncRepo(ctx context.Context, r proto.SyncRepo) proto.SyncResult {
	res := proto.SyncResult{Repo: r.Name, Ref: r.Ref, To: r.Commit, Status: "failed"}
	fail := func(err error) proto.SyncResult {
		res.Error = err.Error()
		if len(res.Error) > 1000 {
			res.Error = res.Error[:1000]
		}
		return res
	}
	if _, err := os.Stat(filepath.Join(r.Path, ".git")); err != nil {
		return fail(fmt.Errorf("%s has no checkout", r.Path))
	}
	from, err := gitIn(ctx, r.Path, "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	res.From = from
	if _, err := gitIn(ctx, r.Path, "fetch", "--quiet", "--no-tags", r.Bundle, "+refs/lux/sync:refs/lux/sync"); err != nil {
		if r.Base != "" {
			_, lacks := gitIn(ctx, r.Path, "cat-file", "-e", r.Base+"^{commit}")
			res.MissingBase = lacks != nil || strings.Contains(err.Error(), "prerequisite")
		}
		return fail(err)
	}
	if got, err := gitIn(ctx, r.Path, "rev-parse", "refs/lux/sync"); err != nil || got != r.Commit {
		return fail(fmt.Errorf("the bundle has %q, not %s", got, r.Commit))
	}
	status, err := gitIn(ctx, r.Path, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fail(err)
	}
	res.Dirty = status != ""
	if from == r.Commit && !res.Dirty {
		res.Status = "up-to-date"
		return res
	}
	_, notAncestor := gitIn(ctx, r.Path, "merge-base", "--is-ancestor", from, r.Commit)
	res.Diverged = notAncestor != nil
	checkout := []string{"checkout", "--quiet"}
	if r.Branch != "" {
		checkout = append(checkout, "-B", r.Branch, r.Commit)
	} else {
		checkout = append(checkout, "--detach", r.Commit)
	}
	if !res.Dirty && !res.Diverged {
		if _, err := gitIn(ctx, r.Path, checkout...); err != nil {
			return fail(err)
		}
		res.Status = "fast-forward"
		return res
	}
	saved := from
	if res.Dirty {
		// A commit of the changes, on no branch: nothing in the checkout
		// changes making it.
		if st, err := gitIn(ctx, r.Path, "stash", "create", "lux sync: tracked changes before "+r.Commit); err == nil && st != "" {
			saved = st
		}
	}
	if _, err := gitIn(ctx, r.Path, "update-ref", "refs/lux/pre-sync", saved); err != nil {
		return fail(err)
	}
	res.Saved = "refs/lux/pre-sync"
	force := append([]string{checkout[0], checkout[1], "--force"}, checkout[2:]...)
	if _, err := gitIn(ctx, r.Path, force...); err != nil {
		return fail(err)
	}
	if _, err := gitIn(ctx, r.Path, "reset", "--quiet", "--hard", r.Commit); err != nil {
		return fail(err)
	}
	res.Status = "reset"
	return res
}

// syncBeforeInit moves the checkouts a resume asked for, as the workload
// user (this binary's `sync`, through command), and records the results as
// a lux.sync event: the runner reports them. A failure is a result, never
// the placement's. A checkout that lacks its bundle's base is retried once
// with the whole history, which only the runner can bundle: the shim asks
// for it (lux.sync.fallback) and waits for ShimSync.
func (s *Shim) syncBeforeInit(env []string) {
	results := s.runSync(*s.cfg.Sync, env)
	if missing := proto.MissingBase(results); len(missing) > 0 {
		s.out.Event(proto.EvSyncFallback, map[string]any{"repos": missing})
		if retry := s.awaitSyncRetry(syncRetryWait); retry != nil && len(retry.Repos) > 0 {
			results = proto.MergeSyncRetry(results, s.runSync(*retry, env))
		}
	}
	s.out.Event(proto.EvSync, map[string]any{"results": results})
}

// syncRetryWait bounds the wait for the runner's whole-history bundles:
// SyncBundle's own limit.
const syncRetryWait = 10 * time.Minute

// awaitSyncRetry is the runner's answer to lux.sync.fallback; nil when
// none came within wait, or the Run is stopping.
func (s *Shim) awaitSyncRetry(wait time.Duration) *proto.SyncArgs {
	deadline := time.After(wait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case a := <-s.syncCh:
			return a
		case <-deadline:
			return nil
		case <-tick.C:
			if s.isStopping() {
				return nil
			}
		}
	}
}

// runSync runs `lux-shim sync` as the workload user; every repository
// failed when it cannot.
func (s *Shim) runSync(a proto.SyncArgs, env []string) []proto.SyncResult {
	cmd := s.command([]string{proto.ShimBinary, "sync", string(proto.Marshal(a))}, env)
	cmd.Dir = "/"
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	// The reaper reaps every child: it hands this one's exit over.
	exited, err := s.startTracked(cmd.Start, &cmd.Process)
	var results []proto.SyncResult
	if err == nil {
		ws := <-exited
		// The reaper reaped it; Wait only waits for its output to be copied.
		_ = cmd.Wait()
		if ws.ExitStatus() != 0 {
			err = fmt.Errorf("exit %d: %s", exitCode(ws), strings.TrimSpace(errOut.String()))
		} else {
			err = json.Unmarshal(out.Bytes(), &results)
		}
	}
	if err != nil {
		return a.Failed("lux-shim sync: " + err.Error())
	}
	return results
}
