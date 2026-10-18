package proto

import "strings"

const DefaultCheckpointBytes int64 = 512 << 20

// ValidBranch reports whether name is a branch name `git check-ref-format
// --branch` accepts, as refs/heads/<name>: a workload-reported name later
// reaches git's argv on another host.
func ValidBranch(name string) bool {
	if name == "" || name == "HEAD" || name == "@" || strings.HasPrefix(name, "-") ||
		strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsAny(name, " ~^:?*[\\\x7f") {
		return false
	}
	for _, c := range name {
		if c < 0x20 {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

const MsgCheckpointReady = "checkpoint.ready"

type CheckpointReady struct {
	Blob BlobInfo `json:"blob"`
	StagedCheckpoint
}

// CheckpointRepo is a checkout to bundle as the workload user.
type CheckpointRepo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Previous string `json:"previous,omitempty"`
}

type CheckpointArgs struct {
	Dir      string           `json:"dir"`
	Repos    []CheckpointRepo `json:"repos"`
	MaxBytes int64            `json:"maxBytes"`
}

// StagedCheckpoint names files relative to CheckpointArgs.Dir.
type StagedCheckpoint struct {
	Repo      string            `json:"repo"`
	Head      string            `json:"head,omitempty"`
	Branch    string            `json:"branch,omitempty"`
	Refs      map[string]string `json:"refs,omitempty"`
	Size      int64             `json:"size"`
	File      string            `json:"file,omitempty"`
	Unchanged bool              `json:"unchanged,omitempty"`
	Error     string            `json:"error,omitempty"`
}
