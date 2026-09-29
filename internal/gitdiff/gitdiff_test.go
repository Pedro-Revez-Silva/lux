package gitdiff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkout is a repository at a base commit with a file of every kind of
// change on top: committed, staged, unstaged, untracked, deleted, renamed,
// binary, and an ignored file.
func checkout(t *testing.T) (dir, base string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir = t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, "committed.txt", "one\n")
	write(t, dir, "staged.txt", "one\n")
	write(t, dir, "unstaged.txt", "one\n")
	write(t, dir, "deleted.txt", "gone soon\n")
	write(t, dir, "rename-me.txt", strings.Repeat("a line that stays the same\n", 20))
	write(t, dir, ".gitignore", "*.log\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base = run(t, dir, "rev-parse", "HEAD")

	write(t, dir, "committed.txt", "one\ntwo\n")
	run(t, dir, "commit", "-q", "-am", "work")
	write(t, dir, "staged.txt", "one\nstaged\n")
	run(t, dir, "add", "staged.txt")
	write(t, dir, "unstaged.txt", "one\nunstaged\n")
	write(t, dir, "untracked.txt", "new\n")
	os.Remove(filepath.Join(dir, "deleted.txt"))
	run(t, dir, "mv", "rename-me.txt", "renamed.txt")
	write(t, dir, "image.bin", "\x00\x01\x02binary\x00")
	write(t, dir, "debug.log", "ignored\n")
	return dir, base
}

// digest hashes everything in .git and the working tree (paths, modes,
// contents), so a test can tell nothing changed.
func digest(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := rel + " " + info.Mode().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			line += " " + hex.EncodeToString(h[:]) + " " + info.ModTime().String()
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

func files(st proto.DiffStat) map[string]proto.DiffFile {
	m := map[string]proto.DiffFile{}
	for _, f := range st.FileStats {
		m[f.Path] = f
	}
	return m
}

func TestEveryKindOfChangeAgainstTheCloneBase(t *testing.T) {
	dir, base := checkout(t)
	before := digest(t, dir)
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone, proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	if after := digest(t, dir); after != before {
		t.Fatal("computing the diff changed the checkout (.git or working tree)")
	}
	clone := diffs[0]
	if clone.Stat.Error != "" {
		t.Fatal(clone.Stat.Error)
	}
	if clone.Stat.Base != base || clone.Stat.Head != run(t, dir, "rev-parse", "HEAD") {
		t.Errorf("base..head = %s..%s", clone.Stat.Base, clone.Stat.Head)
	}
	fs := files(clone.Stat)
	for _, want := range []string{"committed.txt", "staged.txt", "unstaged.txt", "untracked.txt", "deleted.txt", "renamed.txt", "image.bin"} {
		if _, ok := fs[want]; !ok {
			t.Errorf("%s missing from the stat: %+v", want, clone.Stat.FileStats)
		}
	}
	if _, ok := fs["debug.log"]; ok {
		t.Error("an ignored file is in the diff")
	}
	if f := fs["renamed.txt"]; f.OldPath != "rename-me.txt" {
		t.Errorf("rename not detected: %+v", f)
	}
	if !fs["image.bin"].Binary {
		t.Errorf("image.bin not binary: %+v", fs["image.bin"])
	}
	if clone.Stat.Files != 7 || clone.Stat.Insertions != 4 || clone.Stat.Deletions != 1 {
		t.Errorf("stat: %d files +%d -%d", clone.Stat.Files, clone.Stat.Insertions, clone.Stat.Deletions)
	}
	p := string(clone.Patch)
	for _, want := range []string{"+two", "+staged", "+unstaged", "+new", "-gone soon", "rename from rename-me.txt", "GIT binary patch"} {
		if !strings.Contains(p, want) {
			t.Errorf("patch lacks %q:\n%s", want, p)
		}
	}
	// Against HEAD: the committed change is not there, the rest is.
	head := files(diffs[1].Stat)
	if _, ok := head["committed.txt"]; ok || len(head) != 6 {
		t.Errorf("head diff: %+v", diffs[1].Stat.FileStats)
	}
	if strings.Contains(string(diffs[1].Patch), "+two") {
		t.Error("head diff has the committed change")
	}
	applyAndCompare(t, dir, base, clone.Patch)
	applyAndCompare(t, dir, clone.Stat.Head, diffs[1].Patch)
}

// tree is a working tree as git sees it, ignored files and .git aside:
// per path, its type, executable bit, and content (a symlink's target).
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	ignored := map[string]bool{}
	for _, p := range strings.Split(run(t, dir, "ls-files", "-oi", "--exclude-standard", "--directory"), "\n") {
		if p != "" {
			ignored[strings.TrimSuffix(p, "/")] = true
		}
	}
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == ".git" || ignored[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file x=%v %q", info.Mode()&0o111 != 0, b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// applyAndCompare applies patch to a fresh clone of dir at base and checks
// the result is dir's working tree: contents, types and modes.
func applyAndCompare(t *testing.T, dir, base string, patch []byte) {
	t.Helper()
	fresh := t.TempDir()
	run(t, fresh, "clone", "-q", "--no-checkout", dir, ".")
	run(t, fresh, "checkout", "-q", base)
	pf := filepath.Join(t.TempDir(), "p")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, fresh, "apply", "--allow-empty", pf)
	got, want := tree(t, fresh), tree(t, dir)
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: applied %.80s, workload %.80s", p, got[p], w)
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: applied has it (%.80s), workload does not", p, g)
		}
	}
}

// The patch is faithful: applied at the base it makes the workload's tree,
// for every kind of change git can carry.
func TestPatchAppliesFaithfully(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, "modified.txt", "one\ntwo\n")
	write(t, dir, "deleted.txt", "gone\n")
	write(t, dir, "rename-me.txt", strings.Repeat("a line that stays the same\n", 20))
	write(t, dir, "tool.sh", "#!/bin/sh\necho hi\n")
	write(t, dir, "crlf.txt", "one\r\ntwo\r\n")
	write(t, dir, "eol.txt", "one\ntwo\n")
	write(t, dir, "image.bin", "\x00\x01\x02 old binary \x00\xff")
	write(t, dir, ".gitignore", "*.log\n")
	if err := os.Symlink("modified.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")

	write(t, dir, "modified.txt", "one\n2\n")
	os.Remove(filepath.Join(dir, "deleted.txt"))
	run(t, dir, "mv", "rename-me.txt", "renamed.txt")
	write(t, dir, "added.txt", "added\n")
	write(t, dir, "image.bin", "\x00\x01\x02 new binary \x00\xfe\xfd")
	write(t, dir, "new.bin", "\x00\x00\x00\x07\x08")
	os.Remove(filepath.Join(dir, "link"))
	os.Symlink("added.txt", filepath.Join(dir, "link"))
	write(t, dir, "newdir/sub/file.txt", "deep\n")
	os.Symlink("../eol.txt", filepath.Join(dir, "newdir", "sub", "link2"))
	os.Chmod(filepath.Join(dir, "tool.sh"), 0o755)
	write(t, dir, "crlf.txt", "one\r\n2\r\nthree\r\n")
	write(t, dir, "eol.txt", "one\ntwo") // no final newline
	write(t, dir, "debug.log", "ignored\n")
	run(t, dir, "add", "newdir/sub/file.txt") // staged; the rest is not

	before := digest(t, dir)
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	if digest(t, dir) != before {
		t.Fatal("the checkout changed")
	}
	if diffs[0].Stat.Truncated {
		t.Fatal("truncated")
	}
	if !bytes.Contains(diffs[0].Patch, []byte("GIT binary patch")) {
		t.Errorf("no binary patch:\n%s", diffs[0].Patch)
	}
	applyAndCompare(t, dir, base, diffs[0].Patch)
}

// The checkout's own config and hooks name programs; none of them may run.
func TestWorkloadConfigRunsNothing(t *testing.T) {
	dir, base := checkout(t)
	marker := t.TempDir()
	sh := func(name string) string {
		p := filepath.Join(marker, name+".sh")
		os.WriteFile(p, []byte("#!/bin/sh\ntouch "+filepath.Join(marker, name+".ran")+"\ncat\n"), 0o755)
		return p
	}
	run(t, dir, "config", "diff.external", sh("external"))
	run(t, dir, "config", "core.pager", sh("pager"))
	run(t, dir, "config", "core.fsmonitor", sh("fsmonitor"))
	run(t, dir, "config", "filter.evil.clean", sh("clean"))
	run(t, dir, "config", "filter.evil.required", "true")
	run(t, dir, "config", "diff.evil.textconv", sh("textconv"))
	run(t, dir, "config", "diff.evil.command", sh("driver"))
	write(t, dir, ".gitattributes", "*.txt filter=evil diff=evil\n")
	for _, hook := range []string{"pre-commit", "post-index-change", "pre-auto-gc", "reference-transaction"} {
		write(t, dir, ".git/hooks/"+hook, "#!/bin/sh\ntouch "+filepath.Join(marker, hook+".ran")+"\n")
		os.Chmod(filepath.Join(dir, ".git/hooks", hook), 0o755)
	}
	before := digest(t, dir)
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	if diffs[0].Stat.Error != "" {
		t.Fatal(diffs[0].Stat.Error)
	}
	ran, _ := filepath.Glob(filepath.Join(marker, "*.ran"))
	if len(ran) > 0 {
		t.Errorf("workload programs ran: %v", ran)
	}
	if !strings.Contains(string(diffs[0].Patch), "diff --git a/staged.txt b/staged.txt") {
		t.Errorf("not a plain git patch:\n%s", diffs[0].Patch)
	}
	if digest(t, dir) != before {
		t.Error("the checkout changed")
	}
}

// Over the limit: the patch is cut at a file boundary and marked; the stat
// is still the whole diff's.
func TestTruncationKeepsTheWholeStat(t *testing.T) {
	dir, base := checkout(t)
	for _, n := range []string{"big1.txt", "big2.txt", "big3.txt"} {
		write(t, dir, n, strings.Repeat("0123456789abcdef\n", 400))
	}
	full, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(len(full[0].Patch) / 2)
	cut, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, limit)
	if err != nil {
		t.Fatal(err)
	}
	c, f := cut[0].Stat, full[0].Stat
	if !c.Truncated || f.Truncated {
		t.Fatalf("truncated: cut %v, full %v", c.Truncated, f.Truncated)
	}
	if int64(len(cut[0].Patch)) > limit || len(cut[0].Patch) == 0 || c.PatchBytes != int64(len(cut[0].Patch)) {
		t.Fatalf("kept %d bytes of a %d-byte limit", len(cut[0].Patch), limit)
	}
	if c.Files != f.Files || c.Insertions != f.Insertions || c.Deletions != f.Deletions || len(c.FileStats) != len(f.FileStats) {
		t.Errorf("stat changed with truncation: %+v vs %+v", c, f)
	}
	if !bytes.HasPrefix(full[0].Patch, cut[0].Patch) || !bytes.HasPrefix(full[0].Patch[len(cut[0].Patch):], []byte("diff --git ")) {
		t.Error("not cut at a file boundary")
	}
	// What is kept applies.
	fresh := t.TempDir()
	run(t, fresh, "clone", "-q", dir, ".")
	run(t, fresh, "checkout", "-q", base)
	pf := filepath.Join(t.TempDir(), "p")
	os.WriteFile(pf, cut[0].Patch, 0o644)
	run(t, fresh, "apply", pf)
}

// A cut between files: limits on either side of each boundary keep whole
// files only.
func TestTruncationBetweenFiles(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	write(t, dir, "a.txt", strings.Repeat("a\n", 50))
	write(t, dir, "b.bin", "\x00"+strings.Repeat("b", 500))
	write(t, dir, "c.txt", strings.Repeat("c\n", 50))
	full, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	p := full[0].Patch
	starts := []int{0}
	for i := 0; ; {
		j := bytes.Index(p[i+1:], []byte("\ndiff --git "))
		if j < 0 {
			break
		}
		i += j + 1
		starts = append(starts, i+1)
	}
	if len(starts) != 3 {
		t.Fatalf("%d files in\n%s", len(starts), p)
	}
	s1, s2 := int64(starts[1]), int64(starts[2])
	for _, limit := range []int64{int64(len(p)) - 1, s2 + 1, s2, s2 - 1, s1 + 1, s1} {
		cut, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, limit)
		if err != nil {
			t.Fatal(err)
		}
		want := p[:starts[2]]
		if limit < int64(starts[2]) {
			want = p[:starts[1]]
		}
		if !cut[0].Stat.Truncated || !bytes.Equal(cut[0].Patch, want) {
			t.Fatalf("limit %d: kept %d bytes, want %d", limit, len(cut[0].Patch), len(want))
		}
	}
}

// The first file alone is over the limit: an empty patch, marked, which
// applies (as nothing), and the whole stat.
func TestTruncationOfTheFirstFileKeepsNothing(t *testing.T) {
	dir, base := checkout(t)
	full, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	// first: the newline that ends the first file's diff.
	first := bytes.Index(full[0].Patch[1:], []byte("\ndiff --git ")) + 1
	for _, limit := range []int64{1, int64(first) / 2, int64(first), int64(first) + 1} {
		cut, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, limit)
		if err != nil {
			t.Fatal(err)
		}
		st := cut[0].Stat
		if limit <= int64(first) {
			if !st.Truncated || len(cut[0].Patch) != 0 || st.PatchBytes != 0 {
				t.Fatalf("limit %d: %d bytes kept, truncated %v", limit, len(cut[0].Patch), st.Truncated)
			}
			if st.Files != full[0].Stat.Files || st.Insertions != full[0].Stat.Insertions {
				t.Errorf("stat not whole: %+v", st)
			}
			fresh := t.TempDir()
			run(t, fresh, "init", "-q")
			pf := filepath.Join(t.TempDir(), "p")
			os.WriteFile(pf, cut[0].Patch, 0o644)
			run(t, fresh, "apply", "--check", "--allow-empty", pf)
		} else if !bytes.Equal(cut[0].Patch, full[0].Patch[:first+1]) {
			// Exactly the first file's diff and its newline fit.
			t.Errorf("limit %d: kept %q", limit, cut[0].Patch)
		}
	}
}

func TestNoChangeAndErrors(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, "a", "a\n")
	run(t, dir, "add", "a")
	run(t, dir, "commit", "-q", "-m", "a")
	base := run(t, dir, "rev-parse", "HEAD")
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone, proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if d.Stat.Error != "" || d.Stat.Files != 0 || len(d.Patch) != 0 {
			t.Errorf("%s: %+v %q", d.Stat.Kind, d.Stat, d.Patch)
		}
	}
	// An unknown base commit is that kind's error, not the call's.
	diffs, err = Compute(context.Background(), dir, strings.Repeat("0", 40), []string{proto.DiffBaseClone, proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error == "" || diffs[1].Stat.Error != "" {
		t.Errorf("unknown base: %v %+v", err, diffs)
	}
	// A path that is no checkout (deleted by the workload) fails the call.
	if _, err := Compute(context.Background(), filepath.Join(dir, "nope"), base, []string{proto.DiffBaseClone}, false, 1); err == nil {
		t.Error("no error for a missing checkout")
	}
}

// A TMPDIR inside the checkout (the workload's environment) receives none
// of the diff's scratch files: the patch is what it is without it, and
// the checkout, .git included, is byte for byte unchanged.
func TestTMPDIRInsideTheCheckout(t *testing.T) {
	dir, base := checkout(t)
	want, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, tmp := range []string{filepath.Join(dir, "tmp"), filepath.Join(dir, ".git")} {
		os.MkdirAll(tmp, 0o755)
		t.Setenv("TMPDIR", tmp)
		before := digest(t, dir)
		got, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
		if err != nil || got[0].Stat.Error != "" {
			t.Fatal(err, got)
		}
		if !bytes.Equal(got[0].Patch, want[0].Patch) || got[0].Stat.Files != want[0].Stat.Files {
			t.Errorf("TMPDIR=%s: the patch changed:\n%s", tmp, got[0].Patch)
		}
		if digest(t, dir) != before {
			t.Errorf("TMPDIR=%s: the checkout changed", tmp)
		}
	}
}

// A scratch root that resolves inside the checkout (a /tmp symlinked into
// it) is refused: an error, nothing written.
func TestScratchRootInsideTheCheckoutIsRefused(t *testing.T) {
	dir, base := checkout(t)
	os.MkdirAll(filepath.Join(dir, "scratch"), 0o755)
	link := filepath.Join(t.TempDir(), "tmp")
	for _, target := range []string{filepath.Join(dir, "scratch"), filepath.Join(dir, ".git")} {
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		old := scratchRoot
		scratchRoot = link
		before := digest(t, dir)
		_, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
		scratchRoot = old
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("scratch root %s -> %s: %v", link, target, err)
		}
		if digest(t, dir) != before {
			t.Errorf("scratch root -> %s: the checkout changed", target)
		}
	}
}

// Run's stream reads back record by record, patches intact; a repository
// that fails is a record with its error and the rest go on.
func TestStreamRoundTrip(t *testing.T) {
	dir, base := checkout(t)
	var buf bytes.Buffer
	err := Run(context.Background(), proto.DiffArgs{
		Repos: []proto.DiffRepo{{Name: "gone", Path: filepath.Join(dir, "missing")}, {Name: "app", Path: dir, Base: base}},
		Kinds: []string{proto.DiffBaseClone, proto.DiffBaseHead},
	}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	var got []proto.DiffStat
	var patches [][]byte
	incomplete, err := ReadStream(&buf, []string{"gone", "app"}, []string{proto.DiffBaseClone, proto.DiffBaseHead}, proto.DiffLimit, func(st proto.DiffStat, r io.Reader) error {
		b, err := io.ReadAll(r)
		got, patches = append(got, st), append(patches, b)
		return err
	})
	if err != nil || len(incomplete) != 0 {
		t.Fatal(err, incomplete)
	}
	if len(got) != 4 || got[0].Repo != "gone" || got[0].Error == "" || got[2].Repo != "app" || got[2].Error != "" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(string(patches[2]), "+unstaged") || int64(len(patches[2])) != got[2].PatchBytes {
		t.Errorf("patch: %q", patches[2])
	}
}

func record(t *testing.T, st proto.DiffStat, patch string) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteRecord(&b, Diff{Stat: st, Patch: []byte(patch)}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func readAll(stream string, repos, kinds []string) (map[string]string, map[string]string, error) {
	got := map[string]string{}
	incomplete, err := ReadStream(strings.NewReader(stream), repos, kinds, proto.DiffLimit, func(st proto.DiffStat, r io.Reader) error {
		b, err := io.ReadAll(r)
		got[st.Repo+"/"+st.Kind] = string(b)
		return err
	})
	return got, incomplete, err
}

// A stream is accepted only when it is whole: every patch its full length,
// exactly one record per repository and kind asked for.
func TestStreamMustBeComplete(t *testing.T) {
	kinds := []string{proto.DiffBaseClone}
	a := record(t, proto.DiffStat{Repo: "a", Kind: "clone", Files: 1}, "patch a\n")
	b := record(t, proto.DiffStat{Repo: "b", Kind: "clone", Files: 1}, "patch b\n")

	if got, inc, err := readAll(a+b, []string{"a", "b"}, kinds); err != nil || len(inc) != 0 || got["b/clone"] != "patch b\n" {
		t.Fatalf("whole: %v %v %v", got, inc, err)
	}
	// The body ends early (the process died mid-patch).
	short := a + b[:len(b)-3]
	if _, _, err := readAll(short, []string{"a", "b"}, kinds); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short body: %v", err)
	}
	// A header claiming more bytes than follow, fn reading it all.
	lying := strings.Replace(a, `"patchBytes":8`, `"patchBytes":800`, 1)
	if lying == a {
		t.Fatal("no patchBytes to change in " + a)
	}
	if _, _, err := readAll(lying, []string{"a"}, kinds); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("header over its body: %v", err)
	}
	// A header line cut short.
	if _, _, err := readAll(a+b[:10], []string{"a", "b"}, kinds); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut header: %v", err)
	}
	// The stream (a zero-exit shim) leaves a repository out: that one is
	// incomplete, the other whole.
	got, inc, err := readAll(a, []string{"a", "b"}, kinds)
	if err != nil || inc["b"] == "" || inc["a"] != "" || got["a/clone"] != "patch a\n" {
		t.Errorf("omitted: %v %v %v", got, inc, err)
	}
	// A kind left out is the same.
	if _, inc, _ := readAll(a, []string{"a"}, []string{"clone", "head"}); inc["a"] == "" {
		t.Errorf("omitted kind: %v", inc)
	}
	// A duplicate: that repository is incomplete, and fn saw it once.
	a2 := record(t, proto.DiffStat{Repo: "a", Kind: "clone", Files: 2}, "other\n")
	got, inc, err = readAll(a+a2, []string{"a", "b"}, kinds)
	if err != nil || !strings.Contains(inc["a"], "2 clone records") || got["a/clone"] != "patch a\n" {
		t.Errorf("duplicate: %v %v %v", got, inc, err)
	}
	// Something not asked for.
	if _, _, err := readAll(a+b, []string{"a"}, kinds); err == nil {
		t.Error("an unrequested repository was accepted")
	}
}

// numstat keeps maxFileStats entries but totals every file, fed in
// arbitrary pieces.
func TestNumstatOverTheEntryCap(t *testing.T) {
	var in bytes.Buffer
	n := maxFileStats + 5
	for i := range n {
		if i%1000 == 7 {
			fmt.Fprintf(&in, "1\t2\t\x00old%d\x00new%d\x00", i, i)
		} else if i%1000 == 8 {
			fmt.Fprintf(&in, "-\t-\tbin%d\x00", i)
		} else {
			fmt.Fprintf(&in, "3\t1\tf%d\x00", i)
		}
	}
	s := &numstat{maxBytes: maxStatBytes}
	w := &nulFields{fn: s.field}
	for b := in.Bytes(); len(b) > 0; {
		k := min(len(b), 7)
		if _, err := w.Write(b[:k]); err != nil {
			t.Fatal(err)
		}
		b = b[k:]
	}
	if err := errors.Join(w.end(), s.end()); err != nil {
		t.Fatal(err)
	}
	renames, bins := 10, 10 // i%1000 == 7 or 8 for i < 10005
	if s.n != n || len(s.files) != maxFileStats {
		t.Fatalf("%d files, %d entries", s.n, len(s.files))
	}
	plain := n - renames - bins
	if s.ins != 3*plain+renames || s.del != plain+2*renames {
		t.Errorf("totals +%d -%d", s.ins, s.del)
	}
	if f := s.files[7]; f.OldPath != "old7" || f.Path != "new7" || s.files[8].Path != "bin8" || !s.files[8].Binary {
		t.Errorf("entries: %+v %+v", f, s.files[8])
	}
	// Malformed or cut short: an error, not a wrong count.
	for _, bad := range []string{"3\t1\tf\x00x\x00", "3\t1\t\x00old\x00", "a\tb\tf\x00", "3\t1\tf"} {
		s := &numstat{}
		w := &nulFields{fn: s.field}
		_, err := w.Write([]byte(bad))
		if err == nil {
			err = errors.Join(w.end(), s.end())
		}
		if err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A field over the bound is refused as it arrives.
	if _, err := (&nulFields{fn: (&numstat{}).field}).Write(bytes.Repeat([]byte("x"), maxPath+1)); err == nil {
		t.Error("an unbounded field was buffered")
	}
}

// A legal diff's record always fits the reader's line limit: 10,000 files
// with paths near PATH_MAX would be ~40 MB of per-file stats, so the stats
// stop at maxStatBytes of JSON while the totals count every file.
func TestStatRecordIsBounded(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	run(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	deep := dir
	for i := range 14 {
		deep = filepath.Join(deep, fmt.Sprintf("%02d%s", i, strings.Repeat("d", 240)))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 10000
	for i := range n {
		if err := os.WriteFile(filepath.Join(deep, fmt.Sprintf("%05d%s", i, strings.Repeat("f", 200))), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := Run(context.Background(), proto.DiffArgs{Repos: []proto.DiffRepo{{Name: "app", Path: dir, Base: base}},
		Kinds: []string{proto.DiffBaseClone}, StatOnly: true}, &buf); err != nil {
		t.Fatal(err)
	}
	line, _, _ := bytes.Cut(buf.Bytes(), []byte("\n"))
	var st proto.DiffStat
	inc, err := ReadStream(&buf, []string{"app"}, []string{proto.DiffBaseClone}, proto.DiffLimit, func(s proto.DiffStat, _ io.Reader) error { st = s; return nil })
	if err != nil || len(inc) != 0 {
		t.Fatal(err, inc)
	}
	if st.Error != "" || st.Files != n || len(st.FileStats) == 0 || len(st.FileStats) >= n {
		t.Fatalf("files %d, %d stats, error %q", st.Files, len(st.FileStats), st.Error)
	}
	if len(line) > maxStatBytes+64<<10 {
		t.Errorf("a %d-byte record", len(line))
	}
	// The stats kept are the first ones, whole.
	if !strings.HasSuffix(st.FileStats[0].Path, "00000"+strings.Repeat("f", 200)) {
		t.Errorf("first stat: %.80s…", st.FileStats[0].Path)
	}
}

// The lists of paths a stat carries are bounded in bytes as well as count.
func TestBoundList(t *testing.T) {
	long := strings.Repeat("\x01", maxPath/2) // escaped 6 bytes each in JSON
	var paths []string
	for range maxListed {
		paths = append(paths, long)
	}
	got := boundList(paths)
	if len(got) == 0 || len(got) >= maxListed || jsonSize(got) > maxListedBytes+len(got) {
		t.Errorf("kept %d paths, %d bytes", len(got), jsonSize(got))
	}
	short := []string{"a", "b"}
	if got := boundList(short); len(got) != 2 {
		t.Errorf("short list cut: %v", got)
	}
	many := make([]string, maxListed+5)
	if got := boundList(many); len(got) != maxListed {
		t.Errorf("%d kept of %d", len(got), len(many))
	}
}

// git's stderr is held only up to its cap, however much it writes.
func TestGitStderrIsBounded(t *testing.T) {
	h := &head{max: 10}
	for range 1000 {
		h.Write(bytes.Repeat([]byte("e"), 1000))
	}
	if len(h.b) != 10 || !h.over {
		t.Errorf("kept %d bytes", len(h.b))
	}
}

// Filters are never run, but a diff says which changed files have one:
// they are compared raw.
func TestFiltersIgnoredAreReported(t *testing.T) {
	dir, base := checkout(t)
	marker := t.TempDir()
	clean := filepath.Join(marker, "clean.sh")
	os.WriteFile(clean, []byte("#!/bin/sh\ntouch "+filepath.Join(marker, "ran")+"\ntr a-z A-Z\n"), 0o755)
	run(t, dir, "config", "filter.up.clean", clean)
	run(t, dir, "config", "filter.up.smudge", "cat")
	write(t, dir, ".gitattributes", "staged.txt filter=up\nunstaged.txt filter=up\nuntouched.txt filter=up\n")
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	st := diffs[0].Stat
	if !st.FiltersIgnored || strings.Join(st.FilteredPaths, ",") != "staged.txt,unstaged.txt" {
		t.Errorf("filtersIgnored %v, paths %v", st.FiltersIgnored, st.FilteredPaths)
	}
	if !strings.Contains(string(diffs[0].Patch), "+unstaged") {
		t.Error("the filtered file was not compared raw")
	}
	if _, err := os.Stat(filepath.Join(marker, "ran")); err == nil {
		t.Error("the filter ran")
	}
	// No filter attributes: nothing reported.
	os.Remove(filepath.Join(dir, ".gitattributes"))
	diffs, _ = Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if diffs[0].Stat.FiltersIgnored || diffs[0].Stat.FilteredPaths != nil {
		t.Errorf("no attributes: %+v", diffs[0].Stat)
	}
}

// git config failing for any reason but "no match" is an error, not "no
// filters" (which would let a filter run). A git wrapper makes it fail.
func TestFilterConfigErrorIsAnError(t *testing.T) {
	dir, base := checkout(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
for a; do [ "$a" = --get-regexp ] && { echo "fatal: cannot read config" >&2; exit 128; }; done
exec `+real+` "$@"
`), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit); err == nil ||
		!strings.Contains(err.Error(), "cannot read config") {
		t.Errorf("a failed git config: %v", err)
	}
}

// A cone-mode sparse checkout: untracked files outside the cone are in the
// diff, and do not fail it.
func TestSparseCheckout(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, "in/a.txt", "a\n")
	write(t, dir, "out/b.txt", "b\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	run(t, dir, "sparse-checkout", "set", "--cone", "in")
	write(t, dir, "in/new.txt", "new in\n")
	write(t, dir, "out/new.txt", "new out\n")
	before := digest(t, dir)
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	if digest(t, dir) != before {
		t.Error("the checkout changed")
	}
	fs := files(diffs[0].Stat)
	if _, ok := fs["in/new.txt"]; !ok {
		t.Errorf("in/new.txt missing: %+v", diffs[0].Stat.FileStats)
	}
	if _, ok := fs["out/new.txt"]; !ok {
		t.Errorf("out/new.txt missing: %+v", diffs[0].Stat.FileStats)
	}
	// Files outside the cone that are not checked out are not deletions.
	if _, ok := fs["out/b.txt"]; ok || len(fs) != 2 {
		t.Errorf("stat: %+v", diffs[0].Stat.FileStats)
	}
}

// An unborn branch diffs from the empty tree; a HEAD naming a commit that
// is not there is an error for base=head, not an empty base.
func TestUnbornAndDamagedHead(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, "a.txt", "a\n")
	diffs, err := Compute(context.Background(), dir, "", []string{proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" || diffs[0].Stat.Files != 1 || diffs[0].Stat.Head != "" {
		t.Fatalf("unborn: %v %+v", err, diffs)
	}
	run(t, dir, "add", "a.txt")
	run(t, dir, "commit", "-q", "-m", "a")
	// The branch names a commit that is not in the repository.
	missing := strings.Repeat("1", 40)
	os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(missing+"\n"), 0o644)
	diffs, err = Compute(context.Background(), dir, "", []string{proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error == "" || diffs[0].Stat.Files != 0 {
		t.Errorf("missing HEAD commit: %v %+v", err, diffs)
	}
	// Detached at a missing commit.
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte(missing+"\n"), 0o644)
	diffs, err = Compute(context.Background(), dir, "", []string{proto.DiffBaseHead}, false, proto.DiffLimit)
	if err == nil && diffs[0].Stat.Error == "" {
		t.Errorf("detached at a missing commit: %+v", diffs)
	}
}

// A SHA-256 repository: an unborn HEAD diffs from its own empty tree, and
// a patch against a base applies.
func TestSHA256Repository(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q", "--object-format=sha256")
	write(t, dir, "a.txt", "a\n")
	diffs, err := Compute(context.Background(), dir, "", []string{proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" || diffs[0].Stat.Files != 1 {
		t.Fatalf("unborn: %v %+v", err, diffs)
	}
	run(t, dir, "add", "a.txt")
	run(t, dir, "commit", "-q", "-m", "a")
	base := run(t, dir, "rev-parse", "HEAD")
	if len(base) != 64 {
		t.Fatalf("not sha256: %s", base)
	}
	write(t, dir, "a.txt", "b\n")
	write(t, dir, "bin", "\x00\x01")
	diffs, err = Compute(context.Background(), dir, base, []string{proto.DiffBaseClone, proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" || diffs[1].Stat.Error != "" || diffs[0].Stat.Files != 2 {
		t.Fatalf("%v %+v", err, diffs)
	}
	applyAndCompare(t, dir, base, diffs[0].Patch)
}

// A checkout the workload damaged, or whose history no longer holds its
// base: an error for that repository (base_unreachable when the base is
// gone, with base=head still working), the checkout unchanged, and the
// stream going on to the next repository.
func TestDamagedCheckouts(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, base string) (wantCode string, headWorks bool){
		"reset to an unrelated commit": func(t *testing.T, dir, base string) (string, bool) {
			run(t, dir, "checkout", "-q", "--orphan", "other")
			run(t, dir, "commit", "-q", "-m", "unrelated")
			run(t, dir, "branch", "-D", "main")
			run(t, dir, "reflog", "expire", "--expire=now", "--all")
			run(t, dir, "gc", "-q", "--prune=now")
			return proto.DiffBaseUnreachable, true
		},
		"history rewritten, base still stored": func(t *testing.T, dir, base string) (string, bool) {
			run(t, dir, "checkout", "-q", "--orphan", "rewritten")
			run(t, dir, "commit", "-q", "-m", "rewritten")
			return proto.DiffBaseUnreachable, true
		},
		"shallow clone without the base": func(t *testing.T, dir, base string) (string, bool) {
			src := t.TempDir()
			run(t, src, "clone", "-q", "--bare", dir, ".")
			os.RemoveAll(dir)
			run(t, filepath.Dir(dir), "clone", "-q", "--depth=1", "file://"+src, dir)
			return proto.DiffBaseUnreachable, true
		},
		".git replaced by a fresh git init": func(t *testing.T, dir, base string) (string, bool) {
			os.RemoveAll(filepath.Join(dir, ".git"))
			run(t, dir, "init", "-q")
			return proto.DiffBaseUnreachable, true
		},
		".git deleted": func(t *testing.T, dir, base string) (string, bool) {
			os.RemoveAll(filepath.Join(dir, ".git"))
			return "", false
		},
		"the path replaced by a file": func(t *testing.T, dir, base string) (string, bool) {
			os.RemoveAll(dir)
			os.WriteFile(dir, []byte("a file\n"), 0o644)
			return "", false
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir, base := checkout(t)
			run(t, dir, "add", "-A")
			run(t, dir, "commit", "-q", "-m", "more")
			// A second commit on top of base, so a depth-1 clone lacks it.
			write(t, dir, "committed.txt", "three\n")
			run(t, dir, "commit", "-q", "-am", "more")
			wantCode, headWorks := damage(t, dir, base)
			other, otherBase := checkout(t)
			// The parent too: a file where the checkout was must stay one.
			before := digest(t, filepath.Dir(dir))
			var buf bytes.Buffer
			err := Run(context.Background(), proto.DiffArgs{
				Repos: []proto.DiffRepo{{Name: "bad", Path: dir, Base: base}, {Name: "ok", Path: other, Base: otherBase}},
				Kinds: []string{proto.DiffBaseClone, proto.DiffBaseHead},
			}, &buf)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]proto.DiffStat{}
			inc, err := ReadStream(&buf, []string{"bad", "ok"}, []string{proto.DiffBaseClone, proto.DiffBaseHead}, proto.DiffLimit,
				func(st proto.DiffStat, _ io.Reader) error { got[st.Repo+"/"+st.Kind] = st; return nil })
			if err != nil || len(inc) != 0 {
				t.Fatal(err, inc)
			}
			if digest(t, filepath.Dir(dir)) != before {
				t.Error("the checkout changed")
			}
			if c := got["bad/clone"]; c.Error == "" || c.ErrorCode != wantCode {
				t.Errorf("clone: error %q code %q, want code %q", c.Error, c.ErrorCode, wantCode)
			}
			if h := got["bad/head"]; (h.Error == "") != headWorks {
				t.Errorf("head: %+v", h)
			}
			if got["ok/clone"].Error != "" || got["ok/clone"].Files == 0 {
				t.Errorf("the next repository: %+v", got["ok/clone"])
			}
		})
	}
}

// A same-size change within the instant of the last index write (racy git)
// is still seen: git rehashes entries not older than its index, so the
// index copy must keep the index's time.
func TestRacyCleanChangeIsSeen(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	// Stat data is mtime (whole seconds) and size only: as on a git built
	// without nanosecond times, within one second.
	run(t, dir, "config", "core.checkStat", "minimal")
	write(t, dir, "a.txt", "a\n")
	at := time.Now().Truncate(time.Second)
	os.Chtimes(filepath.Join(dir, "a.txt"), at, at)
	run(t, dir, "add", "a.txt")
	run(t, dir, "commit", "-q", "-m", "a")
	base := run(t, dir, "rev-parse", "HEAD")
	// Same size, same mtime as the index entry, the index written in that
	// second too: only git's racy check can tell.
	write(t, dir, "a.txt", "b\n")
	os.Chtimes(filepath.Join(dir, "a.txt"), at, at)
	os.Chtimes(filepath.Join(dir, ".git", "index"), at, at)
	time.Sleep(1100 * time.Millisecond)
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Files != 1 {
		t.Fatalf("the change was missed: %v %+v", err, diffs)
	}
}

// With watchStdin, the diff's context ends when stdin does: the runner
// closes it to stop a live diff whose caller is gone.
func TestUntilEOF(t *testing.T) {
	r, w := io.Pipe()
	ctx := untilEOF(context.Background(), r)
	select {
	case <-ctx.Done():
		t.Fatal("ended with stdin open")
	case <-time.After(50 * time.Millisecond):
	}
	w.Close()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("did not end with stdin")
	}
}

// submoduleCheckout is a checkout at base with a submodule sub (its own
// repository beside it), and the submodule's second commit.
func submoduleCheckout(t *testing.T) (dir, base, subNext string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.Mkdir(sub, 0o755)
	run(t, sub, "init", "-q")
	write(t, sub, "f", "one\n")
	run(t, sub, "add", "-A")
	run(t, sub, "commit", "-q", "-m", "s1")
	write(t, sub, "g", "two\n")
	run(t, sub, "add", "-A")
	run(t, sub, "commit", "-q", "-m", "s2")
	subNext = run(t, sub, "rev-parse", "HEAD")
	run(t, sub, "checkout", "-q", "HEAD~1")
	dir = filepath.Join(root, "top")
	os.Mkdir(dir, 0o755)
	run(t, dir, "init", "-q")
	write(t, dir, "x.txt", "x\n")
	run(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	run(t, dir, "commit", "-q", "-m", "base")
	return dir, run(t, dir, "rev-parse", "HEAD"), subNext
}

// A changed gitlink (the submodule at another commit) is in the patch,
// which applies; changes inside a submodule are not (the parent's patch
// cannot carry them): the submodule is listed as dirty, and the patch
// still applies.
func TestSubmodules(t *testing.T) {
	dir, base, next := submoduleCheckout(t)
	run(t, filepath.Join(dir, "sub"), "checkout", "-q", next)
	write(t, dir, "x.txt", "x\ny\n")
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	st := diffs[0].Stat
	if _, ok := files(st)["sub"]; !ok || len(st.DirtySubmodules) != 0 {
		t.Errorf("committed gitlink change: %+v", st)
	}
	if !bytes.Contains(diffs[0].Patch, []byte("+Subproject commit "+next)) {
		t.Errorf("no gitlink in the patch:\n%s", diffs[0].Patch)
	}
	fresh := applyAt(t, dir, base, diffs[0].Patch, "--index")
	if got := run(t, fresh, "ls-files", "-s", "sub"); !strings.Contains(got, next) {
		t.Errorf("applied gitlink: %s", got)
	}

	// Dirty inside: an edit and an untracked file.
	write(t, dir, "sub/f", "one\nedited\n")
	write(t, dir, "sub/new", "new\n")
	diffs, err = Compute(context.Background(), dir, base, []string{proto.DiffBaseClone, proto.DiffBaseHead}, false, proto.DiffLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if d.Stat.Error != "" || !slices.Equal(d.Stat.DirtySubmodules, []string{"sub"}) {
			t.Errorf("%s: %+v", d.Stat.Kind, d.Stat)
		}
		if bytes.Contains(d.Patch, []byte("edited")) {
			t.Errorf("%s: the submodule's own change is in the parent's patch", d.Stat.Kind)
		}
	}
	applyAt(t, dir, base, diffs[0].Patch, "--index")
}

// applyAt applies patch (with args) to a fresh clone of dir at base, and
// returns the clone.
func applyAt(t *testing.T, dir, base string, patch []byte, args ...string) string {
	t.Helper()
	fresh := t.TempDir()
	run(t, fresh, "clone", "-q", "--no-checkout", dir, ".")
	run(t, fresh, "checkout", "-q", base)
	pf := filepath.Join(t.TempDir(), "p")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, fresh, append(append([]string{"apply", "--allow-empty"}, args...), pf)...)
	return fresh
}

// With text=auto, git compares a file as it would store it: a CRLF file
// whose lines did not change shows no change, and a CRLF file that did
// change applies with LF endings. Both are listed (normalizedPaths); a
// file git stores as it is is not.
func TestLineEndingNormalization(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, ".gitattributes", "* text=auto\n*.bin -text\n")
	write(t, dir, "same.txt", "a\nb\n")
	write(t, dir, "edited.txt", "a\n")
	write(t, dir, "plain.txt", "p\n")
	write(t, dir, "raw.bin", "r\r\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	write(t, dir, "same.txt", "a\r\nb\r\n")   // only the endings: hidden
	write(t, dir, "edited.txt", "a\r\nb\r\n") // and a line
	write(t, dir, "plain.txt", "p\nq\n")      // no conversion
	write(t, dir, "raw.bin", "r\r\ns\r\n")    // -text: stored as it is
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	st := diffs[0].Stat
	if _, ok := files(st)["same.txt"]; ok {
		t.Errorf("an endings-only change shows as changed: %+v", st.FileStats)
	}
	got := slices.Sorted(slices.Values(st.NormalizedPaths))
	if !slices.Equal(got, []string{"edited.txt", "same.txt"}) {
		t.Errorf("normalizedPaths %v", st.NormalizedPaths)
	}
	// Applied at the base, the patch gives git's stored form of the files.
	fresh := applyAt(t, dir, base, diffs[0].Patch)
	for name, want := range map[string]string{"edited.txt": "a\nb\n", "same.txt": "a\nb\n", "plain.txt": "p\nq\n", "raw.bin": "r\r\ns\r\n"} {
		if b, _ := os.ReadFile(filepath.Join(fresh, name)); string(b) != want {
			t.Errorf("%s applied: %q, want %q", name, b, want)
		}
	}
}

// export-ignore only affects git archive: such files are in the diff like
// any other, and the patch reproduces them.
func TestExportIgnoreIsDiffed(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	write(t, dir, ".gitattributes", "internal/** export-ignore\nsecret.txt export-ignore\n")
	write(t, dir, "secret.txt", "one\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base := run(t, dir, "rev-parse", "HEAD")
	write(t, dir, "secret.txt", "one\ntwo\n")
	write(t, dir, "internal/new.txt", "new\n")
	diffs, err := Compute(context.Background(), dir, base, []string{proto.DiffBaseClone}, false, proto.DiffLimit)
	if err != nil || diffs[0].Stat.Error != "" {
		t.Fatal(err, diffs)
	}
	fs := files(diffs[0].Stat)
	if _, ok := fs["secret.txt"]; !ok {
		t.Errorf("secret.txt missing: %+v", diffs[0].Stat.FileStats)
	}
	if _, ok := fs["internal/new.txt"]; !ok {
		t.Errorf("internal/new.txt missing: %+v", diffs[0].Stat.FileStats)
	}
	applyAndCompare(t, dir, base, diffs[0].Patch)
}
