package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// `lux-shim diff <json>`: each repository's diff from its base to its
// working tree, tracked and untracked, as one proto.DiffResult on stdout.
// The runner runs it as the workload user inside the Run's container.
// Nothing is written in the checkout: diff-index and ls-files never
// refresh the index, as `git diff` does even with GIT_OPTIONAL_LOCKS=0.

const (
	untrackedCap      = 32 << 10 // per untracked file
	untrackedCapSmall = 8 << 10  // per untracked file, once the diff passes diffSoftLimit
	diffSoftLimit     = 1 << 20
	diffHardLimit     = 16 << 20
	binaryPrefix      = 8000 // as git: a NUL in the first 8000 bytes means binary
)

var errTooLarge = fmt.Errorf("the diff is larger than %d MiB, more than lux diff returns", diffHardLimit>>20)

// Diff is `lux-shim diff`'s main.
func Diff(args []string) int {
	var a proto.DiffArgs
	if len(args) != 1 || json.Unmarshal([]byte(args[0]), &a) != nil {
		fmt.Fprintln(os.Stderr, "usage: lux-shim diff <json>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	if err := json.NewEncoder(os.Stdout).Encode(diffRepos(ctx, a)); err != nil {
		return 1
	}
	return 0
}

type repoWork struct {
	d          proto.RepoDiff
	path       string
	tracked    []byte
	untracked  []string
	extra      []byte // the untracked files' patches, at the current cap
	extraIns   int
	extraStats []proto.DiffFileStat
	cut        bool
}

func diffRepos(ctx context.Context, a proto.DiffArgs) proto.DiffResult {
	work := make([]*repoWork, len(a.Repos))
	for i, r := range a.Repos {
		w := &repoWork{d: proto.RepoDiff{Repo: r.Name}, path: r.Path}
		if err := w.trackedDiff(ctx, r, a.Base); err != nil {
			if errors.Is(err, errTooLarge) {
				return proto.DiffResult{Error: err.Error()}
			}
			w.d.Error = err.Error()
		}
		work[i] = w
	}
	// Untracked files at 32 KiB each; if the whole diff then passes 1 MiB,
	// at 8 KiB each instead.
	var size int
	for _, limit := range []int64{untrackedCap, untrackedCapSmall} {
		size = 0
		for _, w := range work {
			if w.d.Error == "" {
				if err := w.untrackedDiff(limit); err != nil {
					w.d.Error = err.Error()
				}
			}
			size += len(w.tracked) + len(w.extra)
		}
		if size <= diffSoftLimit {
			break
		}
	}
	if size > diffHardLimit {
		return proto.DiffResult{Error: errTooLarge.Error()}
	}
	res := proto.DiffResult{Repos: []proto.RepoDiff{}}
	for _, w := range work {
		if w.d.Error == "" {
			w.d.Patch = append(w.tracked, w.extra...)
			w.d.Files += len(w.untracked)
			w.d.Insertions += w.extraIns
			w.d.FileStats = append(w.d.FileStats, w.extraStats...)
			w.d.Truncated = w.cut || w.d.OmittedCount > 0
		}
		res.Repos = append(res.Repos, w.d)
	}
	return res
}

func (w *repoWork) trackedDiff(ctx context.Context, r proto.DiffRepo, base string) error {
	if _, err := os.Stat(r.Path); err != nil {
		return fmt.Errorf("the checkout: %w", err)
	}
	// Unborn: no HEAD yet.
	if head, err := git(ctx, r.Path, 1<<10, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		w.d.Head = strings.TrimSpace(string(head))
	}
	switch {
	case base == proto.DiffBaseHead && w.d.Head != "":
		w.d.Base = w.d.Head
	case base == proto.DiffBaseHead:
		empty, err := git(ctx, r.Path, 1<<10, "hash-object", "-t", "tree", "/dev/null")
		if err != nil {
			return err
		}
		w.d.Base = strings.TrimSpace(string(empty))
	case r.Base == "":
		return errors.New("the commit it was cloned at is not known; lux diff --base head diffs from its HEAD")
	default:
		w.d.Base = r.Base
	}
	opts := []string{"--no-color", "--no-ext-diff", "--no-textconv", w.d.Base, "--"}
	var err error
	if w.tracked, err = git(ctx, r.Path, diffHardLimit, append([]string{"diff-index", "-p", "--binary"}, opts...)...); err != nil {
		return err
	}
	numstat, err := git(ctx, r.Path, diffHardLimit, append([]string{"diff-index", "--numstat", "-z"}, opts...)...)
	if err != nil {
		return err
	}
	// -z records: "insertions\tdeletions\tpath" ("-" for a binary file).
	for rec := range strings.SplitSeq(string(numstat), "\x00") {
		if f := strings.SplitN(rec, "\t", 3); len(f) == 3 {
			ins, _ := strconv.Atoi(f[0])
			del, _ := strconv.Atoi(f[1])
			w.d.FileStats = append(w.d.FileStats, proto.DiffFileStat{Name: quoteName(f[2]), Insertions: ins, Deletions: del, Binary: f[0] == "-"})
			w.d.Files, w.d.Insertions, w.d.Deletions = w.d.Files+1, w.d.Insertions+ins, w.d.Deletions+del
		}
	}
	// diff-index does not look at the working tree copy of an entry marked
	// assume-unchanged (lowercase tag) or skip-worktree (S): its changes
	// are not in the patch.
	flagged, err := git(ctx, r.Path, diffHardLimit, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for rec := range strings.SplitSeq(string(flagged), "\x00") {
		if len(rec) > 2 && (rec[0] == 'S' || (rec[0] >= 'a' && rec[0] <= 'z')) {
			w.omit(rec[2:])
		}
	}
	list, err := git(ctx, r.Path, diffHardLimit, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for p := range strings.SplitSeq(string(list), "\x00") {
		// Git lists nested repositories as "sub/", but no fifos or sockets.
		if fi, err := os.Lstat(filepath.Join(r.Path, p)); p == "" || err != nil {
			continue
		} else if fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			w.untracked = append(w.untracked, p)
		} else {
			w.omit(p)
		}
	}
	return nil
}

func (w *repoWork) omit(p string) {
	if w.d.OmittedCount < proto.OmitCap {
		w.d.Omitted = append(w.d.Omitted, quoteName(p))
	}
	w.d.OmittedCount++
}

func (w *repoWork) untrackedDiff(limit int64) error {
	var b bytes.Buffer
	w.extraIns, w.cut = 0, false
	w.extraStats = nil
	for _, p := range w.untracked {
		ins, cut, err := newFilePatch(&b, w.path, p, limit)
		if err != nil {
			return err
		}
		w.extraIns += ins
		w.cut = w.cut || cut
		w.extraStats = append(w.extraStats, proto.DiffFileStat{Name: quoteName(p), Insertions: ins, Binary: cut && ins == 0})
	}
	w.extra = b.Bytes()
	return nil
}

// newFilePatch writes git's patch creating the untracked file p, its
// content cut at limit bytes. cut: the patch leaves content out (cut at
// the limit, or a binary file's), so it does not recreate the file.
func newFilePatch(b *bytes.Buffer, dir, p string, limit int64) (ins int, cut bool, err error) {
	full := filepath.Join(dir, p)
	fi, err := os.Lstat(full)
	if err != nil {
		return 0, false, err
	}
	mode, size := "100644", fi.Size()
	var content []byte
	if fi.Mode()&os.ModeSymlink != 0 {
		mode = "120000"
		t, err := os.Readlink(full)
		if err != nil {
			return 0, false, err
		}
		content, size = []byte(t), int64(len(t))
	} else {
		if fi.Mode()&0o100 != 0 {
			mode = "100755"
		}
		f, err := os.Open(full)
		if err != nil {
			return 0, false, err
		}
		content, err = io.ReadAll(io.LimitReader(f, limit))
		f.Close()
		if err != nil {
			return 0, false, err
		}
	}
	a, bn := quoteName("a/"+p), quoteName("b/"+p)
	fmt.Fprintf(b, "diff --git %s %s\nnew file mode %s\n", a, bn, mode)
	if len(content) == 0 {
		return 0, false, nil
	}
	if bytes.IndexByte(content[:min(len(content), binaryPrefix)], 0) >= 0 {
		fmt.Fprintf(b, "Binary files /dev/null and %s differ\n", bn)
		return 0, true, nil
	}
	cut = size > int64(len(content))
	lines := bytes.SplitAfter(content, []byte("\n"))
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	hunk := "+1"
	if len(lines) != 1 {
		hunk = fmt.Sprintf("+1,%d", len(lines))
	}
	// git ends a name with a space with a tab, for GNU patch.
	tab := ""
	if strings.Contains(bn, " ") {
		tab = "\t"
	}
	fmt.Fprintf(b, "--- /dev/null\n+++ %s%s\n@@ -0,0 %s @@\n", bn, tab, hunk)
	for _, l := range lines {
		b.WriteByte('+')
		b.Write(l)
	}
	if content[len(content)-1] != '\n' {
		b.WriteByte('\n')
		if !cut {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
	if cut {
		fmt.Fprintf(b, "\\ lux: truncated at %d of %d bytes\n", len(content), size)
	}
	return len(lines), cut, nil
}

// quoteName is a path as git's patch headers write it with
// core.quotePath=false: C-quoted if it has a quote, a backslash or a
// control character.
func quoteName(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r == '"' || r == '\\' || r < 0x20 || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\a', '\b', '\t', '\n', '\v', '\f', '\r':
			b.WriteByte('\\')
			b.WriteByte("abtnvfr"[strings.IndexByte("\a\b\t\n\v\f\r", c)])
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// git runs git in dir and returns its stdout, failing with errTooLarge past
// limit bytes.
func git(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotePath=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(out, int64(limit)+1))
	if len(b) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errTooLarge
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
