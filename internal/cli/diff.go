package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/server"
)

func (a *app) diffCmd() *cobra.Command {
	var repo, base, color, snapshot string
	var stat, wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "diff <run>",
		Short: "Show what a Run changed in its repositories",
		Long: `Show each repository's diff, from the commit it was cloned at (--base clone,
the default) or from its HEAD (--base head: uncommitted work only), to its
working tree: commits, staged, unstaged and untracked files. While the Run
runs, the diff is computed in its container now; once it has stopped, it is
the one saved with its latest snapshot (--snapshot: with that one).

The latest snapshot's diff is computed after the Run stops: until it is,
lux diff says so and exits 4 (--wait: waits for it, up to --timeout). A
snapshot without a diff (skipped, failed) exits 3, with why.

Each repository's patch is headed by a "# repo" comment line, which git apply
skips. Prints nothing when nothing changed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if base != "clone" && base != "head" {
				return fmt.Errorf("--base must be clone or head")
			}
			colored, err := a.colorOn(color)
			if err != nil {
				return err
			}
			q := url.Values{"base": {base}}
			if repo != "" {
				q.Set("repo", repo)
			}
			if stat {
				q.Set("stat", "true")
			}
			if snapshot != "" {
				q.Set("snapshot", snapshot)
			}
			d, err := a.getDiff(ctxOf(cmd), "/v1/runs/"+args[0]+"/diff?"+q.Encode(), wait, timeout)
			if err != nil {
				return err
			}
			if a.output == "json" {
				if err := a.json(d); err != nil {
					return err
				}
				for _, r := range d.Repos {
					if r.Error != "" {
						return exitCode(1)
					}
				}
				return nil
			}
			return a.printDiff(d, stat, colored)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository")
	cmd.Flags().StringVar(&base, "base", "clone", "clone: from the commit each repository was cloned at; head: from its HEAD")
	cmd.Flags().BoolVar(&stat, "stat", false, "only the files changed, as git diff --stat")
	cmd.Flags().StringVar(&color, "color", "auto", "never | always | auto (when stdout is a terminal)")
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "the diff saved with this snapshot (lux snapshots <run>), not the latest")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait while the snapshot's diff is still being computed")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "with --wait: give up after this long")
	return cmd
}

// getDiff GETs a diff; with wait, again after each diff_pending (as it
// says), until timeout. diff_pending is exit 4, diff_unavailable exit 3.
func (a *app) getDiff(ctx context.Context, path string, wait bool, timeout time.Duration) (server.RunDiff, error) {
	deadline := time.Now().Add(timeout)
	for {
		var d server.RunDiff
		err := a.c.Do(ctx, "GET", path, nil, &d)
		var ae *client.APIError
		if !errors.As(err, &ae) {
			return d, err
		}
		switch ae.Code {
		case "diff_pending":
			retry := time.Duration(max(ae.RetryAfter, 1)) * time.Second
			if wait && time.Now().Add(retry).Before(deadline) {
				select {
				case <-ctx.Done():
					return d, ctx.Err()
				case <-time.After(retry):
				}
				continue
			}
			msg := fmt.Sprintf("diff for snapshot %s is still being computed; try again in %ds", ae.SnapshotID, max(ae.RetryAfter, 1))
			if wait {
				msg = fmt.Sprintf("diff for snapshot %s is still being computed after %s", ae.SnapshotID, timeout)
			}
			fmt.Fprintln(a.stderr, "lux:", msg)
			return d, exitCode(4)
		case "diff_unavailable":
			fmt.Fprintf(a.stderr, "lux: snapshot %s has no diff: %s\n", ae.SnapshotID, ae.Reason)
			return d, exitCode(3)
		}
		return d, err
	}
}

func (a *app) colorOn(mode string) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		f, ok := a.stdout.(*os.File)
		return ok && term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == "", nil
	}
	return false, fmt.Errorf("--color must be never, always or auto")
}

// printDiff prints each changed repository's section; one whose diff
// failed is reported on stderr, and fails the command.
func (a *app) printDiff(d server.RunDiff, stat, colored bool) error {
	failed := 0
	for _, r := range d.Repos {
		if r.Error != "" {
			fmt.Fprintf(a.stderr, "lux: repo %s: %s\n", r.Repo, r.Error)
			failed++
			continue
		}
		if r.Files == 0 {
			continue
		}
		if r.FiltersIgnored {
			fmt.Fprintf(a.stderr, "lux: repo %s: files with clean/smudge filters are compared raw (filters are not run): %s\n",
				r.Repo, strings.Join(r.FilteredPaths, ", "))
		}
		header := diffHeader(r)
		if colored {
			header = "\x1b[33m" + header + "\x1b[m"
		}
		fmt.Fprintln(a.stdout, header)
		if stat {
			writeStat(a.stdout, r, colored)
			continue
		}
		patch := []byte(r.Patch)
		if r.PatchBase64 != nil {
			patch = r.PatchBase64
		}
		if colored {
			patch = colorPatch(patch)
		}
		a.stdout.Write(patch)
		if r.Truncated {
			fmt.Fprintf(a.stderr, "lux: repo %s: the patch was cut at the size limit; see --stat for all of it\n", r.Repo)
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// diffHeader is a comment line naming the repository and where its diff
// came from: `# repo app: 1a2b3c4..5d6e7f8 (snapshot snap_x at 2026-…)`.
func diffHeader(r server.RepoDiff) string {
	h := fmt.Sprintf("# repo %s: %s..%s (%s", r.Repo, short(r.Base), short(r.Head), r.Source)
	if r.Source == "snapshot" {
		h += fmt.Sprintf(", snapshot %s at %s", r.SnapshotID, r.At.UTC().Format(time.RFC3339))
	}
	h += ")"
	if r.Truncated {
		h += ", TRUNCATED"
	}
	return h
}

func short(sha string) string {
	if sha == "" {
		return "?"
	}
	return sha[:min(len(sha), 12)]
}

// colorPatch colours a patch as git diff does: headers bold, hunks cyan,
// removed lines red, added lines green.
func colorPatch(p []byte) []byte {
	var out bytes.Buffer
	inHeader := false
	for _, line := range bytes.SplitAfter(p, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		body := bytes.TrimRight(line, "\n")
		nl := line[len(body):]
		code := ""
		switch {
		case bytes.HasPrefix(line, []byte("diff --git ")):
			inHeader, code = true, "1"
		case bytes.HasPrefix(line, []byte("@@")):
			inHeader, code = false, "36"
		case inHeader:
			code = "1"
		case line[0] == '+':
			code = "32"
		case line[0] == '-':
			code = "31"
		}
		if code == "" {
			out.Write(line)
			continue
		}
		fmt.Fprintf(&out, "\x1b[%sm%s\x1b[m%s", code, body, nl)
	}
	return out.Bytes()
}

// writeStat prints git diff --stat's lines for one repository.
func writeStat(w io.Writer, r server.RepoDiff, colored bool) {
	width, most := 0, 0
	for _, f := range r.FileStats {
		width = max(width, len(statName(f.Path, f.OldPath)))
		most = max(most, f.Insertions+f.Deletions)
	}
	const bar = 40
	for _, f := range r.FileStats {
		name := statName(f.Path, f.OldPath)
		if f.Binary {
			fmt.Fprintf(w, " %-*s | Bin\n", width, name)
			continue
		}
		plus, minus := f.Insertions, f.Deletions
		if most > bar {
			plus, minus = scale(plus, most, bar), scale(minus, most, bar)
		}
		p, m := strings.Repeat("+", plus), strings.Repeat("-", minus)
		if colored {
			p, m = "\x1b[32m"+p+"\x1b[m", "\x1b[31m"+m+"\x1b[m"
		}
		fmt.Fprintf(w, " %-*s | %d %s%s\n", width, name, f.Insertions+f.Deletions, p, m)
	}
	if n := r.Files - len(r.FileStats); n > 0 {
		fmt.Fprintf(w, " ... and %d more\n", n)
	}
	fmt.Fprintf(w, " %d %s changed", r.Files, plural(r.Files, "file", "files"))
	if r.Insertions > 0 {
		fmt.Fprintf(w, ", %d %s(+)", r.Insertions, plural(r.Insertions, "insertion", "insertions"))
	}
	if r.Deletions > 0 {
		fmt.Fprintf(w, ", %d %s(-)", r.Deletions, plural(r.Deletions, "deletion", "deletions"))
	}
	fmt.Fprintln(w)
}

func statName(path, old string) string {
	if old == "" {
		return path
	}
	return old + " => " + path
}

// scale shrinks n of most to fit width, keeping any change visible.
func scale(n, most, width int) int {
	if n == 0 {
		return 0
	}
	return max(1, n*width/most)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
