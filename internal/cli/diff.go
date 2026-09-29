package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

func (a *app) diffCmd() *cobra.Command {
	var base string
	var stat bool
	cmd := &cobra.Command{
		Use:   "diff <run>",
		Short: "Show what a running Run changed in its repositories",
		Long: `Show each repository's diff, from the commit it was cloned at (--base clone,
the default) or from its HEAD (--base head), to its working tree: commits,
staged, unstaged and untracked files, computed now in the Run's container.
Only while the Run is running (otherwise exit 4); to keep a Run's changes
past a stop, save a patch with workload.beforeStop (see docs/runspec.md).

Each repository's patch is headed by a "# repo" comment line, which git apply
skips. Untracked files are cut at 32 KiB (8 KiB when the diff passes 1 MiB)
and binary ones only named: such a patch does not apply, and lux diff says so
on stderr. Exit 3: the Run has no repositories; 1: a repository's diff failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if base != "clone" && base != "head" {
				return fmt.Errorf("--base must be clone or head")
			}
			q := url.Values{"base": {base}}
			// Text --stat counts from the patch; JSON --stat leaves it out.
			if stat && a.output == "json" {
				q.Set("stat", "true")
			}
			var d server.RunDiff
			if err := a.c.Do(ctxOf(cmd), "GET", "/v1/runs/"+args[0]+"/diff?"+q.Encode(), nil, &d); err != nil {
				return err
			}
			failed := false
			for _, r := range d.Repos {
				failed = failed || r.Error != ""
			}
			if a.output == "json" {
				if err := a.json(d); err != nil {
					return err
				}
			} else {
				a.printDiff(d, stat)
			}
			if failed {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "clone", "clone: from the commit each repository was cloned at; head: from its HEAD")
	cmd.Flags().BoolVar(&stat, "stat", false, "per file, lines added and removed, as git diff --stat")
	return cmd
}

func (a *app) printDiff(d server.RunDiff, stat bool) {
	for _, r := range d.Repos {
		if r.Error != "" {
			fmt.Fprintf(a.stderr, "lux: repo %s: %s\n", r.Repo, r.Error)
			continue
		}
		if r.Files == 0 {
			continue
		}
		fmt.Fprintf(a.stdout, "# repo %s: %s..%s\n", r.Repo, short(r.Base), short(r.Head))
		patch := []byte(r.Patch)
		if r.PatchBase64 != nil {
			patch = r.PatchBase64
		}
		if stat {
			writeStat(a.stdout, patch, r)
		} else {
			a.stdout.Write(patch)
		}
		if r.Truncated {
			fmt.Fprintf(a.stderr, "lux: repo %s: untracked files were cut or are binary; the patch will not apply cleanly\n", r.Repo)
		}
	}
}

func short(sha string) string {
	if sha == "" {
		return "000000000000" // no commit yet
	}
	return sha[:min(len(sha), 12)]
}

type fileStat struct {
	name     string
	ins, del int
	binary   bool
}

// patchStats counts each file's lines in a patch. lux's patches never
// rename, so a file's two header paths are the same.
func patchStats(patch []byte) []fileStat {
	var out []fileStat
	inHunk := false
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(nil, len(patch)+1)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "diff --git "):
			names := l[len("diff --git "):]
			name := names[2 : (len(names)-1)/2] // "a/P b/P"
			if strings.HasPrefix(names, `"`) {
				name = `"` + names[3:(len(names)-1)/2] // `"a/P" "b/P"`, left quoted
			}
			out = append(out, fileStat{name: name})
			inHunk = false
		case len(out) == 0:
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case strings.HasPrefix(l, "GIT binary patch") || strings.HasPrefix(l, "Binary files "):
			out[len(out)-1].binary = true
		case inHunk && strings.HasPrefix(l, "+"):
			out[len(out)-1].ins++
		case inHunk && strings.HasPrefix(l, "-"):
			out[len(out)-1].del++
		}
	}
	return out
}

// writeStat prints git diff --stat's lines for one repository.
func writeStat(w io.Writer, patch []byte, r server.RepoDiff) {
	files := patchStats(patch)
	width, most := 0, 0
	for _, f := range files {
		width, most = max(width, len(f.name)), max(most, f.ins+f.del)
	}
	const bar = 50
	scale := func(n int) int {
		if most <= bar || n == 0 {
			return n
		}
		return max(1, n*bar/most)
	}
	for _, f := range files {
		if f.binary {
			fmt.Fprintf(w, " %-*s | Bin\n", width, f.name)
			continue
		}
		fmt.Fprintf(w, " %-*s | %d %s%s\n", width, f.name, f.ins+f.del, strings.Repeat("+", scale(f.ins)), strings.Repeat("-", scale(f.del)))
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

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
