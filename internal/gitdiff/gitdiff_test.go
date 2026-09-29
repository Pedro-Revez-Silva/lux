package gitdiff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	for _, want := range []string{"+two", "+staged", "+unstaged", "+new", "-gone soon", "rename from rename-me.txt", "Binary files /dev/null and b/image.bin differ"} {
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
	// Applies to a fresh checkout at the base (but for the binary file: a
	// "Binary files differ" patch carries no content to apply).
	fresh := t.TempDir()
	run(t, fresh, "clone", "-q", dir, ".")
	run(t, fresh, "checkout", "-q", base)
	patch := filepath.Join(t.TempDir(), "p")
	os.WriteFile(patch, clone.Patch, 0o644)
	run(t, fresh, "apply", "--check", "--exclude=image.bin", patch)
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
	err = ReadStream(&buf, 4, proto.DiffLimit, func(st proto.DiffStat, r io.Reader) error {
		b, err := io.ReadAll(r)
		got, patches = append(got, st), append(patches, b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Repo != "gone" || got[0].Error == "" || got[2].Repo != "app" || got[2].Error != "" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(string(patches[2]), "+unstaged") || int64(len(patches[2])) != got[2].PatchBytes {
		t.Errorf("patch: %q", patches[2])
	}
}
