package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Servers: named ports of a Run, each optionally with a command lux starts
// in its container (docs/concepts.md). A record (run_servers) outlives
// placements; its process lives only as long as the placement it was
// started in.
//
// luxd owns the state machine's edges it causes (start → starting, stop →
// stopped, a placement's end → stopped) and the runner reports the rest
// (ready, unreachable, exited) for the server's current gen. What a
// placement should run is sent as the whole desired set (MsgServers),
// durable and fenced by epoch, whenever it changes.

// Server states.
const (
	ServerStopped     = "stopped"
	ServerStarting    = "starting"
	ServerReady       = "ready"
	ServerUnreachable = "unreachable"
	ServerExited      = "exited"
)

// RunServer is a Run's server, as the API shows it.
type RunServer struct {
	Name          string            `json:"name"`
	Port          int               `json:"port"`
	Command       []string          `json:"command" nullable:"true" doc:"argv; null: only the port is exposed."`
	Workdir       string            `json:"workdir"`
	Env           map[string]string `json:"env"`
	FromSpec      bool              `json:"fromSpec" doc:"Declared in the spec's workload.servers (started on every start of the Run)."`
	State         string            `json:"state" enum:"stopped,starting,ready,unreachable,exited"`
	ExitCode      *int              `json:"exitCode,omitempty" doc:"When exited."`
	Error         *string           `json:"error,omitempty" doc:"When exited: the last line its command wrote to stderr, if any."`
	Since         time.Time         `json:"since" doc:"When its state last changed."`
	ReadySince    *time.Time        `json:"readySince" nullable:"true" doc:"When it last became ready (null unless ready)."`
	StopReason    *string           `json:"stopReason" nullable:"true" enum:"stopped,run stopped,migrated,host lost" doc:"Why it is stopped: stopped by request, or its placement ended."`
	StoppedEpoch  *int              `json:"stoppedEpoch" nullable:"true" doc:"The placement epoch it stopped in (null if it never ran)."`
	Epoch         *int              `json:"epoch" nullable:"true" doc:"The placement epoch its current state is of."`
	URL           *string           `json:"url" nullable:"true" doc:"Its preview URL; null when previews are not configured."`
	LastRequestAt *time.Time        `json:"lastRequestAt,omitempty" doc:"When its preview URL was last requested (updated at most every 30s)."`
	// gen: which start its state is of (internal).
	gen int64
}

const serverColumns = `name, port, command, workdir, env, from_spec, state, exit_code, error, since, ready_since,
	stop_reason, stopped_epoch, epoch, last_request_at, gen`

func (s *Server) scanServer(row pgx.Row, runID string) (RunServer, error) {
	var sv RunServer
	err := row.Scan(&sv.Name, &sv.Port, &sv.Command, &sv.Workdir, &sv.Env, &sv.FromSpec, &sv.State, &sv.ExitCode, &sv.Error,
		&sv.Since, &sv.ReadySince, &sv.StopReason, &sv.StoppedEpoch, &sv.Epoch, &sv.LastRequestAt, &sv.gen)
	if sv.Env == nil {
		sv.Env = map[string]string{}
	}
	sv.URL = s.previewURL(sv.Name, runID)
	return sv, err
}

// previewURL is a server's preview URL, nil without a preview domain.
func (s *Server) previewURL(name, runID string) *string {
	if s.cfg.Preview.Domain == "" {
		return nil
	}
	u := "https://" + name + "-" + strings.TrimPrefix(runID, "run_") + "." + s.cfg.Preview.Domain
	return &u
}

func (s *Server) listServersTx(ctx context.Context, tx pgx.Tx, runID string) ([]RunServer, error) {
	rows, err := tx.Query(ctx, `SELECT `+serverColumns+` FROM run_servers WHERE run_id = $1 ORDER BY created_at, name`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunServer, error) { return s.scanServer(row, runID) })
}

func (s *Server) getServerTx(ctx context.Context, tx pgx.Tx, runID, name string) (RunServer, error) {
	sv, err := s.scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` FROM run_servers WHERE run_id = $1 AND name = $2`, runID, name), runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sv, errf(http.StatusNotFound, "not_found", "the Run has no server %q", name)
	}
	return sv, err
}

// insertSpecServers creates the records of a spec's workload.servers, at
// submit.
func insertSpecServers(ctx context.Context, tx pgx.Tx, tenantID, runID string, sp spec.RunSpec) error {
	for _, sv := range sp.Workload.Servers {
		if _, err := tx.Exec(ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, command, workdir, env, from_spec)
			VALUES ($1, $2, $3, $4, $5, $6, $7, true)`, tenantID, runID, sv.Name, sv.Port, nilIfEmpty(sv.Command), sv.Workdir, nonNilMap(sv.Env)); err != nil {
			return err
		}
	}
	return nil
}

func nilIfEmpty(cmd []string) []string {
	if len(cmd) == 0 {
		return nil
	}
	return cmd
}

// setServerState moves servers to state and records a server.state event
// for each that changed. where selects them on run_servers (as rs, $1 is
// the Run's id; further args from $5), and the update sets what the state
// means: starting (a new start: gen, active config and epoch), stopped
// (stopReason, stoppedEpoch). Returns how many changed.
func setServerState(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, state, stopReason, where string, args ...any) (int, error) {
	rows, err := tx.Query(ctx, `UPDATE run_servers rs SET
			state = $2,
			since = now(),
			gen = nextval('run_servers_gen'),
			exit_code = NULL, error = NULL, ready_since = NULL,
			active = CASE WHEN $2 = 'starting' THEN jsonb_build_object('port', port, 'command', command, 'workdir', workdir, 'env', env) END,
			epoch = CASE WHEN $2 = 'starting' THEN nullif($3, 0) ELSE epoch END,
			stop_reason = CASE WHEN $2 = 'stopped' THEN $4 END,
			-- The placement it stopped in: one already stopped keeps its,
			-- unless now stopped by request (its watching ends in this one).
			stopped_epoch = CASE WHEN $2 = 'stopped' AND (rs.state <> 'stopped' OR $4 = 'stopped')
				THEN coalesce(nullif($3, 0), rs.epoch) ELSE stopped_epoch END
		WHERE rs.run_id = $1 AND (`+where+`)
		RETURNING name`, append([]any{runID, state, epoch, stopReason}, args...)...)
	if err != nil {
		return 0, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, n := range names {
		if err := addEvent(ctx, tx, tenantID, runID, epoch, "server.state", map[string]any{"name": n, "state": state, "epoch": epoch}); err != nil {
			return 0, err
		}
	}
	return len(names), nil
}

// stopServersAtEnd marks every server of a placement that ended stopped:
// its processes went with it. why is the stop reason (run stopped,
// migrated, host lost).
func stopServersAtEnd(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, why string) error {
	_, err := setServerState(ctx, tx, tenantID, runID, epoch, ServerStopped, why, `rs.state <> 'stopped'`)
	return err
}

// endReason is the stopReason servers get when their placement ended with
// a placement stop reason (placements.stop_reason).
func endReason(placementStop string) string {
	if slices.Contains(movedStops, placementStop) {
		return "migrated"
	}
	return "run stopped"
}

// startSpecServers starts the spec's servers on a new placement, and
// sends the placement its servers.
// A Run without servers is sent nothing.
func (s *Server) startSpecServers(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int) error {
	if _, err := setServerState(ctx, tx, tenantID, runID, epoch, ServerStarting, "", `rs.from_spec`); err != nil {
		return err
	}
	var any bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_servers WHERE run_id = $1)`, runID).Scan(&any); err != nil || !any {
		return err
	}
	_, err := syncServersTx(ctx, tx, runID)
	return err
}

// syncServersTx sends a Run's live placement the servers it should run
// now, if it has one (in a system scope: host messages are not a
// tenant's). Returns the host to notify after commit.
func syncServersTx(ctx context.Context, tx pgx.Tx, runID string) (string, error) {
	var hostID string
	var epoch int
	var rev int64
	err := tx.QueryRow(ctx, `UPDATE runs r SET servers_rev = servers_rev + 1
		FROM placements p WHERE r.id = $1 AND p.run_id = r.id AND p.epoch = r.current_epoch AND p.state IN `+livePlacementStates+`
		RETURNING p.host_id, p.epoch, r.servers_rev`, runID).Scan(&hostID, &epoch, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	msg, err := desiredServers(ctx, tx, runID, epoch, rev)
	if err != nil {
		return "", err
	}
	return hostID, enqueue(ctx, tx, hostID, runID, epoch, proto.MsgServers, msg)
}

// desiredServers is what placement epoch should run: every server started
// (and not since stopped or exited), as it was started; every server
// without a command, to watch, unless stopped by request in this
// placement; and the ports of all.
func desiredServers(ctx context.Context, tx pgx.Tx, runID string, epoch int, rev int64) (proto.Servers, error) {
	msg := proto.Servers{Rev: rev, Servers: []proto.ServerSpec{}}
	var workdir string
	if err := tx.QueryRow(ctx, `SELECT coalesce(spec->'workload'->>'workdir', '') FROM runs WHERE id = $1`, runID).Scan(&workdir); err != nil {
		return msg, err
	}
	rows, err := tx.Query(ctx, `SELECT name, port, command IS NULL, state, `+watched("$2")+`, gen, active
		FROM run_servers WHERE run_id = $1 ORDER BY name`, runID, epoch)
	if err != nil {
		return msg, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, state string
		var port int
		var portOnly, watched bool
		var gen int64
		var active *struct {
			Port    int               `json:"port"`
			Command []string          `json:"command"`
			Workdir string            `json:"workdir"`
			Env     map[string]string `json:"env"`
		}
		if err := rows.Scan(&name, &port, &portOnly, &state, &watched, &gen, &active); err != nil {
			return msg, err
		}
		msg.Ports = append(msg.Ports, port)
		if portOnly && state == ServerStopped && watched {
			// Its port opening (someone started it by hand) makes it ready.
			msg.Servers = append(msg.Servers, proto.ServerSpec{Name: name, Port: port, Gen: gen})
			continue
		}
		if active == nil || (state != ServerStarting && state != ServerReady && state != ServerUnreachable) {
			continue
		}
		if active.Port != port {
			msg.Ports = append(msg.Ports, active.Port)
		}
		msg.Servers = append(msg.Servers, proto.ServerSpec{Name: name, Port: active.Port, Gen: gen, Command: active.Command,
			Workdir: spec.ServerWorkdir(workdir, active.Workdir), Env: active.Env})
	}
	slices.Sort(msg.Ports)
	msg.Ports = slices.Compact(msg.Ports)
	return msg, rows.Err()
}

// watched, for SQL on run_servers with the placement's epoch as the
// parameter epoch: a stopped server without a command is watched unless it
// was stopped by request in this placement.
func watched(epoch string) string {
	return `NOT (coalesce(stop_reason, '') = 'stopped' AND stopped_epoch IS NOT DISTINCT FROM ` + epoch + `)`
}

// syncServers is syncServersTx in its own system transaction, notifying
// the host after.
func (s *Server) syncServers(ctx context.Context, runID string) error {
	var hostID string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		hostID, err = syncServersTx(ctx, tx, runID)
		return err
	})
	if hostID != "" {
		s.hub.Notify(hostID)
	}
	return err
}

// applyServerState records a runner's report of a server's state, for the
// server's current gen only (a report of an earlier start is stale).
func applyServerState(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, data map[string]any) error {
	name, _ := data["name"].(string)
	state, _ := data["state"].(string)
	gen, _ := data["gen"].(float64)
	switch state {
	case ServerStarting, ServerReady, ServerUnreachable, ServerExited:
	default:
		return nil
	}
	var code *int
	if c, ok := data["exitCode"].(float64); ok && state == ServerExited {
		n := int(c)
		code = &n
	}
	var msg *string
	if e, ok := data["error"].(string); ok && e != "" && state == ServerExited {
		e = truncate(e, 1000)
		msg = &e
	}
	// A watched server without a command becomes ready when its port
	// opens, whatever it was.
	tag, err := tx.Exec(ctx, `UPDATE run_servers SET state = $4, since = now(), epoch = $5,
			ready_since = CASE WHEN $4 = 'ready' THEN now() END,
			exit_code = $6, error = $7, stop_reason = NULL,
			active = CASE WHEN $4 = 'exited' THEN NULL
				WHEN active IS NULL THEN jsonb_build_object('port', port) ELSE active END
		WHERE run_id = $1 AND name = $2 AND gen = $3 AND state <> $4
		  AND (state IN ('starting', 'ready', 'unreachable')
		       OR ($4 = 'ready' AND command IS NULL AND state = 'stopped' AND `+watched("$5")+`))`,
		runID, name, int64(gen), state, epoch, code, msg)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	ev := map[string]any{"name": name, "state": state, "epoch": epoch}
	if code != nil {
		ev["exitCode"] = *code
	}
	if msg != nil {
		ev["error"] = *msg
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, "server.state", ev)
}

// ---- the API ---------------------------------------------------------------

type serverListOutput struct {
	Body struct {
		Servers []RunServer `json:"servers"`
	} `nameHint:"ServerList"`
}

func (s *Server) listServers(ctx context.Context, in *RunPath) (*serverListOutput, error) {
	out := &serverListOutput{}
	err := s.db.Tx(ctx, store.Tenant(principal(ctx).TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		var err error
		out.Body.Servers, err = s.listServersTx(ctx, tx, in.ID)
		return err
	})
	return out, err
}

// ServerInput is a server to add, or (without name) its new definition.
type ServerInput struct {
	Name    string            `json:"name,omitempty" doc:"Unique within the Run: 1-30 of a-z, 0-9 and -, starting with a letter, not ending in -. (POST only.)"`
	Port    int               `json:"port" doc:"The port it listens on in the container."`
	Command []string          `json:"command,omitempty" nullable:"true" doc:"argv, run as the workload's user with its environment. None: only the port is exposed."`
	Workdir string            `json:"workdir,omitempty" doc:"Where the command runs; relative: against the workload's workdir."`
	Env     map[string]string `json:"env,omitempty" doc:"More environment for its command (not secret)."`
	Start   *bool             `json:"start,omitempty" doc:"POST: start it now (the Run must be running). Default: true when it has a command."`
}

type addServerInput struct {
	RunPath
	Body ServerInput
}

type serverOutput struct {
	Status int
	Body   RunServer
}

// checkServer validates a server against its Run's spec.
func checkServer(sp spec.RunSpec, sv spec.Server) error {
	if problems := sp.ValidateServer("server", sv); len(problems) > 0 {
		he := errf(http.StatusUnprocessableEntity, "invalid_server", "%s", strings.Join(problems, "; "))
		he.Details = problems
		return he
	}
	return nil
}

// serverRun locks a Run for a change to its servers: its state, current
// epoch and spec. 409 once it has finished.
func serverRun(ctx context.Context, tx pgx.Tx, runID string) (state string, epoch int, sp spec.RunSpec, err error) {
	err = tx.QueryRow(ctx, `SELECT state, current_epoch, spec FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&state, &epoch, &sp)
	if err == nil && terminal(state) {
		err = errf(http.StatusConflict, "finished", "run is %s", state)
	}
	return state, epoch, sp, err
}

func (s *Server) addServer(ctx context.Context, in *addServerInput) (*serverOutput, error) {
	p := principal(ctx)
	b := in.Body
	sv := spec.Server{Name: b.Name, Port: b.Port, Command: b.Command, Workdir: b.Workdir, Env: b.Env}
	start := len(b.Command) > 0
	if b.Start != nil {
		if *b.Start && len(b.Command) == 0 {
			return nil, errNoCommand(b.Name)
		}
		start = *b.Start
	}
	var out RunServer
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		state, epoch, sp, err := serverRun(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if err := checkServer(sp, sv); err != nil {
			return err
		}
		if start && state != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: a server starts only in a running Run (add it with start: false)", state)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO run_servers (tenant_id, run_id, name, port, command, workdir, env)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (run_id, name) DO NOTHING`,
			p.TenantID, in.ID, sv.Name, sv.Port, nilIfEmpty(sv.Command), sv.Workdir, nonNilMap(sv.Env))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(http.StatusConflict, "name_taken", "the Run already has a server %q", sv.Name)
		}
		if err := addEvent(ctx, tx, p.TenantID, in.ID, 0, "server.added", map[string]any{"name": sv.Name, "by": p.Actor()}); err != nil {
			return err
		}
		if start {
			if _, err := setServerState(ctx, tx, p.TenantID, in.ID, epoch, ServerStarting, "", `rs.name = $5`, sv.Name); err != nil {
				return err
			}
		}
		out, err = s.getServerTx(ctx, tx, in.ID, sv.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Started or not, its port is one a tunnel may now reach.
	if err := s.syncServers(ctx, in.ID); err != nil {
		return nil, err
	}
	return &serverOutput{http.StatusCreated, out}, nil
}

// ServerPath names a Run's server.
type ServerPath struct {
	RunPath
	Name string `path:"name" doc:"The server's name."`
}

type putServerInput struct {
	ServerPath
	Body ServerInput
}

func (s *Server) putServer(ctx context.Context, in *putServerInput) (*serverOutput, error) {
	p := principal(ctx)
	b := in.Body
	if b.Name != "" && b.Name != in.Name {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_server", "a server cannot be renamed: remove it and add another")
	}
	if b.Start != nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_server", "start is for POST: use POST .../start or .../restart")
	}
	var out RunServer
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		_, _, sp, err := serverRun(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if err := checkServer(sp, spec.Server{Name: in.Name, Port: b.Port, Command: b.Command, Workdir: b.Workdir, Env: b.Env}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE run_servers SET port = $3, command = $4, workdir = $5, env = $6 WHERE run_id = $1 AND name = $2`,
			in.ID, in.Name, b.Port, nilIfEmpty(b.Command), b.Workdir, nonNilMap(b.Env))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(http.StatusNotFound, "not_found", "the Run has no server %q", in.Name)
		}
		out, err = s.getServerTx(ctx, tx, in.ID, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Its port may be one a tunnel may now reach.
	if err := s.syncServers(ctx, in.ID); err != nil {
		return nil, err
	}
	return &serverOutput{http.StatusOK, out}, nil
}

// serverAction is start, stop or restart.
func (s *Server) serverAction(action string) func(context.Context, *ServerPath) (*serverOutput, error) {
	return func(ctx context.Context, in *ServerPath) (*serverOutput, error) {
		p := principal(ctx)
		var out RunServer
		err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
			state, epoch, _, err := serverRun(ctx, tx, in.ID)
			if err != nil {
				return err
			}
			sv, err := s.getServerTx(ctx, tx, in.ID, in.Name)
			if err != nil {
				return err
			}
			if action == "stop" {
				// Stopped by request, it is no longer watched either.
				if sv.State != ServerStopped || sv.StopReason == nil || *sv.StopReason != "stopped" {
					if _, err := setServerState(ctx, tx, p.TenantID, in.ID, epoch, ServerStopped, "stopped", `rs.name = $5`, in.Name); err != nil {
						return err
					}
				}
			} else {
				if len(sv.Command) == 0 {
					return errNoCommand(in.Name)
				}
				if state != StateRunning {
					return errf(http.StatusConflict, "not_running", "run is %s: a server starts only in a running Run", state)
				}
				running := sv.State == ServerStarting || sv.State == ServerReady || sv.State == ServerUnreachable
				// start is idempotent while it runs; restart restarts.
				if action == "restart" || !running {
					if _, err := setServerState(ctx, tx, p.TenantID, in.ID, epoch, ServerStarting, "", `rs.name = $5`, in.Name); err != nil {
						return err
					}
				}
			}
			out, err = s.getServerTx(ctx, tx, in.ID, in.Name)
			return err
		})
		if err != nil {
			return nil, err
		}
		if err := s.syncServers(ctx, in.ID); err != nil {
			return nil, err
		}
		return &serverOutput{http.StatusOK, out}, nil
	}
}

// errNoCommand: lux runs nothing for a server without a command; its port
// is watched whenever the Run runs.
func errNoCommand(name string) error {
	return errf(http.StatusConflict, "no_command", "server %q has no command: lux starts nothing for it, and watches its port while the Run runs", name)
}

type noContent struct {
	Status int
}

func (s *Server) removeServer(ctx context.Context, in *ServerPath) (*noContent, error) {
	p := principal(ctx)
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if _, _, _, err := serverRun(ctx, tx, in.ID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM run_servers WHERE run_id = $1 AND name = $2`, in.ID, in.Name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errf(http.StatusNotFound, "not_found", "the Run has no server %q", in.Name)
		}
		return addEvent(ctx, tx, p.TenantID, in.ID, 0, "server.removed", map[string]any{"name": in.Name, "by": p.Actor()})
	})
	if err != nil {
		return nil, err
	}
	// Its process, if any, stops with the next set.
	if err := s.syncServers(ctx, in.ID); err != nil {
		return nil, err
	}
	return &noContent{http.StatusNoContent}, nil
}

// ---- its log -------------------------------------------------------------

// ServerLogLine is one line a server's command wrote.
type ServerLogLine struct {
	Time   int64  `json:"t" doc:"Unix milliseconds."`
	Stream string `json:"stream" enum:"stdout,stderr"`
	Text   string `json:"text"`
}

type serverLogInput struct {
	ServerPath
	Tail int `query:"tail" doc:"The last this many lines, 1 to 5000 (default 200)."`
}

// ServerLog is a server's recent output.
type ServerLog struct {
	Lines []ServerLogLine `json:"lines"`
}

type serverLogOutput struct {
	Body ServerLog
}

// serverLog is a server's recent output, newest last: from its current
// placement (relayed from its host) and earlier ones (their uploaded
// output), newest placement first until there are enough lines. A
// placement whose output cannot be had (lost with its host, or not
// uploaded yet from a host that is not connected) is skipped.
func (s *Server) serverLog(ctx context.Context, in *serverLogInput) (*serverLogOutput, error) {
	p := principal(ctx)
	tail := in.Tail
	if tail <= 0 {
		tail = 200
	}
	tail = min(tail, 5000)
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		_, err := s.getServerTx(ctx, tx, in.ID, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	placements, _, err := s.placementsFrom(ctx, p.TenantID, in.ID, 0)
	if err != nil {
		return nil, err
	}
	var lines []ServerLogLine
	for i := len(placements) - 1; i >= 0 && len(lines) < tail; i-- {
		pl := placements[i]
		var got []ServerLogLine
		collect := func(rec proto.Record) error {
			if rec.Ch != "server" || rec.Server != in.Name || rec.Data == "" {
				return nil
			}
			for _, l := range strings.Split(strings.TrimSuffix(rec.Data, "\n"), "\n") {
				got = append(got, ServerLogLine{Time: rec.Time, Stream: rec.Stream, Text: strings.TrimSuffix(l, "\r")})
			}
			if len(got) > 2*tail {
				got = got[len(got)-tail:]
			}
			return nil
		}
		switch {
		case pl.blobLoc == "s3":
			err = s.streamOutputBlob(ctx, pl.blobKey, 0, collect)
		case s.hub.Streaming(pl.hostID) && pl.state != "lost":
			_, err = s.relayOutput(ctx, pl, 0, false, collect, func() error { return nil })
		default:
			continue
		}
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lines = append(got, lines...)
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	out := &serverLogOutput{}
	out.Body.Lines = nonNil(lines)
	return out, nil
}

// serverPort is the port a stream to a Run's server goes to: the one it
// was started with, else its own. ok is false if it has no such server.
func serverPort(ctx context.Context, tx pgx.Tx, runID, name string) (int, bool, error) {
	var port int
	var active json.RawMessage
	err := tx.QueryRow(ctx, `SELECT port, active FROM run_servers WHERE run_id = $1 AND name = $2`, runID, name).Scan(&port, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var a struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(active, &a) == nil && a.Port > 0 {
		port = a.Port
	}
	return port, true, nil
}
