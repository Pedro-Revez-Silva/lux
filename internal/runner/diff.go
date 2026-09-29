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

// diffTimeout bounds one diff: live, and each snapshot's.
const diffTimeout = 60 * time.Second

// diffKinds are what a snapshot stores: against the clone and against HEAD.
var diffKinds = []string{proto.DiffBaseClone, proto.DiffBaseHead}

// diffArgs is the command line that computes the diffs, after the shim.
func diffArgs(repos []proto.DiffRepo, kinds []string, statOnly bool) []string {
	a, _ := json.Marshal(proto.DiffArgs{Repos: repos, Kinds: kinds, StatOnly: statOnly, Limit: proto.DiffLimit})
	return []string{"diff", string(a)}
}

// readDiffs reads the shim's stream: at most one record per repository and
// kind, each patch at most the limit.
func readDiffs(r io.Reader, n int, fn func(proto.DiffStat, io.Reader) error) error {
	return gitdiff.ReadStream(r, n, proto.DiffLimit, fn)
}

// liveDiff computes a running placement's diffs in its container and sends
// each as it comes.
func (p *placement) liveDiff(ctx context.Context, req proto.DiffRequest, send func(proto.DiffResult) error) error {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	user := p.userSpec()
	if user == "" {
		return errors.New("the workload's user is not known yet")
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		args := append([]string{"exec", "--user", user, "--workdir", "/", containerName(p.runID), proto.ShimBinary},
			diffArgs(req.Repos, []string{req.Kind}, req.StatOnly)...)
		err := p.r.pm.RunTo(ctx, pw, args...)
		pw.CloseWithError(err)
		done <- err
	}()
	err := readDiffs(pr, len(req.Repos), func(st proto.DiffStat, body io.Reader) error {
		patch, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		return send(proto.DiffResult{SubID: req.SubID, Stat: st, Patch: patch})
	})
	pr.CloseWithError(errors.New("diff read ended"))
	cancel()
	if perr := <-done; err == nil && perr != nil {
		err = perr
	}
	return err
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

// snapshotDiffs computes every repository's diffs from the exited
// placement's state volumes, each patch a blob for the snapshot. It never
// fails the snapshot: whatever goes wrong is a diff.failed event, and each
// repository it could not diff is recorded with its error.
func (p *placement) snapshotDiffs(ctx context.Context) []proto.SnapshotDiff {
	repos := p.diffRepos()
	if len(repos) == 0 {
		return nil
	}
	out, err := p.computeSnapshotDiffs(ctx, repos)
	failed := map[string]bool{}
	for _, d := range out {
		if d.Error != "" && !failed[d.Repo] {
			failed[d.Repo] = true
			p.event(ctx, proto.EvDiffFailed, map[string]any{"repo": d.Repo, "kind": d.Kind, "error": d.Error})
		}
	}
	if err != nil {
		p.logf("diff failed", "err", err)
		p.event(ctx, proto.EvDiffFailed, map[string]any{"error": err.Error()})
		// Each repository without a result is recorded with the error, so
		// this snapshot is not taken for one without a diff.
		have := map[string]bool{}
		for _, d := range out {
			have[d.Repo+"\x00"+d.Kind] = true
		}
		for _, r := range repos {
			for _, k := range diffKinds {
				if !have[r.Name+"\x00"+k] {
					out = append(out, proto.SnapshotDiff{DiffStat: proto.DiffStat{Repo: r.Name, Kind: k, Base: baseOf(r, k), Error: err.Error()}})
				}
			}
		}
	}
	return out
}

func baseOf(r proto.DiffRepo, kind string) string {
	if kind == proto.DiffBaseClone {
		return r.Base
	}
	return ""
}

// computeSnapshotDiffs runs the throwaway container: the Run's image, its
// state volumes at their paths, the workload user, the Run's limits, no
// network, no capabilities, and a deadline.
func (p *placement) computeSnapshotDiffs(ctx context.Context, repos []proto.DiffRepo) ([]proto.SnapshotDiff, error) {
	user := p.userSpec()
	if user == "" || p.state.Image == "" {
		return nil, errors.New("the Run's image or user is not known (it never started here)")
	}
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	name := containerName(p.runID) + "-diff"
	// One a crashed runner left behind.
	_ = p.r.pm.Remove(context.WithoutCancel(ctx), name)
	defer p.r.pm.Remove(context.WithoutCancel(ctx), name)
	args := []string{"run", "--rm", "--name", name,
		"--label", LabelManaged + "=true", "--label", LabelRun + "=" + p.runID, "--label", LabelTenant + "=" + p.tenantID,
		"--userns=auto:size=65536", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network=none", "--cgroups=enabled", "--cgroupns=private", "--ipc=private", "--uts=private", "--pid=private",
		"--restart=no", "--hostname=lux", "--init=false", "--log-driver=none", "--pull=never",
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
			args = append(args, "-v", v.Volume+":"+v.Path+":idmap,nocopy")
		}
	}
	args = append(args, p.state.Image)
	args = append(args, diffArgs(repos, diffKinds, false)...)

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := p.r.pm.RunTo(ctx, pw, args...)
		pw.CloseWithError(err)
		done <- err
	}()
	var out []proto.SnapshotDiff
	err := readDiffs(pr, len(repos)*len(diffKinds), func(st proto.DiffStat, body io.Reader) error {
		d, err := p.diffBlob(st, body)
		if err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	pr.CloseWithError(errors.New("diff read ended"))
	if perr := <-done; err == nil && perr != nil {
		err = perr
	}
	if err != nil {
		for _, d := range out {
			if d.Blob != nil {
				os.Remove(p.r.blobPath(d.Blob.BlobID))
			}
		}
		return nil, err
	}
	return out, nil
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
