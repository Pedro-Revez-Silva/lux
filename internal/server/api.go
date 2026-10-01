package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
		Summary: "List Runs", Description: "Newest first. Each Run carries `cost`: its totals per currency (final and estimate parts) and its cost status, as `GET /v1/runs/{id}/cost` has them.",
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
			"Without `follow` the stream ends after what is there now. " +
			"The output of the Run's servers (ch `server`) is left out unless `servers=true`, or `server=<name>` for one server's alone.",
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
			"Adding needs a stopped, lost or failed Run: while it is resuming, 409. " +
			"A Run whose only snapshot report was refused has nothing to restore: 409 no_snapshot, unless fromSnapshot names one.",
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
		OperationID: "runCost", Method: http.MethodGet, Path: "/v1/runs/{id}/cost", Tags: []string{"runs"},
		Summary: "What a Run cost",
		Description: "Its cost lines (one per source and item), with totals per currency and per family and currency. " +
			"Amounts in different currencies are never added together. Each total splits into the part from final lines " +
			"and the part from estimates, which may still change. Amounts are list prices (basis: list).",
		Errors: []int{http.StatusNotFound},
	}, "read", s.runCost)
	register(s, api, huma.Operation{
		OperationID: "costSummary", Method: http.MethodGet, Path: "/v1/costs", Tags: []string{"costs"},
		Summary: "Summarize costs by currency, time and up to two groups",
		Errors:  []int{http.StatusBadRequest},
	}, "read", s.costSummary)
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
		OperationID: "runDiff", Method: http.MethodGet, Path: "/v1/runs/{id}/diff", Tags: []string{"runs"},
		Summary: "What a running Run changed in its repositories",
		Description: "Per repository, from its base to its working tree: committed, staged, unstaged and untracked (not ignored) changes, " +
			"computed now in the Run's container. `base=clone` (default) diffs from the commit the repository was cloned at, `base=head` from its HEAD. " +
			"Untracked files are cut at 32 KiB each, or 8 KiB each if the whole diff would pass 1 MiB, and binary ones are named only: " +
			"such a patch is `truncated` and does not apply as it is. A diff over 16 MiB is refused (502 `diff_failed`).\n\n" +
			"Only while the Run is running: otherwise 409 `run_not_running`. 404 `no_diff` when the Run has no repositories. " +
			"One diff per Run at a time (409 `diff_busy`). 503 `diff_unsupported` when the Run's runner predates diffs.",
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
	}, "read", s.runDiff)
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
		Description:   "Agents get text as a message, read at their next model step where the adapter can (the Run's steer says), else when the current turn ends; generic workloads get it on stdin. Its first answer is one lux.input record (phase accepted, or failed) and an input.delivered or input.failed event; after acceptance, lux.input.consumed (where the adapter has a receipt) or lux.input.failed records, and input.consumed or input.failed events. A stopped Run takes its input through resume instead.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict},
	}, "run", s.postInput)
	register(s, api, s.streamOp(api, "execRun", "/v1/runs/{id}/exec", "exec", "Run a command in a running Run",
		"The client's first message is a StreamOpen: `{\"command\": [...], \"tty\": true, \"rows\": 24, \"cols\": 80}`. "),
		"run", streamed(s, s.runStream("exec")))
	register(s, api, s.streamOp(api, "attachRun", "/v1/runs/{id}/attach", "attach", "Attach to a running Run's terminal",
		"Needs a generic workload with workload.tty. "),
		"run", streamed(s, s.runStream("attach")))
	register(s, api, s.streamOp(api, "portForward", "/v1/runs/{id}/ports/{name}", "tunnel", "Reach one of a running Run's ports",
		"A TCP connection to a port the spec declares in network.ports, or to one of the Run's servers (by its name), carried as StreamData. "),
		"run", streamed(s, s.streamHandler("tunnel")))
	register(s, api, huma.Operation{
		OperationID: "mintTicket", Method: http.MethodPost, Path: "/v1/runs/{id}/tickets", Tags: []string{"interactive"},
		Summary: "Mint a stream ticket",
		Description: "A single-use credential, good for 60 seconds, for what a browser cannot send an Authorization header with. " +
			"kind exec: `?ticket=` on this Run's exec, attach and ports streams, which then act as the caller. " +
			"kind preview: the sign-in of this Run's preview URLs (`https://<host>/.lux/auth?ticket=...&to=/path`). " +
			"An exec ticket needs the `run` scope; a preview one, `read`.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "read", s.mintTicket)

	// Servers.
	register(s, api, huma.Operation{
		OperationID: "listServers", Method: http.MethodGet, Path: "/v1/runs/{id}/servers", Tags: []string{"servers"},
		Summary: "List a Run's servers", Errors: []int{http.StatusNotFound},
	}, "read", s.listServers)
	register(s, api, huma.Operation{
		OperationID: "addServer", Method: http.MethodPost, Path: "/v1/runs/{id}/servers", Tags: []string{"servers"},
		Summary: "Add a server to a Run",
		Description: "A named port of the Run, with an optional command lux runs in its container. " +
			"It starts now if start (default: when it has a command), which needs the Run running. Any state but finished may add one. " +
			"One without a command is watched: ready whenever its port accepts connections while the Run runs.",
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", s.addServer)
	register(s, api, huma.Operation{
		OperationID: "putServer", Method: http.MethodPut, Path: "/v1/runs/{id}/servers/{name}", Tags: []string{"servers"},
		Summary: "Change a server", Description: "Its port, command, workdir and env. A running one keeps what it was started with until it is started again.",
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "run", s.putServer)
	for _, a := range []struct{ action, summary, doc string }{
		{"start", "Start a server", "Runs its command. Idempotent while it runs. 409 not_running unless the Run is running; 409 no_command for a server without one (its port is watched whenever the Run runs)."},
		{"stop", "Stop a server", "Stops its command; a server without one is no longer watched, until the Run's next placement. Idempotent."},
		{"restart", "Restart a server", "Stops its command and runs it again, as it is defined now. 409 not_running unless the Run is running; 409 no_command without a command."},
	} {
		register(s, api, huma.Operation{
			OperationID: a.action + "Server", Method: http.MethodPost, Path: "/v1/runs/{id}/servers/{name}/" + a.action, Tags: []string{"servers"},
			Summary: a.summary, Description: a.doc,
			Errors: []int{http.StatusNotFound, http.StatusConflict},
		}, "run", s.serverAction(a.action))
	}
	register(s, api, huma.Operation{
		OperationID: "removeServer", Method: http.MethodDelete, Path: "/v1/runs/{id}/servers/{name}", Tags: []string{"servers"},
		Summary: "Remove a server", Description: "Stops it first.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict},
	}, "run", s.removeServer)
	register(s, api, huma.Operation{
		OperationID: "serverLog", Method: http.MethodGet, Path: "/v1/runs/{id}/servers/{name}/log", Tags: []string{"servers"},
		Summary: "A server's recent output",
		Description: "The last lines its command wrote, newest last, across placements: the current one's from its host, earlier ones' from their uploaded output. " +
			"A placement whose output cannot be had (lost with its host, or on a host not connected to this luxd) is skipped.",
		Errors: []int{http.StatusNotFound},
	}, "read", s.serverLog)

	// Hosts and pools.
	register(s, api, huma.Operation{
		OperationID: "listHosts", Method: http.MethodGet, Path: "/v1/hosts", Tags: []string{"hosts"},
		Summary: "List hosts", Description: "The tenant's own hosts, and platform hosts in pools it can use. Operators: every host.",
	}, "read", s.listHosts)
	register(s, api, huma.Operation{
		OperationID: "hostSummary", Method: http.MethodGet, Path: "/v1/hosts/summary", Tags: []string{"hosts"},
		Summary: "Live hosts in total",
		Description: "Over the hosts GET /v1/hosts lists without filters (not terminated, the caller's view): how many, and the capacity of the ready and draining ones against what their live placements hold (a tenant: its own placements). " +
			"A host named summary is read by its id.",
	}, "read", s.hostSummary)
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
		OperationID: "hostCost", Method: http.MethodGet, Path: "/v1/hosts/{id}/cost", Tags: []string{"hosts", "costs"},
		Summary: "A host's hourly allocation and rate periods",
		Errors:  []int{http.StatusNotFound, http.StatusForbidden, http.StatusConflict},
	}, "read", s.hostCost)
	register(s, api, huma.Operation{
		OperationID: "listHostEvents", Method: http.MethodGet, Path: "/v1/hosts/{id}/events", Tags: []string{"hosts"},
		Summary: "List a host's events",
		Description: "What happened to the host, newest first: registered, ready, placements assigned and ended, drains and their cause, lost, termination, provider errors. " +
			"A tenant sees its own hosts' events only, and so does an operator narrowed to it with `?tenant=`.",
		Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusConflict},
	}, "read", s.listHostEvents)
	register(s, api, huma.Operation{
		OperationID: "drainHost", Method: http.MethodPost, Path: "/v1/hosts/{id}/drain", Tags: []string{"hosts"},
		Summary: "Drain a host",
		Description: "No new placements; its live Runs finish where they are. With forceEvict, they are also stopped and resumed elsewhere " +
			"(also applies to a host that is already draining). Only the tenant's own hosts; operators, any host.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound},
	}, "admin", s.drainHost)
	register(s, api, huma.Operation{
		OperationID: "setHostPrice", Method: http.MethodPut, Path: "/v1/hosts/{id}/price", Tags: []string{"hosts"},
		Summary: "Set a static host's hourly price",
		Description: "The flat price its compute cost is worked out at, from now: the host's current rate period closes and a new one opens. " +
			"Only for a host that registered itself (a provisioned host is priced by its provider). " +
			"The tenant's own hosts; operators, any host (a platform host's price is theirs to set).",
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "admin", s.setHostPrice)
	register(s, api, huma.Operation{
		OperationID: "clearHostPrice", Method: http.MethodDelete, Path: "/v1/hosts/{id}/price", Tags: []string{"hosts"},
		Summary:     "Clear a static host's hourly price",
		Description: "Its current rate period closes now and no new one opens: its Runs get no compute cost from then on. Who may: as for setting it.",
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "admin", s.clearHostPrice)
	register(s, api, huma.Operation{
		OperationID: "listPools", Method: http.MethodGet, Path: "/v1/pools", Tags: []string{"pools"},
		Summary: "List pools", Description: "The tenant's pools and the platform pools it can use. Operators: every pool.",
	}, "read", s.listPools)
	s.poolRoutes(api)
	register(s, api, huma.Operation{
		OperationID: "listPoolEvents", Method: http.MethodGet, Path: "/v1/pools/{name}/events", Tags: []string{"pools"},
		Summary: "List a pool's events",
		Description: "What happened to the pool, newest first: scale-ups and why, launches and their failures, placements on its hosts, hosts released and why, spot interruptions, configuration changes, removal (retired) and being set again (restored). " +
			"A failure repeated on every provisioner pass is one event, its `count` and `lastTime` updated in place. " +
			"A tenant's own pool of that name, else the platform's (`?owner=` picks one); a platform pool's events are the operators' (they name other tenants' Runs), and not shown to an operator narrowed with `?tenant=`.",
		Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusConflict},
	}, "read", s.listPoolEvents)

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
		Summary: "The state of the system over time", Description: "Samples of what GET /v1/status reports. " +
			"For an operator key reading the whole system (no tenant), each sample also carries `control`: the machine luxd runs on and its Postgres.",
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
		Description: "`isDefault: true` makes it the tenant's default pool, where Runs whose spec names no pool go from then on " +
			"(Runs already submitted keep theirs); the tenant's previous default loses the mark. `false` clears it. " +
			"A body of exactly `name` and `isDefault` marks an existing pool and changes nothing else; " +
			"any other field without `provider` is a 422 `invalid_pool`. " +
			"From an operator key naming no tenant, a marker-only body marks a platform pool as the platform's default, " +
			"for tenants without one of their own.",
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, "admin", s.putPool)
	register(s, api, huma.Operation{
		OperationID: "deletePool", Method: http.MethodDelete, Path: "/v1/pools/{name}", Tags: []string{"pools"},
		Summary: "Remove a pool",
		Description: "Its provisioned hosts are cordoned and terminated once idle; its Runs wait for a pool of that name again. " +
			"If it was the tenant's default pool, the tenant has none until another is marked. " +
			"forceEvict also stops its hosts' live Runs so they resume elsewhere.",
		Errors: []int{http.StatusNotFound},
	}, "admin", forTenant(s.deletePool))
	register(s, api, huma.Operation{
		OperationID: "renamePool", Method: http.MethodPost, Path: "/v1/pools/{name}/rename", Tags: []string{"pools"},
		Summary: "Rename a pool",
		Description: "Changes the pool's name and nothing else: a pool is its id, which hosts, host tokens, Runs, costs and instances refer to, so they all stay with it. " +
			"A default pool stays the default. Runs keep the name they were submitted with in their spec; lists show the pool's current name. " +
			"The old name is free at once: a Run naming it no longer finds this pool. " +
			"409 pool_exists if the owner has a pool (live or removed) of the new name; 422 invalid_pool for a name outside the pool-name rule. " +
			"A tenant's own pools; with an operator key and no tenant, or with `owner=platform`, a platform pool.",
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, "admin", s.renamePool)
}

type statusBody struct {
	Status string `json:"status" example:"ok"`
}

type statusOutput struct {
	Body statusBody
}

// streamOp declares an interactive stream: a WebSocket (stream.go),
// authenticated by a key, the console's sign-in or a ticket (streamAuth).
func (s *Server) streamOp(api huma.API, id, path, kind, summary, doc string) huma.Operation {
	// OpenAPI cannot describe WebSocket messages: named in x-websocket.
	ws := map[string]any{"message": schemaRef[proto.StreamData](api)}
	if kind == "exec" {
		ws["open"] = schemaRef[proto.StreamOpen](api)
	}
	return huma.Operation{
		OperationID: id, Method: http.MethodGet, Path: path, Tags: []string{"interactive"},
		Summary: summary,
		Description: "A WebSocket (send `Upgrade: websocket`), relayed to the Run's host, carrying JSON text messages. " + doc +
			"A browser authenticates with `?ticket=` (POST /v1/runs/{id}/tickets, kind exec) instead of a header; a page of another origin than luxd's is refused (403 bad_origin). " +
			"Then StreamData both ways: `{\"data\": <base64>}` for bytes, `{\"eof\": true}` to close input, `{\"rows\", \"cols\"}` to resize. " +
			"The stream ends with one `{\"exitCode\": N}` (exec) or `{\"error\": \"...\"}`, or just the socket closing, and luxd closes it.\n\n" +
			"Without the upgrade the same URL answers 200 `{\"status\": \"ok\"}` if the stream could be opened, and the error it would get otherwise, so a client can check first.",
		Responses: map[string]*huma.Response{
			"101": {Description: "Switching to the WebSocket."},
			"200": {Description: "The stream can be opened (no Upgrade header).", Content: jsonContent(schemaRef[statusBody](api))},
		},
		Errors:      []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
		Extensions:  map[string]any{"x-websocket": ws},
		Middlewares: huma.Middlewares{s.streamAuth},
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
	Image          *ImageResolution `json:"image,omitempty"`
	Secrets        []spec.SecretRef `json:"secrets"`
	CreatedAt      time.Time        `json:"createdAt"`
	ScheduledAt    *time.Time       `json:"firstScheduledAt,omitempty"`
	StartedAt      *time.Time       `json:"firstStartedAt,omitempty"`
	FinishedAt     *time.Time       `json:"finishedAt,omitempty"`
	RuntimeSeconds float64          `json:"runtimeSeconds" doc:"Seconds its placements have spent running, summed: each from reaching running to exiting or being lost; one still running counts up to the time of the response."`
	RuntimeSince   *time.Time       `json:"runtimeSince,omitempty" doc:"When the placement still running started, if one is: runtimeSeconds grows from the response's time on."`
	// Placement time: what getting the Run onto a host and started took.
	PlacementSeconds      float64     `json:"placementSeconds" doc:"Time spent placing the Run, summed over its placements: for each, from when the Run needed a host (created, or its previous placement ending) until that placement's workload started. A Run still being placed counts up to the time of the response (placing). waitSeconds + startSeconds."`
	PlacementWaitSeconds  float64     `json:"placementWaitSeconds" doc:"The part of placementSeconds spent waiting for a host (needing one until assigned)."`
	PlacementStartSeconds float64     `json:"placementStartSeconds" doc:"The part spent starting on the host (assigned until the workload started, or the placement ended without starting)."`
	Placing               bool        `json:"placing,omitempty" doc:"The Run is being placed now: waiting for a host, or starting on one; placementSeconds grows from the response's time on."`
	Placements            []Placement `json:"placements,omitempty"`
	Usage                 *RunUsage   `json:"usage,omitempty"`
	// Resume: on GET /v1/runs/{id} of a stopped, lost or failed Run, what
	// a resume would take.
	Resume *Resumability `json:"resume,omitempty"`
	// Servers: on GET /v1/runs/{id}, its servers.
	Servers []RunServer   `json:"servers,omitzero" doc:"On GET /v1/runs/{id}: the Run's servers."`
	Cost    *RunCostBrief `json:"cost,omitempty" doc:"In GET /v1/runs only: the totals per currency and the status of GET /v1/runs/{id}/cost."`
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
	MemoryLimit        *int64     `json:"memoryLimit,omitempty" doc:"Bytes: the container's memory limit, the Run's resources.memory scaled to its host's share (MemTotal less the runner's headroom, over the memory the host offers). Absent until the container starts, and for placements from before luxd recorded it."`
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
	coalesce(rh.name, ''), coalesce(rp.host_id, ''), coalesce(rpool.name, ''), coalesce(r.pool_id, ''), rr.seconds, rr.since,
	rpt.wait, rpt.start, rpt.placing`

// runsFrom: a Run with its tenant, its current placement's host, and its
// runtime (rr). Runtime is the sum over its placements of started_at
// (reached running) to ended_at (exited or lost), or to now() for one still
// live (stopping included); a placement that never reached running adds 0,
// and so does a terminal one missing ended_at, rather than growing forever.
// since is when the live one started. Per Run, one scan of placements
// (run_id, epoch).
var runsFrom = runsFromAt("now()")

// runsFromAt is runsFrom with its clock (what live runtimes and placement
// times count to) at now: a paged list sorts by the clock its first page
// was read at.
func runsFromAt(now string) string {
	return `runs r ` + runTenantJoin + runHostJoin + runPoolJoin + runRuntimeJoin(now) + runPlacementJoin(now)
}

// The joins of runsFrom, one per alias, so a paged list's keys read only
// what their sort value needs (runSortKeys' from).
const (
	runTenantJoin = ` JOIN tenants rt ON rt.id = r.tenant_id`
	runHostJoin   = ` LEFT JOIN placements rp ON rp.run_id = r.id AND rp.epoch = r.current_epoch
	LEFT JOIN hosts rh ON rh.id = rp.host_id`
	runPoolJoin = ` LEFT JOIN pools rpool ON rpool.id = r.pool_id`
)

func runRuntimeJoin(now string) string {
	return ` CROSS JOIN LATERAL (SELECT
			coalesce(sum(greatest(0, extract(epoch FROM coalesce(p.ended_at, CASE WHEN p.state IN ` + livePlacementStates + ` THEN ` + now + ` ELSE p.started_at END) - p.started_at))), 0)::float8 AS seconds,
			max(p.started_at) FILTER (WHERE p.ended_at IS NULL AND p.state IN ` + livePlacementStates + `) AS since
		FROM placements p WHERE p.run_id = r.id AND p.started_at IS NOT NULL) rr`
}

func runPlacementJoin(now string) string {
	return ` CROSS JOIN LATERAL (` + placementTimeSQL(now) + `) rpt`
}

// placementTimeSQL is a lateral over the Run r's placements (one scan of
// (run_id, epoch)): wait, the seconds each spent needing a host (from its
// needed_since, else the previous placement's end, else the Run's
// creation, until assigned) plus, while the Run is queued now, the wait
// since it last needed one; start, the seconds from assignment until its
// workload started, else until luxd saw it running (started_at: runners
// that never report the workload's start), else until it ended; one still
// starting counts to now. placing: either is still counting. now is the
// clock (a cursor's, when a page sorts by it).
func placementTimeSQL(now string) string {
	return `SELECT
			coalesce(sum(greatest(0, extract(epoch FROM x.created_at - x.req))), 0)::float8
				+ CASE WHEN r.state IN ` + queuedRunStates + ` THEN greatest(0, extract(epoch FROM ` + now + ` - coalesce(r.needs_host_since, max(x.ended_at), r.created_at)))::float8 ELSE 0 END AS wait,
			coalesce(sum(greatest(0, extract(epoch FROM coalesce(x.workload_started_at, x.started_at, x.ended_at,
				CASE WHEN x.state IN ` + livePlacementStates + ` THEN ` + now + ` ELSE x.created_at END) - x.created_at))), 0)::float8 AS start,
			r.state IN ` + queuedRunStates + ` OR coalesce(bool_or(x.workload_started_at IS NULL AND x.started_at IS NULL AND x.ended_at IS NULL AND x.state IN ` + livePlacementStates + `), false) AS placing
		FROM (SELECT p.created_at, p.ended_at, p.workload_started_at, p.started_at, p.state,
				coalesce(p.needed_since, lag(p.ended_at) OVER (ORDER BY p.epoch), r.created_at) AS req
			FROM placements p WHERE p.run_id = r.id) x`
}

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Tenant, &r.Name, &r.Labels, &r.State, &r.StateReason, &r.Activity, &r.ExitCode, &r.Epoch,
		&r.SessionID, &r.SnapshotID, &r.Spec, &r.Image, &r.Secrets, &r.CreatedAt, &r.ScheduledAt, &r.StartedAt, &r.FinishedAt, &r.Host, &r.HostID,
		&r.Pool, &r.PoolID, &r.RuntimeSeconds, &r.RuntimeSince, &r.PlacementWaitSeconds, &r.PlacementStartSeconds, &r.Placing)
	r.PlacementSeconds = r.PlacementWaitSeconds + r.PlacementStartSeconds
	if info, ok := spec.Adapters[r.Spec.Workload.Adapter]; ok && info.Steer.Lands != "" {
		st := info.Steer
		r.Steer = &st
	}
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
	if sp.Placement.Pool != "" && sp.Placement.PoolID != "" {
		// Checked here, not in Normalize: the stored spec carries both.
		return nil, invalidSpec(&spec.ValidationError{Problems: []string{
			"placement: name the pool by pool or poolId, not both"}})
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
		// The Run is bound to its pool's id: every later placement stays
		// in that pool whatever the default becomes or the pool is called.
		// The spec keeps the name as resolved now.
		rp, err := resolvePool(ctx, tx, p.TenantID, stored.Placement.Pool, stored.Placement.PoolID)
		if err != nil {
			return err
		}
		stored.Placement.Pool = rp.Name
		_, err = tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, name, labels, spec, secrets, state, idempotency_key, pool_id, needs_host_since)
			VALUES ($1, $2, $3, $4, $5, $6, 'submitted', $7, $8, now())`,
			id, p.TenantID, sp.Name, nonNilMap(sp.Labels), stored, refs, idemArg, rp.ID)
		if err != nil {
			return err
		}
		ev := map[string]any{"by": p.Actor(), "pool": rp.Name, "poolFrom": rp.From}
		if o := rp.ownerLabel(); o != "" {
			ev["poolOwner"] = o
		}
		if err := addEvent(ctx, tx, p.TenantID, id, 0, "submitted", ev); err != nil {
			return err
		}
		if err := insertSpecServers(ctx, tx, p.TenantID, id, sp); err != nil {
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
	PageQuery
	State     string   `query:"state" doc:"Only Runs in these states (comma-separated)." example:"running,stopped"`
	Resumable bool     `query:"resumable" doc:"Only Runs resume accepts: stopped, lost or failed, except one whose only snapshot report was refused."`
	Host      string   `query:"host" doc:"Only Runs with a placement (any epoch) on this host, by id or name."`
	Label     []string `query:"label,explode" doc:"Only Runs with this label (key=value); repeat to require several."`
	Before    string   `query:"before" doc:"Only Runs created before this time (RFC 3339): the next page after a list's last Run. Unpaged lists only: with sort or a cursor it is a 400."`
	Limit     string   `query:"limit" doc:"Unpaged lists: at most this many Runs, newest first, 1 to 1000 (default 100; a value out of range is ignored). Paged lists (sort or a cursor): Runs per page, 1 to 200 (default 50; out of range is a 400). limit alone does not page." example:"100"`
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
	Next string `json:"next,omitempty" doc:"Paged lists: the next page's cursor (?next=)."`
	Prev string `json:"prev,omitempty" doc:"Paged lists: the previous page's cursor (?prev=)."`
	Page string `json:"page,omitempty" doc:"Paged lists: this page's own cursor (?at=), to read it again in place."`
}

// runSortKeys: GET /v1/runs' sort keys. cost is the total in the first of
// its currencies (as totals list them), a Run with no cost reported yet
// last, read once per Run by one grouped read of cost_lines; runtime is
// missing for a Run that never ran.
var runSortKeys = map[string]sortKey{
	"created":    {expr: `r.created_at`, cast: "timestamptz", first: "desc", notNull: true},
	"id":         {expr: `r.id`, cast: "text", first: "asc", notNull: true},
	"name":       {expr: `coalesce(nullif(r.name, ''), r.id)`, cast: "text", first: "asc", notNull: true},
	"tenant":     {expr: `rt.name`, cast: "text", first: "asc", notNull: true, from: runTenantJoin},
	"state":      {expr: `array_position(ARRAY['submitted', 'scheduled', 'provisioning', 'starting', 'running', 'stopping', 'stopped', 'resuming', 'succeeded', 'failed', 'cancelled', 'lost'], r.state)`, cast: "bigint", first: "asc"},
	"host":       {expr: `rh.name`, cast: "text", first: "asc", from: runHostJoin},
	"pool":       {expr: `rpool.name`, cast: "text", first: "asc", from: runPoolJoin},
	"adapter":    {expr: `r.spec->'workload'->>'adapter'`, cast: "text", first: "asc"},
	"runtime":    {expr: `CASE WHEN rr.seconds > 0 OR rr.since IS NOT NULL THEN rr.seconds END`, cast: "float8", first: "desc", from: runRuntimeJoin("{now}")},
	"placements": {expr: `r.current_epoch`, cast: "bigint", first: "desc", notNull: true},
	"placement":  {expr: `rpt.wait + rpt.start`, cast: "float8", first: "desc", notNull: true, from: runPlacementJoin("{now}")},
	"cost": {expr: `rc.amount`, cast: "numeric", first: "desc", from: ` LEFT JOIN (SELECT DISTINCT ON (cl.run_id) cl.run_id, sum(cl.amount) AS amount
		FROM cost_lines cl GROUP BY cl.run_id, cl.currency ORDER BY cl.run_id, cl.currency) rc ON rc.run_id = r.id`},
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
		where = append(where, "r.state IN "+resumableRunStates+` AND NOT (r.snapshot_id IS NULL AND EXISTS (
			SELECT 1 FROM placements p WHERE p.run_id = r.id AND p.epoch = r.current_epoch AND p.snapshot_refused))`)
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
	pg, paged, err := resolvePaging(in.PageQuery, runSortKeys, "created", in.Limit, 50, 200)
	if err != nil {
		return nil, err
	}
	if paged {
		if in.Before != "" {
			return nil, errf(http.StatusBadRequest, "bad_request", "before does not go with sort and cursors")
		}
		return s.listRunsPage(ctx, p, pg, where, args)
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
	err = s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		// The page is chosen first, so runsFrom's joins and runtime
		// aggregate run for its rows only, not for every Run matched.
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE r.id IN (SELECT r.id FROM runs r WHERE `+
			strings.Join(where, " AND ")+` ORDER BY r.created_at DESC, r.id DESC LIMIT `+strconv.Itoa(limit)+`) ORDER BY r.created_at DESC, r.id DESC`, args...)
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
		if err := rows.Err(); err != nil {
			return err
		}
		return s.listRunCosts(ctx, tx, runs)
	})
	if err != nil {
		return nil, err
	}
	out := &listRunsOutput{}
	out.Body.Runs = runs
	return out, nil
}

// listRunsPage is a page of GET /v1/runs in any sort key's order, keyed by
// (value, id). The page's ids and sort values are read first, at the
// cursor's clock; then its rows, at now, for display.
func (s *Server) listRunsPage(ctx context.Context, p Principal, pg *paging, where []string, base []any) (*listRunsOutput, error) {
	out := &listRunsOutput{}
	out.Body.Runs = []*Run{}
	err := s.db.Tx(ctx, p.scope(), func(tx pgx.Tx) error {
		stamp, err := pageClock(ctx, tx, pg)
		if err != nil {
			return err
		}
		filter := strings.Join(where, " AND ")
		// keys reads the page's keys, one past its end; with ahead, whether
		// any row precedes ahead, existence only: an order there can lead the
		// planner away from the predicate's own plan.
		keys := func(ahead *keyRow) ([]keyRow, error) {
			q, from, expr := pg.keySource(`runs r`, stamp, base)
			cond, limit := filter, pg.limit+1
			if ahead != nil {
				cond += " AND " + pg.beforeWhere(expr, "r.id", *ahead, q.arg)
				limit = 1
			} else {
				if pg.cursor != nil {
					cond += " AND " + pg.where(expr, "r.id", q.arg)
				}
				cond += " ORDER BY " + pg.order(expr, "r.id", pg.mode == "before")
			}
			rows, err := tx.Query(ctx, `SELECT r.id AS key_id, `+expr+`::text AS key_value FROM `+from+` WHERE `+cond+` LIMIT `+strconv.Itoa(limit), q.list...)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowToStructByPos[keyRow])
		}
		found, err := keys(nil)
		if err != nil {
			return err
		}
		page, next, prev, self, err := pg.pageLinks(found, stamp, func(first keyRow) (bool, error) {
			if pg.cursor == nil {
				return false, nil
			}
			ahead, err := keys(&first)
			return len(ahead) > 0, err
		})
		if err != nil {
			return err
		}
		out.Body.Next, out.Body.Prev, out.Body.Page = next, prev, self
		ids := pageIDs(page)
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM `+runsFrom+` WHERE r.id = ANY($1)`, ids)
		if err != nil {
			return err
		}
		loaded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Run, error) { return scanRun(row) })
		if err != nil {
			return err
		}
		out.Body.Runs = inPageOrder(ids, loaded, func(r *Run) string { return r.ID })
		return s.listRunCosts(ctx, tx, out.Body.Runs)
	})
	if err != nil {
		return nil, err
	}
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
				p.peak_memory_bytes, p.peak_disk_bytes, p.peak_pids, p.cpu_seconds, p.net_rx_bytes, p.net_tx_bytes, p.snapshot_bytes, p.memory_limit
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
				&pl.PeakMemoryBytes, &pl.PeakDiskBytes, &pl.PeakPids, &pl.CPUSeconds, &pl.NetRxBytes, &pl.NetTxBytes, &pl.SnapshotBytes, &pl.MemoryLimit); err != nil {
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
		if err := rows.Err(); err != nil {
			return err
		}
		servers, err := s.listServersTx(ctx, tx, id)
		run.Servers = nonNil(servers)
		return err
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
		var noSnapshot bool
		if err := tx.QueryRow(ctx, `SELECT `+refusedWithoutSnapshot+` FROM `+runsFrom+` WHERE r.id = $1`, run.ID).Scan(&noSnapshot); err != nil {
			return err
		}
		if noSnapshot {
			rs.Blockers = append(rs.Blockers, noSnapshotReason)
		}
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
		var noSnapshot bool
		if err := tx.QueryRow(ctx, `SELECT r.state, r.secrets, r.spec, `+refusedWithoutSnapshot+` FROM `+runsFrom+` WHERE r.id = $1 FOR UPDATE OF r`, id).
			Scan(&state, &refs, &sp, &noSnapshot); err != nil {
			return err
		}
		switch state {
		case StateStopped, StateLost, StateFailed:
			// Resuming would start from scratch, not from the refused state.
			if noSnapshot && req.FromSnapshot == "" {
				return errf(http.StatusConflict, "no_snapshot", "run cannot be resumed: %s", noSnapshotReason)
			}
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
	Pool        string            `json:"pool" doc:"Its pool's current name."`
	PoolID      string            `json:"poolId,omitempty" doc:"Its pool's id."`
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
	InstanceType  *string               `json:"instanceType,omitempty" doc:"The instance type the provider launched; absent for a host that registered itself."`
	Zone          *string               `json:"zone,omitempty" doc:"The availability zone the provider launched it in."`
	Market        *string               `json:"market,omitempty" enum:"on-demand,spot" doc:"on-demand or spot, as the provider launched it."`
	LastHeartbeat *time.Time            `json:"lastHeartbeat,omitempty"`
	Times         map[string]*time.Time `json:"times" doc:"Its lifecycle. A host whose launch failed has no terminateRequested or terminated: no instance ever ran."`
	Launch        *HostLaunch           `json:"launch,omitempty" doc:"How luxd's launch of it went; absent for a host that registered itself, or one from before launches were recorded."`
	// Placements: on GET /v1/hosts/{id} only, its live placements (the
	// caller's; every tenant's for an operator).
	Placements []HostPlacement `json:"placements,omitempty"`
}

// HostLaunch is a provisioned host's launch, apart from its state: a host
// the provider refused is operationally terminated, with outcome failed.
type HostLaunch struct {
	Outcome     string     `json:"outcome" enum:"requested,launched,failed,abandoned" doc:"requested: asked, no answer yet; launched: an instance started (it may since have ended, never registered or disappeared: see the host's state and reason); failed: the provider refused, no instance ever existed; abandoned: no answer was ever recorded and the row was written off."`
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty" doc:"When the provider answered (launched or failed)."`
	Error       string     `json:"error,omitempty" doc:"The provider's error, for a failed launch."`
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
const hostColumns = `h.id, h.name, coalesce(ht.name, ''), coalesce(hp.name, ''), coalesce(h.pool_id, ''), h.state, ` + hostStateReason + `,
	h.draining, h.labels, h.capacity, h.versions, h.tenant_id IS NULL,
	hl.n, jsonb_build_object('cpus', hl.cpus, 'memory', hl.mem, 'disk', hl.disk),
	h.provider_id, h.instance_type, h.zone, h.market, h.last_heartbeat,
	h.provision_requested_at, h.provisioned_at, h.registered_at, h.first_placement_at, h.last_placement_ended_at,
	h.drain_requested_at, h.terminate_requested_at, h.terminated_at, h.lost_at, h.created_at,
	h.launch_outcome, h.launch_finished_at, ` + hostLaunchError + ``

// hostSeesProviderError, for SQL on hosts h with $1 as visibleHosts: a
// platform host's provider error (account ids, role ARNs) is only for a
// principal that sees platform events ($1 empty: an operator not narrowed to
// a tenant), as for its pool.launch_failed events.
const hostSeesProviderError = `(h.tenant_id IS NOT NULL OR $1 = '')`

const hostLaunchError = `CASE WHEN ` + hostSeesProviderError + ` THEN coalesce(h.launch_error, '') ELSE '' END`

// hostStateReason: the provisioner writes "launch failed: <provider error>"
// as a failed launch's reason, so it is redacted under the same rule.
const hostStateReason = `CASE WHEN h.launch_outcome = 'failed' AND NOT ` + hostSeesProviderError + ` THEN 'launch failed' ELSE h.state_reason END`

const hostsFrom = `hosts h` + hostTenantJoin + hostPoolJoin + hostLoadJoin

// The joins of hostsFrom, so a paged list's keys and counts read only what
// their sort value and filter need (hostSortKeys' from). hl is only read for
// ready and draining hosts' sort values.
const (
	hostTenantJoin = ` LEFT JOIN tenants ht ON ht.id = h.tenant_id`
	hostPoolJoin   = ` LEFT JOIN pools hp ON hp.id = h.pool_id`
	hostLoadJoin   = ` CROSS JOIN LATERAL (SELECT count(*) AS n, coalesce(sum((pl.resources->>'cpus')::float8), 0) AS cpus,
		coalesce(sum((pl.resources->>'memory')::int8), 0) AS mem, coalesce(sum((pl.resources->>'disk')::int8), 0) AS disk
		FROM placements pl WHERE pl.host_id = h.id AND pl.state IN ` + livePlacementStates + ` AND ` + visiblePlacements + `) hl`
	// The same for a sort value, which is NULL unless ready or draining:
	// a one-time filter skips the scan for every other host.
	hostLoadSortJoin = ` CROSS JOIN LATERAL (SELECT count(*) AS n, coalesce(sum((pl.resources->>'cpus')::float8), 0) AS cpus,
		coalesce(sum((pl.resources->>'memory')::int8), 0) AS mem
		FROM placements pl WHERE h.state IN ('ready', 'draining') AND pl.host_id = h.id AND pl.state IN ` + livePlacementStates + ` AND ` + visiblePlacements + `) hl`
)

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	d := hostScanDest(&h)
	if err := row.Scan(d.fields...); err != nil {
		return h, err
	}
	d.finish()
	return h, nil
}

// hostDest is where a row of hostColumns is scanned, and finish fills in
// what is derived from it.
type hostDest struct {
	fields []any
	finish func()
}

func hostScanDest(h *Host) hostDest {
	var t [10]*time.Time
	var outcome *string
	var launched *time.Time
	var launchErr string
	fields := []any{&h.ID, &h.Name, &h.Tenant, &h.Pool, &h.PoolID, &h.State, &h.StateReason, &h.Draining, &h.Labels, &h.Capacity, &h.Versions,
		&h.Platform, &h.LiveRuns, &h.Allocated, &h.ProviderID, &h.InstanceType, &h.Zone, &h.Market, &h.LastHeartbeat,
		&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9], &outcome, &launched, &launchErr}
	return hostDest{fields, func() {
		h.Times = map[string]*time.Time{
			"provisionRequested": t[0], "provisioned": t[1], "registered": t[2], "firstPlacement": t[3],
			"lastPlacementEnded": t[4], "drainRequested": t[5], "terminateRequested": t[6], "terminated": t[7],
			"lost": t[8], "created": t[9],
		}
		if outcome != nil {
			h.Launch = &HostLaunch{Outcome: *outcome, RequestedAt: t[0], FinishedAt: launched, Error: launchErr}
			if *outcome == launchFailed && h.State == "terminated" {
				// Its row was closed, but no instance ever ran to terminate.
				h.Times["terminateRequested"], h.Times["terminated"] = nil, nil
			}
		}
	}}
}

type listHostsInput struct {
	TenantQuery
	PageQuery
	All       string `query:"all" doc:"true to include terminated hosts." example:"true"`
	Pool      string `query:"pool" doc:"Only this pool's hosts."`
	PoolID    string `query:"poolId" doc:"Only the hosts of the pool with this id (names repeat across owners)."`
	State     string `query:"state" doc:"Only hosts in this state: provisioning, ready, draining, lost or terminated; or launch_failed, the terminated hosts whose launch the provider refused (terminated then means the others)."`
	Lifecycle string `query:"lifecycle" enum:"live,ended," doc:"live: hosts not terminated; ended: terminated ones (launch failures included). Implies all."`
	Limit     string `query:"limit" doc:"Hosts per page, 1 to 500 (default 25). limit alone (or offset alone) pages the list too, newest first (sort=created)."`
	Offset    string `query:"offset" doc:"Paged lists: skip this many hosts (a numbered page), instead of a cursor; with next, prev or at it is a 400."`
}

type listHostsOutput struct {
	Body struct {
		Hosts  []Host `json:"hosts"`
		Total  *int   `json:"total,omitempty" doc:"Paged lists: how many hosts match."`
		Offset *int   `json:"offset,omitempty" doc:"Paged lists: how many matching hosts come before this page's first, in its order."`
		Next   string `json:"next,omitempty" doc:"Paged lists: the next page's cursor (?next=)."`
		Prev   string `json:"prev,omitempty" doc:"Paged lists: the previous page's cursor (?prev=)."`
		Page   string `json:"page,omitempty" doc:"Paged lists: this page's own cursor (?at=), to read it again in place."`
	} `nameHint:"HostList"`
}

// launchFailed is hosts.launch_outcome of a launch the provider refused.
const launchFailed = "failed"

// hostFailedSQL: the host row h is a launch the provider refused.
const hostFailedSQL = `(h.state = 'terminated' AND h.launch_outcome IS NOT DISTINCT FROM 'failed')`

// hostSortKeys: GET /v1/hosts' sort keys. A launch-failed host has no
// terminated time and no uptime (no instance ran): they sort last.
var hostSortKeys = map[string]sortKey{
	"name":       {expr: `h.name`, cast: "text", first: "asc", notNull: true},
	"id":         {expr: `h.id`, cast: "text", first: "asc", notNull: true},
	"tenant":     {expr: `ht.name`, cast: "text", first: "asc", from: hostTenantJoin},
	"pool":       {expr: `hp.name`, cast: "text", first: "asc", from: hostPoolJoin},
	"state":      {expr: `CASE WHEN ` + hostFailedSQL + ` THEN 6 ELSE array_position(ARRAY['provisioning', 'ready', 'draining', 'lost', 'terminated'], h.state) END`, cast: "bigint", first: "asc"},
	"runs":       {expr: `CASE WHEN h.state IN ('ready', 'draining') THEN hl.n END`, cast: "bigint", first: "desc", from: hostLoadSortJoin},
	"cpu":        {expr: `CASE WHEN h.state IN ('ready', 'draining') AND (h.capacity->>'cpus')::float8 > 0 THEN hl.cpus / (h.capacity->>'cpus')::float8 END`, cast: "float8", first: "desc", from: hostLoadSortJoin},
	"memory":     {expr: `CASE WHEN h.state IN ('ready', 'draining') AND (h.capacity->>'memory')::float8 > 0 THEN hl.mem / (h.capacity->>'memory')::float8 END`, cast: "float8", first: "desc", from: hostLoadSortJoin},
	"created":    {expr: `h.created_at`, cast: "timestamptz", first: "desc", notNull: true},
	"terminated": {expr: `CASE WHEN NOT ` + hostFailedSQL + ` THEN h.terminated_at END`, cast: "timestamptz", first: "desc"},
	"uptime":     {expr: `CASE WHEN NOT ` + hostFailedSQL + ` THEN extract(epoch FROM coalesce(h.terminated_at, {now}) - h.created_at) END`, cast: "float8", first: "desc"},
	"heartbeat":  {expr: `h.last_heartbeat`, cast: "timestamptz", first: "desc"},
}

// listHosts lists hosts by name: a tenant's own and the platform's, or
// every host for an operator. With sort, a cursor or limit it is paged
// (listHostsPage); without, it is the whole list, as it always was.
func (s *Server) listHosts(ctx context.Context, in *listHostsInput) (*listHostsOutput, error) {
	p := principal(ctx)
	out := &listHostsOutput{}
	out.Body.Hosts = []Host{}
	args := []any{p.TenantID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{visibleHosts}
	switch in.Lifecycle {
	case "live":
		where = append(where, "h.state <> 'terminated'")
	case "ended":
		where = append(where, "h.state = 'terminated'")
	case "":
		if in.All != "true" && in.State != "terminated" && in.State != "launch_failed" {
			where = append(where, "h.state <> 'terminated'")
		}
	default:
		return nil, errf(http.StatusBadRequest, "bad_request", "lifecycle: live or ended")
	}
	if in.Pool != "" {
		// Not hp.name: a filter on hosts h alone, so a page's count
		// reads hosts only.
		where = append(where, "h.pool_id IN (SELECT id FROM pools WHERE name = "+arg(in.Pool)+")")
	}
	if in.PoolID != "" {
		where = append(where, "h.pool_id = "+arg(in.PoolID))
	}
	switch in.State {
	case "":
	case "launch_failed":
		where = append(where, hostFailedSQL)
	case "terminated":
		where = append(where, "h.state = 'terminated' AND NOT "+hostFailedSQL)
	default:
		where = append(where, "h.state = "+arg(in.State))
	}
	pq := in.PageQuery
	if pq == (PageQuery{Dir: pq.Dir}) && (in.Limit != "" || in.Offset != "") {
		// limit or offset alone pages too, newest first (a dir alone is ignored).
		pq = PageQuery{Sort: "created"}
	}
	pg, paged, err := resolvePaging(pq, hostSortKeys, "created", in.Limit, 25, 500)
	if err != nil {
		return nil, err
	}
	if in.Offset != "" && paged && pg.cursor != nil {
		return nil, errf(http.StatusBadRequest, "bad_request", "offset does not go with next, prev or at")
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if !paged {
			rows, err := tx.Query(ctx, `SELECT `+hostColumns+` FROM `+hostsFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY h.name, h.id`, args...)
			if err != nil {
				return err
			}
			out.Body.Hosts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Host, error) { return scanHost(row) })
			return err
		}
		return listHostsPage(ctx, tx, pg, in.Offset, p.TenantID, where, args, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listHostsPage reads one page of hosts, the count of all that match and
// how many precede the page, in one transaction. base holds the filter's
// placeholders; the filter reads hosts h alone. As listRunsPage, the page's
// ids and sort values come first, from hosts h and only the joins the sort
// key needs; then its rows, by id. tenant is the principal's tenant id
// (visibleHosts' $1), which the rows' placements are read under.
func listHostsPage(ctx context.Context, tx pgx.Tx, pg *paging, offset, tenant string, where []string, base []any, out *listHostsOutput) error {
	stamp, err := pageClock(ctx, tx, pg)
	if err != nil {
		return err
	}
	filter := strings.Join(where, " AND ")
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM hosts h WHERE `+filter, base...).Scan(&total); err != nil {
		return err
	}
	skip := 0
	if offset != "" && pg.cursor == nil {
		n, err := strconv.Atoi(offset)
		if err != nil || n < 0 {
			return errf(http.StatusBadRequest, "bad_request", "offset: a count of hosts")
		}
		skip = n
	}
	q, from, expr := pg.keySource(`hosts h`, stamp, base)
	keyed := filter
	if pg.cursor != nil {
		keyed += " AND " + pg.where(expr, "h.id", q.arg)
	}
	rows, err := tx.Query(ctx, `SELECT h.id AS key_id, `+expr+`::text AS key_value FROM `+from+` WHERE `+keyed+
		` ORDER BY `+pg.order(expr, "h.id", pg.mode == "before")+` LIMIT `+strconv.Itoa(pg.limit+1)+` OFFSET `+strconv.Itoa(skip), q.list...)
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowToStructByPos[keyRow])
	if err != nil {
		return err
	}
	// How many matching hosts come before the page's first, in its order:
	// its offset, and whether there is a previous page.
	before := skip
	countBefore := func(first keyRow) error {
		c, from, expr := pg.keySource(`hosts h`, stamp, base)
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+from+` WHERE `+filter+` AND `+pg.beforeWhere(expr, "h.id", first, c.arg), c.list...).Scan(&before)
	}
	page, next, prev, self, err := pg.pageLinks(keys, stamp, func(first keyRow) (bool, error) {
		if pg.cursor == nil {
			return skip > 0, nil
		}
		err := countBefore(first)
		return before > 0, err
	})
	if err != nil {
		return err
	}
	if pg.mode == "before" && len(page) > 0 {
		if err := countBefore(page[0]); err != nil {
			return err
		}
	}
	ids := pageIDs(page)
	rows, err = tx.Query(ctx, `SELECT `+hostColumns+` FROM `+hostsFrom+` WHERE h.id = ANY($2)`, tenant, ids)
	if err != nil {
		return err
	}
	loaded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Host, error) { return scanHost(row) })
	if err != nil {
		return err
	}
	hosts := inPageOrder(ids, loaded, func(h Host) string { return h.ID })
	out.Body.Hosts, out.Body.Total, out.Body.Offset, out.Body.Next, out.Body.Prev, out.Body.Page = hosts, &total, &before, next, prev, self
	return nil
}

type HostResources struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"`
}

type hostSummaryOutput struct {
	Body struct {
		Live      int           `json:"live" doc:"Hosts not terminated."`
		Capacity  HostResources `json:"capacity" doc:"Of the ready and draining hosts."`
		Allocated HostResources `json:"allocated" doc:"What live placements on the ready and draining hosts hold (a tenant: its own)."`
	} `nameHint:"HostSummary"`
}

// hostSummary is the totals of the hosts GET /v1/hosts lists unfiltered,
// as a sum over its rows' allocated and capacity would give them, in one
// grouped read instead of the whole list.
func (s *Server) hostSummary(ctx context.Context, _ *TenantQuery) (*hostSummaryOutput, error) {
	p := principal(ctx)
	out := &hostSummaryOutput{}
	b := &out.Body
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		const up = `FILTER (WHERE h.state IN ('ready', 'draining'))`
		return tx.QueryRow(ctx, `SELECT count(*),
				coalesce(sum((h.capacity->>'cpus')::float8) `+up+`, 0), coalesce(sum((h.capacity->>'memory')::int8) `+up+`, 0)::bigint,
				coalesce(sum(hl.cpus), 0), coalesce(sum(hl.mem), 0)::bigint
			FROM hosts h`+hostLoadSortJoin+` WHERE `+visibleHosts+` AND h.state <> 'terminated'`, p.TenantID).
			Scan(&b.Live, &b.Capacity.CPUs, &b.Capacity.Memory, &b.Allocated.CPUs, &b.Allocated.Memory)
	})
	if err != nil {
		return nil, err
	}
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
	err := retryHostPlacements(ctx, func() error {
		return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			id, err := s.resolveHost(ctx, tx, p, in.ID, false)
			if err != nil {
				return err
			}
			// drainHosts writes its events last: lock nothing after it here.
			hosts, err = s.drainHosts(ctx, tx, "drain requested", causeManual, stopReason, "id = $1 AND (tenant_id = $2 OR $3)", id, p.TenantID, p.Operator)
			if err == nil && len(hosts) == 0 {
				return errNotFound
			}
			return err
		})
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

	poolRemovedReason = "pool removed"
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
	var later laterEvents
	hosts, err := s.drainHostsLater(ctx, tx, &later, reason, cause, stopReason, where, args...)
	if err != nil {
		return nil, err
	}
	return hosts, later.write()
}

// drainHostsLater is drainHosts leaving its events in later, for a caller
// that locks more rows after it.
func (s *Server) drainHostsLater(ctx context.Context, tx pgx.Tx, later *laterEvents, reason, cause, stopReason, where string, args ...any) ([]string, error) {
	var candidates []string
	if stopReason != "" {
		rows, err := tx.Query(ctx, `SELECT id FROM hosts WHERE state <> 'terminated' AND `+where+` ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		candidates, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		live, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", candidates)
		if err != nil {
			return nil, err
		}
		runs := make([]string, 0, len(live))
		for _, p := range live {
			runs = append(runs, p.RunID)
		}
		if err := lockReaperRuns(ctx, tx, runs); err != nil {
			return nil, err
		}
		rows, err = tx.Query(ctx, `SELECT host_id FROM (
			SELECT unnest($1::text[]) AS host_id
			UNION SELECT host_id FROM placements WHERE run_id = ANY($2)
		) all_hosts ORDER BY host_id`, candidates, runs)
		if err != nil {
			return nil, err
		}
		allHosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, host := range allHosts {
			if err := lockCostHost(ctx, tx, host); err != nil {
				return nil, err
			}
		}
		rows, err = tx.Query(ctx, `SELECT id FROM hosts WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, candidates)
		if err != nil {
			return nil, err
		}
		if _, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, err
		}
		// Check membership after the host rows are locked. A host that joins
		// the pool during discovery has no advisory lock in this transaction.
		rows, err = tx.Query(ctx, `SELECT id FROM hosts WHERE state <> 'terminated' AND `+where+` ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		eligible, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		candidateSet := make(map[string]bool, len(candidates))
		for _, host := range candidates {
			candidateSet[host] = true
		}
		for _, host := range eligible {
			if !candidateSet[host] {
				return nil, errHostPlacementsChanged
			}
		}
		current, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", candidates)
		if err != nil {
			return nil, err
		}
		locked := make(map[string]bool, len(runs))
		for _, run := range runs {
			locked[run] = true
		}
		for _, p := range current {
			if !locked[p.RunID] {
				return nil, errHostPlacementsChanged
			}
		}
	}
	updateWhere := where
	updateArgs := args
	if stopReason != "" {
		updateWhere = where + fmt.Sprintf(" AND id = ANY($%d)", len(args)+1)
		updateArgs = append(append([]any{}, args...), candidates)
	}
	// fresh: the hosts this cause is new on (locked in id order first, so
	// two drains for one cause cannot both see it new).
	n := len(updateArgs)
	rows, err := tx.Query(ctx, fmt.Sprintf(`WITH old AS (
			SELECT id, $%d = ANY(drain_causes) AS had FROM hosts
			WHERE state <> 'terminated' AND %s ORDER BY id FOR NO KEY UPDATE)
		UPDATE hosts SET draining = true,
			state = CASE WHEN state = 'ready' THEN 'draining' ELSE state END,
			state_reason = $%d,
			drain_causes = CASE WHEN old.had THEN drain_causes ELSE array_append(drain_causes, $%d) END,
			drain_requested_at = coalesce(drain_requested_at, now())
		FROM old WHERE hosts.id = old.id
		RETURNING hosts.id, NOT old.had`, n+2, updateWhere, n+1, n+2), append(updateArgs, reason, cause)...)
	if err != nil {
		return nil, err
	}
	type drained struct {
		ID    string
		Fresh bool
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[drained])
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(got))
	for _, h := range got {
		hosts = append(hosts, h.ID)
	}
	// An eviction is news where it stops a placement not already stopping.
	evicting := map[string]bool{}
	if stopReason != "" && len(hosts) > 0 {
		rows, err := tx.Query(ctx, `SELECT DISTINCT host_id FROM placements
			WHERE host_id = ANY($1) AND state IN `+livePlacementStates+` AND stop_requested_at IS NULL`, hosts)
		if err != nil {
			return nil, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			evicting[id] = true
		}
	}
	// One event per new cause or new eviction: draining a drained host
	// again for the same cause says nothing new. Left to later, after the
	// stops, which lock placements: event streams come last (infraevents.go).
	if len(hosts) > 0 && stopReason != "" {
		live, err := livePlacements(ctx, tx, "p.host_id = ANY($1)", hosts)
		if err != nil {
			return nil, err
		}
		for _, p := range live {
			if _, err := s.requestStop(ctx, tx, p.TenantID, p.RunID, stopReason); err != nil {
				return nil, err
			}
		}
	}
	for _, h := range got {
		if !h.Fresh && !evicting[h.ID] {
			continue
		}
		d := map[string]any{"cause": cause, "reason": reason}
		if evicting[h.ID] {
			d["evict"] = true
		}
		later.host(ctx, tx, h.ID, evDrainRequested, d)
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
	ID        string         `json:"id" readOnly:"true" doc:"The pool's immutable identity; unchanged by rename."`
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
	Platform        bool          `json:"platform" readOnly:"true" doc:"A platform pool (no tenant). In a request, true is refused unless the caller acts on the platform's pools (an operator key without a tenant)."`
	// HourlyPrice and Currency: a static pool's default price, copied to
	// each host when it first registers. Changing it does not reprice the
	// pool's existing hosts (PUT /v1/hosts/{id}/price does, one host).
	HourlyPrice string `json:"hourlyPrice,omitempty" doc:"Static pools: the default hourly price of hosts registering into the pool, a decimal string with up to 9 fractional digits. Copied to each host when it first registers; changing it does not reprice existing hosts. Refused for ec2 pools, which the provider prices." example:"0.40"`
	Currency    string `json:"currency,omitempty" doc:"The currency of hourlyPrice (ISO 4217); both or neither." example:"USD"`
	// IsDefault: nil in a request leaves the mark as it is.
	IsDefault *bool `json:"isDefault,omitempty" doc:"Runs whose spec names no pool go to this pool (the tenant's; a platform default serves tenants without one). At most one per tenant: marking one clears the tenant's previous default. On create or update, omitted leaves the mark as it is. A body without provider changes only the mark of an existing pool: besides name and isDefault its fields must be absent or zero (\"\", 0, false, null, {}); any other value needs provider."`
	// HostSize, HostSizeFrom and InstanceType are read-only: a request's
	// are ignored (poolReadOnly).
	HostSize     *HostSize `json:"hostSize,omitempty" readOnly:"true" doc:"The capacity the pool's hosts register with, as the scheduler plans new hosts: the minimum of each resource over the latest 8 registrations (for an ec2 pool, of its current template), so a pool scaled to zero still has one. Runs ask in these terms: a Run's resources.memory is a share of hostSize.memory. Absent when no host ever registered in the pool."`
	HostSizeFrom string    `json:"hostSizeFrom,omitempty" readOnly:"true" enum:"running,history" doc:"running: the pool has at least one ready or draining host now; history: hostSize comes only from hosts that have gone. Absent with hostSize."`
	InstanceType string    `json:"instanceType,omitempty" readOnly:"true" doc:"The provider's instance type of the latest host hostSize is taken from; absent for static hosts and with hostSize." example:"c7a.8xlarge"`
}

// HostSize is a host's capacity as it registers: CPUs, and bytes of
// memory and disk (disk 0: the host reserves none).
type HostSize struct {
	CPUs   float64 `json:"cpus" doc:"CPUs."`
	Memory int64   `json:"memory" doc:"Bytes of memory: the machine's gross memory, in which Runs ask."`
	Disk   int64   `json:"disk" doc:"Bytes of disk reserved for Runs; 0 when the host reserves none."`
}

// Pool.HostSizeFrom values.
const (
	HostSizeFromRunning = "running"
	HostSizeFromHistory = "history"
)

// poolReadOnly are the Pool fields luxd reports and a request cannot set.
var poolReadOnly = []string{"platform", "hostSize", "hostSizeFrom", "instanceType"}

// withHostSizes sets each of pools' host size, from one query.
func withHostSizes(ctx context.Context, tx pgx.Tx, pools []Pool) error {
	ids := make([]string, len(pools))
	for i, pl := range pools {
		ids[i] = pl.ID
	}
	sizes, err := poolHostSizes(ctx, tx, ids)
	if err != nil {
		return err
	}
	for i := range pools {
		if s, ok := sizes[pools[i].ID]; ok {
			pools[i].HostSize, pools[i].HostSizeFrom, pools[i].InstanceType = &s.Size, s.From, s.InstanceType
		}
	}
	return nil
}

// poolInput is putPool's body: a Pool (its schema too, newAPI), decoded
// with unknown fields refused.
type poolInput Pool

func (pl *poolInput) UnmarshalJSON(b []byte) error {
	type plain Pool
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(pl))
}

// markerOnly: a body with no provider and an isDefault moves only the
// mark. Its other fields must be zero, as a full Pool with only name and
// isDefault set serializes (earlier CLIs sent that); the non-zero ones are
// returned for the refusal. The decoded values are checked, not the keys:
// the decoder matches keys case-insensitively, and absent and zero decode
// alike. platform is checked by putPool against the caller.
func (pl *Pool) markerOnly() (bool, []string) {
	if pl.Provider != "" || pl.IsDefault == nil {
		return false, nil
	}
	var extra []string
	v := reflect.ValueOf(*pl)
	for i, f := range reflect.VisibleFields(v.Type()) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "name" || name == "isDefault" || slices.Contains(poolReadOnly, name) {
			continue
		}
		fv := v.Field(i)
		if !fv.IsZero() && !(fv.Kind() == reflect.Map && fv.Len() == 0) {
			extra = append(extra, name)
		}
	}
	return true, extra
}

type listPoolsOutput struct {
	Body struct {
		Pools []Pool `json:"pools"`
	} `nameHint:"PoolList"`
}

// poolColumns: Select poolColumns FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id.
const poolColumns = `p.id, p.name, coalesce(t.name, ''), p.provider, p.template, p.min_hosts, p.max_hosts, p.warm_hosts,
	coalesce(p.scale_down_after_s, 0), p.warm_while_active,
	p.shared, p.tenant_id IS NULL, coalesce(trim_scale(p.hourly_price)::text, ''), coalesce(p.price_currency, ''), p.is_default`

func scanPool(row pgx.Row) (Pool, error) {
	var pl Pool
	var sda int
	var isDefault bool
	err := row.Scan(&pl.ID, &pl.Name, &pl.Tenant, &pl.Provider, &pl.Template, &pl.MinHosts, &pl.MaxHosts, &pl.WarmHosts,
		&sda, &pl.WarmWhileActive, &pl.Shared, &pl.Platform, &pl.HourlyPrice, &pl.Currency, &isDefault)
	pl.ScaleDownAfter.Duration = time.Duration(sda) * time.Second
	pl.IsDefault = &isDefault
	return pl, err
}

func (s *Server) listPools(ctx context.Context, _ *TenantQuery) (*listPoolsOutput, error) {
	p := principal(ctx)
	pools := []Pool{}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+poolColumns+`
			FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id
			WHERE ($1 = '' OR p.tenant_id = $1 OR p.tenant_id IS NULL) AND NOT p.retired ORDER BY p.name, t.name NULLS FIRST`, p.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pl, err := scanPool(rows)
			if err != nil {
				return err
			}
			pools = append(pools, pl)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return withHostSizes(ctx, tx, pools)
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
	err := retryHostPlacements(ctx, func() error {
		return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			// The owner's default lock, then (ChangePool) the pool-name lock
			// and the pool row, then its Runs and hosts (drainHosts), and its
			// pool.retired event last, after the drain: the lock order of
			// infraevents.go puts event streams after every row lock.
			if err := lockDefaultPool(ctx, tx, p.TenantID); err != nil {
				return err
			}
			return ChangePool(ctx, tx, &p.TenantID, name, func() error {
				tag, err := tx.Exec(ctx, `UPDATE pools SET retired = true, min_hosts = 0, warm_hosts = 0, max_hosts = 0, is_default = false
					WHERE tenant_id = $1 AND name = $2 AND NOT retired`, p.TenantID, name)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 0 {
					return errNotFound
				}
				// Provisioned hosts, including those whose launch is in
				// flight (no provider id yet): cordoned now, a launch
				// that completes later lands on a draining host.
				hosts, err = s.drainHosts(ctx, tx, poolRemovedReason, causeManual, stopReason,
					"tenant_id = $1 AND pool_id = (SELECT id FROM pools WHERE tenant_id = $1 AND name = $2) AND (provider_id IS NOT NULL OR provision_requested_at IS NOT NULL)", p.TenantID, name)
				return err
			})
		})
	})
	if err != nil {
		return nil, err
	}
	s.notifyAll(hosts)
	return &struct{}{}, nil
}

// Where a Run's pool came from, in its submitted event's poolFrom.
const (
	poolFromSpec     = "spec"             // the spec named it
	poolFromTenant   = "tenant-default"   // the tenant's default pool
	poolFromPlatform = "platform-default" // the platform's default pool (set by lux_default_pool, in SQL)
	poolFromFallback = "fallback"         // neither is marked: the pool named "default"
)

// resolvedPool is the pool a Run was submitted to. ID is runs.pool_id:
// nil when no active pool has the name (the scheduler binds the Run once
// one does).
type resolvedPool struct {
	Name, From string
	ID         *string
	Platform   bool
}

// ownerLabel is the owner as the submitted event's poolOwner says it.
func (rp resolvedPool) ownerLabel() string {
	switch {
	case rp.ID == nil:
		return ""
	case rp.Platform:
		return "platform"
	default:
		return "tenant"
	}
}

// resolvePool is the pool of a Run whose spec names pool ("" for none), in
// the submitting tenant's scope: the named one, else the tenant's default,
// else the platform's, else "default". A default is that pool row; a name
// is the tenant's pool of that name, else the platform's, as hosts and
// provisioning have always preferred. A poolID is the tenant's active pool
// with that id, else the platform's; none is a 422 unknown_pool, since a
// Run bound to no pool would wait for one forever.
func resolvePool(ctx context.Context, tx pgx.Tx, tenantID, pool, poolID string) (resolvedPool, error) {
	rp := resolvedPool{Name: pool, From: poolFromSpec}
	if poolID != "" {
		err := tx.QueryRow(ctx, `SELECT name, tenant_id IS NULL FROM pools
			WHERE id = $1 AND NOT retired AND (tenant_id = $2 OR tenant_id IS NULL)`, poolID, tenantID).Scan(&rp.Name, &rp.Platform)
		if errors.Is(err, pgx.ErrNoRows) {
			return rp, errf(http.StatusUnprocessableEntity, "unknown_pool", "no pool has id %s", poolID)
		}
		rp.ID = &poolID
		return rp, err
	}
	if pool == "" {
		var id, name, from *string
		if err := tx.QueryRow(ctx, `SELECT pool_id, pool, pool_from FROM lux_default_pool()`).Scan(&id, &name, &from); err != nil {
			return rp, err
		}
		if id != nil {
			return resolvedPool{Name: *name, From: *from, ID: id, Platform: *from == poolFromPlatform}, nil
		}
		rp = resolvedPool{Name: "default", From: poolFromFallback}
	}
	var platform *bool
	if err := tx.QueryRow(ctx, `SELECT pool_id, platform FROM lux_pool_id($1)`, rp.Name).Scan(&rp.ID, &platform); err != nil {
		return rp, err
	}
	rp.Platform = platform != nil && *platform
	return rp, nil
}

// checkTemplateTags refuses an EC2 template's tags that are not a string
// map (null included), or that set a lux:* key: lux tags every instance itself (lux:pool,
// lux:host, …) and finds a pool's instances by those tags.
func checkTemplateTags(raw any) error {
	tags, isMap := raw.(map[string]any)
	if !isMap {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "template.tags must be an object of strings, got %T", raw)
	}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		if _, isString := tags[k].(string); !isString {
			return errf(http.StatusUnprocessableEntity, "invalid_pool", "template.tags.%s must be a string, got %T", k, tags[k])
		}
		if strings.HasPrefix(strings.ToLower(k), "lux:") {
			return errf(http.StatusUnprocessableEntity, "invalid_pool", "template.tags.%s: lux:* tags are set by lux", k)
		}
	}
	return nil
}

// putPool creates or updates one of the tenant's pools.
type poolBody struct {
	TenantQuery
	Body poolInput
}

func (s *Server) putPool(ctx context.Context, in *poolBody) (*poolBody, error) {
	p := principal(ctx)
	pl := Pool(in.Body)
	// platform is read-only, but a round-tripped Pool carries it: accept
	// it when it says what the caller's pool is. false and absent decode
	// alike, so only a platform claim from a tenant's scope is refused.
	if pl.Platform && p.TenantID != "" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "platform: true, but this pool is the tenant's; platform pools are an operator's without a tenant")
	}
	if marker, extra := pl.markerOnly(); marker && pl.Name != "" && (p.TenantID != "" || p.Operator) {
		if len(extra) > 0 {
			return nil, errf(http.StatusUnprocessableEntity, "invalid_pool",
				"without provider, a body only marks the default: name and isDefault, other fields absent or zero (got also %s); to change the pool's settings, give provider and all of them",
				strings.Join(extra, ", "))
		}
		return s.markDefaultPool(ctx, p.TenantID, pl.Name, *pl.IsDefault)
	}
	if p.TenantID == "" {
		return nil, errf(http.StatusBadRequest, "tenant_required", "an operator key must name a tenant: ?tenant=<id or name>")
	}
	if pl.Name == "" || (pl.Provider != "static" && pl.Provider != "ec2") {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "name and provider (static | ec2) are required")
	}
	if ValidPoolName(pl.Name) != nil {
		// A pool stored before the rule may keep its name.
		var owner *string
		if p.TenantID != "" {
			owner = &p.TenantID
		}
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return CheckPoolName(ctx, tx, owner, pl.Name) }); err != nil {
			if pne := (*PoolNameError)(nil); errors.As(err, &pne) {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
			}
			return nil, err
		}
	}
	if pl.Shared {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "only platform pools can be shared (luxd admin create-pool --shared)")
	}
	if pl.Provider == "ec2" {
		if raw, present := pl.Template["tags"]; present {
			if err := checkTemplateTags(raw); err != nil {
				return nil, err
			}
		}
		if raw, present := pl.Template["userData"]; present {
			ud, isString := raw.(string)
			if !isString {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.userData must be a string, got %T", raw)
			}
			if !hostboot.ValidUserData(ud) {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.userData %q: want ignition, script or env", ud)
			}
		}
		// A JSON boolean only: "true" or 1 would read as false at launch and
		// leave nested Runs waiting on hosts that never offer it.
		if raw, present := pl.Template["nestedContainers"]; present {
			if _, isBool := raw.(bool); !isBool {
				return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.nestedContainers must be true or false, got %T", raw)
			}
		}
	} else if _, present := pl.Template["nestedContainers"]; present {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool",
			"template.nestedContainers is for ec2 pools; a static host offers nested containers with lux-runner --nested")
	}
	if err := ValidPoolPrice(pl.Provider, pl.HourlyPrice, pl.Currency); err != nil {
		return nil, err
	}
	if pl.Template == nil {
		pl.Template = map[string]any{}
	}
	sda := scaleDownSeconds(pl.ScaleDownAfter.Duration)
	if pl.ScaleDownAfter.Duration < 0 || pl.ScaleDownAfter.Duration > 0 && sda == nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "scaleDownAfter must be at least 1s")
	}
	var out Pool
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		err := SavePool(ctx, tx, p.TenantID, pl.Name, pl.IsDefault, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, template, min_hosts, max_hosts, warm_hosts,
					scale_down_after_s, warm_while_active, hourly_price, price_currency)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, '')::numeric, nullif($12, ''))
				ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET provider = EXCLUDED.provider, template = EXCLUDED.template,
					min_hosts = EXCLUDED.min_hosts, max_hosts = EXCLUDED.max_hosts, warm_hosts = EXCLUDED.warm_hosts,
					scale_down_after_s = EXCLUDED.scale_down_after_s, warm_while_active = EXCLUDED.warm_while_active,
					hourly_price = EXCLUDED.hourly_price, price_currency = EXCLUDED.price_currency,
					`+PoolRevive,
				ids.New(ids.Pool), p.TenantID, pl.Name, pl.Provider, pl.Template, pl.MinHosts, pl.MaxHosts, pl.WarmHosts,
				sda, pl.WarmWhileActive, pl.HourlyPrice, pl.Currency)
			return err
		})
		if err != nil {
			return err
		}
		out, err = readPool(ctx, tx, p.TenantID, pl.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.Kick()
	return &poolBody{Body: poolInput(out)}, nil
}

// markDefaultPool marks (or clears) one of the tenant's pools as its
// default, or, tenantID "" (an operator), a platform pool as the
// platform's, and changes nothing else about it.
func (s *Server) markDefaultPool(ctx context.Context, tenantID, name string, mark bool) (*poolBody, error) {
	var out Pool
	scope := store.Tenant(tenantID)
	if tenantID == "" {
		scope = store.System()
	}
	err := s.db.Tx(ctx, scope, func(tx pgx.Tx) error {
		if err := SetDefaultPool(ctx, tx, tenantID, name, mark); err != nil {
			return err
		}
		var err error
		out, err = readPool(ctx, tx, tenantID, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &poolBody{Body: poolInput(out)}, nil
}

// lockDefaultPool serializes changes to an owner's default mark (tenantID
// "" for the platform's), taken before any pool row is touched: a second
// mark then reads the first one's result, and moves the mark rather than
// failing on pools_one_default.
func lockDefaultPool(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('default-pool:' || $1, 0))`, tenantID)
	return err
}

// PoolRevive ends a pool upsert's DO UPDATE SET: re-creating a retired
// pool brings it back without the default mark it had.
const PoolRevive = `is_default = pools.is_default AND NOT pools.retired, retired = false`

// SavePool runs upsert, which creates or replaces the pool name of
// tenantID's ("" a platform pool), and then sets its default mark unless
// mark is nil, in ChangePool, so the pool's event records both. A mark
// taken off another pool is that pool's config_changed event. Lock order:
// the owner's default lock (only with a mark) before ChangePool's
// pool-name lock and row, as in deletePool; then the other pool's row,
// in the marking statement; event streams last.
func SavePool(ctx context.Context, tx pgx.Tx, tenantID, name string, mark *bool, upsert func() error) error {
	if mark != nil {
		if err := lockDefaultPool(ctx, tx, tenantID); err != nil {
			return err
		}
	}
	var owner *string
	if tenantID != "" {
		owner = &tenantID
	}
	var unmarked []string
	err := ChangePool(ctx, tx, owner, name, func() error {
		if err := upsert(); err != nil || mark == nil {
			return err
		}
		var err error
		unmarked, err = markPools(ctx, tx, tenantID, name, *mark)
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range unmarked {
		if err := poolEvent(ctx, tx, id, evConfigChanged, map[string]any{"created": false,
			"changes": map[string]any{"isDefault": map[string]any{"old": true, "new": false}}}); err != nil {
			return err
		}
	}
	return nil
}

// SetDefaultPool marks the pool name as its owner's default (tenantID ""
// for the platform's), or clears it, changing nothing else: SavePool with
// no upsert.
func SetDefaultPool(ctx context.Context, tx pgx.Tx, tenantID, name string, mark bool) error {
	return SavePool(ctx, tx, tenantID, name, &mark, func() error { return nil })
}

// markPools sets name's mark, clearing the previous one in the same
// statement (pools_one_default is checked at its end), and returns the ids
// of the other pools it took a mark off. A retired pool, or none of that
// name, is a 404. The caller holds the owner's default lock.
func markPools(ctx context.Context, tx pgx.Tx, tenantID, name string, mark bool) ([]string, error) {
	const owner = `tenant_id IS NOT DISTINCT FROM nullif($1, '')`
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pools WHERE `+owner+` AND name = $2 AND NOT retired)`,
		tenantID, name).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errf(http.StatusNotFound, "not_found", "no pool %q", name)
	}
	if !mark {
		_, err := tx.Exec(ctx, `UPDATE pools SET is_default = false WHERE `+owner+` AND name = $2`, tenantID, name)
		return nil, err
	}
	// A retired pool is never marked, and loses a mark it kept (a removal
	// that raced a mark before both took the lock), which would otherwise
	// hold pools_one_default against every later mark.
	rows, err := tx.Query(ctx, `UPDATE pools SET is_default = (name = $2 AND NOT retired)
		WHERE `+owner+` AND (name = $2 OR is_default)
		RETURNING id, name <> $2 AND NOT is_default`, tenantID, name)
	if err != nil {
		return nil, err
	}
	var unmarked []string
	var id string
	var cleared bool
	_, err = pgx.ForEachRow(rows, []any{&id, &cleared}, func() error {
		if cleared {
			unmarked = append(unmarked, id)
		}
		return nil
	})
	return unmarked, err
}

func readPool(ctx context.Context, tx pgx.Tx, tenantID, name string) (Pool, error) {
	return scanPool(tx.QueryRow(ctx, `SELECT `+poolColumns+` FROM pools p LEFT JOIN tenants t ON t.id = p.tenant_id
		WHERE p.tenant_id IS NOT DISTINCT FROM nullif($1, '') AND p.name = $2`, tenantID, name))
}

type renamePoolInput struct {
	TenantQuery
	Name  string `path:"name" doc:"The pool's current name."`
	Owner string `query:"owner" enum:"platform" doc:"platform: the platform's pool of that name (operators)."`
	Body  struct {
		Name string `json:"name" doc:"The new name."`
	}
}

// renamePool is UPDATE pools SET name for one pool id; nothing refers to
// a pool by name, so nothing else changes.
func (s *Server) renamePool(ctx context.Context, in *renamePoolInput) (*poolBody, error) {
	p := principal(ctx)
	tenantID := p.TenantID
	if in.Owner == "platform" {
		if !p.Operator {
			return nil, errf(http.StatusForbidden, "forbidden", "platform pools are the operators'")
		}
		tenantID = ""
	}
	to := in.Body.Name
	if err := ValidPoolName(to); err != nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
	}
	var out Pool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// ChangePool's pool-name locks, both names in one order.
		for _, n := range slices.Sorted(slices.Values([]string{in.Name, to})) {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pool-name:' || $1 || '/' || $2, 0))`, tenantID, n); err != nil {
				return err
			}
		}
		var id string
		err := tx.QueryRow(ctx, `UPDATE pools SET name = $3
			WHERE coalesce(tenant_id, '') = $1 AND name = $2 AND NOT retired RETURNING id`, tenantID, in.Name, to).Scan(&id)
		var pe *pgconn.PgError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNotFound
		case errors.As(err, &pe) && pe.Code == "23505":
			return errf(http.StatusConflict, "pool_exists", "a pool named %s exists", to)
		case err != nil:
			return err
		}
		if err := poolEvent(ctx, tx, id, evRenamed, map[string]any{"from": in.Name, "to": to}); err != nil {
			return err
		}
		out, err = readPool(ctx, tx, tenantID, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &poolBody{Body: poolInput(out)}, nil
}
