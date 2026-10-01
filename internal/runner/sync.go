package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Syncing a Run's checkouts to new commits (a resume's sync, or
// MsgSync while it runs). The runner fetches each ref through the host's
// mirror with its credential and writes a bundle of it on the Run's
// runtime volume; `lux-shim sync`, as the workload user in the container,
// moves the checkout (shim/sync.go has the rule). The runner never runs
// git in the checkout, and no credential enters the container. Each
// repository's outcome is a git.sync event.

const syncDir = "sync" // on the runtime volume: /.lux/run/sync/<syncSubdir>

// prepareSync fetches and bundles each ref, and returns what the shim is
// to do (nil: nothing), and a failed result for each repository that
// cannot be prepared: the caller reports those, and the Run goes on. Each
// bundle holds only the history after the checkout's known base
// (GitBases); full: the whole history, the retry for a checkout that lacks
// its base.
func (p *placement) prepareSync(ctx context.Context, sp spec.RunSpec, refs []proto.SyncRef, requestID string, full bool) (*proto.SyncArgs, []proto.SyncResult) {
	if len(refs) == 0 {
		return nil, nil
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	sub := syncSubdir(requestID)
	if err == nil {
		dir := filepath.Join(rt, syncDir, sub)
		os.RemoveAll(dir)
		if err = os.MkdirAll(filepath.Dir(dir), 0o755); err == nil {
			err = os.Mkdir(dir, 0o755)
		}
	}
	bases := map[string]string{}
	if !full {
		p.mu.Lock()
		if p.state != nil {
			bases = maps.Clone(p.state.GitBases)
		}
		p.mu.Unlock()
	}
	args := &proto.SyncArgs{}
	var failed []proto.SyncResult
	for _, ref := range refs {
		res := proto.SyncResult{Repo: ref.Repo, Ref: ref.Ref, Status: "failed"}
		var repo *spec.Repository
		if sp.Git != nil {
			for i := range sp.Git.Repositories {
				if sp.Git.Repositories[i].Name == ref.Repo {
					repo = &sp.Git.Repositories[i]
				}
			}
		}
		switch {
		case err != nil:
			res.Error = "runtime volume: " + err.Error()
		case repo == nil:
			res.Error = "no repository " + ref.Repo + " in the spec"
		default:
			r := p.gitRepo(*repo)
			r.Ref = ref.Ref
			name := ref.Repo + ".bundle"
			t, berr := p.r.git.SyncBundle(ctx, r, bases[ref.Repo], filepath.Join(rt, syncDir, sub, name))
			if berr == nil {
				args.Repos = append(args.Repos, proto.SyncRepo{Name: ref.Repo, Path: repo.Path, Ref: ref.Ref, Commit: t.Commit,
					Branch: t.Branch, Bundle: proto.ShimRunDir + "/" + syncDir + "/" + sub + "/" + name, Base: t.Base})
				continue
			}
			res.Error = scrubURL(berr, repo.URL)
		}
		failed = append(failed, res)
	}
	if len(args.Repos) == 0 {
		return nil, failed
	}
	return args, failed
}

// retryRefs are the refs of args to bundle again with the whole history:
// those of the named repositories, whose checkout lacked its bundle's base.
func retryRefs(args *proto.SyncArgs, names []string) []proto.SyncRef {
	var refs []proto.SyncRef
	for _, name := range names {
		for _, r := range args.Repos {
			if r.Name == name {
				refs = append(refs, proto.SyncRef{Repo: r.Name, Ref: r.Ref})
			}
		}
	}
	return refs
}

// syncSubdir is a sync's own directory of bundles: per request id (two
// syncs of a Run at once keep their bundles apart), "resume" for a
// resume's. The id is the caller's: hashed, never a path.
func syncSubdir(requestID string) string {
	if requestID == "" {
		return "resume"
	}
	h := sha256.Sum256([]byte(requestID))
	return "r-" + hex.EncodeToString(h[:8])
}

// removeSyncBundles deletes a sync's bundles once the shim has fetched
// them: the runtime volume keeps none.
func (p *placement) removeSyncBundles(ctx context.Context, requestID string) {
	if rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID)); err == nil {
		_ = os.RemoveAll(filepath.Join(rt, syncDir, syncSubdir(requestID)))
	}
}

// moved: the sync changed the checkout.
func moved(res proto.SyncResult) bool {
	return res.Status == "fast-forward" || res.Status == "reset"
}

// reportSync sends a repository's git.sync event, and keeps a moved
// checkout's commit as the base of its live diffs.
func (p *placement) reportSync(ctx context.Context, res proto.SyncResult, requestID string) {
	if moved(res) && res.To != "" {
		p.setGitBase(res.Repo, res.To)
	}
	var d map[string]any
	b, _ := json.Marshal(res)
	_ = json.Unmarshal(b, &d)
	if requestID != "" {
		d["requestId"] = requestID
	}
	_ = p.reportRetrying(ctx, proto.RunEvent{Type: proto.EvGitSync, Data: d})
}

// syncRunning is MsgSync: move a running placement's checkouts, then tell
// luxd (sync.done) whether any moved, so servers with afterSync restart.
func (p *placement) syncRunning(ctx context.Context, req proto.Sync) {
	p.mu.Lock()
	running := p.phase == "running"
	user := ""
	var sp spec.RunSpec
	if p.state != nil {
		user = p.state.User
		if p.state.Spec != nil {
			sp = *p.state.Spec
		}
	}
	p.mu.Unlock()
	done := map[string]any{"requestId": req.RequestID, "changed": false}
	// Reported with ctx, never a context cancelled meanwhile: a write
	// cancelled half-way closes the connection to luxd.
	defer func() { _ = p.reportRetrying(ctx, proto.RunEvent{Type: proto.EvSyncDone, Data: done}) }()
	if !running || user == "" {
		for _, r := range req.Repos {
			p.reportSync(ctx, proto.SyncResult{Repo: r.Repo, Ref: r.Ref, Status: "failed", Error: "the Run is not running"}, req.RequestID)
		}
		return
	}
	defer p.removeSyncBundles(ctx, req.RequestID)
	args, failed := p.prepareSync(ctx, sp, req.Repos, req.RequestID, false)
	for _, res := range failed {
		p.reportSync(ctx, res, req.RequestID)
	}
	if args == nil {
		return
	}
	results := p.execSync(ctx, user, args)
	// A repository retried with the whole history is reported once: the
	// retry's outcome (moved, or why it failed) replaces its missingBase.
	if refs := retryRefs(args, proto.MissingBase(results)); len(refs) > 0 {
		retry, again := p.prepareSync(ctx, sp, refs, req.RequestID, true)
		if retry != nil {
			again = append(again, p.execSync(ctx, user, retry)...)
		}
		results = proto.MergeSyncRetry(results, again)
	}
	for _, res := range results {
		p.reportSync(ctx, res, req.RequestID)
		if moved(res) {
			done["changed"] = true
		}
	}
}

// execSync runs `lux-shim sync` in the container as the workload user;
// when it cannot, every repository's result is failed.
func (p *placement) execSync(ctx context.Context, user string, args *proto.SyncArgs) (results []proto.SyncResult) {
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := p.r.execShim(execCtx, p.runID, user, "sync", args)
	if err == nil {
		err = json.Unmarshal(out, &results)
	}
	if err != nil {
		return args.Failed(fmt.Sprintf("lux-shim sync: %v %s", err, strings.TrimSpace(string(out[:min(len(out), 300)]))))
	}
	return results
}

// onSyncFallback answers the shim's lux.sync.fallback before init: the
// named repositories bundled again with the whole history, sent as
// ShimSync (an empty one when none could be: the shim goes on).
func (p *placement) onSyncFallback(ctx context.Context, data json.RawMessage) {
	var d struct {
		Repos []string `json:"repos"`
	}
	_ = json.Unmarshal(data, &d)
	p.mu.Lock()
	first := p.sync
	var sp spec.RunSpec
	if p.state != nil && p.state.Spec != nil {
		sp = *p.state.Spec
	}
	p.mu.Unlock()
	var retry *proto.SyncArgs
	if first != nil {
		var failed []proto.SyncResult
		retry, failed = p.prepareSync(ctx, sp, retryRefs(first, d.Repos), "", true)
		// Reported with the shim's results (onSyncRecord), in place of
		// their missingBase.
		p.mu.Lock()
		p.syncRetryFailed = failed
		p.mu.Unlock()
	}
	if retry == nil {
		retry = &proto.SyncArgs{}
	}
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimSync, Sync: retry}); err != nil {
		p.logf("sync retry not sent", "err", err)
	}
}

// onSyncRecord reports the shim's sync before init (a lux.sync event in
// the output file) as git.sync events.
func (p *placement) onSyncRecord(ctx context.Context, data json.RawMessage) {
	var d struct {
		Results []proto.SyncResult `json:"results"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	p.removeSyncBundles(ctx, "")
	p.mu.Lock()
	failed := p.syncRetryFailed
	p.syncRetryFailed = nil
	p.mu.Unlock()
	for _, res := range proto.MergeSyncRetry(d.Results, failed) {
		p.reportSync(ctx, res, "")
	}
}
