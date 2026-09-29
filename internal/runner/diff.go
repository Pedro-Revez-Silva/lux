package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/gitdiff"
	"github.com/marcioapm/lux/internal/proto"
)

// Live diffs of a running Run's repositories. As for a push, the runner
// never runs git in a checkout: `lux-shim diff` does, as the workload user,
// inside the Run's own container. Its output is read as untrusted
// (gitdiff.ReadStream bounds it).

var (
	// diffTimeout bounds one live diff.
	diffTimeout = 60 * time.Second
	// diffExecGrace is how long podman exec may outlive the diff's end
	// (the shim stops when its stdin closes) before it is signalled.
	diffExecGrace = 10 * time.Second
)

// A diff request and its cancel are two frames that must take effect in
// the order luxd sent them: both are handled on the connection's read
// loop (subscribeDiff registers before the diff is served, asynchronously),
// and a cancel for a request not seen yet is remembered for a while.
const (
	diffCancelTTL  = time.Minute
	diffCancelKeep = 1024
)

// subscribeDiff registers a diff request's subId; false when its cancel
// came first (nothing is to be served).
func (r *Runner) subscribeDiff(ctx context.Context, subID string) (context.Context, context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, gone := r.diffCancels[subID]; gone {
		delete(r.diffCancels, subID)
		return nil, nil, false
	}
	sctx, cancel := context.WithCancel(ctx)
	r.subs[subID] = cancel
	return sctx, cancel, true
}

// cancelDiff cancels a diff request, or remembers the cancel of one not
// seen yet (the most recent diffCancelKeep, for diffCancelTTL).
func (r *Runner) cancelDiff(subID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.subs[subID]; c != nil {
		c()
		return
	}
	now := time.Now()
	if r.diffCancels == nil {
		r.diffCancels = map[string]time.Time{}
	}
	for id, at := range r.diffCancels {
		if now.Sub(at) > diffCancelTTL {
			delete(r.diffCancels, id)
		}
	}
	if len(r.diffCancels) >= diffCancelKeep {
		oldest, at := "", now
		for id, t := range r.diffCancels {
			if t.Before(at) {
				oldest, at = id, t
			}
		}
		delete(r.diffCancels, oldest)
	}
	r.diffCancels[subID] = now
}

// serveDiffRequest answers a registered diff request (subscribeDiff).
func (r *Runner) serveDiffRequest(ctx, sctx context.Context, cancel context.CancelFunc, f proto.Frame, req proto.DiffRequest) {
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.subs, req.SubID)
		r.mu.Unlock()
	}()
	end := r.answerDiff(sctx, f.RunID, f.Epoch, req, func(res proto.DiffResult) error {
		return r.conn.Send(sctx, proto.Frame{Type: proto.MsgDiffResult, RunID: f.RunID, Epoch: f.Epoch, Data: proto.Marshal(res)})
	})
	if sctx.Err() == nil {
		_ = r.conn.Send(ctx, proto.Frame{Type: proto.MsgDiffEnd, RunID: f.RunID, Epoch: f.Epoch, Data: proto.Marshal(end)})
	}
}

// answerDiff computes a diff request's results, passing each to send, and
// returns how it ended.
func (r *Runner) answerDiff(ctx context.Context, runID string, epoch int, req proto.DiffRequest, send func(proto.DiffResult) error) proto.DiffEnd {
	end := proto.DiffEnd{SubID: req.SubID}
	p := r.placement(runID, epoch)
	if p == nil || !p.running(ctx) {
		end.NotRunning = true
		return end
	}
	req.Repos = p.withGitBases(req.Repos)
	if busy, err := p.serveLiveDiff(ctx, req, send); busy {
		end.Busy = true
	} else if err != nil {
		end.Error = err.Error()
	}
	return end
}

// liveRun is a live diff under way in a placement's container: at most
// one at a time, from its start until its exec has ended. Identical
// requests share it; each reads its results from the start.
type liveRun struct {
	key     string
	cancel  context.CancelFunc
	exited  chan struct{} // closed once its exec has ended
	mu      sync.Mutex
	results []proto.DiffResult
	done    bool
	err     error
	changed chan struct{} // closed, and replaced, on every change
	waiters int
	// abandoned: its last request went; it is being stopped.
	abandoned bool
}

func (l *liveRun) add(res proto.DiffResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, res)
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}

func (l *liveRun) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done, l.err = true, err
	close(l.changed)
}

func liveKey(req proto.DiffRequest) string {
	req.SubID = ""
	b, _ := json.Marshal(req)
	return string(b)
}

// serveLiveDiff answers one live diff request: it starts the diff, or
// shares the identical one under way. busy: a different one is under way,
// or still stopping (its exec has not ended). When the last request
// sharing a diff goes (ctx ends), the diff stops; an identical request
// meanwhile waits for it to end and starts afresh.
func (p *placement) serveLiveDiff(ctx context.Context, req proto.DiffRequest, send func(proto.DiffResult) error) (busy bool, err error) {
	key := liveKey(req)
	var l *liveRun
	for {
		p.mu.Lock()
		l = p.live
		if l == nil {
			break
		}
		if l.key != key {
			p.mu.Unlock()
			return true, nil
		}
		// Joined under its lock: it cannot be abandoned in between.
		l.mu.Lock()
		joined := !l.abandoned
		if joined {
			l.waiters++
		}
		l.mu.Unlock()
		if joined {
			break
		}
		p.mu.Unlock()
		select {
		case <-l.exited:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if l == nil {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		l = &liveRun{key: key, cancel: cancel, changed: make(chan struct{}), exited: make(chan struct{}), waiters: 1}
		p.live = l
		go func() {
			defer close(l.exited)
			defer cancel()
			err := p.liveDiff(runCtx, req, l.add)
			l.finish(err)
			p.mu.Lock()
			if p.live == l {
				p.live = nil
			}
			p.mu.Unlock()
		}()
	}
	p.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.waiters--; l.waiters == 0 && !l.done {
			// It stays the placement's live diff until its exec ends.
			l.abandoned = true
			l.cancel()
		}
		l.mu.Unlock()
	}()
	for sent := 0; ; {
		l.mu.Lock()
		res, done, lerr, changed := l.results[sent:], l.done, l.err, l.changed
		l.mu.Unlock()
		for _, r := range res {
			r.SubID = req.SubID
			if err := send(r); err != nil {
				return false, err
			}
			sent++
		}
		if done {
			return false, lerr
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// liveDiff computes a running placement's diffs in its container and
// passes each on as it comes. A repository whose records turn out
// incomplete is passed on again, with the error, after them. When ctx
// ends, the exec's stdin is closed, which stops the shim and its git.
func (p *placement) liveDiff(ctx context.Context, req proto.DiffRequest, send func(proto.DiffResult) error) error {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	user := p.userSpec()
	if user == "" {
		return errors.New("the workload's user is not known yet")
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdinR.Close()
	defer stdinW.Close()
	defer context.AfterFunc(ctx, func() { stdinW.Close() })()
	// podman itself is signalled only if the shim has not ended by then.
	execCtx, cancelExec := context.WithTimeout(context.WithoutCancel(ctx), diffTimeout+diffExecGrace)
	defer cancelExec()
	pr, pw := io.Pipe()
	// The reading ends with ctx, not only once podman does.
	defer context.AfterFunc(ctx, func() { pr.CloseWithError(context.Cause(ctx)) })()
	done := make(chan error, 1)
	go func() {
		args := proto.DiffArgs{Repos: req.Repos, Kinds: []string{req.Kind}, StatOnly: req.StatOnly, WatchStdin: true, Limit: proto.DiffLimit}
		b, _ := json.Marshal(args)
		err := p.r.pm.RunIO(execCtx, stdinR, pw,
			"exec", "-i", "--user", user, "--workdir", "/", containerName(p.runID), proto.ShimBinary, "diff", string(b))
		pw.CloseWithError(err)
		done <- err
	}()
	names := make([]string, len(req.Repos))
	for i, rp := range req.Repos {
		names[i] = rp.Name
	}
	incomplete, err := gitdiff.ReadStream(pr, names, []string{req.Kind}, proto.DiffLimit, func(st proto.DiffStat, body io.Reader) error {
		patch, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		return send(proto.DiffResult{Stat: st, Patch: patch})
	})
	pr.CloseWithError(errors.New("diff read ended"))
	cancel()
	if perr := <-done; err == nil && perr != nil {
		err = perr
	}
	if err != nil {
		return err
	}
	for _, rp := range req.Repos {
		if why, ok := incomplete[rp.Name]; ok {
			if err := send(proto.DiffResult{Stat: proto.DiffStat{Repo: rp.Name, Kind: req.Kind, Error: why}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// running reports whether the placement's container is running now.
func (p *placement) running(ctx context.Context) bool {
	st, err := p.r.pm.Inspect(ctx, containerName(p.runID))
	return err == nil && st.Running
}

func (p *placement) userSpec() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != nil {
		return p.state.User
	}
	return ""
}

// withGitBases fills in the base of each repository luxd sent none for:
// its git.clone event may not have reached luxd yet, and the placement
// knows the commit it cloned (or, from its assignment, an earlier
// placement's).
func (p *placement) withGitBases(repos []proto.DiffRepo) []proto.DiffRepo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(repos)
	for i := range out {
		if out[i].Base == "" && p.state != nil {
			out[i].Base = p.state.GitBases[out[i].Name]
		}
	}
	return out
}

// setGitBase records the commit a repository was just cloned at.
func (p *placement) setGitBase(repo, commit string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.GitBases == nil {
		p.state.GitBases = map[string]string{}
	}
	p.state.GitBases[repo] = commit
	_ = writeRunState(p.dir, p.state)
}
