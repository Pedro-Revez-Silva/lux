package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Servers: named URLs that reach a port in a Run, each optionally with a
// command lux starts in its container (docs/concepts.md). A server is a
// tenant's own resource (run_servers, its id srv_…): it outlives the Runs
// that serve it, and is attached to at most one Run at a time (run_id).
// Its process lives only as long as the placement it was started in.
//
// luxd owns the state machine's edges it causes (start → starting, stop →
// stopped, a placement's end → stopped) and the runner reports the rest
// (ready, unreachable, exited) for the server's current gen. What a
// placement should run is sent as the whole desired set (MsgServers),
// durable and fenced by epoch, whenever it changes.
//
// Desired state is not stored apart: a server is down when it was stopped
// by request (state stopped, stop_reason stopped) and up otherwise; every
// new placement of its Run starts every attached server that is up.

// Server process states.
const (
	ServerStopped     = "stopped"
	ServerStarting    = "starting"
	ServerReady       = "ready"
	ServerUnreachable = "unreachable"
	ServerExited      = "exited"
)

// A server's state as /v1/servers shows it (derived: serverRow.derive).
const (
	SrvReady       = "ready"
	SrvWaking      = "waking"
	SrvAsleep      = "asleep"
	SrvStopped     = "stopped"
	SrvUnreachable = "unreachable"
	SrvExited      = "exited"
	SrvNoAnswer    = "no answer"
)

// Lifetimes and wake modes.
const (
	LifetimeRun   = "run"
	LifetimeOwner = "owner"
	WakeRequest   = "request"
	WakeNever     = "never"
)

// Defaults of a server's timings.
const (
	DefaultIdleAfter   = 10 * time.Minute
	DefaultWakeTimeout = 5 * time.Minute
	DefaultExpireAfter = 30 * 24 * time.Hour
)

// serverUp: a server in a state its process (or port) is up in, or on
// its way up.
func serverUp(state string) bool {
	return state == ServerStarting || state == ServerReady || state == ServerUnreachable
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
	Lifetime      string            `json:"lifetime" enum:"run,owner" doc:"run: deleted when its Run finishes for good (succeeded, cancelled). owner: kept until its owner deletes it."`
	Labels        map[string]string `json:"labels"`
	LastRequestAt *time.Time        `json:"lastRequestAt,omitempty" doc:"When its preview URL was last requested (written at most every preview.activity_every)."`
}

// serverRow is a run_servers row with what its Run says about it.
type serverRow struct {
	ID, TenantID, Name, Host string
	Port                     int
	Command                  []string
	Workdir                  string
	Env, Labels              map[string]string
	FromSpec                 bool
	State                    string
	ExitCode                 *int
	Error                    *string
	Since                    time.Time
	ReadySince               *time.Time
	StopReason               *string
	StoppedEpoch, Epoch      *int
	LastRequestAt            *time.Time
	RunID                    *string
	Wake, Lifetime, Owner    string
	IdleAfterS, WakeTimeoutS int
	ExpireAfterS             *int
	AfterSync                []string
	WakeRequestedAt          *time.Time
	WakeBy, WakePath         *string
	Wakes                    int
	IdleNotifiedAt           *time.Time
	CreatedAt, UpdatedAt     time.Time
	RunState, RunName        string
	PlacementStop            string
	Moving                   bool
}

const serverSelect = `SELECT sv.id, sv.tenant_id, sv.name, sv.host, sv.port, sv.command, sv.workdir, sv.env, sv.labels, sv.from_spec,
	sv.state, sv.exit_code, sv.error, sv.since, sv.ready_since, sv.stop_reason, sv.stopped_epoch, sv.epoch, sv.last_request_at,
	sv.run_id, sv.wake, sv.lifetime, sv.owner, sv.idle_after_s, sv.wake_timeout_s, sv.expire_after_s, sv.after_sync,
	sv.wake_requested_at, sv.wake_by, sv.wake_path, sv.wakes, sv.idle_notified_at, sv.created_at, sv.updated_at,
	coalesce(r.state, ''), coalesce(r.name, ''), coalesce(p.stop_reason, '')
	FROM run_servers sv LEFT JOIN runs r ON r.id = sv.run_id
	LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch `

func scanServerRow(row pgx.Row) (serverRow, error) {
	var v serverRow
	err := row.Scan(&v.ID, &v.TenantID, &v.Name, &v.Host, &v.Port, &v.Command, &v.Workdir, &v.Env, &v.Labels, &v.FromSpec,
		&v.State, &v.ExitCode, &v.Error, &v.Since, &v.ReadySince, &v.StopReason, &v.StoppedEpoch, &v.Epoch, &v.LastRequestAt,
		&v.RunID, &v.Wake, &v.Lifetime, &v.Owner, &v.IdleAfterS, &v.WakeTimeoutS, &v.ExpireAfterS, &v.AfterSync,
		&v.WakeRequestedAt, &v.WakeBy, &v.WakePath, &v.Wakes, &v.IdleNotifiedAt, &v.CreatedAt, &v.UpdatedAt,
		&v.RunState, &v.RunName, &v.PlacementStop)
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	// Moving: its placement is stopping to move, or its Run is on its way
	// to the next one after a move.
	v.Moving = (v.RunState == StateStopping && slices.Contains(movedStops, v.PlacementStop)) ||
		(v.StopReason != nil && *v.StopReason == "migrated" && slices.Contains(startingRunStates, v.RunState))
	return v, err
}

func collectServerRows(rows pgx.Rows, err error) ([]serverRow, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (serverRow, error) { return scanServerRow(row) })
}

// down: stopped by request; a new placement leaves it stopped.
func (v serverRow) down() bool {
	return v.State == ServerStopped && v.StopReason != nil && *v.StopReason == "stopped"
}

func (v serverRow) wakeOpen(now time.Time) bool {
	return v.WakeRequestedAt != nil && now.Before(v.WakeRequestedAt.Add(time.Duration(v.WakeTimeoutS)*time.Second))
}

// derive is the server's state as /v1/servers shows it, from its process
// state, its Run's and its open wake.
func (v serverRow) derive(now time.Time) string {
	running := v.RunState == StateRunning
	switch {
	case running && v.State == ServerReady:
		return SrvReady
	case running && v.State == ServerUnreachable:
		return SrvUnreachable
	case running && v.State == ServerExited:
		return SrvExited
	case v.down():
		return SrvStopped
	case running && v.State == ServerStarting:
		return SrvWaking
	case v.RunID != nil && (slices.Contains(startingRunStates, v.RunState) || v.Moving):
		return SrvWaking
	case running && v.State == ServerStopped && v.Command == nil:
		// Port only: nothing to start; lux watches the port while the Run runs.
		return SrvWaking
	case running:
		return SrvStopped
	case v.Wake == WakeRequest && v.wakeOpen(now):
		return SrvWaking
	case v.Wake == WakeRequest && v.WakeRequestedAt != nil:
		return SrvNoAnswer
	case v.Wake == WakeRequest:
		return SrvAsleep
	}
	return SrvStopped
}

// idleAt is when a ready server goes idle without another request (nil:
// it is not ready, or never goes idle).
func (v serverRow) idleAt() *time.Time {
	if v.RunState != StateRunning || v.State != ServerReady || v.IdleAfterS <= 0 || v.ReadySince == nil {
		return nil
	}
	base := *v.ReadySince
	if v.LastRequestAt != nil && v.LastRequestAt.After(base) {
		base = *v.LastRequestAt
	}
	t := base.Add(time.Duration(v.IdleAfterS) * time.Second)
	return &t
}

// previewHostname is a server's full preview host name ("" without previews).
func (s *Server) previewHostname(host string) string {
	if s.cfg.Preview.Domain == "" {
		return ""
	}
	return host + "." + s.cfg.Preview.Domain
}

// previewURL is a server's preview URL, nil without a preview domain.
func (s *Server) previewURL(host string) *string {
	h := s.previewHostname(host)
	if h == "" {
		return nil
	}
	u := s.previewOrigin(h)
	return &u
}

// previewOrigin is scheme://hostname[:port] of a preview host.
func (s *Server) previewOrigin(hostname string) string {
	scheme := cmp.Or(s.cfg.Preview.Scheme, "https")
	if p := s.cfg.Preview.PublicPort; p != 0 {
		return fmt.Sprintf("%s://%s:%d", scheme, hostname, p)
	}
	return scheme + "://" + hostname
}

func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func (s *Server) runServer(v serverRow) RunServer {
	return RunServer{ID: v.ID, Name: v.Name, Port: v.Port, Command: v.Command, Workdir: v.Workdir, Env: v.Env, FromSpec: v.FromSpec,
		State: v.State, ExitCode: v.ExitCode, Error: v.Error, Since: v.Since, ReadySince: v.ReadySince, StopReason: v.StopReason,
		StoppedEpoch: v.StoppedEpoch, Epoch: v.Epoch, Hostname: optString(s.previewHostname(v.Host)), URL: s.previewURL(v.Host),
		Wake: v.Wake, Lifetime: v.Lifetime, Labels: v.Labels, LastRequestAt: v.LastRequestAt}
}

func (s *Server) listServersTx(ctx context.Context, tx pgx.Tx, runID string) ([]RunServer, error) {
	rows, err := collectServerRows(tx.Query(ctx, serverSelect+`WHERE sv.run_id = $1 ORDER BY sv.created_at, sv.name`, runID))
	out := make([]RunServer, 0, len(rows))
	for _, v := range rows {
		out = append(out, s.runServer(v))
	}
	return out, err
}

func (s *Server) getServerTx(ctx context.Context, tx pgx.Tx, runID, name string) (RunServer, error) {
	v, err := serverByName(ctx, tx, runID, name)
	if err != nil {
		return RunServer{}, err
	}
	return s.runServer(v), nil
}

func serverByName(ctx context.Context, tx pgx.Tx, runID, name string) (serverRow, error) {
	v, err := scanServerRow(tx.QueryRow(ctx, serverSelect+`WHERE sv.run_id = $1 AND sv.name = $2`, runID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, errNoServer(name)
	}
	return v, err
}

// insertSpecServers creates the records of a spec's workload.servers, at
// submit: attached to the Run, ending with it.
func insertSpecServers(ctx context.Context, tx pgx.Tx, tenantID, runID, owner string, sp spec.RunSpec) error {
	for _, sv := range sp.Workload.Servers {
		if _, err := tx.Exec(ctx, `INSERT INTO run_servers (id, tenant_id, run_id, name, port, command, workdir, env, from_spec, owner, after_sync)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, $10)`, ids.New(ids.Server), tenantID, runID, sv.Name, sv.Port,
			nilIfEmpty(sv.Command), sv.Workdir, nonNilMap(sv.Env), owner, nilIfEmpty(sv.AfterSync)); err != nil {
			return uniqueViolation(err)
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

// ---- server events -------------------------------------------------------

// serverEvent records an event of a server: on the feed and its page, and
// on its attached Run's events (run_id), if any. Every one carries the
// server's id, name, host and labels and its Run's id; readers add the
// full hostname and URL (eventDetail).
func serverEvent(ctx context.Context, tx pgx.Tx, tenantID string, runID *string, serverID string, epoch int, typ string, ref srvRef, data map[string]any) error {
	d := map[string]any{"serverId": serverID, "name": ref.name, "host": ref.host, "labels": nonNilMap(ref.labels), "runId": runID}
	maps.Copy(d, data)
	var ep *int
	if epoch > 0 {
		ep = &epoch
	}
	_, err := tx.Exec(ctx, `INSERT INTO run_events (tenant_id, run_id, server_id, epoch, type, data) VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, runID, serverID, ep, typ, d)
	return err
}

// srvRef is what a server event names a server by.
type srvRef struct {
	name, host string
	labels     map[string]string
}

func (v serverRow) ref() srvRef { return srvRef{v.Name, v.Host, v.Labels} }

// eventDetail adds what a reader's configuration says to a server event:
// its full hostname and URL.
func (s *Server) eventDetail(e *Event) {
	if e.ServerID == "" {
		return
	}
	host, _ := e.Data["host"].(string)
	if host != "" {
		if h := s.previewHostname(host); h != "" {
			e.Data["hostname"] = h
			e.Data["url"] = s.previewOrigin(h)
		}
	}
}

// ---- process state ---------------------------------------------------------

// stateChange is a change setServerState makes to a Run's servers.
type stateChange struct {
	tenantID, runID string
	epoch           int
	state           string
	// stopReason: when stopping, why.
	stopReason string
	// afterSync: when starting after a sync, run each server's afterSync
	// before its command.
	afterSync bool
}

// setServerState moves servers to a state and records a server.state
// event for each that changed. where selects them on run_servers (as rs,
// $1 is the Run's id; further args from $6), and the update sets what the
// state means: starting (a new start: gen, active config and epoch),
// stopped (stopReason, stoppedEpoch).
func setServerState(ctx context.Context, tx pgx.Tx, c stateChange, where string, args ...any) error {
	rows, err := tx.Query(ctx, `UPDATE run_servers rs SET
			state = $2,
			since = now(),
			gen = nextval('run_servers_gen'),
			exit_code = NULL, error = NULL, ready_since = NULL,
			active = CASE WHEN $2 = 'starting' THEN jsonb_build_object('port', port, 'command', command, 'workdir', workdir, 'env', env,
				'afterSync', CASE WHEN $5 THEN after_sync END) END,
			epoch = CASE WHEN $2 = 'starting' THEN nullif($3, 0) ELSE epoch END,
			stop_reason = CASE WHEN $2 = 'stopped' THEN $4 END,
			-- The placement it stopped in: one already stopped keeps its,
			-- unless now stopped by request (its watching ends in this one).
			stopped_epoch = CASE WHEN $2 = 'stopped' AND (rs.state <> 'stopped' OR $4 = 'stopped')
				THEN coalesce(nullif($3, 0), rs.epoch) ELSE stopped_epoch END
		WHERE rs.run_id = $1 AND (`+where+`)
		RETURNING id, name, host, labels`, append([]any{c.runID, c.state, c.epoch, c.stopReason, c.afterSync}, args...)...)
	if err != nil {
		return err
	}
	type changed struct {
		id  string
		ref srvRef
	}
	var done []changed
	for rows.Next() {
		var ch changed
		if err := rows.Scan(&ch.id, &ch.ref.name, &ch.ref.host, &ch.ref.labels); err != nil {
			rows.Close()
			return err
		}
		done = append(done, ch)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ch := range done {
		d := map[string]any{"state": c.state, "epoch": c.epoch}
		if c.state == ServerStopped && c.stopReason != "" {
			d["stopReason"] = c.stopReason
		}
		if err := serverEvent(ctx, tx, c.tenantID, &c.runID, ch.id, c.epoch, "server.state", ch.ref, d); err != nil {
			return err
		}
	}
	return nil
}

// stopServersAtEnd marks every server of a placement that ended stopped:
// its processes went with it. why is the stop reason (run stopped,
// migrated, host lost).
func stopServersAtEnd(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, why string) error {
	return setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStopped, stopReason: why}, `rs.state <> 'stopped'`)
}

// endReason is the stopReason servers get when their placement ended with
// a placement stop reason (placements.stop_reason).
func endReason(placementStop string) string {
	if slices.Contains(movedStops, placementStop) {
		return "migrated"
	}
	return "run stopped"
}

// upWithCommand, for SQL on run_servers (as rs): a server lux starts on a
// new placement: one with a command that was not stopped by request.
const upWithCommand = `rs.command IS NOT NULL AND NOT (rs.state = 'stopped' AND coalesce(rs.stop_reason, '') = 'stopped')`

// startAttachedServers starts a Run's servers on a new placement (every
// placement: a first start, a resume, a migration, a resume after a lost
// host), and sends the placement its servers. afterSync: the placement
// syncs repositories first, so servers run their afterSync. A Run without
// servers is sent nothing.
func (s *Server) startAttachedServers(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, afterSync bool) error {
	if err := setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStarting, afterSync: afterSync}, upWithCommand); err != nil {
		return err
	}
	var any bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_servers WHERE run_id = $1)`, runID).Scan(&any); err != nil || !any {
		return err
	}
	_, err := syncServersTx(ctx, tx, runID)
	return err
}

// endServers ends a Run's servers with the Run, once it can never run
// again (succeeded, cancelled): lifetime run servers are deleted, owner
// ones detached. A failed Run can be resumed: its servers stay.
func endServers(ctx context.Context, tx pgx.Tx, tenantID, runID, state string) error {
	if state != StateSucceeded && state != StateCancelled {
		return nil
	}
	rows, err := collectServerRows(tx.Query(ctx, serverSelect+`WHERE sv.run_id = $1 FOR UPDATE OF sv`, runID))
	if err != nil {
		return err
	}
	for _, v := range rows {
		why := map[string]any{"reason": "run " + state}
		if v.Lifetime == LifetimeRun {
			if _, err := tx.Exec(ctx, `DELETE FROM run_servers WHERE id = $1`, v.ID); err != nil {
				return err
			}
			if err := serverEvent(ctx, tx, tenantID, &runID, v.ID, 0, "server.deleted", v.ref(), why); err != nil {
				return err
			}
			continue
		}
		// Stopped first (its process ends with the placement), then
		// detached: never a detached server with a live process state.
		if err := detachTx(ctx, tx, tenantID, v, "run "+state, "lux"); err != nil {
			return err
		}
	}
	return nil
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
			Port      int               `json:"port"`
			Command   []string          `json:"command"`
			Workdir   string            `json:"workdir"`
			Env       map[string]string `json:"env"`
			AfterSync []string          `json:"afterSync"`
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
		if active == nil || !serverUp(state) {
			continue
		}
		if active.Port != port {
			msg.Ports = append(msg.Ports, active.Port)
		}
		msg.Servers = append(msg.Servers, proto.ServerSpec{Name: name, Port: active.Port, Gen: gen,
			Command: withAfterSync(active.AfterSync, active.Command),
			Workdir: spec.ServerWorkdir(workdir, active.Workdir), Env: active.Env})
	}
	slices.Sort(msg.Ports)
	msg.Ports = slices.Compact(msg.Ports)
	return msg, rows.Err()
}

// withAfterSync is command preceded by afterSync: one sh -c that runs
// afterSync and, if it succeeds, execs the command (whose exit is the
// server's either way).
func withAfterSync(afterSync, command []string) []string {
	if len(afterSync) == 0 || len(command) == 0 {
		return command
	}
	return []string{"/bin/sh", "-c", shellJoin(afterSync) + " && exec " + shellJoin(command)}
}

// shellJoin quotes argv for sh: each word in single quotes.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// watched, for SQL on run_servers with the placement's epoch as the
// parameter epoch: a stopped server without a command is watched unless it
// was stopped by request in this placement.
func watched(epoch string) string {
	return `NOT (coalesce(stop_reason, '') = 'stopped' AND stopped_epoch IS NOT DISTINCT FROM ` + epoch + `)`
}

// changeServers runs fn, a change to a Run's servers, and sends the Run's
// live placement its new set in the same transaction, so a change is never
// committed without it; the host is notified after the commit. The
// transaction is system-scoped (host messages are not a tenant's), so the
// Run is first checked to be tenantID's: fn sees every tenant's rows, and
// must touch only runID's.
func (s *Server) changeServers(ctx context.Context, tenantID, runID string, fn func(pgx.Tx) error) error {
	var hostID string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var mine bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1 AND tenant_id = $2)`, runID, tenantID).Scan(&mine); err != nil {
			return err
		}
		if !mine {
			return errNotFound
		}
		if err := fn(tx); err != nil {
			return err
		}
		var err error
		hostID, err = syncServersTx(ctx, tx, runID)
		return err
	})
	if err == nil && hostID != "" {
		s.hub.Notify(hostID)
	}
	return err
}

// applyServerState records a runner's report of a server's state, for the
// server's current gen only (a report of an earlier start is stale). A
// server that becomes ready resolves its open wake.
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
	var id string
	var ref srvRef
	err := tx.QueryRow(ctx, `UPDATE run_servers SET state = $4, since = now(), epoch = $5,
			ready_since = CASE WHEN $4 = 'ready' THEN now() END,
			exit_code = $6, error = $7, stop_reason = NULL,
			active = CASE WHEN $4 = 'exited' THEN NULL
				WHEN active IS NULL THEN jsonb_build_object('port', port) ELSE active END,
			wake_requested_at = CASE WHEN $4 = 'ready' THEN NULL ELSE wake_requested_at END,
			idle_notified_at = CASE WHEN $4 = 'ready' THEN NULL ELSE idle_notified_at END
		WHERE run_id = $1 AND name = $2 AND gen = $3 AND state <> $4
		  AND (state IN ('starting', 'ready', 'unreachable')
		       OR ($4 = 'ready' AND command IS NULL AND state = 'stopped' AND `+watched("$5")+`))
		RETURNING id, name, host, labels`,
		runID, name, int64(gen), state, epoch, code, msg).Scan(&id, &ref.name, &ref.host, &ref.labels)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ev := map[string]any{"state": state, "epoch": epoch}
	if code != nil {
		ev["exitCode"] = *code
	}
	if msg != nil {
		ev["error"] = *msg
	}
	return serverEvent(ctx, tx, tenantID, &runID, id, epoch, "server.state", ref, ev)
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

// ServerInput is a server to add to a Run, or (without name) its new
// definition.
type ServerInput struct {
	Name      string            `json:"name,omitempty" doc:"Unique within the Run: 1-30 of a-z, 0-9 and -, starting with a letter, not ending in -. (POST only.)"`
	Port      int               `json:"port" doc:"The port it listens on in the container."`
	Command   []string          `json:"command,omitempty" nullable:"true" doc:"argv, run as the workload's user with its environment. None: only the port is exposed."`
	Workdir   string            `json:"workdir,omitempty" doc:"Where the command runs; relative: against the workload's workdir."`
	Env       map[string]string `json:"env,omitempty" doc:"More environment for its command (not secret)."`
	AfterSync []string          `json:"afterSync,omitempty" doc:"argv run before the command when it starts after a repository sync."`
	Start     *bool             `json:"start,omitempty" doc:"POST: start it now (the Run must be running). Default: true when it has a command."`
	Lifetime  string            `json:"lifetime,omitempty" enum:"run,owner" doc:"POST: run (default): deleted when the Run finishes for good; owner: kept, detached, until deleted (DELETE /v1/servers/{id})."`
	Labels    map[string]string `json:"labels,omitempty" doc:"POST: free-form labels (GET /v1/servers?label=k=v)."`
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
	sv := spec.Server{Name: b.Name, Port: b.Port, Command: b.Command, Workdir: b.Workdir, Env: b.Env, AfterSync: b.AfterSync}
	lifetime := cmp.Or(b.Lifetime, LifetimeRun)
	if lifetime != LifetimeRun && lifetime != LifetimeOwner {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_server", "lifetime: want run or owner")
	}
	if err := checkLabels(b.Labels); err != nil {
		return nil, err
	}
	start := len(b.Command) > 0
	if b.Start != nil {
		if *b.Start && len(b.Command) == 0 {
			return nil, errNoCommand(b.Name)
		}
		start = *b.Start
	}
	var out RunServer
	// Started or not, its port is one a tunnel may now reach.
	err := s.changeServers(ctx, p.TenantID, in.ID, func(tx pgx.Tx) error {
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
		id := ids.New(ids.Server)
		tag, err := tx.Exec(ctx, `INSERT INTO run_servers (id, tenant_id, run_id, name, port, command, workdir, env, after_sync, lifetime, labels, owner)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (run_id, name) DO NOTHING`,
			id, p.TenantID, in.ID, sv.Name, sv.Port, nilIfEmpty(sv.Command), sv.Workdir, nonNilMap(sv.Env), nilIfEmpty(sv.AfterSync),
			lifetime, nonNilMap(b.Labels), p.Actor())
		if err != nil {
			return uniqueViolation(err)
		}
		if tag.RowsAffected() == 0 {
			return errf(http.StatusConflict, "name_taken", "the Run already has a server %q", sv.Name)
		}
		v, err := serverByName(ctx, tx, in.ID, sv.Name)
		if err != nil {
			return err
		}
		// server.added (the Run API's name for it) is the Run's event and
		// the server's.
		if err := serverEvent(ctx, tx, p.TenantID, &in.ID, id, 0, "server.added", v.ref(), map[string]any{"by": p.Actor(), "lifetime": lifetime}); err != nil {
			return err
		}
		if start {
			if err := setServerState(ctx, tx, stateChange{tenantID: p.TenantID, runID: in.ID, epoch: epoch, state: ServerStarting}, `rs.name = $6`, sv.Name); err != nil {
				return err
			}
		}
		out, err = s.getServerTx(ctx, tx, in.ID, sv.Name)
		return err
	})
	if err != nil {
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
	// Its port may be one a tunnel may now reach.
	err := s.changeServers(ctx, p.TenantID, in.ID, func(tx pgx.Tx) error {
		_, _, sp, err := serverRun(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if b.Lifetime != "" || b.Labels != nil {
			return errf(http.StatusUnprocessableEntity, "invalid_server", "lifetime and labels are changed with PATCH /v1/servers/{id}")
		}
		if err := checkServer(sp, spec.Server{Name: in.Name, Port: b.Port, Command: b.Command, Workdir: b.Workdir, Env: b.Env, AfterSync: b.AfterSync}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE run_servers SET port = $3, command = $4, workdir = $5, env = $6, after_sync = $7, updated_at = now()
			WHERE run_id = $1 AND name = $2`,
			in.ID, in.Name, b.Port, nilIfEmpty(b.Command), b.Workdir, nonNilMap(b.Env), nilIfEmpty(b.AfterSync))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoServer(in.Name)
		}
		out, err = s.getServerTx(ctx, tx, in.ID, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &serverOutput{http.StatusOK, out}, nil
}

// serverAction is start, stop or restart.
func (s *Server) serverAction(action string) func(context.Context, *ServerPath) (*serverOutput, error) {
	return func(ctx context.Context, in *ServerPath) (*serverOutput, error) {
		p := principal(ctx)
		var out RunServer
		err := s.changeServers(ctx, p.TenantID, in.ID, func(tx pgx.Tx) error {
			state, epoch, _, err := serverRun(ctx, tx, in.ID)
			if err != nil {
				return err
			}
			sv, err := s.getServerTx(ctx, tx, in.ID, in.Name)
			if err != nil {
				return err
			}
			if err := serverActionTx(ctx, tx, p.TenantID, in.ID, state, epoch, sv, action); err != nil {
				return err
			}
			out, err = s.getServerTx(ctx, tx, in.ID, in.Name)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &serverOutput{http.StatusOK, out}, nil
	}
}

// serverActionTx starts, stops or restarts a Run's server sv, the Run
// locked (state, epoch). Stopped by request, a server stays down: not
// watched, and not started by a new placement, until started again.
func serverActionTx(ctx context.Context, tx pgx.Tx, tenantID, runID, state string, epoch int, sv RunServer, action string) error {
	if action == "stop" {
		if sv.State != ServerStopped || sv.StopReason == nil || *sv.StopReason != "stopped" {
			return setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStopped, stopReason: "stopped"}, `rs.id = $6`, sv.ID)
		}
		return nil
	}
	if len(sv.Command) == 0 {
		return errNoCommand(sv.Name)
	}
	if state != StateRunning {
		return errf(http.StatusConflict, "not_running", "run is %s: a server starts only in a running Run", state)
	}
	// start is idempotent while it runs; restart restarts.
	if action == "restart" || !serverUp(sv.State) {
		return setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStarting}, `rs.id = $6`, sv.ID)
	}
	return nil
}

// errNoServer is the not_found of a server the Run does not have.
func errNoServer(name string) error {
	return errf(http.StatusNotFound, "not_found", "the Run has no server %q", name)
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
	// Its process, if any, stops with the next set.
	err := s.changeServers(ctx, p.TenantID, in.ID, func(tx pgx.Tx) error {
		if _, _, _, err := serverRun(ctx, tx, in.ID); err != nil {
			return err
		}
		v, err := serverByName(ctx, tx, in.ID, in.Name)
		if err != nil {
			return err
		}
		if v.Lifetime == LifetimeOwner {
			return errf(http.StatusConflict, "lifetime_owner", "server %s (%s) is its owner's, not the Run's: detach it (POST /v1/servers/%s/detach) or delete it (DELETE /v1/servers/%s)",
				in.Name, v.ID, v.ID, v.ID)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM run_servers WHERE id = $1`, v.ID); err != nil {
			return err
		}
		return serverEvent(ctx, tx, p.TenantID, &in.ID, v.ID, 0, "server.removed", v.ref(), map[string]any{"by": p.Actor()})
	})
	if err != nil {
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
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		_, err := s.getServerTx(ctx, tx, in.ID, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.serverLogOf(ctx, p.TenantID, in.ID, in.Name, in.Tail)
}

// serverLogOf is the last tail lines (1 to 5000, 200 when 0 or less) of a
// server's output in a Run.
func (s *Server) serverLogOf(ctx context.Context, tenantID, runID, name string, tail int) (*serverLogOutput, error) {
	if tail <= 0 {
		tail = 200
	}
	tail = min(tail, 5000)
	placements, _, err := s.placementsFrom(ctx, tenantID, runID, 0)
	if err != nil {
		return nil, err
	}
	var lines []ServerLogLine
	for i := len(placements) - 1; i >= 0 && len(lines) < tail; i-- {
		pl := placements[i]
		var got []ServerLogLine
		collect := func(rec proto.Record) error {
			if rec.Ch != "server" || rec.Server != name || rec.Data == "" {
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

// restartAfterSync is a running Run's sync.done: when a checkout moved,
// its servers with afterSync restart, running it first; the others keep
// running (a dev server reloads by itself). Returns the host to notify.
func restartAfterSync(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, data map[string]any) (string, error) {
	if changed, _ := data["changed"].(bool); !changed {
		return "", nil
	}
	var current int
	var state string
	if err := tx.QueryRow(ctx, `SELECT state, current_epoch FROM runs WHERE id = $1`, runID).Scan(&state, &current); err != nil {
		return "", err
	}
	if current != epoch || state != StateRunning {
		return "", nil
	}
	if err := setServerState(ctx, tx, stateChange{tenantID: tenantID, runID: runID, epoch: epoch, state: ServerStarting, afterSync: true},
		upWithCommand+` AND rs.after_sync IS NOT NULL`); err != nil {
		return "", err
	}
	return syncServersTx(ctx, tx, runID)
}
