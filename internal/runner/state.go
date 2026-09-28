package runner

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/marcioapm/lux/internal/spec"
)

// runState is what the runner keeps on disk per Run, so a restarted runner
// knows what its containers and volumes are. Never holds secrets.
type runState struct {
	RunID    string `json:"runId"`
	TenantID string `json:"tenantId"`
	Epoch    int    `json:"epoch"`
	// Phase of the current placement: assigned | started | exited | reported
	Phase string `json:"phase"`
	// The snapshot the local state volumes currently hold (after an exit),
	// and its epoch. A resume from that snapshot here moves nothing.
	VolumesSnapshot string `json:"volumesSnapshot,omitempty"`
	VolumesEpoch    int    `json:"volumesEpoch,omitempty"`
	// The Run's volumes as of the current placement.
	Volumes []volumeRef `json:"volumes"`
	Image   string      `json:"image"`
	// Times, unix ms, for status reports.
	Times map[string]int64 `json:"times,omitempty"`
	// Exit, once known.
	Exit *exitRecord `json:"exit,omitempty"`
	// StopReason from luxd, if any.
	StopReason string `json:"stopReason,omitempty"`
	// Stale: luxd fenced this placement off; do not report or upload.
	Stale bool `json:"stale,omitempty"`
	// Egress, as applied: re-applied when a restarted runner re-adopts a
	// running container (the nftables table starts empty). Unrestricted
	// Runs have none.
	Egress *egressState `json:"egress,omitempty"`
	// Spec is the Run's spec, without secret values: what a placement
	// re-adopted after a runner restart (which has no assignment) goes by.
	Spec *spec.RunSpec `json:"spec,omitempty"`
	// LastExitAt, unix ms, for host-local TTL.
	LastExitAt int64 `json:"lastExitAt,omitempty"`
}

type volumeRef struct {
	Name   string `json:"name"`   // spec name
	Volume string `json:"volume"` // podman volume
	Path   string `json:"path"`
	Kind   string `json:"kind"`
}

type exitRecord struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	Message   string `json:"message,omitempty"`
	OutputSeq int64  `json:"outputSeq"`
	Failed    bool   `json:"failed,omitempty"`
}

func readRunState(dir string) (*runState, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s runState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeRunState(dir string, s *runState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "state.json"), b, 0o600)
}

// writeFileAtomic replaces path with b: readers see the old or the new
// contents, never a partial file.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pendingUpload is a blob waiting to be uploaded, persisted beside it.
type pendingUpload struct {
	BlobID string `json:"blobId"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Done   bool   `json:"done"`
}

type snapshotRecord struct {
	RunID   string          `json:"runId"`
	Epoch   int             `json:"epoch"`
	Uploads []pendingUpload `json:"uploads"`
	// Discard: its Run's local copy was removed; delete the files once the
	// last uploads finish.
	Discard bool  `json:"discard,omitempty"`
	Created int64 `json:"created"`
	// Reported: luxd has the snapshot.done that lists these blobs. Before
	// that, luxd answers an upload with 404 (it does not know the blob
	// yet), which must not count as done.
	Reported bool `json:"reported,omitempty"`
}

type egressState struct {
	Unrestricted bool              `json:"unrestricted,omitempty"`
	Interface    string            `json:"interface"`
	Gateway      string            `json:"gateway"`
	Rules        []spec.EgressRule `json:"rules"`
}
