package gitws

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Export writes the tree of commit r.Ref (a full sha) into dst, an empty
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
	// A reader of the mirror (no repack meanwhile); archive reads objects,
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
	cmd := exportGit(ctx, mirror, "archive", "--format=tar", "--end-of-options", r.Ref)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	uerr := untar(root, out, limit)
	if uerr != nil {
		// Stop git rather than wait for it to write the rest.
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, out)
	}
	werr := cmd.Wait()
	if uerr != nil {
		return uerr
	}
	if werr != nil {
		return fmt.Errorf("git archive: %w: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return nil
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

// untar writes a git archive's entries into root: directories, regular
// files (0755 when executable, else 0644) and symlinks. Anything else (the
// pax global header with the commit id) is skipped.
func untar(root *os.Root, r io.Reader, limit int64) error {
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return FixDirModes(root)
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimSuffix(h.Name, "/"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry %q is outside the context", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > limit {
				return ErrContextTooLarge
			}
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return err
				}
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, h.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				// Not the umask's: COPY keeps modes, and a build is the
				// same on every host.
				err = root.Chmod(name, mode)
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return err
				}
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		}
	}
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
