// Package gitws materializes a Run's git repositories and pushes its work,
// on the host. Git credentials are used here, by the runner, and never
// enter the container: the checkout's remote is the plain URL, and fetches
// and pushes authenticate through a one-off header that is never written
// to disk.
//
// The runner never runs git inside a checkout once the workload has had
// it: the workload controls its .git (config, hooks), and git run there as
// root with the token would run the workload's hooks as root and follow
// its config (insteadOf, proxies) with the token. So a push takes a bundle
// the workload produced (as itself, inside its container), unpacks it into
// a runner-owned bare repository, and pushes from there.
package gitws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Repo is one repository to materialize or push to.
type Repo struct {
	// Tenant owns the mirror it is fetched into: mirrors are never shared
	// between tenants, since one may hold what another can't read.
	Tenant     string
	Name       string
	URL        string
	Ref        string // branch, tag or sha
	Token      string // credential for fetch and push; "" for public repos
	Bundle     string
	HeadBranch string
}

// Result is what was checked out.
type Result struct {
	Name   string `json:"name"`
	Cloned bool   `json:"-"`                // false when the checkout already existed (a resume)
	Branch string `json:"branch,omitempty"` // the branch checked out, if Ref is one
	Base   string `json:"base"`             // the commit checked out
}

type Manager struct {
	mirrors string
	mu      sync.Mutex
	locks   map[string]*mirrorLock
	known   map[string]bool // mirrored "tenant\x00url"
}

// mirrorLock guards one mirror against the two ways a local clone of it
// breaks. The clone lists the refs, then objects/, then copies each file.
// A fetch writes temporary files in objects/ (tmp_obj_*, tmp_pack_*) and
// renames or deletes them, so the copy dies on one that vanished. gc packs
// and deletes loose objects, so the clone ends with a ref to an object it
// never copied ("trying to write ref ... with nonexistent object"). So:
//   - files: held exclusively by a fetch, shared by clones copying objects/.
//   - gc: held exclusively by a repack, shared by every reader (clones, and
//     push/sync scratch repositories borrowing objects through alternates,
//     which a fetch does not disturb).
//   - fetch serializes fetches and creation; fetchedAt (when the last
//     successful fetch started) lets callers that queued behind a fetch
//     use it instead of fetching again.
type mirrorLock struct {
	fetch     sync.Mutex
	fetchedAt time.Time
	files     sync.RWMutex
	gc        sync.RWMutex
}

func New(dataDir string) *Manager {
	m := &Manager{mirrors: filepath.Join(dataDir, "mirrors"), locks: map[string]*mirrorLock{}, known: map[string]bool{}}
	entries, _ := os.ReadDir(m.mirrors)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(m.mirrors, e.Name(), "lux-url"))
		if err != nil {
			continue
		}
		t, _ := os.ReadFile(filepath.Join(m.mirrors, e.Name(), "lux-tenant"))
		if len(t) == 0 {
			// From before mirrors had owners: owned by nobody, so used by no
			// one; removed so it can't be taken for anyone's.
			os.RemoveAll(filepath.Join(m.mirrors, e.Name()))
			continue
		}
		m.known[string(t)+"\x00"+strings.TrimSpace(string(b))] = true
	}
	return m
}

func (m *Manager) lockFor(path string) *mirrorLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[path]
	if l == nil {
		l = &mirrorLock{}
		m.locks[path] = l
	}
	return l
}

// Mirrors lists the URLs this host has mirrors of, whoever's: a scheduling
// hint only (a Run lands where its repositories are likely warm; its
// tenant's own mirror is the only one it ever clones from).
func (m *Manager) Mirrors() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for k := range m.known {
		_, u, _ := strings.Cut(k, "\x00")
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func (m *Manager) mirrorPath(tenant, u string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + u))
	return filepath.Join(m.mirrors, hex.EncodeToString(h[:12])+".git")
}

// git runs git on a repository the runner owns, with a token as a one-off
// Authorization header: in this process's environment for this command
// only, never in a config file or a URL. Hooks are off: nothing in a
// repository runs code as the runner.
func git(ctx context.Context, dir, token string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[REDACTED]")
		}
		return msg, fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// noAutoGC keeps a fetch from starting gc or maintenance by itself: by
// default that runs detached, after the fetch has returned, outside
// mirrorLock.gc. update runs gc itself, only when no one reads the mirror.
var noAutoGC = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}

// mirror makes sure the host has a mirror of the repository holding ref,
// and returns it held: no gc until the caller calls release, after its
// last read of the mirror, and when cloning (a copy of objects/) no fetch
// either. A branch is fetched every time (it moves), unless a fetch that
// started after this call already did; a full sha already in the mirror
// is not.
func (m *Manager) mirror(ctx context.Context, r Repo, cloning bool) (path string, release func(), err error) {
	if r.Tenant == "" {
		return "", nil, fmt.Errorf("repository %s: no tenant", r.Name)
	}
	asked := time.Now()
	path = m.mirrorPath(r.Tenant, r.URL)
	l := m.lockFor(path)
	have := false
	if shaRe.MatchString(r.Ref) {
		_, err := git(ctx, path, "", "cat-file", "-e", r.Ref+"^{commit}")
		have = err == nil
	}
	if !have {
		if err := m.update(ctx, r, path, l, asked); err != nil {
			return "", nil, err
		}
	}
	// A fetch may land before the caller reads. It leaves the mirror whole,
	// and a ref it prunes or moves did so upstream.
	if cloning {
		l.files.RLock()
	}
	l.gc.RLock()
	return path, func() {
		l.gc.RUnlock()
		if cloning {
			l.files.RUnlock()
		}
	}, nil
}

// update fetches into the mirror, or creates it.
func (m *Manager) update(ctx context.Context, r Repo, path string, l *mirrorLock, asked time.Time) error {
	l.fetch.Lock()
	defer l.fetch.Unlock()
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if !l.fetchedAt.Before(asked) {
			// Fetched since this caller asked: as fresh as a fetch now.
			return nil
		}
		l.files.Lock()
		defer l.files.Unlock()
		started := time.Now()
		if _, err := git(ctx, path, r.Token, append(noAutoGC, "fetch", "--prune", "--tags", r.URL, "+refs/heads/*:refs/heads/*")...); err != nil {
			return err
		}
		l.fetchedAt = started
		// Then a repack if git thinks one is due, attached, and only while
		// nothing reads the mirror; otherwise a later fetch does it. Best
		// effort: the fetch succeeded, so a failed gc fails no Run.
		if l.gc.TryLock() {
			git(ctx, path, "", "-c", "gc.autoDetach=false", "gc", "--auto", "--quiet")
			l.gc.Unlock()
		}
		return nil
	}
	if err := os.MkdirAll(m.mirrors, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.RemoveAll(tmp)
	started := time.Now()
	if _, err := git(ctx, m.mirrors, r.Token, "clone", "--mirror", "--quiet", r.URL, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	// The URL is kept for Mirrors(), never with a credential.
	if err := os.WriteFile(filepath.Join(tmp, "lux-url"), []byte(r.URL), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "lux-tenant"), []byte(r.Tenant), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	l.fetchedAt = started
	m.mu.Lock()
	m.known[r.Tenant+"\x00"+r.URL] = true
	m.mu.Unlock()
	return nil
}

// Materialize clones a repository into dir (on the Run's workspace
// volume), at r.Ref, and returns what it checked out. An existing checkout
// is left exactly as it is (it holds the Run's work) and not looked into:
// the workload controls it now.
func (m *Manager) Materialize(ctx context.Context, r Repo, dir string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return &Result{Name: r.Name}, nil
	}
	mirror := r.Bundle
	release := func() {}
	var err error
	if mirror == "" {
		if mirror, release, err = m.mirror(ctx, r, true); err != nil {
			return nil, err
		}
	}
	release = sync.OnceFunc(release)
	defer release()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	// Built beside dir and renamed into place when complete: a clone that
	// fails half-way leaves nothing a later start would take for a
	// finished checkout.
	final := dir
	dir = filepath.Join(filepath.Dir(final), ".lux-clone-"+filepath.Base(final))
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	// --no-hardlinks: the clone must stand alone on the volume.
	if _, err := git(ctx, filepath.Dir(dir), "", "clone", "--quiet", "--no-hardlinks", "--no-checkout", mirror, dir); err != nil {
		return nil, err
	}
	ref := r.Ref
	if ref == "" || ref == "HEAD" {
		if r.Bundle != "" {
			ref = r.HeadBranch
			if ref == "" {
				ref = "HEAD"
			}
		} else if ref, err = git(ctx, mirror, "", "symbolic-ref", "--short", "HEAD"); err != nil {
			return nil, err
		}
	}
	// The clone stands alone: nothing below reads the mirror.
	release()
	// The remote the workload sees is the real URL, with no credential.
	if _, err := git(ctx, dir, "", "remote", "set-url", "origin", r.URL); err != nil {
		return nil, err
	}
	res := &Result{Name: r.Name, Cloned: true}
	// A branch is checked out as a local branch tracking origin; a tag or
	// sha detached. --end-of-options: a ref is never read as an option.
	if _, err := git(ctx, dir, "", "rev-parse", "--verify", "-q", "refs/remotes/origin/"+ref); err == nil {
		if _, err := git(ctx, dir, "", "checkout", "--quiet", "-B", ref, "--end-of-options", "origin/"+ref); err != nil {
			return nil, err
		}
		res.Branch = ref
	} else if _, err := git(ctx, dir, "", "checkout", "--quiet", "--detach", "--end-of-options", ref); err != nil {
		return nil, fmt.Errorf("ref %q not found in %s", ref, Scrub(r.URL))
	}
	if res.Base, err = git(ctx, dir, "", "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	if err := os.Rename(dir, final); err != nil {
		return nil, err
	}
	return res, nil
}

// PushResult says what a push did. Operation, when refused: the git
// operation in progress in the checkout (proto.OperationOf).
type PushResult struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit,omitempty"`
	Status    string `json:"status"` // pushed | up-to-date | rejected | refused | failed | skipped
	Operation string `json:"operation,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Push pushes the commit in bundle (made by the workload from its HEAD) to
// branch on the real remote, from a runner-owned repository.
//
// The lease is explicit: replace the branch only if it is still at lease,
// the commit this Run last pushed there ("" the first time: only if the
// branch does not exist). A bare --force-with-lease cannot work: pushing to
// a URL has no remote-tracking ref to compare against.
func (m *Manager) Push(ctx context.Context, r Repo, bundle, branch, lease string) PushResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := PushResult{Repo: r.Name, Branch: branch, Status: "failed"}
	mirror, release, err := m.mirror(ctx, Repo{Tenant: r.Tenant, Name: r.Name, URL: r.URL, Ref: branch, Token: r.Token}, false)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// The scratch repository borrows the mirror's objects (alternates).
	defer release()
	// The bundle's commits land in a scratch repository, never in the
	// shared mirror.
	work, err := m.scratch(ctx, mirror, "push-")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer os.RemoveAll(work)
	if _, err := git(ctx, work, "", "fetch", "--quiet", bundle, "+HEAD:refs/lux/push"); err != nil {
		res.Error = "the workload's commit could not be read: " + err.Error()
		return res
	}
	head, err := git(ctx, work, "", "rev-parse", "refs/lux/push")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Commit = head
	// Always asked of the remote, even when head is the lease: the lease
	// may be a caller's expectation, not where the branch is.
	ref := "refs/heads/" + branch
	out, err := git(ctx, work, r.Token, "push", "--porcelain", "--force-with-lease="+ref+":"+lease, r.URL, "refs/lux/push:"+ref)
	if err != nil {
		res.Error = out
		// Only a failed lease means the branch moved; a server refusing the
		// push (a hook, a protected branch) keeps its own message.
		if strings.Contains(out, "stale info") || strings.Contains(out, "[rejected]") && strings.Contains(out, "fetch first") {
			res.Status = "rejected"
			res.Error = "the branch moved since this Run last pushed it: not overwritten"
		}
		return res
	}
	res.Status = "pushed"
	if pushUpToDate(out) {
		res.Status = "up-to-date"
	}
	return res
}

// pushUpToDate: the --porcelain output of a push that changed nothing (the
// remote already had the commit) flags its ref line "=".
func pushUpToDate(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "=\t") || strings.Contains(l, "[up to date]") {
			return true
		}
	}
	return false
}

// scratch makes a private bare repository under the mirrors, sharing
// mirror's objects (alternates); the caller removes it.
func (m *Manager) scratch(ctx context.Context, mirror, prefix string) (string, error) {
	work, err := os.MkdirTemp(m.mirrors, prefix)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, work, "", "init", "--quiet", "--bare"); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	alt := filepath.Join(work, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(mirror, "objects")+"\n"), 0o600); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	return work, nil
}

// Scrub removes anything credential-like a URL might carry, for display.
func Scrub(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.User == nil {
		return u
	}
	p.User = nil
	return p.String()
}

// Target is a commit a sync moves a checkout to, in a bundle the
// workload's user can read: Commit as refs/lux/sync, with its history
// down to Base.
type Target struct {
	Commit string
	// Branch: the ref is this branch (checked out as it); "" for a tag or
	// sha (checked out detached).
	Branch string
	// Base: the commit the bundle's history stops at (its prerequisite,
	// which the checkout must have); "" for a bundle of the whole history.
	Base string
}

// SyncBundle fetches r.Ref through the host's mirror (with the runner's
// credential) and writes a bundle of it to bundle. The bundle carries no
// credential and no remote: the workload's user fetches from it inside
// its container, so the runner never runs git in the checkout.
//
// base is a commit the checkout is known to have (its clone or last sync);
// the bundle then holds only what is not in base's history. With base ""
// or unknown to the mirror, the bundle holds the whole history.
func (m *Manager) SyncBundle(ctx context.Context, r Repo, base, bundle string) (Target, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var t Target
	if r.Ref == "" {
		return t, fmt.Errorf("repository %s: no ref to sync to", r.Name)
	}
	mirror, release, err := m.mirror(ctx, r, false)
	if err != nil {
		return t, err
	}
	defer release()
	if _, err := git(ctx, mirror, "", "rev-parse", "--verify", "-q", "refs/heads/"+r.Ref); err == nil {
		t.Branch = r.Ref
	}
	if t.Commit, err = git(ctx, mirror, "", "rev-parse", "--verify", "-q", r.Ref+"^{commit}"); err != nil {
		return t, fmt.Errorf("ref %q not found in %s", r.Ref, Scrub(r.URL))
	}
	// A scratch repository, so the shared mirror gets no ref of a Run's.
	work, err := m.scratch(ctx, mirror, "sync-")
	if err != nil {
		return t, err
	}
	defer os.RemoveAll(work)
	if _, err := git(ctx, work, "", "update-ref", "refs/lux/sync", t.Commit); err != nil {
		return t, err
	}
	create := []string{"bundle", "create", "--quiet", bundle, "refs/lux/sync"}
	if exclude := syncPrerequisites(ctx, work, base, t.Commit); len(exclude) > 0 {
		t.Base = base
		for _, c := range exclude {
			create = append(create, "^"+c)
		}
	}
	if _, err := git(ctx, work, "", create...); err != nil {
		return t, err
	}
	return t, os.Chmod(bundle, 0o644)
}

// syncPrerequisites is what a bundle of commit leaves out, given that the
// checkout has base: base itself, or, when commit is already in base's
// history (an empty bundle git refuses to write), commit's parents. nil
// means the whole history: no base, base not in the mirror, or a root
// commit.
func syncPrerequisites(ctx context.Context, work, base, commit string) []string {
	if !shaRe.MatchString(base) {
		return nil
	}
	if _, err := git(ctx, work, "", "cat-file", "-e", base+"^{commit}"); err != nil {
		return nil
	}
	if _, err := git(ctx, work, "", "merge-base", "--is-ancestor", commit, base); err != nil {
		return []string{base}
	}
	parents, err := git(ctx, work, "", "rev-parse", commit+"^@")
	if err != nil || parents == "" {
		return nil
	}
	return strings.Fields(parents)
}
