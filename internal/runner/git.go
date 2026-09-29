package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// materializeRepos clones the Run's repositories into its workspace volume,
// on the host, before the container starts. A resumed Run's checkouts are
// already on its restored volume and are left exactly as they are.
// Credentials are the runner's; the container never sees them.
//
// Each clone is a git.clone event. A repository added on resume whose clone
// fails is dropped and the Run goes on without it (the agent keeps its
// conversation); any other failed clone fails the placement.
func (p *placement) materializeRepos(ctx context.Context, sp spec.RunSpec, user passwd.User) error {
	if sp.Git == nil {
		return nil
	}
	for _, r := range sp.Git.Repositories {
		res, err := p.materialize(ctx, r, user)
		if err != nil && ctx.Err() != nil {
			// Stopped meanwhile: not the repository's failure.
			return err
		}
		if err != nil {
			// gitws redacts the token from git's output; the URL is scrubbed
			// of userinfo too, should one ever get this far.
			msg := strings.ReplaceAll(err.Error(), r.URL, gitws.Scrub(r.URL))
			if err := p.reportClone(ctx, r, map[string]any{"status": "failed", "error": msg}); err != nil {
				return err
			}
			if r.AddedBy == "" {
				return fmt.Errorf("repository %s: %s", r.Name, msg)
			}
			p.logf("added repository not cloned; going on without it", "repo", r.Name, "err", msg)
			p.dropRepo(r.Name)
			continue
		}
		if res == nil {
			continue
		}
		if err := p.reportClone(ctx, r, map[string]any{"status": "cloned", "commit": res.Base}); err != nil {
			return err
		}
		p.event(ctx, "git.checkout", map[string]any{"repo": r.Name, "url": gitws.Scrub(r.URL),
			"ref": r.Ref, "branch": res.Branch, "base": res.Base})
	}
	return nil
}

// materialize clones one repository; nil when its checkout was already
// there.
func (p *placement) materialize(ctx context.Context, r spec.Repository, user passwd.User) (*gitws.Result, error) {
	mp, dir, err := p.checkoutDir(r.Path)
	if err != nil {
		return nil, err
	}
	res, err := p.r.git.Materialize(ctx, p.gitRepo(r), dir)
	if err != nil || !res.Cloned {
		return nil, err
	}
	// Cloned by the runner as root: a fresh clone, and the directories
	// made for it inside the volume, are handed to the workload user
	// (the volume is idmapped, so container uids are host uids).
	if err := chownTree(dir, user.UID, user.GID); err != nil {
		return nil, err
	}
	for d := filepath.Dir(dir); d != mp && strings.HasPrefix(d, mp+"/"); d = filepath.Dir(d) {
		_ = os.Lchown(d, user.UID, user.GID)
	}
	return res, nil
}

// checkoutDir is where a checkout goes on the host: inside its volume,
// whatever the volume holds. The volume is the workload's (restored from
// its snapshot on a resume), so a directory on the way may be a symlink
// it planted; the runner, root on the host, must never follow one out.
// Parents are made through an os.Root (which refuses to leave the
// volume), then resolved and checked to be inside it; the checkout itself
// may not be a symlink. The workload is not running meanwhile (its
// container starts after), so nothing changes between check and use.
func (p *placement) checkoutDir(path string) (mountpoint, dir string, err error) {
	vol := p.volumeRoot(path)
	mp, err := p.hostPath(vol)
	if err != nil {
		return "", "", err
	}
	if mp, err = filepath.EvalSymlinks(mp); err != nil {
		return "", "", err
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, vol), "/")
	if rel == "" {
		return "", "", fmt.Errorf("%s: a checkout needs a directory of its own on its volume", path)
	}
	root, err := os.OpenRoot(mp)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(mp, filepath.Dir(rel)))
	if err != nil {
		return "", "", err
	}
	if parent != mp && !strings.HasPrefix(parent, mp+"/") {
		return "", "", fmt.Errorf("%s: leads outside its volume", path)
	}
	dir = filepath.Join(parent, filepath.Base(rel))
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("%s is a symlink", path)
	}
	return mp, dir, nil
}

// reportClone sends a git.clone event, retrying: luxd drops a failed added
// repository from the Run's spec on it.
func (p *placement) reportClone(ctx context.Context, r spec.Repository, data map[string]any) error {
	data["repo"] = r.Name
	if r.AddedBy != "" {
		data["requestId"] = r.AddedBy
	}
	return p.reportRetrying(ctx, proto.RunEvent{Type: proto.EvGitClone, Data: data})
}

// dropRepo takes a repository out of this placement's spec (the assigned
// one and the one kept for a runner restart), so a push does not try it.
func (p *placement) dropRepo(name string) {
	drop := func(sp *spec.RunSpec) {
		if sp != nil && sp.Git != nil {
			sp.Git.Repositories = slices.DeleteFunc(slices.Clone(sp.Git.Repositories), func(r spec.Repository) bool { return r.Name == name })
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	drop(&p.assign.Spec)
	drop(p.state.Spec)
	_ = writeRunState(p.dir, p.state)
}

func (p *placement) gitRepo(r spec.Repository) gitws.Repo {
	return gitws.Repo{Tenant: p.tenantID, Name: r.Name, URL: r.URL, Ref: r.Ref, Token: p.assign.Secrets[r.Credential]}
}

// hostPath is where a container path on one of the Run's volumes is on the
// host.
// The most specific volume wins, as mounts nest.
func (p *placement) hostPath(path string) (string, error) {
	var best *volumeRef
	for i, v := range p.state.Volumes {
		if spec.Under(path, v.Path) && (best == nil || len(v.Path) > len(best.Path)) {
			best = &p.state.Volumes[i]
		}
	}
	if best != nil && best.Engine {
		return "", fmt.Errorf("%s is hidden by the nested engine store", path)
	}
	if best == nil {
		return "", fmt.Errorf("%s is not on a volume", path)
	}
	mp, err := p.r.mountpoint(context.Background(), best.Volume)
	if err != nil {
		return "", err
	}
	return filepath.Join(mp, strings.TrimPrefix(path, best.Path)), nil
}

// workloadUser resolves who the workload runs as: the spec's user or the
// image's, against the image's /etc/passwd, read by mounting the image
// (no container is started).
func (p *placement) workloadUser(ctx context.Context, sp spec.RunSpec, image string) (passwd.User, error) {
	name := sp.Workload.User
	if name == "" {
		out, _ := p.r.pm.Run(ctx, "image", "inspect", "--format", "{{.Config.User}}", image)
		name = strings.TrimSpace(string(out))
	}
	if name == "" || name == "root" || name == "0" {
		return passwd.Lookup(name, nil)
	}
	mnt, err := p.r.pm.Run(ctx, "image", "mount", image)
	if err != nil {
		return passwd.User{}, err
	}
	defer p.r.pm.Run(context.WithoutCancel(ctx), "image", "unmount", image)
	b, _ := os.ReadFile(filepath.Join(strings.TrimSpace(string(mnt)), "etc", "passwd"))
	return passwd.Lookup(name, b)
}

// workloadEnv is the part of the workload's environment the runner places
// things by (HOME, XDG_DATA_HOME), as the shim builds it: the image's ENV,
// then HOME set to the user's (the image's HOME never reaches the
// workload), then the spec's env over both. Only for nested Runs.
func (p *placement) workloadEnv(ctx context.Context, sp spec.RunSpec, image string) (map[string]string, error) {
	env := map[string]string{}
	if !sp.Sandbox.NestedContainers {
		return env, nil
	}
	out, err := p.r.pm.Run(ctx, "image", "inspect", "--format", "{{json .Config.Env}}", image)
	if err != nil {
		return nil, fmt.Errorf("inspect image env: %w", err)
	}
	var kvs []string
	if err := json.Unmarshal(out, &kvs); err != nil {
		return nil, fmt.Errorf("image env: %w", err)
	}
	for _, kv := range kvs {
		if k, v, _ := strings.Cut(kv, "="); k == "XDG_DATA_HOME" {
			env[k] = v
		}
	}
	for _, k := range []string{"HOME", "XDG_DATA_HOME"} {
		if v, ok := sp.Env[k]; ok {
			env[k] = v
		}
	}
	return env, nil
}

// madeParents returns the directories on the way to each engine store that
// its mount makes, as root: those the image does not have and, where a spec
// volume covers them, that volume (a restored state home, say) does not
// have either. Only those are handed to the workload; anything that exists
// stays as it is. Within the home that is every directory below it; for a
// store outside it ($XDG_DATA_HOME elsewhere) only the data home itself,
// the directory the workload was configured with. A failure to look fails
// the placement rather than leave the workload unable to write there.
func (p *placement) madeParents(ctx context.Context, image, home string, stores []volumeRef) ([]string, error) {
	if len(stores) == 0 {
		return nil, nil
	}
	mnt, err := p.r.pm.Run(ctx, "image", "mount", image)
	if err != nil {
		return nil, fmt.Errorf("mount image for engine parents: %w", err)
	}
	defer p.r.pm.Run(context.WithoutCancel(ctx), "image", "unmount", image)
	imageRoot, err := os.OpenRoot(strings.TrimSpace(string(mnt)))
	if err != nil {
		return nil, fmt.Errorf("open image for engine parents: %w", err)
	}
	defer imageRoot.Close()
	return parentsMissing(stores, home, p.state.Volumes, imageRoot, func(v volumeRef) (string, error) {
		return p.r.mountpoint(ctx, v.Volume)
	})
}

// parentsMissing is madeParents' walk, over an open image and the host
// mountpoints of the spec's volumes.
func parentsMissing(stores []volumeRef, home string, vols []volumeRef, imageRoot *os.Root, mountpoint func(volumeRef) (string, error)) ([]string, error) {
	var made []string
	for _, store := range stores {
		boundary := home
		if boundary == "" || boundary == "/" || !spec.Under(store.Path, boundary) {
			boundary = filepath.Dir(filepath.Dir(store.Path)) // $XDG_DATA_HOME's parent
		}
		for d := filepath.Dir(store.Path); d != boundary && spec.Under(d, boundary); d = filepath.Dir(d) {
			var selected *volumeRef
			for i := range vols {
				v := &vols[i]
				if !v.Engine && spec.Under(d, v.Path) && (selected == nil || len(v.Path) > len(selected.Path)) {
					selected = v
				}
			}
			root := imageRoot
			rel := strings.TrimPrefix(d, "/")
			if selected != nil {
				mp, err := mountpoint(*selected)
				if err != nil {
					return nil, err
				}
				if root, err = os.OpenRoot(mp); err != nil {
					return nil, err
				}
				rel = strings.TrimPrefix(strings.TrimPrefix(d, selected.Path), "/")
				if rel == "" {
					rel = "."
				}
			}
			_, statErr := root.Lstat(rel)
			if selected != nil {
				root.Close()
			}
			if statErr == nil {
				continue
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return nil, fmt.Errorf("inspect engine parent %s: %w", d, statErr)
			}
			if !slices.Contains(made, d) {
				made = append(made, d)
			}
		}
	}
	return made, nil
}

// volumeRoot is the container path of the volume a path is on.
func (p *placement) volumeRoot(path string) string {
	best := ""
	for _, v := range p.state.Volumes {
		if !v.Engine && spec.Under(path, v.Path) && len(v.Path) > len(best) {
			best = v.Path
		}
	}
	return best
}

func chownTree(root string, uid, gid int) error {
	if uid == 0 && gid == 0 {
		return nil
	}
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// push pushes each repository's current commit to the spec's push branch.
// The runner never runs git in the checkout (the workload controls its
// .git): the workload bundles its HEAD, as itself, inside its container,
// into the runtime volume; the runner pushes that bundle from a repository
// it owns, with its credential. One git.push event reports every result.
func (p *placement) push(ctx context.Context, req proto.Push) {
	sp := p.assign.Spec
	if sp.Git == nil || sp.Git.Push == nil {
		return
	}
	branch := sp.Git.Push.Branch
	var results []gitws.PushResult
	for _, r := range sp.Git.Repositories {
		if !r.Pushed() {
			results = append(results, gitws.PushResult{Repo: r.Name, Status: "skipped"})
			continue
		}
		res := gitws.PushResult{Repo: r.Name, Branch: branch, Status: "failed"}
		bundle, err := p.bundle(ctx, r)
		if err == nil {
			res = p.r.git.Push(ctx, p.gitRepo(r), bundle, branch, req.Leases[r.Name])
			os.Remove(bundle)
		} else {
			res.Error = err.Error()
		}
		results = append(results, res)
	}
	p.event(ctx, "git.push", map[string]any{"requestId": req.RequestID, "results": results})
}

// bundle has the workload's user write a git bundle of its checkout's HEAD
// into the runtime volume (inside the container, so the checkout's config
// and hooks run as the workload, never as the runner).
func (p *placement) bundle(ctx context.Context, r spec.Repository) (string, error) {
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return "", err
	}
	// A directory the workload user may write, on the runtime volume the
	// runner can read; emptied for each push.
	dir := filepath.Join(rt, "push")
	os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chown(dir, p.user.UID, p.user.GID); err != nil {
		return "", err
	}
	name := r.Name + ".bundle"
	user := fmt.Sprintf("%d:%d", p.user.UID, p.user.GID)
	out, err := p.r.pm.Run(ctx, "exec", "--user", user, "--workdir", r.Path, containerName(p.runID),
		"git", "bundle", "create", "--quiet", proto.ShimRunDir+"/push/"+name, "HEAD")
	if err != nil {
		return "", fmt.Errorf("git bundle in the container: %v %s", err, strings.TrimSpace(string(out)))
	}
	return filepath.Join(dir, name), nil
}
