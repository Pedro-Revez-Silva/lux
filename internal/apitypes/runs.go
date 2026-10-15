// Package apitypes holds the response types of luxd's API that clients
// decode into: the Run, the submit answer, events and pools. They are
// declared here, apart from internal/server (which aliases them), so a
// client can use them without depending on luxd's own dependencies.
package apitypes

import (
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Run is the API representation of a Run.
type Run struct {
	ID          string            `json:"id"`
	Tenant      string            `json:"tenant" doc:"The owning tenant's name."`
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels"`
	State       string            `json:"state"`
	Resumable   bool              `json:"resumable" doc:"Whether POST /v1/runs/{id}/resume without fromSnapshot would be accepted: its state is stopped, lost, failed or succeeded, its resumePolicy is not never, and it is not left with only a refused snapshot report. The same test as GET /v1/runs?resumable=true. As of this answer: a Run being stopped is not resumable until it is stopped."`
	StateReason string            `json:"stateReason,omitempty"`
	Activity    string            `json:"activity,omitempty"`
	Steer       *spec.Steer       `json:"steer,omitempty" doc:"What the Run's adapter does with input sent while the agent works (POST /v1/runs/{id}/input)."`
	ExitCode    *int              `json:"exitCode,omitempty"`
	Epoch       int               `json:"epoch"`
	SessionID   string            `json:"sessionId,omitempty"`
	SnapshotID  *string           `json:"snapshotId,omitempty"`
	Host        string            `json:"host,omitempty" doc:"The name of the host of its current placement."`
	HostID      string            `json:"hostId,omitempty" doc:"That host's id."`
	Pool        string            `json:"pool,omitempty" doc:"The current name of the pool the Run is bound to (spec.placement.pool is the name it was submitted with)."`
	PoolID      string            `json:"poolId,omitempty" doc:"That pool's id."`
	Spec        spec.RunSpec      `json:"spec"`
	// Image is how a built image was resolved on its first build.
	Image          *proto.ImageResolution `json:"image,omitempty"`
	Secrets        []spec.SecretRef       `json:"secrets"`
	CreatedAt      time.Time              `json:"createdAt"`
	ScheduledAt    *time.Time             `json:"firstScheduledAt,omitempty"`
	StartedAt      *time.Time             `json:"firstStartedAt,omitempty"`
	FinishedAt     *time.Time             `json:"finishedAt,omitempty"`
	RuntimeSeconds float64                `json:"runtimeSeconds" doc:"Seconds its placements have spent running, summed: each from reaching running to exiting or being lost; one still running counts up to the time of the response."`
	RuntimeSince   *time.Time             `json:"runtimeSince,omitempty" doc:"When the placement still running started, if one is: runtimeSeconds grows from the response's time on."`
	// Placement time: what getting the Run onto a host and started took.
	PlacementSeconds      float64     `json:"placementSeconds" doc:"Time spent placing the Run, summed over its placements: for each, from when the Run needed a host (created, or its previous placement ending) until that placement's workload started. A Run still being placed counts up to the time of the response (placing). waitSeconds + startSeconds."`
	PlacementWaitSeconds  float64     `json:"placementWaitSeconds" doc:"The part of placementSeconds spent waiting for a host (needing one until assigned)."`
	PlacementStartSeconds float64     `json:"placementStartSeconds" doc:"The part spent starting on the host (assigned until the workload started, or the placement ended without starting)."`
	Placing               bool        `json:"placing,omitempty" doc:"The Run is being placed now: waiting for a host, or starting on one; placementSeconds grows from the response's time on."`
	Stage                 string      `json:"stage" enum:"waiting,image,volumes,repositories,container,running,stopping,stopped,lost,succeeded,failed,terminated" doc:"Where the Run is now (docs/concepts.md#stages): waiting for a host; image, volumes, repositories, container while it starts on one; running; stopping (stageReason says why); or its resting state. A stage event announces each change."`
	StageSince            time.Time   `json:"stageSince" doc:"When the stage began: one recorded time. volumes, repositories, container and running begin at the runner's marks, on its host's clock (running at luxd's started_at for a runner that reports no containerStarted); waiting, image (acceptedAt), stopping and a resting state at luxd's."`
	StageReason           string      `json:"stageReason,omitempty" doc:"For stopping: the stop's reason (stop, terminate, drain, preempt, migrate, timeout, disk)."`
	Placements            []Placement `json:"placements,omitempty"`
	Usage                 *RunUsage   `json:"usage,omitempty"`
	// Resume: on GET /v1/runs/{id} of a Run resume accepts by its state
	// (stopped, lost, failed, succeeded), what a resume would take.
	Resume *Resumability `json:"resume,omitempty"`
	// Servers: on GET /v1/runs/{id}, its servers.
	Servers     []RunServer   `json:"servers,omitzero" doc:"On GET /v1/runs/{id}: the Run's servers."`
	SubmittedBy *RunSubmitter `json:"submittedBy,omitempty" doc:"On GET /v1/runs/{id}: who submitted it, the API key or the person signed in. Absent for Runs submitted before luxd recorded it."`
	Cost        *RunCostBrief `json:"cost,omitempty" doc:"In GET /v1/runs only: the totals per currency and the status of GET /v1/runs/{id}/cost."`
	Sizing      *RunSizing    `json:"sizing,omitempty" doc:"On a shaped pool: what its submit asked for in resources and the size it has. Absent for a Run submitted to and still in an unshaped pool."`
}

// Resumability says whether a Run can be resumed now, and from what.
type Resumability struct {
	// Snapshot is what it resumes from (none: from scratch), and
	// OnHosts the hosts holding a local copy (a resume there moves nothing).
	Snapshot *string  `json:"snapshot,omitempty"`
	Uploaded bool     `json:"uploaded"`
	OnHosts  []string `json:"onHosts,omitempty"`
	// SecretsHeld: luxd still holds the values of its secrets, so an
	// operator can resume it without them. False when it has none to hold.
	Secrets     []string `json:"secrets,omitempty"`
	SecretsHeld bool     `json:"secretsHeld"`
	// Blockers: why a resume would be refused or wait, in words.
	Blockers []string `json:"blockers,omitempty"`
}

type Placement struct {
	Epoch                  int        `json:"epoch"`
	Host                   string     `json:"host"`
	HostName               string     `json:"hostName"`
	State                  string     `json:"state"`
	ExitCode               *int       `json:"exitCode,omitempty"`
	ExitReason             string     `json:"exitReason,omitempty"`
	StopReason             string     `json:"stopReason,omitempty"`
	AssignedAt             time.Time  `json:"assignedAt"`
	AcceptedAt             *time.Time `json:"acceptedAt,omitempty"`
	ImageReadyAt           *time.Time `json:"imageReadyAt,omitempty"`
	VolumesRestoredAt      *time.Time `json:"volumesRestoredAt,omitempty"`
	ReposReadyAt           *time.Time `json:"reposReadyAt,omitempty" doc:"Its repositories were cloned and a resume's sync fetched (at once without repositories)."`
	ContainerStartedAt     *time.Time `json:"containerStartedAt,omitempty"`
	WorkloadStartedAt      *time.Time `json:"workloadStartedAt,omitempty"`
	StopRequestedAt        *time.Time `json:"stopRequestedAt,omitempty"`
	ExitedAt               *time.Time `json:"exitedAt,omitempty"`
	SnapshotDoneAt         *time.Time `json:"snapshotDoneAt,omitempty"`
	UploadedAt             *time.Time `json:"uploadedAt,omitempty"`
	PeakMemoryBytes        *int64     `json:"peakMemoryBytes,omitempty"`
	PeakDiskBytes          *int64     `json:"peakDiskBytes,omitempty"`
	PeakPids               *int       `json:"peakPids,omitempty"`
	CPUSeconds             *float64   `json:"cpuSeconds,omitempty"`
	NetRxBytes             *int64     `json:"netRxBytes,omitempty"`
	NetTxBytes             *int64     `json:"netTxBytes,omitempty"`
	SnapshotBytes          *int64     `json:"snapshotBytes,omitempty"`
	MemoryLimit            *int64     `json:"memoryLimit,omitempty" doc:"Bytes: the container's memory limit, the Run's resources.memory scaled to its host's share (MemTotal less the runner's headroom, over the memory the host offers). Absent until the container starts, and for placements from before luxd recorded it."`
	PeakCPUCores           *float64   `json:"peakCpuCores,omitempty" doc:"The highest CPU rate, in cores, between two consecutive readings of the container's cgroup (about 10 s apart). Absent from runners that predate it."`
	CPUStallSeconds        *float64   `json:"cpuStallSeconds,omitempty" doc:"cpu.pressure's some total: seconds in which some task waited for CPU. Absent without PSI (or from older runners), never 0 for it."`
	MemoryStallSeconds     *float64   `json:"memoryStallSeconds,omitempty" doc:"memory.pressure's some total, in seconds."`
	MemoryFullStallSeconds *float64   `json:"memoryFullStallSeconds,omitempty" doc:"memory.pressure's full total: seconds in which every task stalled on memory."`
	IOStallSeconds         *float64   `json:"ioStallSeconds,omitempty" doc:"io.pressure's some total, in seconds."`
	IOFullStallSeconds     *float64   `json:"ioFullStallSeconds,omitempty" doc:"io.pressure's full total, in seconds."`
	ImagePlatforms         []string   `json:"imagePlatforms,omitempty" doc:"The platforms (os/arch[/variant]) the image's reference offers: every entry of a manifest list or OCI index, a single-arch image's own, an image built on the host the host's. Absent when the runner could not tell, or predates it."`
}

// RunUsage rolls placements up: peaks are maxima, totals are sums.
type RunUsage struct {
	PeakMemoryBytes int64   `json:"peakMemoryBytes"`
	PeakDiskBytes   int64   `json:"peakDiskBytes"`
	PeakPids        int     `json:"peakPids"`
	CPUSeconds      float64 `json:"cpuSeconds"`
	NetRxBytes      int64   `json:"netRxBytes"`
	NetTxBytes      int64   `json:"netTxBytes"`
	Placements      int     `json:"placements"`
	// Absent when no placement reported them.
	PeakCPUCores           *float64 `json:"peakCpuCores,omitempty"`
	CPUStallSeconds        *float64 `json:"cpuStallSeconds,omitempty"`
	MemoryStallSeconds     *float64 `json:"memoryStallSeconds,omitempty"`
	MemoryFullStallSeconds *float64 `json:"memoryFullStallSeconds,omitempty"`
	IOStallSeconds         *float64 `json:"ioStallSeconds,omitempty"`
	IOFullStallSeconds     *float64 `json:"ioFullStallSeconds,omitempty"`
	// Seconds between being requested and the first workload start.
	QueueSeconds *float64 `json:"queueSeconds,omitempty"`
}

// RunSubmitter is who submitted a Run: an API key, or a person signed in.
type RunSubmitter struct {
	KeyID   string `json:"keyId,omitempty"`
	KeyName string `json:"keyName,omitempty" doc:"Absent for an operator's key when a tenant asks."`
	Revoked bool   `json:"revoked,omitempty"`
	Email   string `json:"email,omitempty"`
}

// SubmitAnswer is either a created/replayed Run, or validateOnly's read-only
// advisory admission result. It never guarantees workload success. The
// validateOnly fields carry names of their own: an embedded Run field of the
// same name (pool, poolId) would be shadowed, in Go and in JSON.
type SubmitAnswer struct {
	*Run
	Valid              bool               `json:"valid,omitempty" doc:"validateOnly: the spec would be accepted; no Run was created and nothing changed."`
	WouldStartNow      bool               `json:"wouldStartNow,omitempty" doc:"validateOnly: a host could take the Run now (spot: settled spare capacity no normal Run is waiting for; normal: a host it fits on now). Advisory: not a reservation, and it may change at once."`
	EffectiveResources *spec.Resources    `json:"effectiveResources,omitempty" doc:"validateOnly: the resources the Run would reserve, after defaults and a shaped pool's rounding."`
	EffectivePool      string             `json:"effectivePool,omitempty" doc:"validateOnly: the pool the Run would be bound to."`
	EffectivePoolID    string             `json:"effectivePoolId,omitempty" doc:"validateOnly: that pool's id."`
	EligibleHost       string             `json:"eligibleHost,omitempty" doc:"validateOnly: the host it would start on now."`
	Blockers           []AdmissionBlocker `json:"blockers,omitempty" doc:"validateOnly: why it would not start now, by kind."`
}

// AdmissionBlocker is why a Run would not start on some hosts now.
type AdmissionBlocker struct {
	Code             string `json:"code" enum:"no_hosts,constraint,capability,not_enough_spare,space_not_settled,normal_demand_pending,disk_pressure" doc:"no_hosts: no live connected host in its pool or chosen host; constraint: labels, nesting, tenancy, pool shape, or another chosen host; capability: the host's lux-runner lacks a feature the Run needs; not_enough_spare: not enough unreserved cpus, memory, disk or run slots; space_not_settled (spot): the spare space has not been continuously free for more than 60s; normal_demand_pending (spot): a queued normal Run could use that space; disk_pressure: the host's runner reports low free disk, and the host takes no new Run until it recovers."`
	Hosts            int    `json:"hosts" doc:"How many hosts this blocks."`
	Reason           string `json:"reason,omitempty" doc:"The first such host's reason, for constraint and capability."`
	RemainingSeconds *int   `json:"remainingSeconds,omitempty" doc:"space_not_settled: the shortest wait after which the space would qualify if nothing changes."`
}

type Event struct {
	ID       int64          `json:"id"`
	ServerID string         `json:"serverId,omitempty" doc:"A server's event: its id (the server.* events)."`
	Epoch    *int           `json:"epoch,omitempty"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data"`
	Time     time.Time      `json:"time"`
}

// FeedEvent is an event in the feed of every Run and server the caller
// sees: a Run's (runId), or a server's (serverId, and runId its attached
// Run's, null while it has none).
type FeedEvent struct {
	Event
	RunID  *string `json:"runId" nullable:"true" doc:"The Run's id. Null only for an event of a server attached to no Run."`
	Tenant string  `json:"tenant"`
}

// RunServer is a Run's server, as the Run's API shows it.
type RunServer struct {
	ID            string            `json:"id" doc:"The server's id (srv_…): GET /v1/servers/{id}."`
	Name          string            `json:"name"`
	Port          int               `json:"port"`
	Command       []string          `json:"command" nullable:"true" doc:"argv; null: only the port is exposed."`
	Workdir       string            `json:"workdir"`
	Env           map[string]string `json:"env"`
	FromSpec      bool              `json:"fromSpec" doc:"Declared in the spec's workload.servers."`
	State         string            `json:"state" enum:"stopped,starting,ready,unreachable,exited"`
	ExitCode      *int              `json:"exitCode,omitempty" doc:"When exited."`
	Error         *string           `json:"error,omitempty" doc:"When exited: the last line its command wrote to stderr, if any."`
	Since         time.Time         `json:"since" doc:"When its state last changed."`
	ReadySince    *time.Time        `json:"readySince" nullable:"true" doc:"When it last became ready (null unless ready)."`
	StopReason    *string           `json:"stopReason" nullable:"true" enum:"stopped,run stopped,migrated,host lost,detached" doc:"Why it is stopped: stopped by request (it stays down: a new placement does not start it), its placement ended, or it was detached."`
	StoppedEpoch  *int              `json:"stoppedEpoch" nullable:"true" doc:"The placement epoch it stopped in (null if it never ran)."`
	Epoch         *int              `json:"epoch" nullable:"true" doc:"The placement epoch its current state is of."`
	Hostname      *string           `json:"hostname" nullable:"true" doc:"Its preview host name; null when previews are not configured."`
	URL           *string           `json:"url" nullable:"true" doc:"Its preview URL; null when previews are not configured."`
	Wake          string            `json:"wake" enum:"request,never"`
	Lifetime      string            `json:"lifetime" enum:"run,owner" doc:"run: deleted when its Run is terminated. owner: kept until its owner deletes it."`
	Labels        map[string]string `json:"labels"`
	LastRequestAt *time.Time        `json:"lastRequestAt,omitempty" doc:"When its preview URL was last requested (written at most every preview.activity_every)."`
}

// RunCostBrief is a Run's cost as the Runs list carries it.
type RunCostBrief struct {
	Status string      `json:"status" enum:"pending,complete,incomplete,final" doc:"As in GET /v1/runs/{id}/cost."`
	Totals []CostTotal `json:"totals" doc:"Per currency, ordered by currency; empty while pending."`
}

// CostTotal is a sum of lines in one currency: amounts in different
// currencies are never added together.
type CostTotal struct {
	Family      string `json:"family,omitempty" doc:"In byFamily only."`
	DisplayName string `json:"displayName,omitempty" doc:"In byFamily when the family is described by a plugin or is compute."`
	Color       string `json:"color,omitempty" doc:"In byFamily when a plugin supplies a color hint."`
	Currency    string `json:"currency"`
	Amount      string `json:"amount" doc:"final + estimate."`
	Final       string `json:"final" doc:"The part from final lines."`
	Estimate    string `json:"estimate" doc:"The part from lines that may still change."`
}

// ListRunsBody is GET /v1/runs's answer. The name is the OpenAPI schema's.
type ListRunsBody struct {
	Runs []*Run `json:"runs"`
	Next string `json:"next,omitempty" doc:"Paged lists: the next page's cursor (?next=)."`
	Prev string `json:"prev,omitempty" doc:"Paged lists: the previous page's cursor (?prev=)."`
	Page string `json:"page,omitempty" doc:"Paged lists: this page's own cursor (?at=), to read it again in place."`
}

// EventList is GET /v1/runs/{id}/events's answer.
type EventList struct {
	Events []Event `json:"events"`
}
