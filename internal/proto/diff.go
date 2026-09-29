package proto

const (
	MsgDiffRequest = "diff.request"
	MsgDiffResult  = "diff.result"
	CapDiff = "diff"
)

const (
	DiffBaseClone = "clone"
	DiffBaseHead  = "head"
)

type DiffRequest struct {
	SubID string `json:"subId"`
	Base  string `json:"base"`
}

type DiffArgs struct {
	Base  string     `json:"base"`
	Repos []DiffRepo `json:"repos"`
}

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

// Patch is bytes because a file may not be valid UTF-8.
type RepoDiff struct {
	Repo       string         `json:"repo"`
	Base       string         `json:"base"`
	Head       string         `json:"head"`
	Patch      []byte         `json:"patch,omitempty"`
	Files      int            `json:"files"`
	Insertions int            `json:"insertions"`
	Deletions  int            `json:"deletions"`
	FileStats  []DiffFileStat `json:"fileStats,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type DiffResult struct {
	SubID      string     `json:"subId,omitempty"`
	Repos      []RepoDiff `json:"repos"`
	Busy       bool       `json:"busy,omitempty"`
	NotRunning bool       `json:"notRunning,omitempty"`
	Error      string     `json:"error,omitempty"`
}
