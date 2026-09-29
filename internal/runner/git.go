package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
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
		p.setGitBase(r.Name, res.Base)
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
// host. The most specific volume wins, as mounts nest; a path under an
// engine store is hidden by it.
func (p *placement) hostPath(path string) (string, error) {
	best := deepestVolume(p.state.Volumes, path)
	if e := deepestVolume(p.state.EngineVolumes, path); e != nil && (best == nil || len(e.Path) > len(best.Path)) {
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

// deepestVolume is the volume a container path is on: the most specific
// one, as mounts nest. nil if none.
func deepestVolume(vols []volumeRef, path string) *volumeRef {
	var best *volumeRef
	for i := range vols {
		if spec.Under(path, vols[i].Path) && (best == nil || len(vols[i].Path) > len(best.Path)) {
			best = &vols[i]
		}
	}
	return best
}

// openImage mounts image once for a placement's setup: whoever needs to read
// it (the workload's passwd, copy-up, the engine parents walk) goes through
// the returned root. close unmounts it; calling it again does nothing.
func (p *placement) openImage(ctx context.Context, image string) (root *os.Root, close func(), err error) {
	mnt, err := p.r.pm.Run(ctx, "image", "mount", image)
	if err != nil {
		return nil, nil, fmt.Errorf("mount image: %w", err)
	}
	unmount := func() { p.r.pm.Run(context.WithoutCancel(ctx), "image", "unmount", image) }
	if root, err = os.OpenRoot(strings.TrimSpace(string(mnt))); err != nil {
		unmount()
		return nil, nil, fmt.Errorf("open image: %w", err)
	}
	return root, sync.OnceFunc(func() { root.Close(); unmount() }), nil
}

// workloadUser resolves who the workload runs as: the spec's user or the
// image's, against the image's /etc/passwd.
func workloadUser(sp spec.RunSpec, info podman.ImageInfo, image *os.Root) (passwd.User, error) {
	name := sp.Workload.User
	if name == "" {
		name = strings.TrimSpace(info.User)
	}
	if name == "" || name == "root" || name == "0" {
		return passwd.Lookup(name, nil)
	}
	b, _ := image.ReadFile("etc/passwd")
	return passwd.Lookup(name, b)
}

// workloadEnv is what the runner places things by in the workload's
// environment (HOME, XDG_DATA_HOME), as the shim builds it: the image's
// XDG_DATA_HOME (its HOME never reaches the workload), then the spec's env.
func workloadEnv(sp spec.RunSpec, info podman.ImageInfo) map[string]string {
	env := map[string]string{}
	for _, kv := range info.Env {
		if k, v, _ := strings.Cut(kv, "="); k == "XDG_DATA_HOME" {
			env[k] = v
		}
	}
	for _, k := range []string{"HOME", "XDG_DATA_HOME"} {
		if v, ok := sp.Env[k]; ok {
			env[k] = v
		}
	}
	return env
}

// madeParents are the directories on the way to each engine store that its
// mount makes, as root: missing from the image and from any spec volume
// over them (a restored state home). Only those are the workload's to have.
// Below the home that is every such directory; for a store outside it, only
// the data home itself. The rules are in docs/podman.md.
func madeParents(stores, vols []volumeRef, home string, image *os.Root, mountpoint func(volumeRef) (string, error)) ([]string, error) {
	var made []string
	for _, store := range stores {
		boundary := home
		if boundary == "" || boundary == "/" || !spec.Under(store.Path, boundary) {
			boundary = filepath.Dir(filepath.Dir(store.Path)) // the data home's parent
		}
		for d := filepath.Dir(store.Path); d != boundary && spec.Under(d, boundary); d = filepath.Dir(d) {
			exists, err := existsIn(d, vols, image, mountpoint)
			if err != nil {
				return nil, fmt.Errorf("inspect engine parent %s: %w", d, err)
			}
			if !exists && !slices.Contains(made, d) {
				made = append(made, d)
			}
		}
	}
	return made, nil
}

// existsIn: container path d is in whatever provides it before the engine
// mounts, the deepest spec volume over it, else the image.
func existsIn(d string, vols []volumeRef, image *os.Root, mountpoint func(volumeRef) (string, error)) (bool, error) {
	root, rel := image, strings.TrimPrefix(d, "/")
	if v := deepestVolume(vols, d); v != nil {
		mp, err := mountpoint(*v)
		if err != nil {
			return false, err
		}
		if root, err = os.OpenRoot(mp); err != nil {
			return false, err
		}
		defer root.Close()
		if rel = strings.TrimPrefix(strings.TrimPrefix(d, v.Path), "/"); rel == "" {
			rel = "."
		}
	}
	_, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// volumeRoot is the container path of the volume a path is on.
func (p *placement) volumeRoot(path string) string {
	if v := deepestVolume(p.state.Volumes, path); v != nil {
		return v.Path
	}
	return ""
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
