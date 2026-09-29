// Package gitdiff computes what a checkout changed: its base commit (or
// its HEAD) against its working tree, committed, staged, unstaged and
// untracked (not ignored) changes alike. It runs inside a Run's container
// as the workload user (`lux-shim diff`), never as the runner on the host.
//
// It never changes the checkout: untracked files are marked intent-to-add
// in a copy of the index, in a temporary directory, and the working tree
// is diffed against the base through that copy. Nothing is written to the
// repository: not its index, HEAD, refs or object store (the one object
// the copy needs, the empty blob, goes to a temporary object directory).
package gitdiff

import (
	"bufio"
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

	"github.com/marcioapm/lux/internal/proto"
)

// emptyTree is git's empty tree (SHA-1): the base of an unborn HEAD.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// safeConfig keeps whatever the workload configured from running programs
// or reshaping the output. Filters (which git add would run) are disabled
// per name in filterOverrides.
var safeConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "diff.external=",
	"-c", "core.untrackedCache=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "color.ui=false",
	"-c", "diff.noprefix=false",
	"-c", "diff.mnemonicPrefix=false",
	"-c", "diff.relative=false",
	"-c", "core.quotePath=true",
}

// diffFlags make the patch git's plain format, whatever the config says.
var diffFlags = []string{
	"--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
	"--src-prefix=a/", "--dst-prefix=b/", "-M", "-O/dev/null", "--ignore-submodules=none",
}

type repo struct {
	dir  string
	env  []string // GIT_INDEX_FILE and friends, once staged
	conf []string
}

func (r *repo) git(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append(append(append([]string{}, safeConfig...), r.conf...), args...)...)
	cmd.Dir = r.dir
	// The container's environment is the Run's: a GIT_DIR, GIT_INDEX_FILE
	// or GIT_EXTERNAL_DIFF there must not redirect or reshape the diff.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0",
		// A partial clone would fetch missing objects from its remote.
		"GIT_NO_LAZY_FETCH=1")
	cmd.Env = append(cmd.Env, r.env...)
	stderr := &head{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// out is a small command's output (at most maxOut bytes).
func (r *repo) out(ctx context.Context, args ...string) (string, error) {
	b := &head{max: maxOut}
	err := r.git(ctx, b, args...)
	if err == nil && b.over {
		err = fmt.Errorf("git %s: more than %d bytes of output", args[0], maxOut)
	}
	return strings.TrimSpace(b.String()), err
}

const maxOut = 1 << 20

// head keeps the first max bytes written to it; the rest is counted as
// over, never buffered.
type head struct {
	max  int
	b    []byte
	over bool
}

func (h *head) Write(p []byte) (int, error) {
	n := min(len(p), h.max-len(h.b))
	h.b = append(h.b, p[:n]...)
	if n < len(p) {
		h.over = true
	}
	return len(p), nil
}

func (h *head) String() string { return string(h.b) }

// Diff is a checkout's diff for one base kind.
type Diff struct {
	Stat  proto.DiffStat
	Patch []byte
}

// Compute diffs the checkout at dir for each kind (proto.DiffBaseClone
// against base, proto.DiffBaseHead against HEAD). Patches are cut at limit
// bytes, after the last whole file's diff that fits (empty if none does);
// the stat always covers the whole diff. A failure is the returned error when the
// checkout could not be staged at all, else in each Diff's Stat.Error.
func Compute(ctx context.Context, dir, base string, kinds []string, statOnly bool, limit int64) ([]Diff, error) {
	r := &repo{dir: dir}
	top, err := r.out(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if filepath.Clean(top) != filepath.Clean(dir) {
		if rt, err := filepath.EvalSymlinks(dir); err != nil || filepath.Clean(top) != rt {
			return nil, fmt.Errorf("%s is not the top of a checkout (%s is)", dir, top)
		}
	}
	head, err := r.out(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		head = "" // unborn: nothing committed yet
	}
	tmp, err := os.MkdirTemp("", "lux-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := r.stage(ctx, tmp); err != nil {
		return nil, err
	}
	var out []Diff
	for _, kind := range kinds {
		d := Diff{Stat: proto.DiffStat{Kind: kind, Head: head}}
		from := ""
		switch kind {
		case proto.DiffBaseClone:
			d.Stat.Base = base
			from = base
			if base == "" {
				d.Stat.Error = "the commit this repository was cloned at is not known"
			}
		case proto.DiffBaseHead:
			d.Stat.Base = head
			from = head
			if head == "" {
				from = emptyTree
			}
		default:
			d.Stat.Error = "unknown base " + strconv.Quote(kind)
		}
		if d.Stat.Error == "" {
			if err := r.diff(ctx, &d, from, statOnly, limit); err != nil {
				d.Stat.Error = err.Error()
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// stage makes a copy of the index, in tmp, that also lists untracked (not
// ignored) files, as intent-to-add entries: the diff against the working
// tree then includes them. Intent-to-add writes no file's content; it
// only needs the empty blob, which is written to tmp's own object
// directory first so that git never writes to (or refreshes the times of)
// the repository's objects.
func (r *repo) stage(ctx context.Context, tmp string) error {
	paths, err := r.out(ctx, "rev-parse", "--path-format=absolute", "--git-path", "index", "--git-path", "objects")
	if err != nil {
		return err
	}
	p := strings.Split(paths, "\n")
	if len(p) != 2 {
		return fmt.Errorf("git rev-parse: unexpected output %q", paths)
	}
	index, objects := filepath.Join(tmp, "index"), filepath.Join(tmp, "objects")
	if err := os.Mkdir(objects, 0o700); err != nil {
		return err
	}
	// A copy keeps git's stat cache: unchanged files are not hashed again.
	if err := copyFile(p[0], index); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	conf, err := r.filterOverrides(ctx)
	if err != nil {
		return err
	}
	r.conf = conf
	r.env = []string{"GIT_INDEX_FILE=" + index, "GIT_OBJECT_DIRECTORY=" + objects}
	if err := r.git(ctx, io.Discard, "hash-object", "-w", "--stdin"); err != nil {
		return err
	}
	r.env = append(r.env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+p[1])
	return r.git(ctx, io.Discard, "add", "--all", "--intent-to-add")
}

// filterOverrides disables every configured clean/smudge filter: git add
// would otherwise run the programs they name.
func (r *repo) filterOverrides(ctx context.Context) ([]string, error) {
	var b bytes.Buffer
	err := r.git(ctx, &b, "config", "--null", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil && b.Len() == 0 {
		return nil, nil // none configured (git config exits 1)
	}
	seen := map[string]bool{}
	var conf []string
	for key := range strings.SplitSeq(b.String(), "\x00") {
		i := strings.LastIndexByte(key, '.')
		if i <= len("filter.") || seen[key[:i]] {
			continue
		}
		name := key[:i]
		seen[name] = true
		conf = append(conf, "-c", name+".clean=", "-c", name+".smudge=", "-c", name+".process=", "-c", name+".required=false")
	}
	return conf, nil
}

func (r *repo) diff(ctx context.Context, d *Diff, from string, statOnly bool, limit int64) error {
	num := &numstat{}
	if err := r.git(ctx, num, append(append([]string{"diff", "--numstat", "-z"}, diffFlags...), from, "--")...); err != nil {
		return err
	}
	if err := num.end(); err != nil {
		return err
	}
	d.Stat.FileStats = num.files
	d.Stat.Files, d.Stat.Insertions, d.Stat.Deletions = num.n, num.ins, num.del
	if statOnly || d.Stat.Files == 0 {
		return nil
	}
	w := &capped{limit: limit}
	// --binary: a binary file's patch carries its content, so it applies.
	// (Not for --numstat: it implies --patch.)
	if err := r.git(ctx, w, append(append([]string{"diff", "--binary"}, diffFlags...), from, "--")...); err != nil {
		return err
	}
	d.Patch, d.Stat.Truncated = w.result()
	d.Stat.PatchBytes = int64(len(d.Patch))
	return nil
}

// fileHeader starts every file's diff but the first. No other patch line
// can match it: content lines start with ' ', '+', '-' or '\', binary
// patch lines are base85 (no spaces), and git quotes a path with a newline.
var fileHeader = []byte("\ndiff --git ")

// capped keeps the first limit bytes written to it, plus enough to see
// whether a file's diff starts right at the limit, and counts the rest.
type capped struct {
	limit int64
	buf   bytes.Buffer
	total int64
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit + int64(len(fileHeader)) - int64(c.buf.Len())
	c.buf.Write(p[:min(int64(len(p)), max(room, 0))])
	c.total += int64(len(p))
	return len(p), nil
}

// result is the kept patch: whole, or cut at the end of the last file's
// diff that fits, so that what is kept applies. Nothing when even the first
// file's does not fit.
func (c *capped) result() ([]byte, bool) {
	b := c.buf.Bytes()
	if c.total <= c.limit {
		return b, false
	}
	// A header at i keeps b[:i+1]: it must start at or before limit-1.
	end := min(int64(len(b)), c.limit-1+int64(len(fileHeader)))
	if end > 0 {
		if i := bytes.LastIndex(b[:end], fileHeader); i >= 0 {
			return b[:i+1], true
		}
	}
	return nil, true
}

// numstat parses `git diff --numstat -z` as git writes it: "ins\tdel\tpath\0",
// or for a rename "ins\tdel\t\0old\0new\0"; "-" counts for a binary file.
// It keeps the first maxFileStats files and totals all of them, holding at
// most one path at a time.
type numstat struct {
	tok   []byte
	cur   proto.DiffFile
	need  int // paths still owed to cur (a rename's two)
	files []proto.DiffFile
	n     int
	ins   int
	del   int
}

// maxPath bounds one NUL-terminated field (PATH_MAX is 4096).
const maxPath = 64 << 10

var errNumstat = errors.New("git diff --numstat: malformed output")

func (s *numstat) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			i = len(p)
		}
		if len(s.tok)+i > maxPath {
			return 0, fmt.Errorf("git diff --numstat: a field over %d bytes", maxPath)
		}
		s.tok = append(s.tok, p[:i]...)
		if i == len(p) {
			break
		}
		if err := s.field(string(s.tok)); err != nil {
			return 0, err
		}
		s.tok, p = s.tok[:0], p[i+1:]
	}
	return n, nil
}

func (s *numstat) field(f string) error {
	switch s.need {
	case 2:
		s.cur.OldPath, s.need = f, 1
		return nil
	case 1:
		s.cur.Path, s.need = f, 0
		s.add()
		return nil
	}
	parts := strings.SplitN(f, "\t", 3)
	if len(parts) != 3 {
		return errNumstat
	}
	s.cur = proto.DiffFile{Path: parts[2]}
	if parts[0] == "-" && parts[1] == "-" {
		s.cur.Binary = true
	} else {
		var err1, err2 error
		s.cur.Insertions, err1 = strconv.Atoi(parts[0])
		s.cur.Deletions, err2 = strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return errNumstat
		}
	}
	if s.cur.Path == "" {
		s.need = 2
		return nil
	}
	s.add()
	return nil
}

func (s *numstat) add() {
	s.n++
	s.ins += s.cur.Insertions
	s.del += s.cur.Deletions
	if len(s.files) < maxFileStats {
		s.files = append(s.files, s.cur)
	}
}

// end checks nothing was left half-written.
func (s *numstat) end() error {
	if len(s.tok) > 0 || s.need > 0 {
		return errNumstat
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- the stream between `lux-shim diff` and the runner ----------------------
//
// One record per repository and kind: a JSON line (proto.DiffStat, with
// PatchBytes), then exactly PatchBytes bytes of patch.

// Run computes every repository's diffs and writes them to w as a stream.
// A repository that fails is a record with its error; the others go on.
func Run(ctx context.Context, args proto.DiffArgs, w io.Writer) error {
	limit := args.Limit
	if limit <= 0 {
		limit = proto.DiffLimit
	}
	for _, rp := range args.Repos {
		diffs, err := Compute(ctx, rp.Path, rp.Base, args.Kinds, args.StatOnly, limit)
		if err != nil {
			for _, k := range args.Kinds {
				diffs = append(diffs, Diff{Stat: proto.DiffStat{Kind: k, Error: err.Error()}})
			}
		}
		for _, d := range diffs {
			d.Stat.Repo = rp.Name
			if err := WriteRecord(w, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func WriteRecord(w io.Writer, d Diff) error {
	d.Stat.PatchBytes = int64(len(d.Patch))
	b, err := json.Marshal(d.Stat)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	_, err = w.Write(d.Patch)
	return err
}

// ReadStream reads a stream Run wrote for repos and kinds, calling fn for
// each record with a reader of exactly its patch (fn need not read it all;
// a body cut short reads as io.ErrUnexpectedEOF). The stream comes from a
// process the workload could interfere with, so it is bounded (at most one
// record per repository and kind, each patch at most maxPatch bytes, each
// JSON line at most maxLine bytes) and checked: fn sees only the first
// record for each requested repository and kind. Incomplete lists, per
// repository, why it is not whole: a kind with no record, or with more
// than one. err is for the stream as a whole (unreadable, over its bounds,
// or a record for something not asked for); the records fn saw before it
// are whole.
func ReadStream(r io.Reader, repos, kinds []string, maxPatch int64, fn func(proto.DiffStat, io.Reader) error) (incomplete map[string]string, err error) {
	type key struct{ repo, kind string }
	seen := map[key]int{}
	for _, rp := range repos {
		for _, k := range kinds {
			seen[key{rp, k}] = 0
		}
	}
	incomplete = map[string]string{}
	defer func() {
		for _, rp := range repos {
			for _, k := range kinds {
				if _, bad := incomplete[rp]; !bad && seen[key{rp, k}] == 0 {
					incomplete[rp] = fmt.Sprintf("the diff stream has no %s record for it", k)
				}
			}
		}
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 0; ; n++ {
		line, err := readLine(br, maxLine)
		if err == io.EOF && len(line) == 0 {
			return incomplete, nil
		}
		if err == io.EOF {
			return incomplete, fmt.Errorf("diff stream: %w", io.ErrUnexpectedEOF)
		}
		if err != nil {
			return incomplete, fmt.Errorf("diff stream: %w", err)
		}
		if n >= len(seen) {
			return incomplete, fmt.Errorf("diff stream: more than %d records", len(seen))
		}
		var st proto.DiffStat
		if err := json.Unmarshal(line, &st); err != nil {
			return incomplete, fmt.Errorf("diff stream: %w", err)
		}
		if st.PatchBytes < 0 || st.PatchBytes > maxPatch {
			return incomplete, fmt.Errorf("diff stream: a %d-byte patch", st.PatchBytes)
		}
		k := key{st.Repo, st.Kind}
		count, asked := seen[k]
		if !asked {
			return incomplete, fmt.Errorf("diff stream: a record for %q (%s), which was not asked for", st.Repo, st.Kind)
		}
		seen[k] = count + 1
		body := &exact{r: br, n: st.PatchBytes}
		if count > 0 {
			incomplete[st.Repo] = fmt.Sprintf("the diff stream has %d %s records for it", count+1, st.Kind)
		} else if err := fn(st, body); err != nil {
			return incomplete, err
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return incomplete, fmt.Errorf("diff stream: %w", err)
		}
	}
}

// exact reads exactly n bytes of r: io.ErrUnexpectedEOF if r ends first.
type exact struct {
	r io.Reader
	n int64
}

func (e *exact) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.n {
		p = p[:e.n]
	}
	n, err := e.r.Read(p)
	e.n -= int64(n)
	if err == io.EOF && e.n > 0 {
		err = io.ErrUnexpectedEOF
	} else if err == io.EOF {
		err = nil
	}
	return n, err
}

// maxLine bounds one record's JSON: maxFileStats entries of long paths.
const maxLine = 16 << 20

// maxFileStats bounds the per-file stats of one diff; the totals still
// count every file.
const maxFileStats = 10000

func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > max {
			return nil, fmt.Errorf("a record over %d bytes", max)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// Main is `lux-shim diff <json proto.DiffArgs>`: the stream on stdout.
func Main(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: lux-shim diff <json>")
		return 2
	}
	var a proto.DiffArgs
	if err := json.Unmarshal([]byte(args[0]), &a); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim diff:", err)
		return 2
	}
	w := bufio.NewWriterSize(os.Stdout, 64<<10)
	if err := Run(context.Background(), a, w); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim diff:", err)
		return 1
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim diff:", err)
		return 1
	}
	return 0
}
