package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

func (a *app) diffCmd() *cobra.Command {
	var repo, base, color string
	var stat bool
	cmd := &cobra.Command{
		Use:   "diff <run>",
		Short: "Show what a running Run changed in its repositories",
		Long: `Show each repository's diff, from the commit it was cloned at (--base clone,
the default) or from its HEAD (--base head: uncommitted work only), to its
working tree: commits, staged, unstaged and untracked files. The diff is
computed in the Run's container, now, so only while the Run is running;
otherwise lux diff says so and exits 4. To keep a Run's changes past a
stop, save a patch with workload.beforeStop
(git add -N . && git diff --binary <base> > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts.

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
			var d server.RunDiff
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/diff?"+q.Encode(), nil, &d); err != nil {
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
	return cmd
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
			msg := r.Error
			if msg == proto.DiffBudgetExceeded {
				msg += "; diff it alone with --repo " + r.Repo
			}
			fmt.Fprintf(a.stderr, "lux: repo %s: %s\n", r.Repo, msg)
			failed++
			continue
		}
		// Said even when nothing shows as changed: a change of line
		// endings alone, or inside a submodule, is not in the diff.
		if r.FiltersIgnored {
			fmt.Fprintf(a.stderr, "lux: repo %s: files with clean/smudge filters are compared raw (filters are not run): %s\n",
				r.Repo, strings.Join(r.FilteredPaths, ", "))
		}
		if len(r.NormalizedPaths) > 0 {
			fmt.Fprintf(a.stderr, "lux: repo %s: git converts these files' line endings or encoding; the patch has its stored form: %s\n",
				r.Repo, strings.Join(r.NormalizedPaths, ", "))
		}
		if len(r.DirtySubmodules) > 0 {
			fmt.Fprintf(a.stderr, "lux: repo %s: submodules with uncommitted changes of their own, not in the patch: %s\n",
				r.Repo, strings.Join(r.DirtySubmodules, ", "))
		}
		if r.Files == 0 {
			continue
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

// diffHeader is a comment line naming the repository and its commits:
// `# repo app: 1a2b3c4d5e6f..5d6e7f8a9b0c`.
func diffHeader(r server.RepoDiff) string {
	h := fmt.Sprintf("# repo %s: %s..%s", r.Repo, short(r.Base), short(r.Head))
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
