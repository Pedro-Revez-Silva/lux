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

// MaxReconnectWait is the longest a runner waits between attempts to reach
// luxd; luxd allows for it after an outage.
const MaxReconnectWait = 10 * time.Second

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
	MsgTerminate       = "cancel" // a terminate request; "cancel" on the wire, as every runner knows it
	MsgPush            = "push"
	MsgSnapshotDiscard = "snapshot.discard"
	MsgDrain           = "drain"
	MsgOutputSubscribe = "output.subscribe"
	MsgOutputCancel    = "output.cancel"
	MsgStreamOpen      = "stream.open" // exec, attach, tunnel
	MsgStreamData      = "stream.data"
	MsgStreamClose     = "stream.close"
	// MsgServers is a placement's servers: the whole desired set, durable
	// and epoch-fenced like input. The runner keeps the newest (by Rev)
	// and reconciles to it.
	MsgServers = "servers"
	// MsgSync moves a running placement's checkouts to new commits.
	MsgSync    = "sync"
	MsgAck     = "ack" // luxd acking a runner report (reply)
	MsgNack    = "nack"
	MsgWelcome = "welcome"
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
	// MsgArtifactPublished: a file the workload published (lux-shim
	// publish), its blob on the host. Acked; Ack.Refused means luxd will
	// never ask for the blob.
	MsgArtifactPublished = "artifact.published"
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
	// Capabilities: optional features this runner has (CapDiff, CapSyncModes).
	Capabilities []string `json:"capabilities,omitempty"`
}

type Capacity struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"`
	Disk   int64   `json:"disk"`
	Runs   int     `json:"runs"`
	// With CapDiskScale, Disk is nominal: each Run's disk limit is its
	// share of FSCapacity - DiskHeadroom (bytes, as the runner read them
	// at start). Absent otherwise.
	DiskHeadroom int64 `json:"diskHeadroom,omitempty"`
	FSCapacity   int64 `json:"fsCapacity,omitempty"`
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
	// PeakCPUCores: the highest CPU rate (cores) between two consecutive
	// readings; CPUCores: the rate over the interval this reading ends, for
	// history. The stall totals are the cgroup's PSI "some" (and "full")
	// totals in seconds, absent without PSI. Absent from older runners.
	PeakCPUCores           *float64 `json:"peakCpuCores,omitempty"`
	CPUCores               *float64 `json:"cpuCores,omitempty"`
	CPUStallSeconds        *float64 `json:"cpuStallSeconds,omitempty"`
	MemoryStallSeconds     *float64 `json:"memoryStallSeconds,omitempty"`
	MemoryFullStallSeconds *float64 `json:"memoryFullStallSeconds,omitempty"`
	IOStallSeconds         *float64 `json:"ioStallSeconds,omitempty"`
	IOFullStallSeconds     *float64 `json:"ioFullStallSeconds,omitempty"`
	// Net: bytes forwarded for the placement's bridge per NetKey,
	// cumulative over the placement (NetRxBytes/NetTxBytes stay the
	// container's own totals). Absent from older runners and when nothing
	// was counted.
	Net map[string]NetBytes `json:"net,omitempty"`
}

// NetClass is a billing class of network transfer, provider-neutral: the
// runner never knows which cloud it is on, only which addresses fall in
// which class (NetworkClasses).
type NetClass string

const (
	// NetLocal: same zone, private network (AWS: same-AZ private IP).
	NetLocal NetClass = "local"
	// NetZone: another zone of the same region, private network (AWS:
	// cross-AZ).
	NetZone NetClass = "zone"
	// NetRegionService: the provider's services in the same region that are
	// free to reach (AWS: S3, DynamoDB).
	NetRegionService NetClass = "region-service"
	// NetRegionPublic: the provider's public addresses in the same region
	// that are not free services (AWS: EC2 and Elastic IP public addresses).
	NetRegionPublic NetClass = "region-public"
	// NetProvider: the provider's addresses in another region. Counted per
	// destination region (NetKey), since inter-region rates differ by it.
	NetProvider NetClass = "provider"
	// NetProviderGlobalFree: provider prefixes published with no region
	// whose transfer from the provider's compute is free (AWS: CLOUDFRONT
	// only; origin fetches from EC2 to CloudFront are not charged).
	NetProviderGlobalFree NetClass = "provider-global-free"
	// NetProviderGlobal: the other provider prefixes published with no
	// region (AWS's "GLOBAL": Route 53, Global Accelerator, AMAZON). AWS
	// bills them as data transfer out to the internet.
	NetProviderGlobal NetClass = "provider-global"
	// NetInternet: everything no list holds, when the host has a provider
	// catalog.
	NetInternet NetClass = "internet"
	// NetUnknown: everything off the host's own subnets when there is no
	// catalog to tell the provider from the internet. Never priced.
	NetUnknown NetClass = "unknown"
)

// NetClasses are every class, in classification precedence: an address in
// several lists takes the first. Narrower before wider: a private subnet is
// never also a public prefix, and a free service's prefixes are listed
// inside the provider's broader in-region ranges (AWS's AMAZON entries
// contain S3's), so the free service must win over region-public, and
// CloudFront over the rest of GLOBAL. Internet and unknown are the
// fall-through, one or the other (NetworkClasses.Catalog).
var NetClasses = []NetClass{NetLocal, NetZone, NetRegionService, NetRegionPublic, NetProvider, NetProviderGlobalFree, NetProviderGlobal, NetInternet, NetUnknown}

// NetKey is a Usage.Net key: the class, or "provider:<region>" for
// NetProvider, which is only ever counted per destination region.
func NetKey(c NetClass, region string) string {
	if c == NetProvider {
		return string(NetProvider) + ":" + region
	}
	return string(c)
}

// NetBytes is one class's bytes: Rx received from peers in the class, Tx
// sent to them. Both are billed by most providers (AWS bills cross-AZ and
// in-region public traffic in each direction). Rx counts what passed the
// host's filters; RxDelivered counts inbound ahead of every filter, as the
// provider delivered (and bills) it, so RxDelivered >= Rx when both were
// counted from the start. Absent from older runners.
type NetBytes struct {
	Rx          int64 `json:"rx"`
	Tx          int64 `json:"tx"`
	RxDelivered int64 `json:"rxDelivered,omitempty"`
}

// MaxNet is the per-key, per-direction maximum of a and b: how luxd merges
// reports of a counter that only grows. nil when both are empty.
func MaxNet(a, b map[string]NetBytes) map[string]NetBytes {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]NetBytes, max(len(a), len(b)))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		o := out[k]
		out[k] = NetBytes{Rx: max(o.Rx, v.Rx), Tx: max(o.Tx, v.Tx), RxDelivered: max(o.RxDelivered, v.RxDelivered)}
	}
	return out
}

func ValidNetworkRegion(r string) bool {
	if r == "" || len(r) > 40 {
		return false
	}
	for _, c := range r {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Catalog sources (NetworkClasses.Source).
const (
	NetSourceEC2    = "ec2-ip-ranges" // AWS's published ip-ranges.json
	NetSourceStatic = "static"        // lists an operator configured
	NetSourceNone   = "none"          // no catalog: Local/Zone at most
)

// NetworkClasses are the address lists a runner classifies traffic by,
// built by luxd from what the host's provider says about it. CIDRs, IPv4
// or IPv6. Precedence is NetClasses' order. With neither Local nor Zone (an
// older luxd, a host luxd knows nothing about), or provisional classes, the
// runner's own interface subnets are Local; a runner without
// CapProvisionalNet does so only when Zone is empty. An address in no list
// is internet when Catalog() and unknown otherwise: without a catalog "not
// listed" may well be the provider's, and must not read as internet or as
// free.
type NetworkClasses struct {
	// Version changes whenever any list does.
	Version string `json:"version"`
	// Source: where the provider lists came from (NetSource*); FetchedAt:
	// when that catalog was fetched, absent for none.
	Source         string     `json:"source"`
	FetchedAt      *time.Time `json:"fetchedAt,omitempty"`
	Local          []string   `json:"local,omitempty"`
	Zone           []string   `json:"zone,omitempty"`
	RegionService  []string   `json:"regionService,omitempty"`
	RegionPublic   []string   `json:"regionPublic,omitempty"`
	ProviderGlobal []string   `json:"providerGlobal,omitempty"`
	// ProviderGlobalFree: GLOBAL prefixes of the provider's free services
	// (AWS: CLOUDFRONT); an older runner ignores the field and counts them
	// as internet.
	ProviderGlobalFree []string `json:"providerGlobalFree,omitempty"`
	// Provider: by region, the provider's prefixes in every other region.
	Provider map[string][]string `json:"provider,omitempty"`
	// Provisional: a new host's classes before its network was discovered
	// (sent only to a runner with CapProvisionalNet): the region's lists,
	// no Local (the runner's interface subnets stand in) and the private
	// address space as Zone. Discovery replaces them.
	Provisional bool `json:"provisional,omitempty"`
}

// CapProvisionalNet is the Hello capability of a runner that fills an empty
// NetworkClasses.Local from its interface subnets for provisional classes
// even when Zone is not empty; only such a runner is sent them.
const CapProvisionalNet = "provisional-net"

// Catalog: nc holds a provider catalog, so unlisted addresses are internet.
func (nc *NetworkClasses) Catalog() bool {
	return nc != nil && nc.Source != "" && nc.Source != NetSourceNone
}

// HostUsage is the whole host's, on each heartbeat: counters (CPU seconds
// since boot) and levels now.
type HostUsage struct {
	CPUSeconds  float64 `json:"cpuSeconds"`
	MemoryBytes int64   `json:"memoryBytes"`
	DiskBytes   int64   `json:"diskBytes"`
	// The filesystem DiskBytes is on: FSFreeBytes what unprivileged
	// writers can still use. FSCapacityBytes and DiskHeadroomBytes are the
	// runner's disk scaling's, absent without CapDiskScale. Absent from
	// older runners.
	FSFreeBytes       *int64 `json:"fsFreeBytes,omitempty"`
	FSCapacityBytes   *int64 `json:"fsCapacityBytes,omitempty"`
	DiskHeadroomBytes *int64 `json:"diskHeadroomBytes,omitempty"`
	// DiskPressure: set while the runner's low-free-space guard holds the
	// host under pressure (disk-scale runners only). Absent otherwise, and
	// from older runners: luxd reads absent as no pressure.
	DiskPressure *DiskPressure `json:"diskPressure,omitempty"`
	// Net: the host's own traffic per NetKey (the runner's link to luxd,
	// image pulls, git fetches, snapshot uploads), cumulative; never a
	// Run's forwarded traffic, which its Usage.Net counts. Absent from
	// older runners.
	Net map[string]NetBytes `json:"net,omitempty"`
}

// DiskPressure is a host under disk pressure: free bytes of the data
// directory's filesystem fell below LowFreeBytes, and pressure lasts until
// they are at least RecoverFreeBytes. LowFree and RecoverFree are the marks
// as the operator gave them ("5%", "8.0 GiB"). NoRelief: the latest pass
// found no unused image to remove and no over-limit placement to stop.
type DiskPressure struct {
	FreeBytes        int64  `json:"freeBytes"`
	LowFreeBytes     int64  `json:"lowFreeBytes"`
	RecoverFreeBytes int64  `json:"recoverFreeBytes"`
	LowFree          string `json:"lowFree"`
	RecoverFree      string `json:"recoverFree"`
	NoRelief         bool   `json:"noRelief,omitempty"`
}

// ProcessUsage is the runner process's own, on each heartbeat. CPUSeconds
// counts from Started, so a restart starts it again. PeakRSSBytes is the
// highest RSS since the previous heartbeat, absent when the runner cannot
// reset the kernel's high-water mark.
type ProcessUsage struct {
	Started      time.Time `json:"started"`
	CPUSeconds   float64   `json:"cpuSeconds"`
	RSSBytes     int64     `json:"rssBytes"`
	PeakRSSBytes *int64    `json:"peakRssBytes,omitempty"`
	HeapBytes    int64     `json:"heapBytes"`
	Goroutines   int64     `json:"goroutines"`
}

type Welcome struct {
	HostID       string  `json:"hostId"`
	LeaseSeconds float64 `json:"leaseSeconds"`
	// Epochs luxd considers live on this host; the runner stops anything else
	// it finds running.
	Live []LivePlacement `json:"live"`
	// Network: the address lists this host classifies traffic by; nil from
	// an older luxd. A heartbeat's Ack.NetworkVersion tells the runner when
	// they change (it fetches GET /runner/v1/network).
	Network *NetworkClasses `json:"network,omitempty"`
	// HostNet: the host's own traffic as luxd last recorded it. A runner
	// whose host counters start from zero (the host rebooted) counts on
	// from these rather than from zero, which luxd's maximum would hide.
	HostNet map[string]NetBytes `json:"hostNet,omitempty"`
	// ControlPlanePorts: TCP ports on the control plane's addresses Runs
	// may reach (runners.control_plane_ports). The runner replaces its
	// list with this one on every Welcome; none (an older luxd included)
	// keeps the control plane fully blocked.
	ControlPlanePorts []int `json:"controlPlanePorts,omitempty"`
}

// MaxControlPlanePorts caps Welcome.ControlPlanePorts: luxd refuses more at
// startup, and a runner applies none of a longer list.
const MaxControlPlanePorts = 16

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
	// Runner: the runner process's own use, for history (absent from
	// older runners).
	Runner *ProcessUsage `json:"runner,omitempty"`
	// RunnerSHA256, ShimSHA256: as in Hello, on every heartbeat, so a
	// binary replaced after the runner started (a bad manual copy) is
	// still noticed. Absent from runners that predate self-update.
	RunnerSHA256 string `json:"runnerSha256,omitempty"`
	ShimSHA256   string `json:"shimSha256,omitempty"`
}

// Assign starts (or resumes) a placement. Secrets travel only in this
// message, only over the WebSocket, and are never persisted by either side.
type Assign struct {
	CheckpointBytes int64             `json:"checkpointBytes,omitempty"`
	CheckpointRefs  map[string]string `json:"checkpointRefs,omitempty"`
	RunID           string            `json:"runId"`
	TenantID        string            `json:"tenantId"`
	Epoch           int               `json:"epoch"`
	Spec            spec.RunSpec      `json:"spec"`
	Secrets         map[string]string `json:"secrets"`
	// Resume is set when the Run has run before.
	Resume *ResumeInfo `json:"resume,omitempty"`
	// Input to deliver as the first message (resume with input).
	Input *Input `json:"input,omitempty"`
	// PromptAttachments: workload.attachments with their bytes, which
	// Spec leaves out; only on a first placement (Resume nil).
	PromptAttachments []spec.Attachment `json:"promptAttachments,omitempty"`
	// Image build resolution from an earlier placement, so rebuilds use the
	// same pinned FROMs.
	ImageResolved *ImageResolution `json:"imageResolved,omitempty"`
	// GitBases: per repository, the commit an earlier placement cloned or
	// last synced it to, for repositories this one restores rather than
	// clones.
	GitBases map[string]string `json:"gitBases,omitempty"`
	// SyncBases: per repository, the last commit an earlier placement
	// fetched into the checkout this one restores (its clone, or any sync
	// that did not fail): the prerequisite of its next sync's bundle.
	SyncBases map[string]string `json:"syncBases,omitempty"`
	// Sync: repositories whose restored checkout moves to a ref before init.
	Sync []SyncRef `json:"sync,omitempty"`
	// RegistryCredentials: pull credentials luxd minted for the operator's
	// in-region registry, attached at send time like Secrets (never
	// stored), only to a runner with CapRegistryMinted.
	RegistryCredentials []RegistryCredential `json:"registryCredentials,omitempty"`
}

// RegistryCredential is one containers auth.json entry. Registry is the
// entry's key, host/namespace (<host>/<owner>), so it applies to that
// owner's repositories only.
type RegistryCredential struct {
	Registry string `json:"registry"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// CapRegistryMinted is the Hello capability of a runner that writes
// Assign.RegistryCredentials into its auth file.
const CapRegistryMinted = "registry-minted"

// SyncRef asks for a repository's checkout to be moved to ref (a branch,
// tag or sha), fetched through the host's mirror. Mode says when it may
// move (SyncModes; "" is SyncMove).
type SyncRef struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Mode string `json:"mode,omitempty" doc:"move (default): the checkout becomes the ref's, a dirty or diverged one reset (what was there saved as refs/lux/pre-sync). fast-forward: it moves only when nothing can be lost (HEAD an ancestor of the ref's commit, no tracked file changed, HEAD on the ref's branch or, for a tag or sha, detached, no merge, rebase, am, cherry-pick or revert in progress); otherwise it is kept as it is. fetch: it never moves. In fast-forward and fetch, a branch's commit is also refs/remotes/lux/<branch> in the checkout, and the git.sync event's operation names a git operation in progress in the checkout (merge, rebase, am, cherry-pick, revert or sequencer). Any other value: 422."`
}

// Sync modes: how a checkout may move.
const (
	SyncMove        = "move"
	SyncFastForward = "fast-forward"
	SyncFetch       = "fetch"
)

// SyncModes are the modes a SyncRef may ask for.
var SyncModes = []string{SyncMove, SyncFastForward, SyncFetch}

// SyncModeOf is mode, or SyncMove for "".
func SyncModeOf(mode string) string {
	if mode == "" {
		return SyncMove
	}
	return mode
}

// SafeSyncModes reports whether refs ask for a mode other than move: one
// an older runner or lux-shim, which knows only move, would run as move.
func SafeSyncModes(refs []SyncRef) bool {
	for _, r := range refs {
		if SyncModeOf(r.Mode) != SyncMove {
			return true
		}
	}
	return false
}

const (
	// CapSyncModes is the Hello capability of a runner that carries a
	// sync's mode, with a lux-shim that applies it.
	CapSyncModes = "sync-modes"
	// SyncModeProbe is no mode: a lux-shim that knows modes fails it and
	// names it in its result's mode; an older one has no mode to name.
	SyncModeProbe = "lux-probe"
)

// Sync is MsgSync: a running placement's repositories to move. Servers
// with afterSync restart after it (luxd sends the new set).
type Sync struct {
	RequestID string    `json:"requestId"`
	Repos     []SyncRef `json:"repos"`
	// Credentials: per repository name, a git credential that replaces the
	// one the runner holds, from this operation on (see Push).
	Credentials map[string]string `json:"credentials,omitempty"`
	// CredentialRepos: luxd's own record of the names in Credentials,
	// whose values it never stores.
	CredentialRepos []string `json:"credentialRepos,omitempty"`
}

// CapGitCredentials is the Hello capability of a runner that takes
// Push.Credentials and Sync.Credentials. A runner without it drops them
// and uses the credential it was assigned.
const CapGitCredentials = "git-credentials"

// CapSpotReclaim identifies a runner enforcing a durable first-notice 15s
// reclamation deadline, independent of workload grace and stop hooks.
const CapSpotReclaim = "spot-reclaim-v1"

// CapDiskScale is the Hello capability of a runner that holds each Run to
// its share of the filesystem (lux-runner --disk with --disk-headroom):
// Capacity.Disk is then nominal. Shaped pools place Runs only on such
// runners.
const CapDiskScale = "disk-scale"

// CapNonPreemptible is the Hello capability of a runner whose disk-pressure
// guard never stops a placement whose spec has placement.preemptible false.
// Non-preemptible Runs are placed only on such runners.
const CapNonPreemptible = "non-preemptible"

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
	// Attachments: images that are one message with Text.
	Attachments []spec.Attachment `json:"attachments,omitempty"`
}

// HasContent: the input carries something for the agent to read.
func (in Input) HasContent() bool { return in.Text != "" || len(in.Attachments) > 0 }

// Evicting: the host's provider is taking it away (a spot interruption)
// at Deadline. luxd moves its Runs elsewhere.
type Evicting struct {
	Deadline time.Time `json:"deadline"`
	Reason   string    `json:"reason"`
}

type StopRequest struct {
	Reason string `json:"reason"` // stop | terminate | preempt | drain | timeout | disk (an older luxd: cancel for terminate)
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
	// Why it exited: exited | stopped | error | ...
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Highest output seq written, once exited.
	OutputSeq int64 `json:"outputSeq,omitempty"`
	// When each phase happened, unix ms: imageReady, volumesRestored,
	// containerStarted, workloadStarted, exited.
	Times map[string]int64 `json:"times,omitempty"`
	Usage *Usage           `json:"usage,omitempty"`
	// MemoryLimit is the container's memory limit in bytes, once it has
	// one: the Run's resources.memory scaled to the host's share.
	MemoryLimit int64 `json:"memoryLimit,omitempty"`
	// ImagePlatforms: the platforms (os/arch[/variant]) the image's
	// reference offers, once the image is ready; absent when unknown.
	ImagePlatforms []string `json:"imagePlatforms,omitempty"`
}

// AdapterEvent reports what the adapter learned from the workload.
type AdapterEvent struct {
	SessionID string `json:"sessionId,omitempty"`
	Activity  string `json:"activity,omitempty"` // idle | busy
	// InputAck names an input by request id: its first answer, once.
	// InputPhase is accepted or failed ("" from an older runner: accepted,
	// or failed with InputError).
	InputAck   string `json:"inputAck,omitempty"`
	InputPhase string `json:"inputPhase,omitempty"`
	InputError string `json:"inputError,omitempty"`
	// InputText is what was delivered (capped; InputTruncated if so).
	InputText      string `json:"inputText,omitempty"`
	InputTruncated bool   `json:"inputTruncated,omitempty"`
	// InputLands and InputReceipt: on accepted, when the agent reads it,
	// and whether a consumed follows.
	InputLands   string `json:"inputLands,omitempty"`
	InputReceipt bool   `json:"inputReceipt,omitempty"`
	// InputAttachments: what the input's images were (never their bytes).
	InputAttachments []spec.AttachmentMeta `json:"inputAttachments,omitempty"`
	// InputProgress is what happened to an input after it was accepted. A
	// field of its own, so a luxd that does not know it ignores it rather
	// than reading it as a second InputAck.
	InputProgress *InputProgress `json:"inputProgress,omitempty"`
}

// InputProgress: an accepted input consumed, or failed.
type InputProgress struct {
	RequestID string `json:"requestId"`
	Phase     string `json:"phase"` // InputConsumed | InputFailed
	Error     string `json:"error,omitempty"`
	// Attachments, on failed: what the input's images were.
	Attachments []spec.AttachmentMeta `json:"attachments,omitempty"`
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

// BlobPartSize is the part size of a direct-to-S3 multipart upload: the
// runner splits a blob larger than this into parts of exactly this size
// (the last one shorter).
const BlobPartSize = 64 << 20

// BlobParts is how many parts a blob of size bytes is uploaded in: one for
// an empty blob.
func BlobParts(size int64) int {
	return max(1, int((size+BlobPartSize-1)/BlobPartSize))
}

// BlobUploadRequest is POST /runner/v1/blobs/{id}/upload: the runner asking
// for presigned URLs to upload a blob straight to S3. SHA256 is the whole
// file's (hex), Parts each part's SHA-256 (base64), in order.
type BlobUploadRequest struct {
	Size   int64    `json:"size"`
	SHA256 string   `json:"sha256"`
	Parts  []string `json:"parts"`
}

// PresignedRequest is a URL to PUT to with exactly Headers: they are
// signed into it.
type PresignedRequest struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type PresignedPart struct {
	Number int32 `json:"number"`
	PresignedRequest
}

// BlobUploadResponse is one of: Uploaded (S3 already has it), Single (one
// PutObject), or UploadID and Parts (a multipart upload).
type BlobUploadResponse struct {
	Uploaded bool              `json:"uploaded,omitempty"`
	Single   *PresignedRequest `json:"single,omitempty"`
	UploadID string            `json:"uploadId,omitempty"`
	Parts    []PresignedPart   `json:"parts,omitempty"`
}

// BlobCompleteRequest is POST /runner/v1/blobs/{id}/complete; UploadID and
// Parts are empty after a single PutObject. Abort drops multipart upload
// UploadID instead (the runner gave up on it): luxd answers
// {"aborted": true} and records nothing.
type BlobCompleteRequest struct {
	UploadID string         `json:"uploadId,omitempty"`
	Parts    []UploadedPart `json:"parts,omitempty"`
	Abort    bool           `json:"abort,omitempty"`
}

type UploadedPart struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"` // base64
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

// ArtifactPublished is MsgArtifactPublished's data. ID is the artifact id
// the shim assigned: luxd's artifact id too, so a report sent again (a
// restarted runner re-reads the output file) is recognised.
type ArtifactPublished struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Artifact
}

// EvDiskExceeded: a placement was over its resources.disk (its writable
// layer and state volumes) on every sample for its resources.diskGrace, or,
// on a host that does not scale disk, on one sample at
// disklimit.CeilingFactor times the disk. luxd stops it.
const EvDiskExceeded = "disk.exceeded"

// EvDiskReleaseFailed: a spot placement's container or volumes could not be
// removed at its end (data: error). Sent once; the runner keeps retrying,
// and the end stays unreported, its disk reserved, until removal succeeds.
const EvDiskReleaseFailed = "disk.release_failed"

// EvDiskPressureStop: the runner's low-free-space guard asks luxd to stop a
// placement over its effective disk limit while the host is under disk
// pressure (data: usedBytes, limitBytes, excessBytes, freeBytes,
// lowFreeBytes, recoverFreeBytes). luxd stops it; it ends failed.
const EvDiskPressureStop = "disk.pressure_stop"

// EvGitClone is a repository the runner cloned, or failed to:
// {repo, status: cloned | failed, commit?, error?, requestId?} (requestId:
// the resume that added it).
const EvGitClone = "git.clone"

// EvGitSync is a repository's sync (proto.SyncResult, and requestId for a
// sync of a running Run).
const EvGitSync = "git.sync"

// EvSyncDone ends a running Run's sync: {requestId, changed}. luxd
// restarts the servers with afterSync when a checkout moved.
const EvSyncDone = "sync.done"

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
	Ch   string `json:"ch"` // stdout | stderr | event | server
	Data string `json:"data,omitempty"`
	// Event payload for ch=event (and a server's lifecycle, ch=server).
	Event json.RawMessage `json:"event,omitempty"`
	// Server and Stream, for ch=server: which server's process wrote it,
	// and on which of its streams (stdout | stderr).
	Server string `json:"server,omitempty"`
	Stream string `json:"stream,omitempty"`
}

// Servers is a placement's servers as luxd wants them: every server that
// should be running (with a command) or watched (without one) now, and
// the ports of all the Run's servers (a tunnel may reach any of them).
type Servers struct {
	Rev     int64        `json:"rev"`
	Servers []ServerSpec `json:"servers"`
	Ports   []int        `json:"ports,omitempty"`
}

// ServerSpec is one server to run or watch. Gen changes with every start
// (and stop): a process or a report of another Gen is not this one's.
type ServerSpec struct {
	Name    string            `json:"name"`
	Port    int               `json:"port"`
	Gen     int64             `json:"gen"`
	Command []string          `json:"command,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// UnmovedCommand, when set, replaces Command if the placement's sync
	// before init moved no checkout: Command runs afterSync first, this
	// one does not. A shim that does not know it runs Command.
	UnmovedCommand []string `json:"unmovedCommand,omitempty"`
}

// EvServerState is the runner's report of a server's state (a RunEvent):
// {name, gen, state: starting | ready | unreachable | exited, exitCode?,
// error?}. luxd applies it only for the server's current gen.
const EvServerState = "server.state"

// Stream channels for interactive access, multiplexed over the WebSocket.
type StreamOpen struct {
	StreamID string   `json:"streamId"`
	Kind     string   `json:"kind"` // exec | attach | tunnel
	Command  []string `json:"command,omitempty"`
	TTY      bool     `json:"tty,omitempty"`
	Port     int      `json:"port,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Cols     int      `json:"cols,omitempty"`
	// Restore, attach only: the output starts with a snapshot of the
	// terminal (sized Rows×Cols when given), then continues live.
	Restore bool `json:"restore,omitempty" doc:"attach only, luxd to runner (a client asks with attach's restore, rows and cols query parameters): start the output with a snapshot of the terminal, then live output."`
	// Window is flow control for the stream's output: the runner sends at
	// most this many output StreamData frames ahead of luxd, which grants
	// more (StreamData.Credit) as its reader takes them. 0 (a luxd from
	// before): no flow control.
	Window int `json:"window,omitempty" doc:"luxd to runner only (tunnel flow control); ignored from clients."`
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
	// Credit, luxd → runner only: this many more output frames may be sent
	// (StreamOpen.Window). A runner from before takes it as empty input.
	Credit int `json:"credit,omitempty" doc:"luxd to runner only (tunnel flow control); a client that sends it ends its stream."`
	// Restored, attach with restore only, host to client: the snapshot
	// (if any) is over and live output follows. Sent once, with no data.
	Restored bool `json:"restored,omitempty" doc:"attach with restore only, host to client: sent once, with no data, after the snapshot's last output message (or first, when the host sends no snapshot); live output follows. A host from before sends neither snapshot nor this."`
}

// Push asks the runner to push each repository's current commit to the
// spec's push branch. Leases holds, per repository, the commit this Run
// last pushed there ("" if never): the branch is replaced only if it is
// still there.
//
// Credentials, like Assign.Secrets, are attached by luxd at delivery and
// never stored by either side: per repository name, a git credential the
// runner uses for this push and keeps, in memory, for every later fetch
// and push of that repository. CredentialRepos is what luxd stores in
// their place: the names, so a luxd that no longer holds the values
// refuses the message rather than delivering it without them.
type Push struct {
	RequestID       string            `json:"requestId"`
	Leases          map[string]string `json:"leases"`
	Credentials     map[string]string `json:"credentials,omitempty"`
	CredentialRepos []string          `json:"credentialRepos,omitempty"`
}

// PushResult is one repository's outcome, reported in a git.push event.
// Refused: the checkout has Operation in progress (OperationOf); nothing
// was pushed.
type PushResult struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit,omitempty"`
	Status    string `json:"status"` // pushed | up-to-date | rejected | refused | failed | skipped
	Operation string `json:"operation,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Ack is the data of an ack, when there is any.
type Ack struct {
	// Refused: luxd accepted a snapshot.done or artifact.published report
	// but did not record it (it did not match its Run's records). Its blobs
	// will not be asked for: the runner does not upload them and may delete
	// them.
	Refused bool `json:"refused,omitempty"`
	// NetworkVersion, on a heartbeat's ack only: the version of the host's
	// NetworkClasses now. A runner holding another fetches them.
	NetworkVersion string `json:"networkVersion,omitempty"`
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
