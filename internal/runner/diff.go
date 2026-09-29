package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

var diffTimeout = 60 * time.Second

func (r *Runner) serveDiff(ctx context.Context, f proto.Frame) {
	var req proto.DiffRequest
	_ = json.Unmarshal(f.Data, &req)
	res := r.diff(ctx, f.RunID, f.Epoch, req.Base)
	res.SubID = req.SubID
	_ = r.conn.Send(ctx, proto.Frame{Type: proto.MsgDiffResult, RunID: f.RunID, Epoch: f.Epoch, Data: proto.Marshal(res)})
}

func (r *Runner) diff(ctx context.Context, runID string, epoch int, base string) proto.DiffResult {
	p := r.placement(runID, epoch)
	if p == nil {
		return proto.DiffResult{NotRunning: true}
	}
	p.mu.Lock()
	running := p.phase == "running" || p.phase == "stopping"
	busy := p.diffing
	if running && !busy {
		p.diffing = true
	}
	args := proto.DiffArgs{Base: base}
	var user string
	if p.state != nil {
		user = p.state.User
		if p.state.Spec != nil && p.state.Spec.Git != nil {
			for _, rp := range p.state.Spec.Git.Repositories {
				args.Repos = append(args.Repos, proto.DiffRepo{Name: rp.Name, Path: rp.Path, Base: p.state.GitBases[rp.Name]})
			}
		}
	}
	p.mu.Unlock()
	switch {
	case !running:
		return proto.DiffResult{NotRunning: true}
	case busy:
		return proto.DiffResult{Busy: true}
	}
	defer func() {
		p.mu.Lock()
		p.diffing = false
		p.mu.Unlock()
	}()
	if user == "" {
		return proto.DiffResult{Error: "the workload's user is not known"}
	}
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	out, err := r.pm.Run(ctx, "exec", "--user", user, "--workdir", "/", containerName(runID),
		proto.ShimBinary, "diff", string(proto.Marshal(args)))
	if ctx.Err() == context.DeadlineExceeded {
		return proto.DiffResult{Error: fmt.Sprintf("the diff took longer than %s", diffTimeout)}
	}
	if err != nil {
		if st, ierr := r.pm.Inspect(ctx, containerName(runID)); ierr == nil && !st.Running {
			return proto.DiffResult{NotRunning: true}
		}
		return proto.DiffResult{Error: err.Error()}
	}
	var res proto.DiffResult
	if err := json.Unmarshal(out, &res); err != nil {
		return proto.DiffResult{Error: "lux-shim diff: " + strings.TrimSpace(string(out[:min(len(out), 200)]))}
	}
	return res
}

func (p *placement) setGitBase(repo, commit string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.GitBases == nil {
		p.state.GitBases = map[string]string{}
	}
	p.state.GitBases[repo] = commit
	_ = writeRunState(p.dir, p.state)
}
