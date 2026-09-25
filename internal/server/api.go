package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/hostboot"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func (s *Server) routes(api huma.API) {
	register(s, api, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/health",
		Summary: "Check luxd is up",
	}, "", func(context.Context, *struct{}) (*statusOutput, error) {
		out := &statusOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	// Runs.
	register(s, api, huma.Operation{
		OperationID: "submitRun", Method: http.MethodPost, Path: "/v1/runs", Tags: []string{"runs"},
		Summary:       "Submit a Run",
		Description:   "Validates and normalizes the RunSpec (docs/runspec.md) and queues the Run. Secret values are held in memory until the Run is placed, never stored.",
		DefaultStatus: http.StatusCreated,
		Responses:     map[string]*huma.Response{"200": {Description: "An earlier submission with the same Idempotency-Key.", Content: jsonContent(schemaRef[Run](api))}},
		Errors:        []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTooManyRequests},
	}, "run", forTenant(s.submitRun))
	register(s, api, huma.Operation{
		OperationID: "listRuns", Method: http.MethodGet, Path: "/v1/runs", Tags: []string{"runs"},
		Summary: "List Runs", Description: "Newest first.",
	}, "read", s.listRuns)
	register(s, api, huma.Operation{
		OperationID: "getRun", Method: http.MethodGet, Path: "/v1/runs/{id}", Tags: []string{"runs"},
		Summary: "Get a Run", Description: "With its placements and resource usage.",
		Errors: []int{http.StatusNotFound},
	}, "read", s.getRun)
	register(s, api, huma.Operation{
		OperationID: "listEvents", Method: http.MethodGet, Path: "/v1/runs/{id}/events", Tags: []string{"runs"},
		Summary: "List a Run's lifecycle events",
		Errors:  []int{http.StatusNotFound},
	}, "read", s.listEvents)
	register(s, api, huma.Operation{
		OperationID: "streamOutput", Method: http.MethodGet, Path: "/v1/runs/{id}/output", Tags: []string{"runs"},
		Summary: "Stream a Run's output",
		Description: "The Run's output as server-sent events, from wherever it is: relayed from its host while it runs, from S3 once uploaded. " +
			"Events: `record` (one output record; its `cursor` resumes the stream with `since`), " +
			"`lux` (a lifecycle event, with `events=true`), `gap` (output that cannot be had, e.g. lost with its host), " +
			"`error` (the stream failed) and `end` (the last event: where to resume and the Run's state). " +
			"Without `follow` the stream ends after what is there now.",
		Responses: map[string]*huma.Response{"200": {Description: "OK", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: sseEvents(
			sseEvent("record", "One output record.", schemaRef[OutputRecord](api)),
			sseEvent("lux", "A lifecycle event (events=true).", schemaRef[Event](api)),
			sseEvent("gap", "Output of a placement that cannot be had.", schemaRef[outputGap](api)),
			sseEvent("error", "The stream failed.", schemaRef[outputError](api)),
			sseEvent("end", "The last event.", schemaRef[outputEnd](api)),
		)}}}},
		Errors: []int{http.StatusBadRequest, http.StatusNotFound},
	}, "read", streamed(s, s.serveOutput))
	register(s, api, huma.Operation{
		OperationID: "stopRun", Method: http.MethodPost, Path: "/v1/runs/{id}/stop", Tags: []string{"runs"},
		Summary:       "Stop a Run",
		Description:   "Gracefully: its state volumes are snapshotted and it can be resumed. Idempotent.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound},
	}, "run", s.stopRun)
	register(s, api, huma.Operation{
		OperationID: "resumeRun", Method: http.MethodPost, Path: "/v1/runs/{id}/resume", Tags: []string{"runs"},
		Summary: "Resume a stopped, lost or failed Run",
		Description: "From its latest snapshot (or fromSnapshot), on any host. Its secrets must be supplied again. Idempotent while resuming.\n\n" +
			"git.repositories adds repositories: the runner clones them into the restored workspace before the Run starts, each reported as a git.clone event " +
			"with the request id (Lux-Request-Id). One whose clone fails is dropped from the spec and the Run goes on without it. " +
			"Adding needs a stopped, lost or failed Run: while it is resuming, 409.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests},
	}, "run", s.resumeRun)
	register(s, api, huma.Operation{
		OperationID: "cancelRun", Method: http.MethodPost, Path: "/v1/runs/{id}/cancel", Tags: []string{"runs"},
		Summary:       "Cancel a Run",
		Description:   "It ends as cancelled and cannot be resumed. Idempotent.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound},
	}, "run", s.cancelRun)
	register(s, api, huma.Operation{
		OperationID: "migrateRun", Method: http.MethodPost, Path: "/v1/runs/{id}/migrate", Tags: []string{"runs", "operators"},
		Summary: "Move a running Run to another host",
		Description: "It is stopped (its state snapshotted), then resumed at once on `to`, or on any host but the one it was on. " +
			"An agent resumes its session; `input`, if given, is delivered once it runs again.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound, http.StatusConflict},
	}, "operator", s.migrateRun)
	register(s, api, huma.Operation{
		OperationID: "runHistory", Method: http.MethodGet, Path: "/v1/runs/{id}/history", Tags: []string{"runs", "history"},
		Summary: "A Run's resource use over time", Description: "Across its placements: each sample carries its epoch.",
		Errors: []int{http.StatusNotFound},
	}, "read", s.runHistory)
	register(s, api, huma.Operation{
		OperationID: "pushRun", Method: http.MethodPost, Path: "/v1/runs/{id}/push", Tags: []string{"runs"},
		Summary: "Push a running Run's repositories",
		Description: "To the spec's git.push branch, with the runner's credentials. The outcome arrives as a git.push event carrying the request id.\n\n" +
			"expect: per repository, the commit the push branch must be at for the push to go ahead (a compare-and-swap). " +
			"Without it, the lease is what this Run last pushed, or, the first time, that the branch does not exist.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", s.pushRun)
	register(s, api, huma.Operation{
		OperationID: "listSnapshots", Method: http.MethodGet, Path: "/v1/runs/{id}/snapshots", Tags: []string{"runs"},
		Summary: "List a Run's snapshots",
		Errors:  []int{http.StatusNotFound},
	}, "read", s.listSnapshots)
	register(s, api, huma.Operation{
		OperationID: "listArtifacts", Method: http.MethodGet, Path: "/v1/runs/{id}/artifacts", Tags: []string{"runs"},
		Summary: "List a Run's artifacts",
		Errors:  []int{http.StatusNotFound},
	}, "read", s.listArtifacts)
	register(s, api, huma.Operation{
		OperationID: "downloadArtifact", Method: http.MethodGet, Path: "/v1/artifacts/{aid}", Tags: []string{"runs"},
		Summary: "Download an artifact",
		Description: "The file the Run wrote, streamed through luxd. Content-Length and X-Lux-SHA256 let a client tell a whole download " +
			"from one cut short (the response is aborted, never ended cleanly, on a failure mid-way). " +
			"409 not_uploaded (with Retry-After) while it is still on its host; 410 gone once deleted by retention.",
		Responses: map[string]*huma.Response{"200": {
			Description: "The file.",
			Content:     map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
			Headers: map[string]*huma.Header{
				"Content-Length":      {Description: "The file's size.", Schema: &huma.Schema{Type: huma.TypeInteger}},
				"X-Lux-SHA256":        {Description: "The file's SHA-256, hex.", Schema: &huma.Schema{Type: huma.TypeString}},
				"Content-Disposition": {Description: "attachment, with the file's name.", Schema: &huma.Schema{Type: huma.TypeString}},
			},
		}},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusGone},
	}, "read", streamed(s, s.downloadArtifact))

	// Interactive.
	register(s, api, huma.Operation{
		OperationID: "postInput", Method: http.MethodPost, Path: "/v1/runs/{id}/input", Tags: []string{"interactive"},
		Summary:       "Steer a running Run",
		Description:   "Agents get text as a message (queued until the current turn ends if the agent cannot take it mid-turn); generic workloads get it on stdin. A stopped Run takes its input through resume instead.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
	}, "run", s.postInput)
	register(s, api, streamOp(api, "execRun", "/v1/runs/{id}/exec", "exec", "Run a command in a running Run",
		"The client's first message is a StreamOpen: `{\"command\": [...], \"tty\": true, \"rows\": 24, \"cols\": 80}`. "),
		"run", streamed(s, s.runStream("exec")))
	register(s, api, streamOp(api, "attachRun", "/v1/runs/{id}/attach", "attach", "Attach to a running Run's terminal",
		"Needs a generic workload with workload.tty. "),
		"run", streamed(s, s.runStream("attach")))
	register(s, api, streamOp(api, "portForward", "/v1/runs/{id}/ports/{name}", "tunnel", "Reach one of a running Run's ports",
		"A TCP connection to a port the spec declares in network.ports, carried as StreamData. "),
		"run", streamed(s, s.streamHandler("tunnel")))

	// Hosts and pools.
	register(s, api, huma.Operation{
		OperationID: "listHosts", Method: http.MethodGet, Path: "/v1/hosts", Tags: []string{"hosts"},
		Summary: "List hosts", Description: "The tenant's own hosts, and platform hosts in pools it can use. Operators: every host.",
	}, "read", s.listHosts)
	register(s, api, huma.Operation{
		OperationID: "getHost", Method: http.MethodGet, Path: "/v1/hosts/{id}", Tags: []string{"hosts"},
		Summary: "Get a host", Description: "With its live placements.",
		Errors: []int{http.StatusNotFound, http.StatusConflict},
	}, "read", s.getHost)
	register(s, api, huma.Operation{
		OperationID: "hostHistory", Method: http.MethodGet, Path: "/v1/hosts/{id}/history", Tags: []string{"history"},
		Summary: "A host's resource use over time",
		Description: "CPU cores and memory in use, disk used, and its live placements and what they asked for. " +
			"A tenant sees its own hosts' history only.",
		Errors: []int{http.StatusNotFound, http.StatusConflict},
	}, "read", s.hostHistory)
	register(s, api, huma.Operation{
		OperationID: "drainHost", Method: http.MethodPost, Path: "/v1/hosts/{id}/drain", Tags: []string{"hosts"},
		Summary: "Drain a host",
		Description: "No new placements; its live Runs finish where they are. With forceEvict, they are also stopped and resumed elsewhere " +
			"(also applies to a host that is already draining). Only the tenant's own hosts; operators, any host.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound},
	}, "admin", s.drainHost)
	register(s, api, huma.Operation{
		OperationID: "listPools", Method: http.MethodGet, Path: "/v1/pools", Tags: []string{"pools"},
		Summary: "List pools", Description: "The tenant's pools and the platform pools it can use. Operators: every pool.",
	}, "read", s.listPools)

	// Operators and the system.
	register(s, api, huma.Operation{
		OperationID: "whoami", Method: http.MethodGet, Path: "/v1/whoami", Tags: []string{"operators"},
		Summary: "Who the caller is", Description: "An API key (whether an operator's, its tenant, its scopes), or a person signed in through the console auth (their email and name); and luxd's console auth.",
	}, "read", s.whoami)
	register(s, api, huma.Operation{
		OperationID: "listTenants", Method: http.MethodGet, Path: "/v1/tenants", Tags: []string{"operators"},
		Summary: "List tenants", Description: "Their quotas and what they use.",
	}, "operator", s.listTenants)
	register(s, api, huma.Operation{
		OperationID: "status", Method: http.MethodGet, Path: "/v1/status", Tags: []string{"history"},
		Summary: "The state of the system now",
		Description: "Runs by state, the queue, time to start, hosts by state, and capacity against what live placements hold: " +
			"the caller's (a tenant's Runs and the hosts it may use), or the whole system's for an operator.",
	}, "read", s.status)
	register(s, api, huma.Operation{
		OperationID: "history", Method: http.MethodGet, Path: "/v1/history", Tags: []string{"history"},
		Summary: "The state of the system over time", Description: "Samples of what GET /v1/status reports.",
	}, "read", s.systemHistory)
	register(s, api, huma.Operation{
		OperationID: "eventFeed", Method: http.MethodGet, Path: "/v1/events", Tags: []string{"runs"},
		Summary: "Every Run's events, as they happen",
		Description: "Server-sent events: one `lux` event per Run event, oldest first, each with its id (`id:`, and resume with Last-Event-ID or `after`). " +
			"From now, from `after`, or the `last` N; with `follow=false` the stream ends after what is there now.",
		Responses: map[string]*huma.Response{"200": {Description: "OK", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: sseEvents(
			sseEvent("lux", "A Run's event.", schemaRef[FeedEvent](api)),
			sseEvent("error", "The stream failed.", schemaRef[outputError](api)),
		)}}}},
	}, "read", streamed(s, s.serveFeed))
	register(s, api, huma.Operation{
		OperationID: "putPool", Method: http.MethodPost, Path: "/v1/pools", Tags: []string{"pools"},
		Summary: "Create or update a pool",
		Errors:  []int{http.StatusUnprocessableEntity},
	}, "admin", forTenant(s.putPool))
	register(s, api, huma.Operation{
		OperationID: "deletePool", Method: http.MethodDelete, Path: "/v1/pools/{name}", Tags: []string{"pools"},
		Summary: "Remove a pool",
		Description: "Its provisioned hosts are cordoned and terminated once idle; its Runs wait for a pool of that name again. " +
			"forceEvict also stops its hosts' live Runs so they resume elsewhere.",
		Errors: []int{http.StatusNotFound},
	}, "admin", forTenant(s.deletePool))
}

type statusBody struct {
	Status string `json:"status" example:"ok"`
}

type statusOutput struct {
	Body statusBody
}

// streamOp declares an interactive stream: a WebSocket (stream.go).
func streamOp(api huma.API, id, path, kind, summary, doc string) huma.Operation {
	// OpenAPI cannot describe WebSocket messages: named in x-websocket.
	ws := map[string]any{"message": schemaRef[proto.StreamData](api)}
	if kind == "exec" {
		ws["open"] = schemaRef[proto.StreamOpen](api)
	}
	return huma.Operation{
		OperationID: id, Method: http.MethodGet, Path: path, Tags: []string{"interactive"},
		Summary: summary,
		Description: "A WebSocket (send `Upgrade: websocket`), relayed to the Run's host, carrying JSON text messages. " + doc +
			"Then StreamData both ways: `{\"data\": <base64>}` for bytes, `{\"eof\": true}` to close input, `{\"rows\", \"cols\"}` to resize. " +
			"The stream ends with one `{\"exitCode\": N}` (exec) or `{\"error\": \"...\"}`, or just the socket closing, and luxd closes it.\n\n" +
			"Without the upgrade the same URL answers 200 `{\"status\": \"ok\"}` if the stream could be opened, and the error it would get otherwise, so a client can check first.",
		Responses: map[string]*huma.Response{
			"101": {Description: "Switching to the WebSocket."},
			"200": {Description: "The stream can be opened (no Upgrade header).", Content: jsonContent(schemaRef[statusBody](api))},
		},
		Errors:     []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
		Extensions: map[string]any{"x-websocket": ws},
	}
}

func jsonContent(schema *huma.Schema) map[string]*huma.MediaType {
	return map[string]*huma.MediaType{"application/json": {Schema: schema}}
}

// Run is the API representation of a Run.
type Run struct {
	ID          string            `json:"id"`
	Tenant      string            `json:"tenant" doc:"The owning tenant's name."`
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels"`
	State       string            `json:"state"`
	StateReason string            `json:"stateReason,omitempty"`
	Activity    string            `json:"activity,omitempty"`
	ExitCode    *int              `json:"exitCode,omitempty"`
	Epoch       int               `json:"epoch"`
	SessionID   string            `json:"sessionId,omitempty"`
	SnapshotID  *string           `json:"snapshotId,omitempty"`
	Host        string            `json:"host,omitempty" doc:"The name of the host of its current placement."`
	HostID      string            `json:"hostId,omitempty" doc:"That host's id."`
	Spec        spec.RunSpec      `json:"spec"`
	// Image is how a built image was resolved on its first build.
	Image       *ImageResolution `json:"image,omitempty"`
	Secrets     []spec.SecretRef `json:"secrets"`
	CreatedAt   time.Time        `json:"createdAt"`
	ScheduledAt *time.Time       `json:"firstScheduledAt,omitempty"`
	StartedAt   *time.Time       `json:"firstStartedAt,omitempty"`
	FinishedAt  *time.Time       `json:"finishedAt,omitempty"`
	Placements  []Placement      `json:"placements,omitempty"`
	Usage       *RunUsage        `json:"usage,omitempty"`
	// Resume: on GET /v1/runs/{id} of a stopped, lost or failed Run, what
	// a resume would take.
	Resume *Resumability `json:"resume,omitempty"`
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

type ImageResolution = proto.ImageResolution

type Placement struct {
	Epoch              int        `json:"epoch"`
	Host               string     `json:"host"`
	HostName           string     `json:"hostName"`
	State              string     `json:"state"`
	ExitCode           *int       `json:"exitCode,omitempty"`
	ExitReason         string     `json:"exitReason,omitempty"`
	StopReason         string     `json:"stopReason,omitempty"`
	AssignedAt         time.Time  `json:"assignedAt"`
	AcceptedAt         *time.Time `json:"acceptedAt,omitempty"`
	ImageReadyAt       *time.Time `json:"imageReadyAt,omitempty"`
	VolumesRestoredAt  *time.Time `json:"volumesRestoredAt,omitempty"`
	ContainerStartedAt *time.Time `json:"containerStartedAt,omitempty"`
	WorkloadStartedAt  *time.Time `json:"workloadStartedAt,omitempty"`
	StopRequestedAt    *time.Time `json:"stopRequestedAt,omitempty"`
	ExitedAt           *time.Time `json:"exitedAt,omitempty"`
	SnapshotDoneAt     *time.Time `json:"snapshotDoneAt,omitempty"`
	UploadedAt         *time.Time `json:"uploadedAt,omitempty"`
	PeakMemoryBytes    *int64     `json:"peakMemoryBytes,omitempty"`
	PeakDiskBytes      *int64     `json:"peakDiskBytes,omitempty"`
	PeakPids           *int       `json:"peakPids,omitempty"`
	CPUSeconds         *float64   `json:"cpuSeconds,omitempty"`
	NetRxBytes         *int64     `json:"netRxBytes,omitempty"`
	NetTxBytes         *int64     `json:"netTxBytes,omitempty"`
	SnapshotBytes      *int64     `json:"snapshotBytes,omitempty"`
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
	// Seconds between being requested and the first workload start.
	QueueSeconds *float64 `json:"queueSeconds,omitempty"`
}

// runColumns: Tenant is the owning tenant's name; Host and HostID are the
// current placement's host.
// Select runColumns FROM runsFrom.
const runColumns = `r.id, rt.name, r.name, r.labels, r.state, r.state_reason, r.activity, r.exit_code, r.current_epoch,
	r.session_id, r.snapshot_id, r.spec, r.image_resolved, r.secrets, r.created_at, r.first_scheduled_at, r.first_started_at, r.finished_at,
	coalesce(rh.name, ''), coalesce(rp.host_id, '')`

// runsFrom: a Run with its tenant and its current placement's host.
const runsFrom = `runs r JOIN tenants rt ON rt.id = r.tenant_id
	LEFT JOIN placements rp ON rp.run_id = r.id AND rp.epoch = r.current_epoch
	LEFT JOIN hosts rh ON rh.id = rp.host_id`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Tenant, &r.Name, &r.Labels, &r.State, &r.StateReason, &r.Activity, &r.ExitCode, &r.Epoch,
		&r.SessionID, &r.SnapshotID, &r.Spec, &r.Image, &r.Secrets, &r.CreatedAt, &r.ScheduledAt, &r.StartedAt, &r.FinishedAt, &r.Host, &r.HostID)
	return &r, err
}

type submitRunInput struct {
	IdempotencyKey string `header:"Idempotency-Key" doc:"Makes the submission safe to retry: another with the same key returns the first Run (200) instead of creating one."`
	Body           spec.RunSpec
}

type submitRunOutput struct {
	Status int
	Body   *Run
}

func (s *Server) submitRun(ctx context.Context, in *submitRunInput) (*submitRunOutput, error) {
	p := principal(ctx)
	sp := in.Body
	if sp.Git != nil {
		for i, r := range sp.Git.Repositories {
			if r.AddedBy != "" {
				// Only luxd sets it: it marks a repository a resume added.
				return nil, invalidSpec(&spec.ValidationError{Problems: []string{
					fmt.Sprintf("git.repositories[%d].addedBy is set by luxd, never submitted", i)}})
			}
		}
	}
	if err := sp.Normalize(s.cfg.Defaults); err != nil {
		return nil, invalidSpec(err)
	}
	if err := s.checkMCPNotControlPlane(ctx, sp); err != nil {
		return nil, err
	}
	stored, refs, values := sp.SplitSecrets()
	if err := requireSecrets(refs, values); err != nil {
		return nil, err
	}
	idem := in.IdempotencyKey

	run := &Run{}
	created := false
	id := ids.New(ids.Run)
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if idem != "" {
			existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE r.idempotency_key = $1`, idem))
			if err == nil {
				run = existing
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if err := checkRunQuota(ctx, tx, p.TenantID); err != nil {
			return err
		}
		var idemArg *string
		if idem != "" {
			idemArg = &idem
		}
		_, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, name, labels, spec, secrets, state, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, 'submitted', $7)`,
			id, p.TenantID, sp.Name, nonNilMap(sp.Labels), stored, refs, idemArg)
		if err != nil {
			return err
		}
		if err := addEvent(ctx, tx, p.TenantID, id, 0, "submitted", map[string]any{"by": p.Actor()}); err != nil {
			return err
		}
		created = true
		// Cached before commit, so the scheduler never sees the Run
		// without its values.
		if len(values) > 0 {
			s.secrets.put(id, values)
		}
		run, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE r.id = $1`, id))
		return err
	})
	if err != nil {
		s.secrets.drop(id)
		return nil, err
	}
	if created {
		s.Kick()
		return &submitRunOutput{http.StatusCreated, run}, nil
	}
	return &submitRunOutput{http.StatusOK, run}, nil
}

// invalidSpec is a 422 invalid_spec for a spec Normalize refused, with
// every problem in details.
func invalidSpec(err error) error {
	var ve *spec.ValidationError
	if errors.As(err, &ve) {
		he := errf(http.StatusUnprocessableEntity, "invalid_spec", "%s", err.Error())
		he.Details = ve.Problems
		return he
	}
	return err
}

// requireRun is 404 unless the Run exists in the transaction's tenant.
func requireRun(ctx context.Context, tx pgx.Tx, runID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, runID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errNotFound
	}
	return nil
}

// inactiveRunStates, for SQL: Runs that hold no host and count against no
// concurrency quota.
const inactiveRunStates = "('succeeded', 'failed', 'cancelled', 'stopped', 'lost')"

// checkRunQuota enforces a tenant's limits on concurrent Runs and on
// stored bytes (snapshots, output and artifacts not yet deleted by
// retention). Checked when a Run is submitted or resumed. Each count runs
// only when its limit is set.
func checkRunQuota(ctx context.Context, tx pgx.Tx, tenantID string) error {
	var maxRuns *int
	var maxBytes *int64
	var n int
	var stored int64
	if err := tx.QueryRow(ctx, `SELECT max_concurrent_runs, max_storage_bytes,
			CASE WHEN max_concurrent_runs IS NULL THEN 0 ELSE
				(SELECT count(*) FROM runs WHERE tenant_id = $1 AND state NOT IN `+inactiveRunStates+`) END,
			CASE WHEN max_storage_bytes IS NULL THEN 0 ELSE
				(SELECT coalesce(sum(size), 0) FROM blobs WHERE tenant_id = $1 AND location <> 'deleted') END
		FROM tenants WHERE id = $1`, tenantID).Scan(&maxRuns, &maxBytes, &n, &stored); err != nil {
		return err
	}
	if maxRuns != nil && n >= *maxRuns {
		return errf(http.StatusTooManyRequests, "quota_exceeded", "quota: tenant has %d active runs (limit %d)", n, *maxRuns)
	}
	if maxBytes != nil && stored >= *maxBytes {
		return errf(http.StatusTooManyRequests, "quota_exceeded", "quota: tenant stores %d bytes (limit %d); cancel Runs or wait for retention", stored, *maxBytes)
	}
	return nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

type listRunsInput struct {
	TenantQuery
	State     string   `query:"state" doc:"Only Runs in these states (comma-separated)." example:"running,stopped"`
	Resumable bool     `query:"resumable" doc:"Only Runs resume accepts: stopped, lost or failed."`
	Host      string   `query:"host" doc:"Only Runs with a placement (any epoch) on this host, by id or name."`
	Label     []string `query:"label,explode" doc:"Only Runs with this label (key=value); repeat to require several."`
	Before    string   `query:"before" doc:"Only Runs created before this time (RFC 3339): the next page after a list's last Run."`
	Limit     string   `query:"limit" doc:"At most this many Runs, newest first: 1 to 1000, default 100." example:"100"`
}

// Resolve reads every label as given: huma drops them all when the first
// is empty.
func (in *listRunsInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	in.Label = u.Query()["label"]
	return nil
}

type listRunsOutput struct {
	Body listRunsBody `nameHint:"RunList"`
}

type listRunsBody struct {
	Runs []*Run `json:"runs"`
}

// listRuns lists the Runs the caller sees (an operator: every tenant's,
// unless narrowed with ?tenant=), newest first.
func (s *Server) listRuns(ctx context.Context, in *listRunsInput) (*listRunsOutput, error) {
	p := principal(ctx)
	where := []string{"true"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if st := in.State; st != "" {
		where = append(where, "r.state = ANY("+arg(strings.Split(st, ","))+")")
	}
	if in.Resumable {
		where = append(where, "r.state IN "+resumableRunStates)
	}
	if in.Host != "" {
		// By id, or by the name of a host not terminated (names are reused).
		n := arg(in.Host)
		where = append(where, `r.id IN (SELECT p.run_id FROM placements p JOIN hosts h ON h.id = p.host_id
			WHERE h.id = `+n+` OR (h.name = `+n+` AND h.state <> 'terminated'))`)
	}
	for _, l := range in.Label {
		k, v, _ := strings.Cut(l, "=")
		where = append(where, "r.labels @> "+arg(map[string]string{k: v}))
	}
	if in.Before != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Before)
		if err != nil {
			return nil, errf(http.StatusBadRequest, "bad_request", "before: %v", err)
		}
		where = append(where, "r.created_at < "+arg(t))
	}
	limit := 100
	if n, err := strconv.Atoi(in.Limit); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	runs := []*Run{}
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE `+strings.Join(where, " AND ")+
			` ORDER BY r.created_at DESC LIMIT `+strconv.Itoa(limit), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return err
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := &listRunsOutput{}
	out.Body.Runs = runs
	return out, nil
}

func (s *Server) loadRun(ctx context.Context, tenantID, id string, detail bool) (*Run, error) {
	var run *Run
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE r.id = $1`, id))
		if err != nil || !detail {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.epoch, p.host_id, h.name, p.state, p.exit_code, p.exit_reason, p.stop_reason,
				p.created_at, p.accepted_at, p.image_ready_at, p.volumes_restored_at, p.container_started_at, p.workload_started_at,
				p.stop_requested_at, p.exited_at, p.snapshot_done_at, p.uploaded_at,
				p.peak_memory_bytes, p.peak_disk_bytes, p.peak_pids, p.cpu_seconds, p.net_rx_bytes, p.net_tx_bytes, p.snapshot_bytes
			FROM placements p JOIN hosts h ON h.id = p.host_id WHERE p.run_id = $1 ORDER BY p.epoch`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		u := &RunUsage{}
		for rows.Next() {
			var pl Placement
			if err := rows.Scan(&pl.Epoch, &pl.Host, &pl.HostName, &pl.State, &pl.ExitCode, &pl.ExitReason, &pl.StopReason,
				&pl.AssignedAt, &pl.AcceptedAt, &pl.ImageReadyAt, &pl.VolumesRestoredAt, &pl.ContainerStartedAt, &pl.WorkloadStartedAt,
				&pl.StopRequestedAt, &pl.ExitedAt, &pl.SnapshotDoneAt, &pl.UploadedAt,
				&pl.PeakMemoryBytes, &pl.PeakDiskBytes, &pl.PeakPids, &pl.CPUSeconds, &pl.NetRxBytes, &pl.NetTxBytes, &pl.SnapshotBytes); err != nil {
				return err
			}
			run.Placements = append(run.Placements, pl)
			u.Placements++
			if pl.PeakMemoryBytes != nil {
				u.PeakMemoryBytes = max(u.PeakMemoryBytes, *pl.PeakMemoryBytes)
			}
			if pl.PeakDiskBytes != nil {
				u.PeakDiskBytes = max(u.PeakDiskBytes, *pl.PeakDiskBytes)
			}
			if pl.PeakPids != nil {
				u.PeakPids = max(u.PeakPids, *pl.PeakPids)
			}
			if pl.CPUSeconds != nil {
				u.CPUSeconds += *pl.CPUSeconds
			}
			if pl.NetRxBytes != nil {
				u.NetRxBytes += *pl.NetRxBytes
			}
			if pl.NetTxBytes != nil {
				u.NetTxBytes += *pl.NetTxBytes
			}
		}
		if len(run.Placements) > 0 && run.Placements[0].WorkloadStartedAt != nil {
			q := run.Placements[0].WorkloadStartedAt.Sub(run.CreatedAt).Seconds()
			u.QueueSeconds = &q
		}
		run.Usage = u
		return rows.Err()
	})
	return run, err
}

// RunPath names a Run. Exported: huma only sees the path parameter of an
// embedded struct that is.
type RunPath struct {
	ID string `path:"id" doc:"The Run's id."`
}

type runOutput struct {
	Body *Run
}

func (s *Server) getRun(ctx context.Context, in *RunPath) (*runOutput, error) {
	run, err := s.loadRun(ctx, principal(ctx).TenantID, in.ID, true)
	if err != nil {
		return nil, err
	}
	if run.State == StateStopped || run.State == StateLost || run.State == StateFailed {
		if run.Resume, err = s.resumability(ctx, principal(ctx).TenantID, run); err != nil {
			return nil, err
		}
	}
	return &runOutput{run}, nil
}

// resumability is what resuming a Run would take, as far as luxd can tell
// without trying: its snapshot, secrets and quota. (Whether a host fits is
// the scheduler's to find out; a resumed Run says why it waits.)
func (s *Server) resumability(ctx context.Context, tenantID string, run *Run) (*Resumability, error) {
	rs := &Resumability{Snapshot: run.SnapshotID}
	for _, ref := range run.Secrets {
		rs.Secrets = append(rs.Secrets, ref.Name)
	}
	if len(rs.Secrets) > 0 {
		_, rs.SecretsHeld = s.secrets.get(run.ID)
	}
	if run.SnapshotID != nil {
		var available bool
		var host *string
		err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT s.available, s.uploaded, CASE WHEN s.host_copy THEN h.name END
				FROM snapshots s LEFT JOIN hosts h ON h.id = s.host_id WHERE s.id = $1 AND s.run_id = $2`, *run.SnapshotID, run.ID).
				Scan(&available, &rs.Uploaded, &host)
		})
		if err != nil {
			return nil, err
		}
		if host != nil {
			rs.OnHosts = append(rs.OnHosts, *host)
		}
		if !available {
			rs.Blockers = append(rs.Blockers, "its snapshot is no longer available: resume --from-snapshot an older one")
		} else if !rs.Uploaded && host == nil {
			rs.Blockers = append(rs.Blockers, "its snapshot was never uploaded and no host holds it")
		}
	}
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		if err := checkRunQuota(ctx, tx, tenantID); err != nil {
			rs.Blockers = append(rs.Blockers, err.Error())
		}
		return nil
	})
	return rs, err
}

type Event struct {
	ID    int64          `json:"id"`
	Epoch *int           `json:"epoch,omitempty"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data"`
	Time  time.Time      `json:"time"`
}

type listEventsInput struct {
	RunPath
	After string `query:"after" doc:"Only events after this id (up to 1000 at a time)." example:"0"`
}

type listEventsOutput struct {
	Body struct {
		Events []Event `json:"events"`
	} `nameHint:"EventList"`
}

func (s *Server) listEvents(ctx context.Context, in *listEventsInput) (*listEventsOutput, error) {
	p := principal(ctx)
	after, _ := strconv.ParseInt(in.After, 10, 64)
	events, err := s.events(ctx, p.TenantID, in.ID, after)
	if err != nil {
		return nil, err
	}
	out := &listEventsOutput{}
	out.Body.Events = events
	return out, nil
}

func (s *Server) events(ctx context.Context, tenantID, runID string, after int64) ([]Event, error) {
	events := []Event{}
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, runID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, epoch, type, data, created_at FROM run_events
			WHERE run_id = $1 AND id > $2 ORDER BY id LIMIT 1000`, runID, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ID, &e.Epoch, &e.Type, &e.Data, &e.Time); err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	return events, err
}

type inputRequest struct {
	Text      string `json:"text,omitempty" doc:"A message: agents get it as a prompt, generic workloads on stdin."`
	Raw       []byte `json:"raw,omitempty" doc:"Raw bytes for a generic workload's stdin."`
	Interrupt bool   `json:"interrupt,omitempty" doc:"Stop the current turn first."`
	RequestID string `json:"requestId,omitempty" doc:"Makes retries safe: the runner delivers each id once. Generated if absent."`
}

type postInputInput struct {
	RunPath
	Body inputRequest
}

// requestIDOutput acknowledges an accepted request.
type requestIDOutput struct {
	Status int
	Body   struct {
		RequestID string `json:"requestId"`
	} `nameHint:"RequestAccepted"`
}

func accepted(requestID string) *requestIDOutput {
	out := &requestIDOutput{Status: http.StatusAccepted}
	out.Body.RequestID = requestID
	return out
}

// postInput steers a live Run. The request id makes retries safe: the
// runner delivers each id once.
func (s *Server) postInput(ctx context.Context, req *postInputInput) (*requestIDOutput, error) {
	p := principal(ctx)
	in := req.Body
	if in.Text == "" && len(in.Raw) == 0 && !in.Interrupt {
		return nil, errf(http.StatusBadRequest, "bad_request", "text, raw or interrupt is required")
	}
	if in.RequestID == "" {
		in.RequestID = ids.New("in")
	}
	id := req.ID
	var hostID string
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var epoch int
		if err := tx.QueryRow(ctx, `SELECT state, current_epoch FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state, &epoch); err != nil {
			return err
		}
		switch {
		case state == StateStopped || state == StateLost:
			return errf(http.StatusConflict, "not_running", "run is %s: use resume, which accepts an input", state)
		case terminal(state):
			return errf(http.StatusConflict, "not_running", "run is %s", state)
		case !live(state):
			return errf(http.StatusConflict, "not_running", "run is %s: not started yet", state)
		}
		if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, id, epoch).Scan(&hostID); err != nil {
			return err
		}
		msg := proto.Input{RequestID: in.RequestID, Text: in.Text, Raw: in.Raw, Interrupt: in.Interrupt}
		typ := proto.MsgInput
		if in.Interrupt && in.Text == "" && len(in.Raw) == 0 {
			typ = proto.MsgInterrupt
		}
		if err := s.systemEnqueue(ctx, hostID, id, epoch, typ, msg); err != nil {
			return err
		}
		return addEvent(ctx, tx, p.TenantID, id, epoch, "input", map[string]any{
			"requestId": in.RequestID, "interrupt": in.Interrupt, "text": in.Text, "rawBytes": len(in.Raw)})
	})
	if err != nil {
		return nil, err
	}
	s.hub.Notify(hostID)
	return accepted(in.RequestID), nil
}

// systemEnqueue writes a host message in its own system-scoped transaction:
// host_messages is not visible to tenant scopes.
func (s *Server) systemEnqueue(ctx context.Context, hostID, runID string, epoch int, typ string, payload any) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return enqueue(ctx, tx, hostID, runID, epoch, typ, payload)
	})
}

// acceptedRun is a Run whose request was accepted.
type acceptedRun struct {
	Status int
	Body   *Run
}

func (s *Server) stopRun(ctx context.Context, in *RunPath) (*acceptedRun, error) {
	return s.stopOrCancel(ctx, in.ID, "stop")
}

func (s *Server) cancelRun(ctx context.Context, in *RunPath) (*acceptedRun, error) {
	return s.stopOrCancel(ctx, in.ID, "cancel")
}

func (s *Server) stopOrCancel(ctx context.Context, id, reason string) (*acceptedRun, error) {
	p := principal(ctx)
	var hostID string
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
			return err
		}
		if terminal(state) {
			return nil // idempotent
		}
		if reason == "cancel" {
			if _, err := tx.Exec(ctx, `UPDATE runs SET cancel_requested = true WHERE id = $1`, id); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, p.TenantID, id, 0, reason+".requested", map[string]any{"by": p.Actor()}); err != nil {
			return err
		}
		switch state {
		case StateSubmitted, StateResuming, StateProvisioning, StateStopped, StateLost:
			// Nothing running: settle it here.
			next := StateStopped
			if reason == "cancel" {
				next = StateCancelled
				s.secrets.drop(id)
			}
			if state == next {
				return nil
			}
			if state == StateLost && reason == "stop" {
				return nil
			}
			return setRunState(ctx, tx, p.TenantID, id, next, reason, 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Live placement: ask its runner, in a system scope (placements are
	// tenant rows, host messages are not).
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var tenant, state string
		if err := tx.QueryRow(ctx, `SELECT tenant_id, state FROM runs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, p.TenantID).Scan(&tenant, &state); err != nil {
			return err
		}
		if !live(state) {
			return nil
		}
		var err error
		hostID, err = s.requestStop(ctx, tx, tenant, id, reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	if hostID != "" {
		s.hub.Notify(hostID)
	}
	run, err := s.loadRun(ctx, p.TenantID, id, false)
	if err != nil {
		return nil, err
	}
	return &acceptedRun{http.StatusAccepted, run}, nil
}

type resumeRequest struct {
	RequestID string        `json:"requestId,omitempty" doc:"Names this resume: in its resume.requested event, in the addedBy of the repositories it adds and in their git.clone events. Generated if absent."`
	Secrets   []spec.Secret `json:"secrets,omitempty" doc:"A value for every one of the Run's secrets, and for the credentials of repositories it adds: luxd never keeps them."`
	Git       *resumeGit    `json:"git,omitempty" doc:"Repositories to add. The runner clones them before the Run starts again; one whose clone fails is dropped and the Run goes on without it (a git.clone event says so)."`
	Input     *resumeInput  `json:"input,omitempty" doc:"A message for the workload once it is back."`
	// FromSnapshot resumes from an older snapshot (e.g. after lost).
	FromSnapshot string           `json:"fromSnapshot,omitempty" doc:"Resume from this snapshot instead of the latest (e.g. after lost)."`
	To           string           `json:"to,omitempty" doc:"Operators: place it on this host (id or name), and nowhere else."`
	Resources    *resumeResources `json:"resources,omitempty" doc:"Change what the Run gets from now on (e.g. more disk after it went over)."`
}

type resumeGit struct {
	Repositories []spec.Repository `json:"repositories" doc:"As in the spec's git.repositories; names must be new. A credential the spec does not declare becomes a secret used only by the runner, and its value must be in secrets."`
}

type resumeResources struct {
	Disk spec.Bytes `json:"disk,omitempty" doc:"A new disk limit: its writable layer plus state volumes."`
}

type resumeInput struct {
	Text string `json:"text"`
}

type resumeRunInput struct {
	RunPath
	Body *resumeRequest
}

// resumeOutput is the resumed Run and the id of the request.
type resumeOutput struct {
	Status    int
	RequestID string `header:"Lux-Request-Id" doc:"The resume's request id (requestId, or the one generated)."`
	Body      *Run
}

// requireSecrets refuses a submit or resume that lacks a value (or has an
// empty one) for any of the Run's secrets, naming them all.
func requireSecrets(refs []spec.SecretRef, values map[string]string) error {
	var missing []string
	for _, ref := range refs {
		if values[ref.Name] == "" {
			missing = append(missing, ref.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	he := errf(http.StatusUnprocessableEntity, "secrets_required", "secret values required: %s", strings.Join(missing, ", "))
	he.Details = missing
	return he
}

// resumeRun puts a stopped, lost or failed Run back in the queue. Its
// secrets must be supplied again: luxd never kept them. An operator may
// resume without them while this luxd still holds them in memory (it has
// not restarted since they were last supplied), and may choose the host.
//
// Repositories added on resume join the stored spec, marked with the
// request id; the runner clones them into the restored workspace.
func (s *Server) resumeRun(ctx context.Context, in *resumeRunInput) (*resumeOutput, error) {
	p := principal(ctx)
	id := in.ID
	req := resumeRequest{}
	if in.Body != nil {
		req = *in.Body
	}
	req.RequestID = cmp.Or(req.RequestID, ids.New("resume"))
	var adding []spec.Repository
	if req.Git != nil {
		adding = req.Git.Repositories
	}
	values := map[string]string{}
	for _, sec := range req.Secrets {
		values[sec.Name] = sec.Value
	}
	cached := false
	if p.Operator && len(req.Secrets) == 0 {
		if v, ok := s.secrets.get(id); ok {
			values, cached = v, true
		}
	}
	placeOn, err := s.chooseHost(ctx, p, req.To)
	if err != nil {
		return nil, err
	}
	resumed := false
	err = s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var refs []spec.SecretRef
		var sp spec.RunSpec
		if err := tx.QueryRow(ctx, `SELECT state, secrets, spec FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&state, &refs, &sp); err != nil {
			return err
		}
		switch state {
		case StateStopped, StateLost, StateFailed:
		case StateResuming:
			if len(adding) > 0 {
				return errf(http.StatusConflict, "not_resumable", "run is resuming already: repositories can only be added to a stopped, lost or failed Run")
			}
			return nil // idempotent: the first resume's secrets stand
		case StateCancelled, StateSucceeded:
			return errf(http.StatusConflict, "not_resumable", "run is %s", state)
		default:
			return errf(http.StatusConflict, "not_resumable", "run is %s: stop it first", state)
		}
		if p.Operator && len(req.Secrets) == 0 && requireSecrets(refs, values) != nil {
			return errf(http.StatusUnprocessableEntity, "secrets_required",
				"luxd no longer holds this Run's secrets: only the tenant can resume it, supplying them")
		}
		if len(adding) > 0 {
			added, err := addRepositories(&sp, refs, adding, req.RequestID, s.cfg.Defaults)
			if err != nil {
				return err
			}
			// A new credential's value comes with the resume, like the
			// others' (held values cover only the secrets the Run had).
			refs = append(refs, added...)
			stored, _, _ := sp.SplitSecrets()
			if _, err := tx.Exec(ctx, `UPDATE runs SET spec = $2 WHERE id = $1`, id, stored); err != nil {
				return err
			}
		}
		if err := requireSecrets(refs, values); err != nil {
			return err
		}
		// Rotation is allowed: record the new fingerprints.
		newRefs := make([]spec.SecretRef, 0, len(refs))
		for _, ref := range refs {
			newRefs = append(newRefs, spec.SecretRef{Name: ref.Name, Fingerprint: spec.Fingerprint(ref.Name, values[ref.Name])})
		}
		slices.SortFunc(newRefs, func(a, b spec.SecretRef) int { return cmp.Compare(a.Name, b.Name) })
		// place_on is this resume's alone: a migration's that never took
		// effect (its host died first) does not steer it.
		if _, err := tx.Exec(ctx, `UPDATE runs SET secrets = $2, cancel_requested = false, place_on = $3, avoid_host = NULL, pending_input = NULL WHERE id = $1`,
			id, newRefs, placeOn); err != nil {
			return err
		}
		if r := req.Resources; r != nil && r.Disk != 0 {
			if r.Disk < 0 {
				return errf(http.StatusUnprocessableEntity, "invalid_request", "resources.disk must not be negative")
			}
			if _, err := tx.Exec(ctx, `UPDATE runs SET spec = jsonb_set(spec, '{resources,disk}', to_jsonb($2::bigint)) WHERE id = $1`,
				id, int64(r.Disk)); err != nil {
				return err
			}
		}
		if req.FromSnapshot != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT available FROM snapshots WHERE id = $1 AND run_id = $2`, req.FromSnapshot, id).Scan(&ok); err != nil {
				return errf(http.StatusNotFound, "not_found", "no snapshot %s for this run", req.FromSnapshot)
			}
			if !ok {
				return errf(http.StatusConflict, "snapshot_unavailable", "snapshot %s is no longer available", req.FromSnapshot)
			}
			if _, err := tx.Exec(ctx, `UPDATE runs SET snapshot_id = $2 WHERE id = $1`, id, req.FromSnapshot); err != nil {
				return err
			}
		}
		var in *proto.Input
		if req.Input != nil && req.Input.Text != "" {
			in = &proto.Input{RequestID: ids.New("in"), Text: req.Input.Text}
		}
		if err := checkRunQuota(ctx, tx, p.TenantID); err != nil {
			return err
		}
		resumed = true
		// Cached before commit, so the scheduler never sees the Run
		// resuming without its values.
		s.secrets.put(id, values)
		why := "resume requested"
		if p.Operator {
			why = "resumed by an operator"
		}
		names := make([]string, 0, len(adding))
		for _, r := range adding {
			names = append(names, r.Name)
		}
		if err := addEvent(ctx, tx, p.TenantID, id, 0, "resume.requested", map[string]any{
			"requestId": req.RequestID, "by": p.Actor(), "addedRepositories": names}); err != nil {
			return err
		}
		return s.requestResume(ctx, tx, p.TenantID, id, in, why)
	})
	if err != nil {
		if resumed && !cached {
			s.secrets.drop(id)
		}
		return nil, err
	}
	s.Kick()
	run, err := s.loadRun(ctx, p.TenantID, id, false)
	if err != nil {
		return nil, err
	}
	return &resumeOutput{http.StatusAccepted, req.RequestID, run}, nil
}

// addRepositories merges repositories added on resume into a Run's stored
// spec (see spec.AddRepositories) and returns refs for the credentials it
// declared, whose values the resume must carry. A spec that no longer
// validates is a 422 invalid_spec, as at submit.
func addRepositories(sp *spec.RunSpec, refs []spec.SecretRef, repos []spec.Repository, requestID string, d spec.Defaults) ([]spec.SecretRef, error) {
	names, err := sp.AddRepositories(repos, requestID, d)
	if err != nil {
		return nil, invalidSpec(err)
	}
	var added []spec.SecretRef
	for _, n := range names {
		// A secret the Run already has a ref for (declared but unused) needs
		// no new one.
		if !slices.ContainsFunc(refs, func(r spec.SecretRef) bool { return r.Name == n }) {
			added = append(added, spec.SecretRef{Name: n})
		}
	}
	return added, nil
}

// pushRun asks the Run's runner to push its repositories to the spec's
// push branch, with the runner's credentials. The outcome arrives as one
// git.push event carrying the request id and every repository's result.
type pushRequest struct {
	RequestID string `json:"requestId,omitempty" doc:"Names the push in its git.push event. Generated if absent."`
	// Expect is, per repository name, the commit the push branch is
	// expected to be at.
	Expect map[string]string `json:"expect,omitempty" doc:"Per repository name, the commit (full id) the push branch must be at for the push to go ahead."`
}

type pushRunInput struct {
	RunPath
	Body *pushRequest
}

func (s *Server) pushRun(ctx context.Context, in *pushRunInput) (*requestIDOutput, error) {
	p := principal(ctx)
	id := in.ID
	req := pushRequest{}
	if in.Body != nil {
		req = *in.Body
	}
	msg := proto.Push{RequestID: cmp.Or(req.RequestID, ids.New("push"))}
	var hostID string
	var epoch int
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		var sp spec.RunSpec
		if err := tx.QueryRow(ctx, `SELECT state, current_epoch, spec, pushed FROM runs WHERE id = $1`, id).
			Scan(&state, &epoch, &sp, &msg.Leases); err != nil {
			return err
		}
		if sp.Git == nil || sp.Git.Push == nil {
			return errf(http.StatusConflict, "no_push", "the run's spec has no git.push branch")
		}
		// An expected commit replaces the lease: a compare-and-swap.
		for repo, sha := range req.Expect {
			i := slices.IndexFunc(sp.Git.Repositories, func(r spec.Repository) bool { return r.Name == repo })
			if i < 0 {
				return errf(http.StatusUnprocessableEntity, "invalid_request", "expect: the run has no repository %q", repo)
			}
			if !sp.Git.Repositories[i].Pushed() {
				return errf(http.StatusUnprocessableEntity, "invalid_request", "expect: repository %q is not pushed (push: false)", repo)
			}
			if !isCommitID(sha) {
				return errf(http.StatusUnprocessableEntity, "invalid_request", "expect[%s]: not a full commit id", repo)
			}
			if msg.Leases == nil {
				msg.Leases = map[string]string{}
			}
			msg.Leases[repo] = sha
		}
		if state != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: pushing needs a live placement", state)
		}
		if err := tx.QueryRow(ctx, `SELECT host_id FROM placements WHERE run_id = $1 AND epoch = $2`, id, epoch).Scan(&hostID); err != nil {
			return err
		}
		data := map[string]any{"requestId": msg.RequestID}
		if len(req.Expect) > 0 {
			data["expect"] = req.Expect
		}
		return addEvent(ctx, tx, p.TenantID, id, epoch, "push.requested", data)
	})
	if err != nil {
		return nil, err
	}
	if err := s.systemEnqueue(ctx, hostID, id, epoch, proto.MsgPush, msg); err != nil {
		return nil, err
	}
	s.hub.Notify(hostID)
	return accepted(msg.RequestID), nil
}

// isCommitID is a full SHA-1 or SHA-256 commit id, lowercase hex.
func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type Snapshot struct {
	ID        string         `json:"id"`
	Epoch     int            `json:"epoch"`
	Manifest  proto.Manifest `json:"manifest"`
	Available bool           `json:"available"`
	OnHost    string         `json:"onHost,omitempty"`
	Uploaded  bool           `json:"uploaded"`
	CreatedAt time.Time      `json:"createdAt"`
}

type listSnapshotsOutput struct {
	Body struct {
		Snapshots []Snapshot `json:"snapshots"`
	} `nameHint:"SnapshotList"`
}

func (s *Server) listSnapshots(ctx context.Context, in *RunPath) (*listSnapshotsOutput, error) {
	p := principal(ctx)
	out := []Snapshot{}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT s.id, s.epoch, s.manifest, s.available,
				CASE WHEN s.host_copy THEN coalesce(h.name, '') ELSE '' END, s.uploaded, s.created_at
			FROM snapshots s LEFT JOIN hosts h ON h.id = s.host_id WHERE s.run_id = $1 ORDER BY s.epoch`, in.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sn Snapshot
			if err := rows.Scan(&sn.ID, &sn.Epoch, &sn.Manifest, &sn.Available, &sn.OnHost, &sn.Uploaded, &sn.CreatedAt); err != nil {
				return err
			}
			out = append(out, sn)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	res := &listSnapshotsOutput{}
	res.Body.Snapshots = out
	return res, nil
}

type Host struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Tenant      string            `json:"tenant,omitempty" doc:"The owning tenant's name; empty for a platform host."`
	Pool        string            `json:"pool"`
	State       string            `json:"state"`
	StateReason string            `json:"stateReason,omitempty"`
	Draining    bool              `json:"draining"`
	Labels      map[string]string `json:"labels"`
	Capacity    proto.Capacity    `json:"capacity"`
	// Allocated and LiveRuns: what its live placements asked for. A tenant
	// sees only its own placements' share of a platform host.
	Allocated     spec.Resources        `json:"allocated"`
	Versions      map[string]any        `json:"versions"`
	Platform      bool                  `json:"platform"`
	LiveRuns      int                   `json:"liveRuns"`
	ProviderID    *string               `json:"providerId,omitempty"`
	LastHeartbeat *time.Time            `json:"lastHeartbeat,omitempty"`
	Times         map[string]*time.Time `json:"times"`
	// Placements: on GET /v1/hosts/{id} only, its live placements (the
	// caller's; every tenant's for an operator).
	Placements []HostPlacement `json:"placements,omitempty"`
}

// HostPlacement is a live placement, seen from its host.
type HostPlacement struct {
	RunID     string         `json:"runId"`
	RunName   string         `json:"runName,omitempty"`
	Tenant    string         `json:"tenant"`
	Epoch     int            `json:"epoch"`
	State     string         `json:"state"`
	Resources spec.Resources `json:"resources"`
	Since     time.Time      `json:"since"`
}

// TenantQuery narrows an operator's request to one tenant (requireKey
// reads it; it is here to be documented). Tenant keys ignore it.
type TenantQuery struct {
	Tenant string `query:"tenant" doc:"Operator keys: only this tenant (id or name). Tenant keys ignore it."`
}

// visibleHosts, for SQL on hosts h with the principal's tenant id as $1:
// tenants see their own hosts and platform hosts; an operator sees every
// host, or, narrowed to a tenant, what that tenant sees.
const visibleHosts = "($1 = '' OR h.tenant_id = $1 OR h.tenant_id IS NULL)"

// visiblePlacements, for SQL on placements pl with $1 as above: a tenant's
// own, or every tenant's for an operator.
const visiblePlacements = "($1 = '' OR pl.tenant_id = $1)"

// Select hostColumns FROM hostsFrom ($1: the principal's tenant id, for
// which of its placements count).
const hostColumns = `h.id, h.name, coalesce(ht.name, ''), h.pool, h.state, h.state_reason,
	h.draining, h.labels, h.capacity, h.versions, h.tenant_id IS NULL,
	hl.n, jsonb_build_object('cpus', hl.cpus, 'memory', hl.mem, 'disk', hl.disk),
	h.provider_id, h.last_heartbeat,
	h.provision_requested_at, h.provisioned_at, h.registered_at, h.first_placement_at, h.last_placement_ended_at,
	h.drain_requested_at, h.terminate_requested_at, h.terminated_at, h.lost_at, h.created_at`

const hostsFrom = `hosts h LEFT JOIN tenants ht ON ht.id = h.tenant_id
	CROSS JOIN LATERAL (SELECT count(*) AS n, coalesce(sum((pl.resources->>'cpus')::float8), 0) AS cpus,
		coalesce(sum((pl.resources->>'memory')::int8), 0) AS mem, coalesce(sum((pl.resources->>'disk')::int8), 0) AS disk
		FROM placements pl WHERE pl.host_id = h.id AND pl.state IN ` + livePlacementStates + ` AND ` + visiblePlacements + `) hl`

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	var t [10]*time.Time
	if err := row.Scan(&h.ID, &h.Name, &h.Tenant, &h.Pool, &h.State, &h.StateReason, &h.Draining, &h.Labels, &h.Capacity, &h.Versions,
		&h.Platform, &h.LiveRuns, &h.Allocated, &h.ProviderID, &h.LastHeartbeat,
		&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9]); err != nil {
		return h, err
	}
	h.Times = map[string]*time.Time{
		"provisionRequested": t[0], "provisioned": t[1], "registered": t[2], "firstPlacement": t[3],
		"lastPlacementEnded": t[4], "drainRequested": t[5], "terminateRequested": t[6], "terminated": t[7],
		"lost": t[8], "created": t[9],
	}
	return h, nil
}

type listHostsInput struct {
	TenantQuery
	All   string `query:"all" doc:"true to include terminated hosts." example:"true"`
	Pool  string `query:"pool" doc:"Only this pool's hosts."`
	State string `query:"state" doc:"Only hosts in this state: provisioning, ready, draining, lost or terminated."`
}

type listHostsOutput struct {
	Body struct {
		Hosts []Host `json:"hosts"`
	} `nameHint:"HostList"`
}

// listHosts lists hosts by name: a tenant's own and the platform's, or
// every host for an operator.
func (s *Server) listHosts(ctx context.Context, in *listHostsInput) (*listHostsOutput, error) {
	p := principal(ctx)
	hosts := []Host{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+hostColumns+` FROM `+hostsFrom+`
			WHERE `+visibleHosts+` AND ($2 OR h.state <> 'terminated') AND ($3 = '' OR h.pool = $3) AND ($4 = '' OR h.state = $4)
			ORDER BY h.name, h.id`, p.TenantID, in.All == "true" || in.State == "terminated", in.Pool, in.State)
		if err != nil {
			return err
		}
		hosts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Host, error) { return scanHost(row) })
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &listHostsOutput{}
	out.Body.Hosts = hosts
	return out, nil
}

// HostPath names a host, by id or name.
type HostPath struct {
	ID string `path:"id" doc:"The host's id or name (a name of a host that is not terminated)."`
}

type getHostInput struct {
	HostPath
	TenantQuery
}

type hostOutput struct {
	Body Host
}

// getHost is one host with its live placements.
func (s *Server) getHost(ctx context.Context, in *getHostInput) (*hostOutput, error) {
	p := principal(ctx)
	var h Host
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, true)
		if err != nil {
			return err
		}
		if h, err = scanHost(tx.QueryRow(ctx, `SELECT `+hostColumns+` FROM `+hostsFrom+` WHERE h.id = $2`, p.TenantID, id)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id, r.name, t.name, pl.epoch, pl.state, pl.resources, pl.created_at
			FROM placements pl JOIN runs r ON r.id = pl.run_id JOIN tenants t ON t.id = pl.tenant_id
			WHERE pl.host_id = $2 AND pl.state IN `+livePlacementStates+` AND `+visiblePlacements+`
			ORDER BY pl.created_at`, p.TenantID, id)
		if err != nil {
			return err
		}
		h.Placements, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (HostPlacement, error) {
			var hp HostPlacement
			err := row.Scan(&hp.RunID, &hp.RunName, &hp.Tenant, &hp.Epoch, &hp.State, &hp.Resources, &hp.Since)
			return hp, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &hostOutput{h}, nil
}

// resolveHost finds a host the principal sees, by id or by name. A name
// names a host that is not terminated (names are reused); with
// terminated, a terminated host is found by id. A name that two visible
// hosts share (an operator's view spans tenants) is ambiguous.
func (s *Server) resolveHost(ctx context.Context, tx pgx.Tx, p Principal, ref string, terminated bool) (string, error) {
	rows, err := tx.Query(ctx, `SELECT h.id FROM hosts h WHERE `+visibleHosts+`
		AND ((h.id = $2 AND ($3 OR h.state <> 'terminated')) OR (h.name = $2 AND h.state <> 'terminated')) LIMIT 2`, p.TenantID, ref, terminated)
	if err != nil {
		return "", err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	switch {
	case err != nil:
		return "", err
	case len(found) == 0:
		return "", errNotFound
	case len(found) > 1:
		return "", errf(http.StatusConflict, "ambiguous", "more than one host is named %s: use its id, or ?tenant=", ref)
	}
	return found[0], nil
}

type drainHostRequest struct {
	ForceEvict bool `json:"forceEvict,omitempty" doc:"Also stop this host's live Runs so they resume elsewhere. Without it, they finish where they are; only new placements are refused."`
}

type drainHostInput struct {
	HostPath
	TenantQuery
	Body *drainHostRequest
}

type drainHostOutput struct {
	Status int
	Body   struct {
		Draining bool   `json:"draining"`
		Host     string `json:"host" doc:"The host's id."`
	} `nameHint:"HostDrain"`
}

// drainHost stops new placements on a host; with forceEvict, it also
// stops its live Runs so they resume elsewhere, even on a host already
// draining. A tenant drains only its own hosts; an operator, any host.
func (s *Server) drainHost(ctx context.Context, in *drainHostInput) (*drainHostOutput, error) {
	p := principal(ctx)
	stopReason := evictReason(in.Body != nil && in.Body.ForceEvict)
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		id, err := s.resolveHost(ctx, tx, p, in.ID, false)
		if err != nil {
			return err
		}
		hosts, err = s.drainHosts(ctx, tx, "drain requested", causeManual, stopReason, "id = $1 AND (tenant_id = $2 OR $3)", id, p.TenantID, p.Operator)
		if err == nil && len(hosts) == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifyAll(hosts)
	out := &drainHostOutput{Status: http.StatusAccepted}
	out.Body.Draining, out.Body.Host = true, hosts[0]
	return out, nil
}

// Drain causes, tracked independently in hosts.drain_causes so several can
// coexist. The reaper, the undrain check and the per-pool cap key off
// these; state_reason is display text any path may overwrite.
const (
	causeOutdated  = "outdated"
	causeManual    = "manual" // drainHost, deletePool
	causeScaleDown = "scale-down"
	causePreempt   = "preempt"
)

// drainHosts takes hosts out of service (no new placements), adds cause to
// their drain_causes, sets state_reason to reason, and, unless stopReason
// is "" (cordon only), asks their live placements to stop with it (drain
// or preempt: both resume elsewhere), including on hosts already draining;
// where selects them (placeholders from $1). A cordoned host's Runs finish
// where they are: the reaper (static hosts) or the pool's replace path
// (provisioned) takes it once idle. Returns their ids, to notify once the
// transaction commits.
func (s *Server) drainHosts(ctx context.Context, tx pgx.Tx, reason, cause, stopReason, where string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`UPDATE hosts SET draining = true,
			state = CASE WHEN state = 'ready' THEN 'draining' ELSE state END,
			state_reason = $%d,
			drain_causes = CASE WHEN $%d = ANY(drain_causes) THEN drain_causes ELSE array_append(drain_causes, $%d) END,
			drain_requested_at = coalesce(drain_requested_at, now())
		WHERE state <> 'terminated' AND %s
		RETURNING id`, len(args)+1, len(args)+2, len(args)+2, where), append(args, reason, cause)...)
	if err != nil {
		return nil, err
	}
	hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(hosts) == 0 || stopReason == "" {
		return hosts, err
	}
	live, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", hosts)
	if err != nil {
		return nil, err
	}
	for _, p := range live {
		if _, err := s.requestStop(ctx, tx, p.TenantID, p.RunID, stopReason); err != nil {
			return nil, err
		}
	}
	return hosts, nil
}

// scaleDownSeconds is a pool's scale_down_after_s: nil for luxd's default
// (zero), whole seconds otherwise (nil too when under one).
func scaleDownSeconds(d time.Duration) *int {
	if n := int(d / time.Second); n > 0 {
		return &n
	}
	return nil
}

func (s *Server) notifyAll(hosts []string) {
	for _, h := range hosts {
		s.hub.Notify(h)
	}
}

// evictReason is the placements' stop reason for a drain: "drain" with
// forceEvict, or "" for cordon-only (drainHosts then leaves them running).
func evictReason(force bool) string {
	if force {
		return "drain"
	}
	return ""
}

type Pool struct {
	Name      string         `json:"name"`
	Tenant    string         `json:"tenant,omitempty" readOnly:"true" doc:"The owning tenant's name; empty for a platform pool."`
	Provider  string         `json:"provider"`
	Template  map[string]any `json:"template,omitempty"`
	MinHosts  int            `json:"minHosts"`
	MaxHosts  int            `json:"maxHosts"`
	WarmHosts int            `json:"warmHosts"`
	// ScaleDownAfter: how long a provisioned host stays idle before it is
	// released; empty for luxd's default.
	ScaleDownAfter  spec.Duration `json:"scaleDownAfter,omitempty" doc:"How long a provisioned host stays idle before it is released, e.g. 600s; empty: luxd's scale_down_after (default 10m)."`
	WarmWhileActive bool          `json:"warmWhileActive,omitempty" doc:"Keep warmHosts only while the pool is in use (a Run placed or ended within scaleDownAfter, or one waiting); an idle pool scales down to minHosts."`
	Shared          bool          `json:"shared"`
	Platform        bool          `json:"platform"`
}

type listPoolsOutput struct {
	Body struct {
		Pools []Pool `json:"pools"`
	} `nameHint:"PoolList"`
}

func (s *Server) listPools(ctx context.Context, _ *TenantQuery) (*listPoolsOutput, error) {
	p := principal(ctx)
	pools := []Pool{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.name, coalesce(t.name, ''), p.provider, p.template, p.min_hosts, p.max_hosts, p.warm_hosts,
				coalesce(p.scale_down_after_s, 0), p.warm_while_active,
				p.shared, p.tenant_id IS NULL
			FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id
			WHERE ($1 = '' OR p.tenant_id = $1 OR p.tenant_id IS NULL) AND NOT p.retired ORDER BY p.name, t.name NULLS FIRST`, p.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pl Pool
			var sda int
			if err := rows.Scan(&pl.Name, &pl.Tenant, &pl.Provider, &pl.Template, &pl.MinHosts, &pl.MaxHosts, &pl.WarmHosts,
				&sda, &pl.WarmWhileActive, &pl.Shared, &pl.Platform); err != nil {
				return err
			}
			pl.ScaleDownAfter.Duration = time.Duration(sda) * time.Second
			pools = append(pools, pl)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := &listPoolsOutput{}
	out.Body.Pools = pools
	return out, nil
}

// deletePool removes one of the tenant's pools. Its provisioned hosts are
// cordoned and terminated by the provisioner once idle (their provider is
// known from the pool row, kept as `retired`); forceEvict also stops their
// live Runs. Its Runs wait for a pool of that name again.
type deletePoolInput struct {
	TenantQuery
	Name       string `path:"name" doc:"The pool's name."`
	ForceEvict bool   `query:"forceEvict" doc:"Also stop this pool's live Runs so they resume elsewhere, instead of finishing where they are."`
}

func (s *Server) deletePool(ctx context.Context, in *deletePoolInput) (*struct{}, error) {
	p := principal(ctx)
	name := in.Name
	stopReason := evictReason(in.ForceEvict)
	var hosts []string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE pools SET retired = true, min_hosts = 0, warm_hosts = 0, max_hosts = 0
			WHERE tenant_id = $1 AND name = $2 AND NOT retired`, p.TenantID, name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		hosts, err = s.drainHosts(ctx, tx, "pool removed", causeManual, stopReason,
			"tenant_id = $1 AND pool = $2 AND provider_id IS NOT NULL", p.TenantID, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifyAll(hosts)
	return &struct{}{}, nil
}

// putPool creates or updates one of the tenant's pools.
type poolBody struct {
	TenantQuery
	Body Pool
}

func (s *Server) putPool(ctx context.Context, in *poolBody) (*poolBody, error) {
	p := principal(ctx)
	pl := in.Body
	if pl.Name == "" || (pl.Provider != "static" && pl.Provider != "ec2") {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "name and provider (static | ec2) are required")
	}
	if pl.Shared {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "only platform pools can be shared (luxd admin create-pool --shared)")
	}
	if pl.Provider == "ec2" {
		if raw, present := pl.Template["userData"]; present {
			ud, isString := raw.(string)
			if !isString {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.userData must be a string, got %T", raw)
			}
			if !hostboot.ValidUserData(ud) {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.userData %q: want ignition, script or env", ud)
			}
		}
	}
	if pl.Template == nil {
		pl.Template = map[string]any{}
	}
	sda := scaleDownSeconds(pl.ScaleDownAfter.Duration)
	if pl.ScaleDownAfter.Duration < 0 || pl.ScaleDownAfter.Duration > 0 && sda == nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "scaleDownAfter must be at least 1s")
	}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, template, min_hosts, max_hosts, warm_hosts,
				scale_down_after_s, warm_while_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET provider = EXCLUDED.provider, template = EXCLUDED.template,
				min_hosts = EXCLUDED.min_hosts, max_hosts = EXCLUDED.max_hosts, warm_hosts = EXCLUDED.warm_hosts,
				scale_down_after_s = EXCLUDED.scale_down_after_s, warm_while_active = EXCLUDED.warm_while_active,
				retired = false`,
			ids.New(ids.Pool), p.TenantID, pl.Name, pl.Provider, pl.Template, pl.MinHosts, pl.MaxHosts, pl.WarmHosts,
			sda, pl.WarmWhileActive)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Kick()
	return &poolBody{Body: pl}, nil
}
