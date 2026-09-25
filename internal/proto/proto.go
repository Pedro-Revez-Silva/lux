// Package proto defines the messages between luxd and lux-runner, and
// between lux-runner and lux-shim.
//
// luxd ↔ runner: JSON frames over one WebSocket per runner (or, in fallback
// mode, POST /runner/v1/poll). Every luxd→runner message has
// an id and is acked; unacked messages are redelivered on reconnect, so the
// runner handles each idempotently. Everything a runner reports about a Run
// carries the placement's epoch; luxd rejects stale epochs.
package proto

import (
	"encoding/json"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// Version of the runner protocol. luxd refuses runners that do not match.
const Version = 1

// Frame is the envelope for every message in either direction.
type Frame struct {
	Type  string `json:"type"`
	ID    int64  `json:"id,omitempty"`
	RunID string `json:"runId,omitempty"`
	Epoch int    `json:"epoch,omitempty"`
	// Stream routes interactive-stream frames (stream.*) without parsing
	// their data.
	Stream string          `json:"stream,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// luxd → runner
const (
	MsgAssign          = "assign"
	MsgInput           = "input"
	MsgInterrupt       = "interrupt"
	MsgStop            = "stop"
	MsgCancel          = "cancel"
	MsgPush            = "push"
	MsgSnapshotDiscard = "snapshot.discard"
	MsgDrain           = "drain"
	MsgOutputSubscribe = "output.subscribe"
	MsgOutputCancel    = "output.cancel"
	MsgStreamOpen      = "stream.open" // exec, attach, tunnel
	MsgStreamData      = "stream.data"
	MsgStreamClose     = "stream.close"
	MsgAck             = "ack" // luxd acking a runner report (reply)
	MsgNack            = "nack"
	MsgWelcome         = "welcome"
	// MsgExit asks the runner to exit with ExitHost.Code: sent only once
	// luxd has drained the host and it has nothing left to lose, so the
	// runner's systemd unit restarts it and its ExecStartPre re-downloads
	// the binaries first. Never a signal to replace itself in place.
	MsgExit = "exit"
)

// runner → luxd
const (
	MsgHello         = "hello"
	MsgHeartbeat     = "heartbeat"
	MsgStatus        = "status"
	MsgAdapterEvent  = "adapter.event"
	MsgSnapshotDone  = "snapshot.done"
	MsgOutputRecords = "output.records"
	MsgOutputEnd     = "output.end"
	MsgRunEvent      = "run.event"
	MsgHostEvicting  = "host.evicting"
)

type Hello struct {
	Name string `json:"name"`
	// ProviderID is the cloud instance id, for provisioned hosts.
	ProviderID      string            `json:"providerId,omitempty"`
	ProtocolVersion int               `json:"protocolVersion"`
	RunnerVersion   string            `json:"runnerVersion"`
	ShimVersion     string            `json:"shimVersion"`
	PodmanVersion   string            `json:"podmanVersion"`
	Arch            string            `json:"arch"`
	Labels          map[string]string `json:"labels"`
	// Nested: the host offers nested containers (lux-runner --nested).
	// luxd labels it nested=true; no configured label can.
	Nested     bool     `json:"nested,omitempty"`
	Capacity   Capacity `json:"capacity"`
	Images     []string `json:"images"`
	GitMirrors []string `json:"gitMirrors"`
	// Runs whose state volumes are on this host, and as of which epoch.
	LocalSnapshots []LocalSnapshot `json:"localSnapshots"`
	// Placements the runner is still running (re-adopted after a restart).
	Live []LivePlacement `json:"live"`
	// RunnerSHA256, ShimSHA256: sha256 of this runner's own binary
	// (os.Executable()) and of the shim it mounts (--shim). Absent from
	// runners that predate self-update: luxd never drains those for it.
	RunnerSHA256 string `json:"runnerSha256,omitempty"`
	ShimSHA256   string `json:"shimSha256,omitempty"`
}

type Capacity struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk"`
	Runs   int     `json:"runs"`
}

type LocalSnapshot struct {
	RunID string `json:"runId"`
	Epoch int    `json:"epoch"`
}

type LivePlacement struct {
	RunID string `json:"runId"`
	Epoch int    `json:"epoch"`
	State string `json:"state"`
	Usage *Usage `json:"usage,omitempty"`
}

// Usage is a placement's resource use so far, from its cgroup. Peaks, so a
// report can only raise them; the current values (memory, pids) are only
// sampled for history.
type Usage struct {
	PeakMemoryBytes int64   `json:"peakMemoryBytes,omitempty"`
	PeakDiskBytes   int64   `json:"peakDiskBytes,omitempty"`
	PeakPids        int     `json:"peakPids,omitempty"`
	CPUSeconds      float64 `json:"cpuSeconds,omitempty"`
	NetRxBytes      int64   `json:"netRxBytes,omitempty"`
	NetTxBytes      int64   `json:"netTxBytes,omitempty"`
	MemoryBytes     int64   `json:"memoryBytes,omitempty"`
	Pids            int     `json:"pids,omitempty"`
}

// HostUsage is the whole host's, on each heartbeat: counters (CPU seconds
// since boot) and levels now.
type HostUsage struct {
	CPUSeconds  float64 `json:"cpuSeconds"`
	MemoryBytes int64   `json:"memoryBytes"`
	DiskBytes   int64   `json:"diskBytes"`
}

type Welcome struct {
	HostID       string  `json:"hostId"`
	LeaseSeconds float64 `json:"leaseSeconds"`
	// Epochs luxd considers live on this host; the runner stops anything else
	// it finds running.
	Live []LivePlacement `json:"live"`
}

type Heartbeat struct {
	Leases []LivePlacement `json:"leases"`
	// LocalSnapshots: the Runs this host still holds local copies of. luxd
	// forgets any other copy it thought the host had (removed by the host
	// TTL, or by hand), so it never prefers a host for a copy that is gone.
	LocalSnapshots []LocalSnapshot `json:"localSnapshots"`
	// GitMirrors: the repositories this host has mirrors of now.
	GitMirrors []string `json:"gitMirrors"`
	// Usage: the host's, for history (absent from older runners).
	Usage *HostUsage `json:"usage,omitempty"`
	// RunnerSHA256, ShimSHA256: as in Hello, on every heartbeat, so a
	// binary replaced after the runner started (a bad manual copy) is
	// still noticed. Absent from runners that predate self-update.
	RunnerSHA256 string `json:"runnerSha256,omitempty"`
	ShimSHA256   string `json:"shimSha256,omitempty"`
}

// Assign starts (or resumes) a placement. Secrets travel only in this
// message, only over the WebSocket, and are never persisted by either side.
type Assign struct {
	RunID    string            `json:"runId"`
	TenantID string            `json:"tenantId"`
	Epoch    int               `json:"epoch"`
	Spec     spec.RunSpec      `json:"spec"`
	Secrets  map[string]string `json:"secrets"`
	// Resume is set when the Run has run before.
	Resume *ResumeInfo `json:"resume,omitempty"`
	// Input to deliver as the first message (resume with input).
	Input *Input `json:"input,omitempty"`
	// Image build resolution from an earlier placement, so rebuilds use the
	// same pinned FROMs.
	ImageResolved *ImageResolution `json:"imageResolved,omitempty"`
}

// ImageResolution is a built image as its Run's first build made it: the
// Containerfile with every FROM pinned to a digest, and the image id.
// Later placements build from this Containerfile.
type ImageResolution struct {
	Containerfile string `json:"containerfile"`
	ImageID       string `json:"imageId"`
}

type ResumeInfo struct {
	SessionID string `json:"sessionId"`
	// Snapshot to restore. If the runner does not hold it locally it
	// downloads each volume with GET /runner/v1/blobs/{blobId}, which redirects
	// to a short-lived presigned URL.
	Snapshot *Manifest `json:"snapshot,omitempty"`
}

// Manifest describes one snapshot: the Run's state volumes as of one exit.
type Manifest struct {
	SnapshotID string           `json:"snapshotId"`
	RunID      string           `json:"runId"`
	Epoch      int              `json:"epoch"`
	SessionID  string           `json:"sessionId"`
	Volumes    []VolumeSnapshot `json:"volumes"` // never null: see nonNil in snapshot
}

type VolumeSnapshot struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	BlobID string `json:"blobId"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Input struct {
	RequestID string `json:"requestId"`
	Text      string `json:"text,omitempty"`
	// Raw bytes for generic workloads, base64 in JSON.
	Raw       []byte `json:"raw,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// Evicting: the host's provider is taking it away (a spot interruption)
// at Deadline. luxd moves its Runs elsewhere.
type Evicting struct {
	Deadline time.Time `json:"deadline"`
	Reason   string    `json:"reason"`
}

type StopRequest struct {
	Reason string `json:"reason"` // stop | cancel | preempt | drain | timeout | disk
}

// ExitHost tells a static runner to exit once it has drained: its
// binaries are outdated, and its systemd unit's Restart=always brings it
// back after ExecStartPre re-downloads them. Code distinguishes this exit
// from a crash in logs and, for the runner, from a bug (os.Exit(Code),
// not a panic or a signal).
type ExitHost struct {
	Reason string `json:"reason"`
	Code   int    `json:"code"`
}

// ExitCodeOutdatedBinaries: the code luxd sends an ExitHost with when a
// static host's runner or shim no longer matches what luxd holds.
const ExitCodeOutdatedBinaries = 42

// Status is a placement's state as the runner sees it.
type Status struct {
	State    string `json:"state"` // starting | running | stopping | exited | failed
	ExitCode *int   `json:"exitCode,omitempty"`
	// Why it exited: exited | stopped | cancelled | error | ...
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Highest output seq written, once exited.
	OutputSeq int64 `json:"outputSeq,omitempty"`
	// When each phase happened, unix ms: imageReady, volumesRestored,
	// containerStarted, workloadStarted, exited.
	Times map[string]int64 `json:"times,omitempty"`
	Usage *Usage           `json:"usage,omitempty"`
}

// AdapterEvent reports what the adapter learned from the workload.
type AdapterEvent struct {
	SessionID string `json:"sessionId,omitempty"`
	Activity  string `json:"activity,omitempty"` // idle | busy
	// InputAck acknowledges delivery of an input by request id.
	InputAck   string `json:"inputAck,omitempty"`
	InputError string `json:"inputError,omitempty"`
	// InputText is what was delivered (capped; InputTruncated if so).
	InputText      string `json:"inputText,omitempty"`
	InputTruncated bool   `json:"inputTruncated,omitempty"`
}

// SnapshotDone ends every placement, however it exited: its state volumes,
// output and artifacts are on the host's disk. Uploads (PUT
// /runner/v1/blobs/{id}) follow in the background.
type SnapshotDone struct {
	Manifest Manifest `json:"manifest"`
	// Error is set when the state could not be saved; Manifest then has no
	// volumes and the Run keeps its previous snapshot.
	Error     string     `json:"error,omitempty"`
	Output    *BlobInfo  `json:"output,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	OutputSeq int64      `json:"outputSeq"`
}

type BlobInfo struct {
	BlobID string `json:"blobId"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Artifact is a collected file: BlobInfo is its stored blob (zstd), and
// FileSize/FileSHA256 the file itself, as a download returns it.
type Artifact struct {
	BlobInfo
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
	FileSize    int64  `json:"fileSize"`
	FileSHA256  string `json:"fileSha256"`
}

// EvDiskExceeded: a placement wrote more than its resources.disk (its
// writable layer and state volumes). luxd stops it.
const EvDiskExceeded = "disk.exceeded"

// EvGitClone is a repository the runner cloned, or failed to:
// {repo, status: cloned | failed, commit?, error?, requestId?} (requestId:
// the resume that added it).
const EvGitClone = "git.clone"

// RunEvent is a runner-side lifecycle note (image built, volumes restored,
// rebuild differed, DNS lookup…) stored with the Run's events.
type RunEvent struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

type OutputSubscribe struct {
	SubID  string `json:"subId"`
	Since  int64  `json:"since"` // seq; records with seq > since
	Follow bool   `json:"follow"`
}

type OutputRecords struct {
	SubID   string   `json:"subId"`
	Records []Record `json:"records"`
}

type OutputEnd struct {
	SubID string `json:"subId"`
	Error string `json:"error,omitempty"`
}

// Record is one line of a placement's output file.
type Record struct {
	Seq  int64  `json:"seq"`
	Time int64  `json:"t"`  // unix ms
	Ch   string `json:"ch"` // stdout | stderr | event
	Data string `json:"data,omitempty"`
	// Event payload for ch=event.
	Event json.RawMessage `json:"event,omitempty"`
}

// Stream channels for interactive access, multiplexed over the WebSocket.
type StreamOpen struct {
	StreamID string   `json:"streamId"`
	Kind     string   `json:"kind"` // exec | attach | tunnel
	Command  []string `json:"command,omitempty"`
	TTY      bool     `json:"tty,omitempty"`
	Port     int      `json:"port,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Cols     int      `json:"cols,omitempty"`
}

// StreamData is one message of an interactive stream, on every link:
// client ↔ luxd, luxd ↔ runner (in a Frame, Frame.Stream naming the
// stream) and runner ↔ shim (JSON lines after the ShimStream handshake).
type StreamData struct {
	Data []byte `json:"data,omitempty"`
	// Resize, for PTYs.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
	// Channel of output data: stdout | stderr (exec without a TTY).
	Channel string `json:"ch,omitempty"`
	// Exit code when the stream's process ended (on close).
	ExitCode *int `json:"exitCode,omitempty"`
	EOF      bool `json:"eof,omitempty"`
	// Error, on close: why the stream could not be opened, or ended.
	Error string `json:"error,omitempty"`
}

// Push asks the runner to push each repository's current commit to the
// spec's push branch. Leases holds, per repository, the commit this Run
// last pushed there ("" if never): the branch is replaced only if it is
// still there.
type Push struct {
	RequestID string            `json:"requestId"`
	Leases    map[string]string `json:"leases"`
}

// PushResult is one repository's outcome, reported in a git.push event.
type PushResult struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit,omitempty"`
	Status string `json:"status"` // pushed | up-to-date | rejected | failed
	Error  string `json:"error,omitempty"`
}

type Nack struct {
	Error string `json:"error"`
	// Stale means the report's epoch is not the Run's current one: the
	// runner should stop and discard that placement.
	Stale bool `json:"stale,omitempty"`
}

// Poll is the fallback transport's request: acks for messages received and
// reports to deliver.
type Poll struct {
	Acks    []int64 `json:"acks"`
	Reports []Frame `json:"reports"`
}

type PollResponse struct {
	Messages []Frame `json:"messages"`
	Replies  []Frame `json:"replies"`
}

func Marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
