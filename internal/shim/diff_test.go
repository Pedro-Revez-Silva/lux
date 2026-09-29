package shim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, dir, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a repository with files, committed; it returns its path and
// that commit.
func newRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		write(t, dir, name, content, mode)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "init")
	return dir, gitT(t, dir, "rev-parse", "HEAD")
}

// manifest is a working tree as git sees it, .git aside: per path its
// type, executable bit, and content or link target.
func manifest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		fi, _ := os.Lstat(p)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			lines = append(lines, "link "+rel+" -> "+target)
		default:
			b, _ := os.ReadFile(p)
			kind := "file"
			if fi.Mode()&0o100 != 0 {
				kind = "exec"
			}
			lines = append(lines, fmt.Sprintf("%s %s %x", kind, rel, sha256.Sum256(b)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// applyAt applies patch to a fresh clone of repo at base and returns the
// result's manifest.
func applyAt(t *testing.T, repo, base string, patch []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	gitT(t, filepath.Dir(dir), "clone", "-q", repo, dir)
	gitT(t, dir, "checkout", "-q", base)
	pf := filepath.Join(t.TempDir(), "p")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "apply", "--allow-empty", pf)
	return manifest(t, dir)
}

func diffOne(t *testing.T, dir, cloneBase, kind string) proto.RepoDiff {
	t.Helper()
	res := diffRepos(context.Background(), proto.DiffArgs{Base: kind, Repos: []proto.DiffRepo{{Name: "app", Path: dir, Base: cloneBase}}})
	if res.Error != "" || len(res.Repos) != 1 {
		t.Fatalf("%+v", res)
	}
	if res.Repos[0].Error != "" {
		t.Fatal(res.Repos[0].Error)
	}
	return res.Repos[0]
}

// Every kind of change, tracked and untracked: the patch applied at the
// base is the working tree, and the counts are git's.
func TestDiffEveryChange(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n", "b.txt": "bee\n", "gone.txt": "bye\n",
		"run.sh": "#!/bin/sh\n", "img.bin": "\x00\x01old", "mode.txt": "m\n", ".gitignore": "*.log\n"})
	// Committed.
	write(t, dir, "c.txt", "committed\n", 0o644)
	gitT(t, dir, "add", "c.txt")
	gitT(t, dir, "commit", "-qm", "c")
	// Staged, unstaged, deleted, mode change, binary, tracked symlink.
	write(t, dir, "a.txt", "one\nstaged\n", 0o644)
	gitT(t, dir, "add", "a.txt")
	write(t, dir, "b.txt", "bee\nunstaged\n", 0o644)
	os.Remove(filepath.Join(dir, "gone.txt"))
	os.Chmod(filepath.Join(dir, "mode.txt"), 0o755)
	write(t, dir, "img.bin", "\x00\x02new", 0o644)
	symlink(t, dir, "tracked-link", "a.txt")
	gitT(t, dir, "add", "tracked-link")
	// Untracked: text, symlink, nested dir, a name with spaces and
	// unicode, one without a final newline, an empty one; one ignored.
	write(t, dir, "new.txt", "new\nfile\n", 0o644)
	symlink(t, dir, "link", "b.txt")
	write(t, dir, "deep/er/x.sh", "#!/bin/sh\necho x\n", 0o755)
	write(t, dir, "sp ace é.txt", "unicode\n", 0o644)
	write(t, dir, "tab\there.txt", "no newline", 0o644)
	write(t, dir, "empty", "", 0o644)
	write(t, dir, "debug.log", "ignored\n", 0o644)

	d := diffOne(t, dir, base, proto.DiffBaseClone)
	if d.Base != base || d.Head != gitT(t, dir, "rev-parse", "HEAD") || d.Truncated {
		t.Fatalf("base %s head %s truncated %v", d.Base, d.Head, d.Truncated)
	}
	patch := string(d.Patch)
	for _, want := range []string{"+committed", "+staged", "+unstaged", "-bye", "old mode 100644\nnew mode 100755",
		"GIT binary patch", "new file mode 120000", "+++ b/sp ace é.txt\t", `"b/tab\there.txt"`, "diff --git a/deep/er/x.sh b/deep/er/x.sh\nnew file mode 100755",
		"diff --git a/empty b/empty\nnew file mode 100644\n"} {
		if !strings.Contains(patch, want) {
			t.Errorf("no %q in\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "debug.log") {
		t.Error("an ignored file is in the patch")
	}
	// Tracked: a b c gone img mode tracked-link (7; 5 insertions: staged,
	// unstaged, committed, the link; 1 deletion). Untracked: new link x.sh
	// sp-ace tab empty (6; 2+1+2+1+1 insertions).
	if d.Files != 13 || d.Insertions != 4+7 || d.Deletions != 1 {
		t.Errorf("files %d +%d -%d", d.Files, d.Insertions, d.Deletions)
	}
	os.Remove(filepath.Join(dir, "debug.log"))
	if got, want := applyAt(t, dir, base, d.Patch), manifest(t, dir); got != want {
		t.Fatalf("applied:\n%s\nworkload:\n%s", got, want)
	}

	// Against HEAD: not the commit.
	h := diffOne(t, dir, base, proto.DiffBaseHead)
	if h.Base != d.Head || strings.Contains(string(h.Patch), "+committed") || h.Files != 12 {
		t.Fatalf("base %s files %d\n%s", h.Base, h.Files, h.Patch)
	}
	if got, want := applyAt(t, dir, h.Base, h.Patch), manifest(t, dir); got != want {
		t.Fatalf("applied:\n%s\nworkload:\n%s", got, want)
	}
}

// An untracked binary file is named, not carried: the patch is truncated.
func TestDiffUntrackedBinary(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n"})
	write(t, dir, "data.bin", "\x00\x01\xffbin\x00", 0o644)
	d := diffOne(t, dir, base, proto.DiffBaseClone)
	want := "diff --git a/data.bin b/data.bin\nnew file mode 100644\nBinary files /dev/null and b/data.bin differ\n"
	if string(d.Patch) != want || !d.Truncated || d.Files != 1 || d.Insertions != 0 {
		t.Fatalf("%+v\n%s", d, d.Patch)
	}
}

func lines(n int) string { return strings.Repeat(strings.Repeat("x", 63)+"\n", n) }

// An untracked file of exactly 32 KiB is whole; one byte more is cut at
// 32 KiB and marked.
func TestDiffUntrackedCap(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n"})
	write(t, dir, "exact.txt", lines(512), 0o644)
	d := diffOne(t, dir, base, proto.DiffBaseClone)
	if d.Truncated || strings.Contains(string(d.Patch), "lux: truncated") || d.Insertions != 512 {
		t.Fatalf("truncated %v +%d", d.Truncated, d.Insertions)
	}
	if got, want := applyAt(t, dir, base, d.Patch), manifest(t, dir); got != want {
		t.Fatal("32 KiB exactly does not apply")
	}
	write(t, dir, "exact.txt", lines(512)+"y", 0o644)
	d = diffOne(t, dir, base, proto.DiffBaseClone)
	want := "@@ -0,0 +1,512 @@\n" + strings.ReplaceAll(lines(512), strings.Repeat("x", 63), "+"+strings.Repeat("x", 63)) +
		"\\ lux: truncated at 32768 of 32769 bytes\n"
	if !d.Truncated || !strings.HasSuffix(string(d.Patch), want) {
		t.Fatalf("truncated %v, ends %q", d.Truncated, string(d.Patch)[max(0, len(d.Patch)-200):])
	}
}

// Past 1 MiB in all, untracked files are cut at 8 KiB instead; tracked
// changes never are.
func TestDiffSwitchesToSmallCap(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"big.txt": "old\n"})
	write(t, dir, "big.txt", lines(8192), 0o644) // 512 KiB, tracked
	for i := range 20 {
		write(t, dir, fmt.Sprintf("u%02d.txt", i), lines(1024), 0o644) // 64 KiB each
	}
	d := diffOne(t, dir, base, proto.DiffBaseClone)
	if !d.Truncated || strings.Count(string(d.Patch), "\\ lux: truncated at 8192 of 65536 bytes\n") != 20 ||
		!strings.Contains(string(d.Patch), "@@ -1 +1,8192 @@") || d.Insertions != 8192+20*128 {
		t.Fatalf("truncated %v, +%d, %d bytes", d.Truncated, d.Insertions, len(d.Patch))
	}
	// Under 1 MiB with 32 KiB each: not switched.
	for i := 8; i < 20; i++ {
		os.Remove(filepath.Join(dir, fmt.Sprintf("u%02d.txt", i)))
	}
	d = diffOne(t, dir, base, proto.DiffBaseClone)
	if strings.Count(string(d.Patch), "\\ lux: truncated at 32768 of 65536 bytes\n") != 8 {
		t.Fatalf("%d bytes", len(d.Patch))
	}
}

// Over 16 MiB in all, the diff is refused: two repositories of 9 MiB each.
func TestDiffHardLimit(t *testing.T) {
	var repos []proto.DiffRepo
	for _, name := range []string{"one", "two"} {
		dir, base := newRepo(t, map[string]string{"a.txt": "one\n"})
		write(t, dir, "a.txt", lines(9<<20/64), 0o644)
		repos = append(repos, proto.DiffRepo{Name: name, Path: dir, Base: base})
	}
	res := diffRepos(context.Background(), proto.DiffArgs{Base: proto.DiffBaseClone, Repos: repos})
	if !strings.Contains(res.Error, "larger than 16 MiB") || res.Repos != nil {
		t.Fatalf("%+v", res.Error)
	}
}

// With no commit yet, --base head diffs from the empty tree.
func TestDiffUnbornHead(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	write(t, dir, "staged.txt", "s\n", 0o644)
	gitT(t, dir, "add", "staged.txt")
	write(t, dir, "loose.txt", "l\n", 0o644)
	d := diffOne(t, dir, "", proto.DiffBaseHead)
	if d.Head != "" || d.Base != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" || d.Files != 2 ||
		!strings.Contains(string(d.Patch), "+s\n") || !strings.Contains(string(d.Patch), "+l\n") {
		t.Fatalf("%+v\n%s", d, d.Patch)
	}
	// --base clone without a known clone commit says so.
	res := diffRepos(context.Background(), proto.DiffArgs{Base: proto.DiffBaseClone, Repos: []proto.DiffRepo{{Name: "app", Path: dir}}})
	if !strings.Contains(res.Repos[0].Error, "--base head") {
		t.Fatalf("%+v", res.Repos[0])
	}
}

// Changes to files marked assume-unchanged or skip-worktree are not in
// diff-index's output: the diff names them and is truncated.
func TestDiffFlaggedEntriesOmitted(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n", "assumed.txt": "a\n", "skipped.txt": "s\n"})
	gitT(t, dir, "update-index", "--assume-unchanged", "assumed.txt")
	gitT(t, dir, "update-index", "--skip-worktree", "skipped.txt")
	write(t, dir, "assumed.txt", "changed\n", 0o644)
	write(t, dir, "skipped.txt", "changed\n", 0o644)
	d := diffOne(t, dir, base, proto.DiffBaseClone)
	if !d.Truncated || d.OmittedCount != 2 || strings.Join(d.Omitted, ",") != "assumed.txt,skipped.txt" || d.Files != 0 {
		t.Fatalf("%+v", d)
	}
}

// A repository that fails does not fail the others.
func TestDiffOneRepoFails(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n"})
	write(t, dir, "a.txt", "two\n", 0o644)
	res := diffRepos(context.Background(), proto.DiffArgs{Base: proto.DiffBaseClone, Repos: []proto.DiffRepo{
		{Name: "gone", Path: filepath.Join(dir, "nope"), Base: base}, {Name: "app", Path: dir, Base: base}}})
	if len(res.Repos) != 2 || res.Repos[0].Error == "" || res.Repos[1].Error != "" || res.Repos[1].Files != 1 {
		t.Fatalf("%+v", res)
	}
}

// Nothing in the checkout is written: its status, and its index's bytes
// and mtime, are the same after a diff, though its stat data is stale
// (git diff would refresh the index).
func TestDiffWritesNothing(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"a.txt": "one\n", "b.txt": "bee\n"})
	write(t, dir, "a.txt", "two\n", 0o644)
	gitT(t, dir, "add", "a.txt")
	write(t, dir, "new.txt", "new\n", 0o644)
	index := filepath.Join(dir, ".git", "index")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "b.txt"), old, old)
	os.Chtimes(index, old.Add(-time.Hour), old.Add(-time.Hour))
	// gitT's status runs with GIT_OPTIONAL_LOCKS=0: it does not refresh
	// the index itself.
	snap := func() string {
		b, _ := os.ReadFile(index)
		fi, _ := os.Stat(index)
		ents, _ := os.ReadDir(filepath.Join(dir, ".git"))
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		return fmt.Sprintf("%s\n%x %v\n%v", gitT(t, dir, "status", "--porcelain"), sha256.Sum256(b), fi.ModTime(), names)
	}
	before := snap()
	for _, kind := range []string{proto.DiffBaseClone, proto.DiffBaseHead} {
		diffOne(t, dir, base, kind)
	}
	if after := snap(); after != before {
		t.Fatalf("before:\n%s\nafter:\n%s", before, after)
	}
	if !bytes.Contains([]byte(before), []byte("?? new.txt")) {
		t.Fatal(before)
	}
}
