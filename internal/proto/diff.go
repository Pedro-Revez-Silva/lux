package proto

// Diffs: what a Run changed in its repositories, from each repository's
// base to its working tree. The runner never runs git in a checkout
// itself: `lux-shim diff` does, as the workload user, inside the Run's
// container (live) or a throwaway container on its volumes (at snapshot).

// luxd → runner (live, not durable) and back.
const (
	MsgDiffRequest = "diff.request"
	MsgDiffResult  = "diff.result" // one per repository and base kind
	MsgDiffEnd     = "diff.end"
)

// EvDiffFailed: a diff could not be computed at a snapshot (the snapshot
// and the exit go on): {error, repo?}.
const EvDiffFailed = "diff.failed"

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
	Error          string   `json:"error,omitempty"`
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

// DiffResult is one repository's diff, live.
type DiffResult struct {
	SubID string   `json:"subId"`
	Stat  DiffStat `json:"stat"`
	Patch []byte   `json:"patch,omitempty"`
}

// DiffEnd ends a live diff. NotRunning: the container is not running (the
// caller falls back to the latest snapshot).
type DiffEnd struct {
	SubID      string `json:"subId"`
	Error      string `json:"error,omitempty"`
	NotRunning bool   `json:"notRunning,omitempty"`
}

// SnapshotDiff is a diff computed at a snapshot: Blob holds the patch
// (zstd, like every blob; nil when empty), PatchSHA256 the patch itself.
type SnapshotDiff struct {
	DiffStat
	Blob        *BlobInfo `json:"blob,omitempty"`
	PatchSHA256 string    `json:"patchSha256,omitempty"`
}
