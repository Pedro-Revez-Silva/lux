package proto

// Diffs: what a Run changed in its repositories, from each repository's
// base to its working tree. The runner never runs git in a checkout
// itself: `lux-shim diff` does, as the workload user, inside the Run's
// container (live) or, once a snapshot is reported, in a throwaway
// container with its state volumes mounted read-only.

// luxd → runner (live, not durable) and back.
const (
	MsgDiffRequest = "diff.request"
	MsgDiffCancel  = "diff.cancel" // DiffRequest's SubID: its caller is gone
	MsgDiffResult  = "diff.result" // one per repository and base kind
	MsgDiffEnd     = "diff.end"
)

// runner → luxd: a snapshot's diffs, after its snapshot.done (durable,
// acked; accepted for an older epoch of the host, as snapshot.done is).
const MsgSnapshotDiffs = "snapshot.diffs"

// EvDiffFailed: a snapshot's diff could not be computed, for one
// repository or all ({snapshotId, error, repo?, kind?}). EvDiffSkipped: it
// was not attempted ({snapshotId, reason}). Neither affects the snapshot.
const (
	EvDiffFailed        = "diff.failed"
	EvDiffSkipped       = "diff.skipped"
	EvDiffCleanupFailed = "diff.cleanup_failed" // {snapshotId, error}
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
// caller falls back to the latest snapshot). Busy: another, different live
// diff of the placement is under way (one at a time; an identical request
// shares it instead).
type DiffEnd struct {
	SubID      string `json:"subId"`
	Error      string `json:"error,omitempty"`
	NotRunning bool   `json:"notRunning,omitempty"`
	Busy       bool   `json:"busy,omitempty"`
}

// SnapshotDiffs reports a snapshot's diffs: one per repository and kind,
// each with its error if it has none. Skipped says why none were computed
// (the host is going, the Run resumed here); Error, why they all failed.
type SnapshotDiffs struct {
	SnapshotID string         `json:"snapshotId"`
	Diffs      []SnapshotDiff `json:"diffs"`
	Skipped    string         `json:"skipped,omitempty"`
	Error      string         `json:"error,omitempty"`
	// CleanupFailed: the diff's helper container could not be confirmed
	// removed (why); the runner keeps the Run's volumes fenced until it is.
	CleanupFailed string `json:"cleanupFailed,omitempty"`
}

// SnapshotDiff is a diff computed at a snapshot: Blob holds the patch
// (zstd, like every blob; nil when empty), PatchSHA256 the patch itself.
type SnapshotDiff struct {
	DiffStat
	Blob        *BlobInfo `json:"blob,omitempty"`
	PatchSHA256 string    `json:"patchSha256,omitempty"`
}
