package proto

// Live diffs: what a running Run changed in its repositories, computed by
// `lux-shim diff` inside its container as the workload user. luxd sends
// diff.request over the runner's WebSocket and gets one diff.result back.
const (
	MsgDiffRequest = "diff.request"
	MsgDiffResult  = "diff.result"
	// CapDiff is the Hello capability of a runner that answers diff.request.
	CapDiff = "diff"
)

// Base kinds: the commit each repository was cloned at, or its HEAD.
const (
	DiffBaseClone = "clone"
	DiffBaseHead  = "head"
)

// DiffRequest asks the runner for a placement's diff.
type DiffRequest struct {
	SubID string `json:"subId"`
	Base  string `json:"base"`
}

// DiffArgs is `lux-shim diff`'s one argument, as JSON.
type DiffArgs struct {
	Base  string     `json:"base"`
	Repos []DiffRepo `json:"repos"`
}

// DiffRepo is a checkout to diff; Base is its clone commit ("" unknown).
type DiffRepo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Base string `json:"base,omitempty"`
}

type DiffFileStat struct {
	Name       string `json:"name"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary,omitempty"`
}

// RepoDiff is one repository's diff. Patch is bytes, not a string: a text
// file in another encoding is not valid UTF-8. Omitted: up to OmitCap of
// the OmittedCount paths the diff cannot show (Truncated then).
type RepoDiff struct {
	Repo         string         `json:"repo"`
	Base         string         `json:"base"`
	Head         string         `json:"head"`
	Patch        []byte         `json:"patch,omitempty"`
	Files        int            `json:"files"`
	Insertions   int            `json:"insertions"`
	Deletions    int            `json:"deletions"`
	FileStats    []DiffFileStat `json:"fileStats,omitempty"`
	Truncated    bool           `json:"truncated,omitempty"`
	Omitted      []string       `json:"omitted,omitempty"`
	OmittedCount int            `json:"omittedCount,omitempty"`
	Error        string         `json:"error,omitempty"`
}

const OmitCap = 50

// DiffResult is the shim's output and the runner's answer. Busy: another
// diff of the placement is under way. NotRunning: its container is not.
type DiffResult struct {
	SubID      string     `json:"subId,omitempty"`
	Repos      []RepoDiff `json:"repos"`
	Busy       bool       `json:"busy,omitempty"`
	NotRunning bool       `json:"notRunning,omitempty"`
	Error      string     `json:"error,omitempty"`
}
