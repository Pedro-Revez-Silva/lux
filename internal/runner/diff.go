package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/gitdiff"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Diffs of a Run's repositories. As for a push, the runner never runs git
// in a checkout: `lux-shim diff` does, as the workload user, inside the
// Run's own container while it runs, and at every snapshot in a throwaway
// container from the Run's image with its state volumes mounted, without a
// network. Its output is read as untrusted (gitdiff.ReadStream bounds it).

// diffKinds are what a snapshot stores: against the clone and against HEAD.
var diffKinds = []string{proto.DiffBaseClone, proto.DiffBaseHead}

// diffArgs is the command line that computes the diffs, after the shim.
func diffArgs(a proto.DiffArgs) []string {
	a.Limit = proto.DiffLimit
	b, _ := json.Marshal(a)
	return []string{"diff", string(b)}
}

// readDiffs reads the shim's stream (see gitdiff.ReadStream): incomplete
// names each repository whose records are not exactly one per kind.
func readDiffs(r io.Reader, repos []proto.DiffRepo, kinds []string, fn func(proto.DiffStat, io.Reader) error) (map[string]string, error) {
	names := make([]string, len(repos))
	for i, rp := range repos {
		names[i] = rp.Name
	}
	return gitdiff.ReadStream(r, names, kinds, proto.DiffLimit, fn)
}

// liveRun is a live diff under way in a placement's container: at most
// one at a time. Identical requests share it; each reads its results from
// the start.
type liveRun struct {
	key     string
	cancel  context.CancelFunc
	mu      sync.Mutex
	results []proto.DiffResult
	done    bool
	err     error
	changed chan struct{} // closed, and replaced, on every change
	waiters int
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
// shares the identical one under way. busy: a different one is under way.
// When the last request sharing a diff goes (ctx ends), the diff stops.
func (p *placement) serveLiveDiff(ctx context.Context, req proto.DiffRequest, send func(proto.DiffResult) error) (busy bool, err error) {
	key := liveKey(req)
	p.mu.Lock()
	l := p.live
	if l != nil && l.key != key {
		p.mu.Unlock()
		return true, nil
	}
	if l == nil {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		l = &liveRun{key: key, cancel: cancel, changed: make(chan struct{})}
		p.live = l
		go func() {
			defer cancel()
			err := p.liveDiff(runCtx, req, l.add)
			p.mu.Lock()
			if p.live == l {
				p.live = nil
			}
			p.mu.Unlock()
			l.finish(err)
		}()
	}
	l.mu.Lock()
	l.waiters++
	l.mu.Unlock()
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		l.mu.Lock()
		if l.waiters--; l.waiters == 0 && !l.done {
			l.cancel()
			if p.live == l {
				p.live = nil // a request from now on starts afresh
			}
		}
		l.mu.Unlock()
		p.mu.Unlock()
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
	execCtx, cancelExec := context.WithTimeout(context.WithoutCancel(ctx), diffTimeout+diffCleanupTimeout)
	defer cancelExec()
	pr, pw := io.Pipe()
	// The reading ends with ctx, not only once podman does.
	defer context.AfterFunc(ctx, func() { pr.CloseWithError(context.Cause(ctx)) })()
	done := make(chan error, 1)
	go func() {
		args := append([]string{"exec", "-i", "--user", user, "--workdir", "/", containerName(p.runID), proto.ShimBinary},
			diffArgs(proto.DiffArgs{Repos: req.Repos, Kinds: []string{req.Kind}, StatOnly: req.StatOnly, WatchStdin: true})...)
		err := p.r.pm.RunIO(execCtx, stdinR, pw, args...)
		pw.CloseWithError(err)
		done <- err
	}()
	incomplete, err := readDiffs(pr, req.Repos, []string{req.Kind}, func(st proto.DiffStat, body io.Reader) error {
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
	if p.state != nil && p.state.User != "" {
		return p.state.User
	}
	return ""
}

// diffRepos are the placement's repositories, with the commits they were
// cloned at.
func (p *placement) diffRepos() []proto.DiffRepo {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sp *spec.RunSpec
	if p.assign != nil {
		sp = &p.assign.Spec
	} else if p.state != nil {
		sp = p.state.Spec
	}
	if sp == nil || sp.Git == nil {
		return nil
	}
	var out []proto.DiffRepo
	for _, r := range sp.Git.Repositories {
		out = append(out, proto.DiffRepo{Name: r.Name, Path: r.Path, Base: p.state.GitBases[r.Name]})
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

// A snapshot's diffs are computed after it is reported and its uploads
// started: they never hold up a stop, a drain or a resume. They are
// reported on their own (snapshot.diffs), their patches uploaded like the
// snapshot's blobs.

var (
	// diffTimeout bounds one diff, live or a snapshot's: creating,
	// running, killing and removing its container.
	diffTimeout = 60 * time.Second
	// diffCleanupTimeout bounds removing the helper container once the
	// diff's own time is up.
	diffCleanupTimeout = 10 * time.Second
)

const (
	// diffEvictMargin is what an evicted host keeps back from a snapshot's
	// diff for its uploads; diffMinBudget the least time worth starting one.
	diffEvictMargin = 30 * time.Second
	diffMinBudget   = 10 * time.Second
)

// errSuperseded cancels a snapshot's diff: the Run resumed on this host,
// and its volumes are about to change.
var errSuperseded = errors.New("superseded: the Run resumed on this host")

// diffBudget is how long a snapshot's diff may take: diffTimeout, or while
// the host is being evicted, the time left before it goes less a margin
// for the uploads. skip says why no diff should be tried at all.
func (r *Runner) diffBudget() (budget time.Duration, skip string) {
	ev := r.evictBy.Load()
	if ev == nil {
		return diffTimeout, ""
	}
	left := time.Until(ev.at)
	if budget = min(diffTimeout, left-diffEvictMargin); budget < diffMinBudget {
		return 0, fmt.Sprintf("the host is going away in %s: its uploads come first", left.Round(time.Second))
	}
	return budget, ""
}

// startSnapshotDiffs computes snapshot snapID's diffs in the background
// and reports them. stopDiffs cancels them.
func (p *placement) startSnapshotDiffs(ctx context.Context, snapID string) {
	repos := p.diffRepos()
	if len(repos) == 0 {
		p.setDiffsFor("")
		return
	}
	// A runner that restarted mid-way: luxd has them already, or what was
	// written for it never reached luxd.
	if rec := p.r.snapshotRecords()[diffsRecord(snapID)]; rec != nil {
		if rec.Reported {
			p.setDiffsFor("")
			return
		}
		removeSnapshotFiles(p.r, diffsRecord(snapID), rec)
	}
	dctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	released, done := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.diffCancel, p.diffReleased, p.diffDone = cancel, released, done
	p.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel(nil)
		p.snapshotDiffs(dctx, snapID, repos, released)
	}()
}

// stopDiffs cancels a snapshot's diff under way for why (nil: lets it
// finish), and waits until its container no longer has the state volumes
// mounted.
func (p *placement) stopDiffs(why error) {
	p.mu.Lock()
	cancel, released := p.diffCancel, p.diffReleased
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	if why != nil {
		cancel(why)
	}
	<-released
}

// setDiffsFor records the snapshot whose diffs are owed, while this is
// still the Run's placement here (a later one owns the Run's state file).
func (p *placement) setDiffsFor(snapID string) {
	if p.r.placement(p.runID, 0) != p {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != nil && p.state.DiffsFor != snapID {
		p.state.DiffsFor = snapID
		_ = writeRunState(p.dir, p.state)
	}
}

// snapshotDiffs computes every repository's diffs from the exited
// placement's state volumes, stores each patch as a blob, and reports them
// (snapshot.diffs). A repository it could not diff is reported with its
// error; nothing here affects the snapshot. released is closed once the
// helper container is gone.
func (p *placement) snapshotDiffs(ctx context.Context, snapID string, repos []proto.DiffRepo, released chan struct{}) {
	rep := proto.SnapshotDiffs{SnapshotID: snapID}
	budget, skip := p.r.diffBudget()
	if skip == "" {
		out, err := p.computeSnapshotDiffs(ctx, repos, budget)
		switch {
		case context.Cause(ctx) != nil:
			skip = context.Cause(ctx).Error()
		case err != nil:
			rep.Error = err.Error()
		default:
			rep.Diffs = out
		}
	}
	close(released)
	if skip != "" || rep.Error != "" {
		rep.Skipped = skip
		why := rep.Error
		if skip != "" {
			why = "not computed: " + skip
		}
		p.logf("snapshot diff", "skipped", skip, "err", rep.Error)
		for _, r := range repos {
			for _, k := range diffKinds {
				rep.Diffs = append(rep.Diffs, proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: r.Name, Kind: k, Base: baseOf(r, k), Error: why}})
			}
		}
	}
	p.reportDiffs(ctx, rep)
}

// diffsRecord names the snapshot record of a snapshot's diff blobs.
func diffsRecord(snapID string) string { return snapID + "-diffs" }

// reportDiffs reports a snapshot's diffs until luxd has them (or fences
// the placement off), then uploads their patches as it does a snapshot's
// blobs. The patches go if luxd never learns of them.
func (p *placement) reportDiffs(ctx context.Context, rep proto.SnapshotDiffs) {
	rec := &snapshotRecord{RunID: p.runID, Epoch: p.epoch, Created: time.Now().UnixMilli(), Diffs: true}
	for _, d := range rep.Diffs {
		if d.Blob != nil {
			rec.Uploads = append(rec.Uploads, pendingUpload{BlobID: d.Blob.BlobID, Path: p.r.blobPath(d.Blob.BlobID), Size: d.Blob.Size})
		}
	}
	id := diffsRecord(rep.SnapshotID)
	if err := p.r.saveSnapshotRecord(id, rec); err != nil {
		p.logf("snapshot diff: saving its record", "err", err)
		removeSnapshotFiles(p.r, id, rec)
		p.setDiffsFor("")
		return
	}
	// Bounded: a snapshot's diff is worth less than a runner that keeps
	// retrying forever for a luxd that refuses it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	for {
		// Not p.report: a stale nack here must not kill the container,
		// which may be a later placement's by now.
		err := p.r.conn.Report(rctx, proto.Frame{Type: proto.MsgSnapshotDiffs, RunID: p.runID, Epoch: p.epoch, Data: proto.Marshal(rep)})
		if err == nil {
			p.r.updateRecord(id, func(rec *snapshotRecord) { rec.Reported = true })
			p.r.uploads.kick()
			break
		}
		if errors.Is(err, errStale) || rctx.Err() != nil {
			p.logf("snapshot diff not reported", "err", err)
			removeSnapshotFiles(p.r, id, rec)
			break
		}
		time.Sleep(time.Second)
	}
	p.setDiffsFor("")
}

func baseOf(r proto.DiffRepo, kind string) string {
	if kind == proto.DiffBaseClone {
		return r.Base
	}
	return ""
}

// computeSnapshotDiffs runs the throwaway container: the Run's image, its
// state volumes read-only at their paths (a tmpfs for git's scratch
// files), the workload user, the Run's limits, no network, no
// capabilities. budget bounds it all; removing the container once it is
// up has its own short timeout.
func (p *placement) computeSnapshotDiffs(ctx context.Context, repos []proto.DiffRepo, budget time.Duration) ([]proto.SnapshotDiff, error) {
	user := p.userSpec()
	if user == "" || p.state.Image == "" {
		return nil, errors.New("the Run's image or user is not known (it never started here)")
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	name := containerName(p.runID) + "-diff"
	remove := func() {
		c, cancel := context.WithTimeout(context.Background(), diffCleanupTimeout)
		defer cancel()
		_ = p.r.pm.Remove(c, name)
	}
	// One a crashed runner left behind.
	_ = p.r.pm.Remove(ctx, name)
	defer remove()
	// The moment the budget ends (or the diff is cancelled), the container
	// goes: podman run then returns.
	defer context.AfterFunc(ctx, remove)()
	args := []string{"run", "--rm", "--name", name,
		"--label", LabelManaged + "=true", "--label", LabelRun + "=" + p.runID, "--label", LabelTenant + "=" + p.tenantID,
		"--userns=auto:size=65536", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network=none", "--cgroups=enabled", "--cgroupns=private", "--ipc=private", "--uts=private", "--pid=private",
		"--restart=no", "--hostname=lux", "--init=false", "--log-driver=none", "--pull=never",
		// Nothing it runs can change the checkout: the volumes are
		// read-only, the root too; git's copy of the index goes to /tmp.
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777", "-e", "TMPDIR=/tmp",
		"--user", user, "--workdir", "/", "--entrypoint", proto.ShimBinary,
		"-v", p.r.cfg.Shim + ":" + proto.ShimBinary + ":ro",
	}
	if p.assign != nil {
		res := p.assign.Spec.Resources
		args = append(args,
			"--cpus", fmt.Sprintf("%g", res.CPUs),
			"--memory", fmt.Sprintf("%d", int64(res.Memory)),
			"--memory-swap", fmt.Sprintf("%d", int64(res.Memory)),
			"--pids-limit", fmt.Sprintf("%d", res.Pids))
	}
	for _, v := range p.state.Volumes {
		if v.Kind == "state" {
			// nocopy: nothing from the image is copied into a volume.
			args = append(args, "-v", v.Volume+":"+v.Path+":ro,idmap,nocopy")
		}
	}
	args = append(args, p.state.Image)
	args = append(args, diffArgs(proto.DiffArgs{Repos: repos, Kinds: diffKinds})...)

	pr, pw := io.Pipe()
	// The reading ends with ctx, not only once podman does.
	defer context.AfterFunc(ctx, func() { pr.CloseWithError(context.Cause(ctx)) })()
	done := make(chan error, 1)
	go func() {
		err := p.r.pm.RunTo(ctx, pw, args...)
		pw.CloseWithError(err)
		done <- err
	}()
	var out []proto.SnapshotDiff
	incomplete, err := readDiffs(pr, repos, diffKinds, func(st proto.DiffStat, body io.Reader) error {
		d, err := p.diffBlob(st, body)
		if err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	pr.CloseWithError(errors.New("diff read ended"))
	if err != nil {
		cancel() // an unreadable stream: its container goes now
	}
	// podman run ends once its container is removed; it is not waited on
	// past the cleanup's own time.
	var perr error
	select {
	case perr = <-done:
	case <-ctx.Done():
		select {
		case perr = <-done:
		case <-time.After(diffCleanupTimeout):
			perr = errors.New("podman run did not end after its container was removed")
		}
	}
	if err == nil {
		err = perr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("the diff did not finish in %s", budget)
	}
	// Only whole repositories are kept: every kind, once each.
	kept := out[:0]
	for _, d := range out {
		if _, bad := incomplete[d.Repo]; bad || err != nil {
			if d.Blob != nil {
				os.Remove(p.r.blobPath(d.Blob.BlobID))
			}
			continue
		}
		kept = append(kept, d)
	}
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if why, bad := incomplete[r.Name]; bad {
			for _, k := range diffKinds {
				kept = append(kept, proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: r.Name, Kind: k, Base: baseOf(r, k), Error: why}})
			}
		}
	}
	return kept, nil
}

// diffBlob stores one patch as a blob; none for an empty patch.
func (p *placement) diffBlob(st proto.DiffStat, body io.Reader) (proto.SnapshotDiff, error) {
	d := proto.SnapshotDiff{DiffStat: st}
	if st.PatchBytes == 0 {
		return d, nil
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return d, err
	}
	sum := sha256.Sum256(buf.Bytes())
	d.PatchSHA256 = hex.EncodeToString(sum[:])
	blobID := ids.New(ids.Blob)
	size, blobSum, err := p.r.writeBlob(blobID, func(w io.Writer) error { _, err := w.Write(buf.Bytes()); return err })
	if err != nil {
		return d, err
	}
	d.Blob = &proto.BlobInfo{BlobID: blobID, Size: size, SHA256: blobSum}
	return d, nil
}
