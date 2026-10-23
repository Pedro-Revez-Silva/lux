package gitws

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// Export writes the tree of commit r.Ref (a full sha), exactly as
// committed, into dst, an empty
// directory, with no .git: an image build's named context. It is fetched
// through the tenant's own mirror, as a checkout is; a commit no branch
// has (a pull request's head, say) is fetched by its sha into the
// mirror's refs/lux/commits/. At most limit bytes of file content are
// written. The tree is untrusted: it is written through an os.Root, so no
// entry, symlink or not, lands outside dst; symlinks are kept as links.
//
// Replacement objects are off throughout (--no-replace-objects, and
// GIT_NO_REPLACE_OBJECTS): the mirror holds whatever refs/replace/ the
// remote advertises, and with them a commit's sha would name another
// tree.
func (m *Manager) Export(ctx context.Context, r Repo, dst string, limit int64) error {
	if !shaRe.MatchString(r.Ref) {
		return fmt.Errorf("context %s: ref %q is not a full commit sha", r.Name, r.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	// A reader of the mirror (no repack meanwhile); ls-tree and cat-file read objects,
	// not the files a fetch writes and renames.
	mirror, release, err := m.mirror(ctx, r, false)
	if err != nil {
		return err
	}
	defer release()
	hasCommit := func() bool {
		cmd := exportGit(ctx, mirror, "cat-file", "-e", r.Ref+"^{commit}")
		return cmd.Run() == nil
	}
	if !hasCommit() {
		if err := m.fetchCommit(ctx, r, mirror); err != nil {
			return err
		}
		if !hasCommit() {
			return fmt.Errorf("commit %s not found in %s", r.Ref, Scrub(r.URL))
		}
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := lsTree(ctx, mirror, r.Ref)
	if err != nil {
		return err
	}
	blobs, err := newBlobReader(ctx, mirror)
	if err != nil {
		return err
	}
	werr := writeTree(root, entries, blobs.read, limit)
	if cerr := blobs.close(); werr == nil && cerr != nil {
		werr = fmt.Errorf("git cat-file: %w", cerr)
	}
	return werr
}

// fetchCommit fetches commit r.Ref by its sha into the mirror, as a
// fetch of the mirror does: serialized with other fetches, and alone with
// the mirror's files.
func (m *Manager) fetchCommit(ctx context.Context, r Repo, mirror string) error {
	l := m.lockFor(mirror)
	l.fetch.Lock()
	defer l.fetch.Unlock()
	l.files.Lock()
	defer l.files.Unlock()
	_, err := git(ctx, mirror, r.Token, append(noAutoGC, "fetch", "--quiet", "--no-tags", r.URL, "+"+r.Ref+":refs/lux/commits/"+r.Ref)...)
	return err
}

// ErrContextTooLarge: an exported tree has more than its limit.
var ErrContextTooLarge = errors.New("the context is larger than the Run's disk")

// treeEntry is one entry of a commit's whole tree (git ls-tree -r).
type treeEntry struct {
	mode, oid, path string
}

// lsTree lists every entry of commit's tree, recursively: the tree as
// committed. Not git archive, which applies the tree's own .gitattributes
// (export-ignore drops files, export-subst rewrites them with the
// repository's current branches and tags), so a pinned commit would not
// always give the same bytes.
func lsTree(ctx context.Context, mirror, commit string) ([]treeEntry, error) {
	cmd := exportGit(ctx, mirror, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", commit+"^{tree}")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var entries []treeEntry
	for _, rec := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected %q", rec)
		}
		entries = append(entries, treeEntry{mode: f[0], oid: f[2], path: name})
	}
	return entries, nil
}

// blobReader reads blobs, as stored (no filter, no conversion), from one
// git cat-file --batch.
type blobReader struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newBlobReader(ctx context.Context, mirror string) (*blobReader, error) {
	cmd := exportGit(ctx, mirror, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &blobReader{cmd: cmd, in: in, out: bufio.NewReader(out)}, nil
}

// read hands fn blob oid's content and size; what fn leaves unread is
// skipped.
func (b *blobReader) read(oid string, fn func(io.Reader, int64) error) error {
	if _, err := io.WriteString(b.in, oid+"\n"); err != nil {
		return err
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return err
	}
	f := strings.Fields(header)
	if len(f) != 3 || f[0] != oid || f[1] != "blob" {
		return fmt.Errorf("git cat-file: %s: %q", oid, strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("git cat-file: %s: bad size %q", oid, f[2])
	}
	lr := io.LimitReader(b.out, size)
	ferr := fn(lr, size)
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return err
	}
	if c, err := b.out.ReadByte(); err != nil || c != '\n' {
		return fmt.Errorf("git cat-file: %s: no end of object", oid)
	}
	return ferr
}

func (b *blobReader) close() error {
	b.in.Close()
	_, _ = io.Copy(io.Discard, b.out)
	return b.cmd.Wait()
}

// writeTree writes a commit's tree entries into root, byte for byte as
// committed: regular files (0755 when executable, else 0644), symlinks
// (as links; os.Root keeps everything, through them or not, inside root),
// and an empty directory for a submodule. At most limit bytes in all.
func writeTree(root *os.Root, entries []treeEntry, read func(string, func(io.Reader, int64) error) error, limit int64) error {
	var total int64
	for _, e := range entries {
		name := e.path
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("tree entry %q is outside the context", e.path)
		}
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		switch e.mode {
		case "100644", "100755":
			mode := os.FileMode(0o644)
			if e.mode == "100755" {
				mode = 0o755
			}
			err := read(e.oid, func(r io.Reader, size int64) error {
				if total += size; total > limit {
					return ErrContextTooLarge
				}
				f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
				if err != nil {
					return err
				}
				_, err = io.CopyN(f, r, size)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err == nil {
					// Not the umask's: COPY keeps modes, and a build is the
					// same on every host.
					err = root.Chmod(name, mode)
				}
				return err
			})
			if err != nil {
				return err
			}
		case "120000":
			err := read(e.oid, func(r io.Reader, size int64) error {
				if total += size; total > limit {
					return ErrContextTooLarge
				}
				target, err := io.ReadAll(r)
				if err != nil {
					return err
				}
				return root.Symlink(string(target), name)
			})
			if err != nil {
				return err
			}
		case "160000":
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tree entry %q: unexpected mode %s", e.path, e.mode)
		}
	}
	return FixDirModes(root)
}

// FixDirModes makes every directory under root 0755, whatever the umask
// made it: COPY keeps modes, and a build is the same on every host.
func FixDirModes(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return root.Chmod(name, 0o755)
		}
		return nil
	})
}

// exportGit is git run on the mirror for Export: replacement objects off,
// hooks off, and no GIT_ variable of the runner's environment (GIT_DIR,
// GIT_REPLACE_REF_BASE, ...) to point it elsewhere.
func exportGit(ctx context.Context, mirror string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = mirror
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}
