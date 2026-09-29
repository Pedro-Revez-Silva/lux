package proto

// Diffs: what a running Run changed in its repositories, from each
// repository's base to its working tree. The runner never runs git in a
// checkout itself: `lux-shim diff` does, as the workload user, inside the
// Run's container.

// luxd → runner (live, not durable) and back.
const (
	MsgDiffRequest = "diff.request"
	MsgDiffCancel  = "diff.cancel" // DiffRequest's SubID: its caller is gone
	MsgDiffResult  = "diff.result" // one per repository
	MsgDiffEnd     = "diff.end"
)

// Base kinds: from the commit the repository was cloned at, or from the
// checkout's current HEAD (uncommitted work only).
const (
	DiffBaseClone = "clone"
	DiffBaseHead  = "head"
)

// DiffBaseUnreachable is DiffStat.ErrorCode when the commit a repository
// was cloned at is not in its history any more (a shallow clone, a history
// rewrite, a new repository): only base=head can be diffed.
const DiffBaseUnreachable = "base_unreachable"

// DiffLimit is the most patch bytes kept per repository and base kind; the
// stat always covers the whole diff.
const DiffLimit = 10 << 20

// DiffBudget bounds one diff's response across all its repositories:
// patches and per-file stats, as DiffStat.Size counts them. Once a diff
// has used it, each further repository has its totals only (OverBudget).
// The shim keeps to it; the runner and luxd enforce it again on what they
// hold (gitdiff.Budget).
const DiffBudget = 32 << 20

// DiffBudgetExceeded is DiffStat.Error for a repository over DiffBudget.
const DiffBudgetExceeded = "response budget exceeded"

// Size is what a repository's diff holds beyond its fixed fields: its
// patch, per-file stats and path lists.
func (s *DiffStat) Size(patch []byte) int64 {
	n := int64(len(patch))
	for _, f := range s.FileStats {
		n += int64(len(f.Path)+len(f.OldPath)) + 32
	}
	for _, l := range [][]string{s.FilteredPaths, s.NormalizedPaths, s.DirtySubmodules} {
		for _, p := range l {
			n += int64(len(p)) + 4
		}
	}
	return n + int64(len(s.Error))
}

// OverBudget reduces s to its totals, marked truncated with
// DiffBudgetExceeded: a repository past the diff's budget. One that failed
// on its own keeps its error code, and its error up to 1 KiB.
func (s DiffStat) OverBudget() DiffStat {
	out := DiffStat{Repo: s.Repo, Kind: s.Kind, Base: s.Base, Head: s.Head, Truncated: true,
		Files: s.Files, Insertions: s.Insertions, Deletions: s.Deletions, FiltersIgnored: s.FiltersIgnored,
		Error: DiffBudgetExceeded}
	if s.Error != "" && s.Error != DiffBudgetExceeded {
		out.Truncated, out.Error, out.ErrorCode = false, s.Error[:min(len(s.Error), 1<<10)], s.ErrorCode
	}
	return out
}

// DiffRepo is one repository to diff: its checkout path in the container
// and the commit it was cloned at ("" if unknown).
type DiffRepo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Base string `json:"base,omitempty"`
}

// DiffArgs is what `lux-shim diff` takes (one JSON argument).
type DiffArgs struct {
	Repos []DiffRepo `json:"repos"`
	Kinds []string   `json:"kinds"`
	// StatOnly skips the patches.
	StatOnly bool  `json:"statOnly,omitempty"`
	Limit    int64 `json:"limit"`
	// WatchStdin: stop when stdin ends (its caller is gone).
	WatchStdin bool `json:"watchStdin,omitempty"`
}

// DiffFile is one file's line counts (git diff --numstat); a binary file
// has none.
type DiffFile struct {
	Path       string `json:"path"`
	OldPath    string `json:"oldPath,omitempty" doc:"The path before a rename or copy."`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary,omitempty"`
}

// DiffStat describes one repository's diff for one base kind. Error is set
// instead when it could not be computed.
type DiffStat struct {
	Repo       string     `json:"repo"`
	Kind       string     `json:"kind"`
	Base       string     `json:"base,omitempty"`
	Head       string     `json:"head,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	Files      int        `json:"files"`
	Insertions int        `json:"insertions"`
	Deletions  int        `json:"deletions"`
	FileStats  []DiffFile `json:"fileStats,omitempty"`
	// PatchBytes is the length of the patch that follows (kept bytes).
	PatchBytes int64 `json:"patchBytes"`
	// FiltersIgnored: some changed files have a clean/smudge filter
	// attribute, which is not run (the workload chose its program): they
	// are compared raw. FilteredPaths lists them (at most 100).
	FiltersIgnored bool     `json:"filtersIgnored,omitempty"`
	FilteredPaths  []string `json:"filteredPaths,omitempty"`
	// NormalizedPaths: files whose bytes on disk git converts (text, eol,
	// core.autocrlf, working-tree-encoding) before it compares them, so
	// the patch may not reproduce them byte for byte, or may not show a
	// change at all (at most 100).
	NormalizedPaths []string `json:"normalizedPaths,omitempty"`
	// DirtySubmodules: submodules with uncommitted changes of their own,
	// which the parent's patch cannot carry (at most 100).
	DirtySubmodules []string `json:"dirtySubmodules,omitempty"`
	Error           string   `json:"error,omitempty"`
	// ErrorCode classifies Error when it has a known cause
	// (DiffBaseUnreachable).
	ErrorCode string `json:"errorCode,omitempty"`
}

// DiffRequest asks the runner for a running placement's diff.
type DiffRequest struct {
	SubID    string     `json:"subId"`
	Kind     string     `json:"kind"`
	StatOnly bool       `json:"statOnly,omitempty"`
	Repos    []DiffRepo `json:"repos"`
}

// DiffResult is one repository's diff.
type DiffResult struct {
	SubID string   `json:"subId"`
	Stat  DiffStat `json:"stat"`
	Patch []byte   `json:"patch,omitempty"`
}

// DiffEnd ends a live diff. NotRunning: the container is not running.
// Busy: another, different live diff of the placement is under way (one at
// a time; an identical request shares it instead).
type DiffEnd struct {
	SubID      string `json:"subId"`
	Error      string `json:"error,omitempty"`
	NotRunning bool   `json:"notRunning,omitempty"`
	Busy       bool   `json:"busy,omitempty"`
}
