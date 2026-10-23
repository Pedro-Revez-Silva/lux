package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Hub holds the runner connections this luxd has.
//
// Control messages are durable (host_messages): luxd writes one, the
// connection that owns the host sends it, and it stays pending until the
// runner acks it. A runner that reconnects gets everything unacked again.
// Streams (output subscriptions, exec, attach, tunnels) are not durable:
// they are frames on the live connection, routed by id.
type Hub struct {
	s     *Server
	mu    sync.Mutex
	conns map[string]*runnerConn // by host id
	subs  map[string]chan proto.Frame
	// polls: hosts on the HTTP polling fallback, by last poll.
	polls map[string]time.Time
}

func newHub(s *Server) *Hub {
	return &Hub{s: s, conns: map[string]*runnerConn{}, subs: map[string]chan proto.Frame{}, polls: map[string]time.Time{}}
}

type runnerConn struct {
	hostID string
	ws     *websocket.Conn
	send   chan proto.Frame
	notify chan struct{}
	done   chan struct{}
	// caps: the optional features its runner said it has (proto.CapDiff).
	caps []string
}

func (h *Hub) conn(hostID string) *runnerConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[hostID]
}

// Reachable reports whether a host's runner talks to this luxd, over a
// WebSocket or by polling recently: it can be given work.
func (h *Hub) Reachable(hostID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[hostID] != nil {
		return true
	}
	return time.Since(h.polls[hostID]) < 10*time.Second
}

// Streaming reports whether a host has a WebSocket here: live output and
// interactive streams need one; a polling host cannot relay them.
func (h *Hub) Streaming(hostID string) bool { return h.conn(hostID) != nil }

// Disconnect closes a host's WebSocket, if it has one here (the host was
// terminated).
func (h *Hub) Disconnect(hostID string) {
	if c := h.conn(hostID); c != nil {
		c.ws.Close(websocket.StatusPolicyViolation, "host terminated")
	}
}

// Gone is closed when the host's current WebSocket ends (nil if it has
// none): a stream relayed over it ends with it, since the runner forgets
// its streams when its connection drops.
func (h *Hub) Gone(hostID string) <-chan struct{} {
	if c := h.conn(hostID); c != nil {
		return c.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

func (h *Hub) polled(hostID string) {
	h.mu.Lock()
	h.polls[hostID] = time.Now()
	h.mu.Unlock()
}

// Notify wakes the sender for a host after a message was queued.
func (h *Hub) Notify(hostID string) {
	if c := h.conn(hostID); c != nil {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

// SendLive sends a non-durable frame (stream control) to a connected runner.
func (h *Hub) SendLive(hostID string, f proto.Frame) error {
	c := h.conn(hostID)
	if c == nil {
		return errors.New("host is not connected")
	}
	select {
	case c.send <- f:
		return nil
	case <-c.done:
		return errors.New("host disconnected")
	case <-time.After(10 * time.Second):
		return errors.New("host connection is congested")
	}
}

// Subscribe routes frames carrying id (subId/streamId) to the returned
// channel until cancel is called.
func (h *Hub) Subscribe(id string) (<-chan proto.Frame, func()) {
	ch := make(chan proto.Frame, 256)
	h.mu.Lock()
	h.subs[id] = ch
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

func (h *Hub) route(id string, f proto.Frame) {
	h.mu.Lock()
	ch := h.subs[id]
	h.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- f:
	default:
		// Never block: this runs on the runner's read loop, and a stall here
		// delays every ack and report from that host. A client that cannot
		// keep up loses its stream (it reconnects from its cursor).
		h.mu.Lock()
		if h.subs[id] == ch {
			delete(h.subs, id)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.ws.Close(websocket.StatusGoingAway, "luxd shutting down")
	}
}

// deliveryLoop re-checks pending messages periodically, so messages queued
// by another luxd instance (or before a notify was possible) go out.
func (h *Hub) deliveryLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			for _, c := range h.conns {
				select {
				case c.notify <- struct{}{}:
				default:
				}
			}
			h.mu.Unlock()
		}
	}
}

// serveRunnerWS is GET /runner/v1/ws.
func (s *Server) serveRunnerWS(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return nil
	}
	ws.SetReadLimit(64 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The first frame must be hello.
	var first proto.Frame
	if err := readFrame(ctx, ws, &first); err != nil || first.Type != proto.MsgHello {
		ws.Close(websocket.StatusPolicyViolation, "expected hello")
		return nil
	}
	var hello proto.Hello
	_ = json.Unmarshal(first.Data, &hello)
	welcome, err := s.registerHost(ctx, tok, hello)
	if err != nil {
		ws.Close(websocket.StatusPolicyViolation, truncate(err.Error(), 120))
		return nil
	}
	// Everything unacked goes out again on this connection.
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = $1 AND acked_at IS NULL`, welcome.HostID)
		return err
	}); err != nil {
		ws.Close(websocket.StatusInternalError, "luxd error")
		return nil
	}
	c := &runnerConn{
		hostID: welcome.HostID,
		ws:     ws,
		send:   make(chan proto.Frame, 256),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
		caps:   hello.Capabilities,
	}
	if err := writeFrame(ctx, ws, proto.Frame{Type: proto.MsgWelcome, Data: proto.Marshal(welcome)}); err != nil {
		return nil
	}

	s.hub.mu.Lock()
	if old := s.hub.conns[c.hostID]; old != nil {
		old.ws.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	s.hub.conns[c.hostID] = c
	s.hub.mu.Unlock()
	s.log.Info("runner connected", "host", c.hostID, "name", hello.Labels["name"])
	defer func() {
		s.hub.mu.Lock()
		if s.hub.conns[c.hostID] == c {
			delete(s.hub.conns, c.hostID)
		}
		s.hub.mu.Unlock()
		close(c.done)
		s.log.Info("runner disconnected", "host", c.hostID)
	}()

	// Writer: live frames and durable messages.
	go func() {
		defer cancel()
		c.notify <- struct{}{}
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-c.send:
				if err := writeFrame(ctx, ws, f); err != nil {
					return
				}
			case <-c.notify:
				msgs, err := s.pendingMessages(ctx, c.hostID, true)
				if err != nil {
					s.log.Warn("pending messages", "err", err)
					continue
				}
				for _, m := range msgs {
					if err := writeFrame(ctx, ws, m); err != nil {
						return
					}
				}
			}
		}
	}()

	for {
		var f proto.Frame
		if err := readFrame(ctx, ws, &f); err != nil {
			return nil
		}
		switch f.Type {
		case proto.MsgAck:
			if err := s.ackMessage(ctx, c.hostID, f.ID); err != nil {
				s.log.Warn("ack", "err", err)
			}
		case proto.MsgStreamData, proto.MsgStreamClose:
			s.hub.route(f.Stream, f)
		case proto.MsgOutputRecords, proto.MsgOutputEnd, proto.MsgDiffResult:
			var ref struct {
				SubID string `json:"subId"`
			}
			_ = json.Unmarshal(f.Data, &ref)
			s.hub.route(ref.SubID, f)
		default:
			reply := s.handleReport(ctx, c.hostID, f)
			select {
			case c.send <- reply:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func readFrame(ctx context.Context, ws *websocket.Conn, f *proto.Frame) error {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, f)
}

func writeFrame(ctx context.Context, ws *websocket.Conn, f proto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

// pendingMessages returns a host's unacked messages in id order, marking
// them delivered, with secrets attached to assignments at send time (they
// are never stored). With undelivered, only messages not yet sent on the
// current connection: delivered_at is cleared whenever a runner connects,
// so it means "sent on this connection" and the query itself skips what was
// sent (whatever order ids committed in). The poll fallback has no
// connection and asks for every unacked message.
func (s *Server) pendingMessages(ctx context.Context, hostID string, undelivered bool) ([]proto.Frame, error) {
	if err := s.refuseUndeliverable(ctx, hostID); err != nil {
		return nil, err
	}
	var out []proto.Frame
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE host_messages SET delivered_at = now()
			WHERE id IN (SELECT id FROM host_messages
				WHERE host_id = $1 AND acked_at IS NULL AND (NOT $2 OR delivered_at IS NULL)
				  AND NOT (`+unsupportedSync+`)
				ORDER BY id LIMIT 500)
			RETURNING id, type, coalesce(run_id, ''), coalesce(epoch, 0), payload`, hostID, undelivered)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f proto.Frame
			if err := rows.Scan(&f.ID, &f.Type, &f.RunID, &f.Epoch, &f.Data); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// RETURNING does not preserve the subquery's order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i := range out {
		switch out[i].Type {
		case proto.MsgAssign:
			out[i].Data = s.attachSecrets(out[i].RunID, out[i].Data)
		case proto.MsgPush, proto.MsgSync:
			out[i].Data = s.attachCredentials(out[i].ID, out[i].Type, out[i].Data)
		}
	}
	return out, nil
}

// unsupportedSync, for SQL on host_messages: a sync or assignment that asks
// for a mode other than move (proto.SafeSyncModes), queued for a host whose
// latest registration lacks proto.CapSyncModes. An older runner drops the
// mode and moves the checkout: such a message is never delivered.
const unsupportedSync = `host_messages.type IN ('sync', 'assign')
	AND jsonb_path_exists(host_messages.payload, CASE host_messages.type
		WHEN 'sync' THEN '$.repos[*] ? (@.mode != "" && @.mode != "move")'
		ELSE '$.sync[*] ? (@.mode != "" && @.mode != "move")' END::jsonpath)
	AND NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id = host_messages.host_id AND 'sync-modes' = ANY(h.capabilities))`

// syncModesGone is the outcome of a sync refused at delivery.
const syncModesGone = "the host's lux-runner no longer supports sync modes"

// credentialedMsg is an unacked push or sync naming credentials, and
// whether its host's latest registration has proto.CapGitCredentials.
type credentialedMsg struct {
	id      int64
	runID   string
	capable bool
}

// refuseUndeliverable settles, in place of delivering them, a host's
// messages that must not reach its runner: unsupportedSync ones
// (refuseSync) and credentialed ones it cannot deliver as asked
// (refuseUncredentialed). Both are found in one transaction, since this
// runs on every delivery pass; each is settled in its own.
func (s *Server) refuseUndeliverable(ctx context.Context, hostID string) error {
	type refused struct {
		id    int64
		runID string
	}
	var syncs, assigns []refused
	var creds []credentialedMsg
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Assignments to a host whose runner lacks managed images: which of
		// them need it is settled one by one (refuseUnmanaged).
		rows, err := tx.Query(ctx, `SELECT id, coalesce(run_id, '') FROM host_messages
			WHERE host_id = $1 AND acked_at IS NULL AND `+unmanagedAssign+` ORDER BY id`, hostID)
		if err != nil {
			return err
		}
		assigns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (refused, error) {
			var m refused
			return m, r.Scan(&m.id, &m.runID)
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT id, coalesce(run_id, '') FROM host_messages
			WHERE host_id = $1 AND acked_at IS NULL AND `+unsupportedSync+` ORDER BY id`, hostID)
		if err != nil {
			return err
		}
		syncs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (refused, error) {
			var m refused
			return m, r.Scan(&m.id, &m.runID)
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT m.id, coalesce(m.run_id, ''),
				EXISTS (SELECT 1 FROM hosts h WHERE h.id = m.host_id AND $4 = ANY(h.capabilities))
			FROM host_messages m
			WHERE m.host_id = $1 AND m.acked_at IS NULL AND m.type IN ($2, $3) AND jsonb_array_length(coalesce(m.payload->'credentialRepos', '[]')) > 0
			ORDER BY m.id`, hostID, proto.MsgPush, proto.MsgSync, proto.CapGitCredentials)
		if err != nil {
			return err
		}
		creds, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (credentialedMsg, error) {
			var m credentialedMsg
			return m, r.Scan(&m.id, &m.runID, &m.capable)
		})
		return err
	})
	if err != nil {
		return err
	}
	for _, m := range syncs {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.refuseSync(ctx, tx, hostID, m.id, m.runID) }); err != nil {
			return err
		}
	}
	requeued := len(syncs) > 0
	for _, m := range assigns {
		var did bool
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) (err error) {
			did, err = s.refuseUnmanaged(ctx, tx, hostID, m.id, m.runID)
			return err
		}); err != nil {
			return err
		}
		requeued = requeued || did
	}
	if requeued {
		s.Kick()
	}
	return s.refuseUncredentialed(ctx, hostID, creds)
}

// unmanagedAssign, for SQL on host_messages: an assignment queued for a
// host whose latest registration lacks proto.CapManagedImages (a runner
// replaced by an older one since it was placed there).
const unmanagedAssign = `host_messages.type = 'assign'
	AND NOT EXISTS (SELECT 1 FROM hosts h WHERE h.id = host_messages.host_id AND '` + proto.CapManagedImages + `' = ANY(h.capabilities))`

// managedImagesGone is why a placement is given up at delivery.
const managedImagesGone = "the host's lux-runner no longer supports managed images"

// refuseUnmanaged settles an unmanagedAssign message whose Run needs a
// runner with managed images (hostFit's test, on the Run's stored spec
// and resolution and luxd's managed registry now) in place of delivering
// it, and acks it: an older runner would build without the context, not
// publish, or pull without the credential. The current, unstarted
// placement is given up and its Run queued again, so that hostFit waits
// for a runner that has them; an older epoch's message is only acked, and
// never ends a later placement. A message whose Run needs nothing new is
// left to be delivered. requeued: a Run was queued again.
func (s *Server) refuseUnmanaged(ctx context.Context, tx pgx.Tx, hostID string, id int64, runID string) (requeued bool, err error) {
	// The Run first, then its message: as a report's transaction locks them.
	var tenantID string
	var current int
	var sp spec.RunSpec
	var res *proto.ImageResolution
	if err := tx.QueryRow(ctx, `SELECT tenant_id, current_epoch, spec, image_resolved FROM runs WHERE id = $1 FOR UPDATE`, runID).
		Scan(&tenantID, &current, &sp, &res); err != nil {
		return false, err
	}
	var epoch int
	var payload json.RawMessage
	err = tx.QueryRow(ctx, `SELECT coalesce(epoch, 0), payload FROM host_messages
		WHERE id = $1 AND acked_at IS NULL AND `+unmanagedAssign+` FOR UPDATE`, id).Scan(&epoch, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // acked, or the host has managed images again
	}
	if err != nil {
		return false, err
	}
	r := pendingRun{Spec: sp, ImageResolved: res}
	s.markManagedAuth(&r)
	if !r.ManagedAuth && !needsManagedImagesRunner(sp) {
		return false, nil
	}
	s.log.Warn("assignment refused at delivery: no managed images", "host", hostID, "run", runID, "epoch", epoch)
	var later laterEvents
	var assigned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM placements WHERE run_id = $1 AND epoch = $2 AND host_id = $3 AND state = 'assigned')`,
		runID, epoch, hostID).Scan(&assigned); err != nil {
		return false, err
	}
	if assigned && epoch == current {
		var a proto.Assign
		if err := json.Unmarshal(payload, &a); err != nil {
			return false, err
		}
		if err := s.requeueUnstartedPlacement(ctx, tx, tenantID, runID, epoch, managedImagesGone, &later, a); err != nil {
			return false, err
		}
		requeued = true
	}
	if _, err := tx.Exec(ctx, `UPDATE host_messages SET acked_at = now(), payload = payload - 'promptAttachments' WHERE id = $1`, id); err != nil {
		return false, err
	}
	return requeued, later.write()
}

// refuseSync settles an unsupportedSync message in place of delivering it,
// and acks it. A running Run's sync ends failed: a failed git.sync per
// repository, then sync.done. An assignment's placement, not yet started,
// is lost and its Run queued again with its sync, so that hostFit waits
// for a host with sync modes.
func (s *Server) refuseSync(ctx context.Context, tx pgx.Tx, hostID string, id int64, runID string) error {
	// The Run first, then its message: as a report's transaction locks them.
	var tenantID string
	var current int
	if err := tx.QueryRow(ctx, `SELECT tenant_id, current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&tenantID, &current); err != nil {
		return err
	}
	var typ string
	var epoch int
	var payload json.RawMessage
	err := tx.QueryRow(ctx, `SELECT type, coalesce(epoch, 0), payload FROM host_messages
		WHERE id = $1 AND acked_at IS NULL AND `+unsupportedSync+` FOR UPDATE`, id).Scan(&typ, &epoch, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // acked, or the host has sync modes again
	}
	if err != nil {
		return err
	}
	s.log.Warn("sync refused at delivery: no sync modes", "host", hostID, "run", runID, "epoch", epoch, "type", typ)
	var later laterEvents
	switch typ {
	case proto.MsgSync:
		var m proto.Sync
		if err := json.Unmarshal(payload, &m); err != nil {
			return err
		}
		for _, r := range m.Repos {
			if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvGitSync, map[string]any{"repo": r.Repo, "ref": r.Ref,
				"mode": proto.SyncModeOf(r.Mode), "status": "failed", "error": syncModesGone, "requestId": m.RequestID}); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvSyncDone, map[string]any{"requestId": m.RequestID, "changed": false}); err != nil {
			return err
		}
	case proto.MsgAssign:
		var a proto.Assign
		if err := json.Unmarshal(payload, &a); err != nil {
			return err
		}
		var assigned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM placements WHERE run_id = $1 AND epoch = $2 AND host_id = $3 AND state = 'assigned')`,
			runID, epoch, hostID).Scan(&assigned); err != nil {
			return err
		}
		if assigned && epoch == current {
			if err := s.requeueUnstartedPlacement(ctx, tx, tenantID, runID, epoch, syncModesGone, &later, a); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE host_messages SET acked_at = now(),
			payload = CASE type WHEN $2 THEN payload - 'promptAttachments' ELSE payload END
		WHERE id = $1`, id, proto.MsgAssign); err != nil {
		return err
	}
	return later.write()
}

// attachCredentials adds to a push or sync the git credentials luxd holds
// for message id: at send time, never stored.
func (s *Server) attachCredentials(id int64, typ string, data json.RawMessage) json.RawMessage {
	creds, ok := s.secrets.getMsg(id)
	if !ok {
		return data
	}
	switch typ {
	case proto.MsgPush:
		var m proto.Push
		if json.Unmarshal(data, &m) != nil {
			return data
		}
		m.Credentials = creds
		return proto.Marshal(m)
	default:
		var m proto.Sync
		if json.Unmarshal(data, &m) != nil {
			return data
		}
		m.Credentials = creds
		return proto.Marshal(m)
	}
}

// credentialsGone is the outcome of a push or sync refused at delivery
// because this luxd does not hold its credentials.
const credentialsGone = "luxd no longer holds this request's credentials (it restarted, or the runner moved to another luxd): send it again"

// refuseUncredentialed settles, in place of delivering, every message of
// found that names credentials (credentialRepos) this luxd does not hold,
// or whose host re-registered without proto.CapGitCredentials: sent
// without them, or to a runner that drops them, it would use the
// credential they were to replace. A push ends with a git.push event,
// every repository failed; a sync with a failed git.sync per repository,
// then sync.done.
func (s *Server) refuseUncredentialed(ctx context.Context, hostID string, found []credentialedMsg) error {
	for _, m := range found {
		why := credentialsGone
		if _, held := s.secrets.getMsg(m.id); held {
			if m.capable {
				continue
			}
			why = credentialsUnsupported
		}
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.refuseUncredentialedMsg(ctx, tx, hostID, m.id, m.runID, why)
		}); err != nil {
			return err
		}
		s.secrets.dropMsg(m.id)
	}
	return nil
}

// credentialsUnsupported is the outcome of a push or sync with credentials
// refused at delivery because its host's runner no longer takes them.
const credentialsUnsupported = "the host's lux-runner no longer takes credentials on a push or sync"

func (s *Server) refuseUncredentialedMsg(ctx context.Context, tx pgx.Tx, hostID string, id int64, runID, why string) error {
	// The Run first, then its message: as a report's transaction locks them.
	var tenantID string
	var sp spec.RunSpec
	if err := tx.QueryRow(ctx, `SELECT tenant_id, spec FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&tenantID, &sp); err != nil {
		return err
	}
	var typ string
	var epoch int
	var payload json.RawMessage
	err := tx.QueryRow(ctx, `SELECT type, coalesce(epoch, 0), payload FROM host_messages
		WHERE id = $1 AND acked_at IS NULL FOR UPDATE`, id).Scan(&typ, &epoch, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.log.Warn("push or sync refused at delivery: credentials not held", "host", hostID, "run", runID, "epoch", epoch, "type", typ)
	switch typ {
	case proto.MsgPush:
		var m proto.Push
		if err := json.Unmarshal(payload, &m); err != nil {
			return err
		}
		var results []proto.PushResult
		if sp.Git != nil && sp.Git.Push != nil {
			for _, r := range sp.Git.Repositories {
				res := proto.PushResult{Repo: r.Name, Branch: sp.Git.Push.Branch, Status: "failed", Error: why}
				if !r.Pushed() {
					res = proto.PushResult{Repo: r.Name, Status: "skipped"}
				}
				results = append(results, res)
			}
		}
		if err := addEvent(ctx, tx, tenantID, runID, epoch, "git.push", map[string]any{"requestId": m.RequestID, "results": results}); err != nil {
			return err
		}
	case proto.MsgSync:
		var m proto.Sync
		if err := json.Unmarshal(payload, &m); err != nil {
			return err
		}
		for _, r := range m.Repos {
			if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvGitSync, map[string]any{"repo": r.Repo, "ref": r.Ref,
				"mode": proto.SyncModeOf(r.Mode), "status": "failed", "error": why, "requestId": m.RequestID}); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvSyncDone, map[string]any{"requestId": m.RequestID, "changed": false}); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE host_messages SET acked_at = now() WHERE id = $1`, id)
	return err
}

func (s *Server) ackMessage(ctx context.Context, hostID string, id int64) error {
	var acked string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var typ, runID string
		var epoch int
		// An assignment's Run first, then the message: the order refuseSync
		// and reports lock them in.
		if err := tx.QueryRow(ctx, `SELECT 1 FROM runs WHERE id = (SELECT run_id FROM host_messages
				WHERE id = $1 AND host_id = $2 AND type = $3 AND acked_at IS NULL) FOR UPDATE`, id, hostID, proto.MsgAssign).Scan(new(int)); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// An acked message is never read again (redelivery reads acked_at
		// IS NULL only): its image bytes go with the ack.
		err := tx.QueryRow(ctx, `UPDATE host_messages SET acked_at = now(),
				payload = CASE type WHEN $3 THEN payload - 'attachments'
					WHEN $4 THEN payload - 'promptAttachments'
					ELSE payload END
			WHERE id = $1 AND host_id = $2 AND acked_at IS NULL
			RETURNING type, coalesce(run_id, ''), coalesce(epoch, 0)`, id, hostID, proto.MsgInput, proto.MsgAssign).Scan(&typ, &runID, &epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		acked = typ
		if typ == proto.MsgAssign {
			if _, err = tx.Exec(ctx, `UPDATE placements SET accepted_at = coalesce(accepted_at, now())
				WHERE run_id = $1 AND epoch = $2`, runID, epoch); err != nil {
				return err
			}
			// Accepted: the Run leaves waiting, if this is its current placement.
			err = noteStage(ctx, tx, runID)
		}
		return err
	})
	// Delivered: the runner keeps a push's or sync's credentials now.
	if err == nil && (acked == proto.MsgPush || acked == proto.MsgSync) {
		s.secrets.dropMsg(id)
	}
	return err
}

// enqueue writes a durable control message for a host. Call inside a
// transaction; notify after commit.
func enqueue(ctx context.Context, tx pgx.Tx, hostID, runID string, epoch int, typ string, payload any) error {
	_, err := enqueueID(ctx, tx, hostID, runID, epoch, typ, payload)
	return err
}

// enqueueID is enqueue, returning the message's id.
func enqueueID(ctx context.Context, tx pgx.Tx, hostID, runID string, epoch int, typ string, payload any) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO host_messages (host_id, run_id, epoch, type, payload)
		VALUES ($1, nullif($2, ''), nullif($3, 0), $4, $5) RETURNING id`, hostID, runID, epoch, typ, proto.Marshal(payload)).Scan(&id)
	return id, err
}

type hostToken struct {
	ID       string
	TenantID *string
	PoolID   *string // the pool its hosts join, whatever it is called
	Labels   map[string]string
}

func (s *Server) authHostToken(r *http.Request) (*hostToken, error) {
	raw := bearer(r)
	if raw == "" {
		return nil, errf(http.StatusUnauthorized, "unauthorized", "missing host token")
	}
	var t hostToken
	err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT id, tenant_id, pool_id, labels FROM host_tokens
			WHERE token_hash = $1 AND revoked_at IS NULL`, ids.Hash(raw)).Scan(&t.ID, &t.TenantID, &t.PoolID, &t.Labels)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errf(http.StatusUnauthorized, "unauthorized", "invalid host token")
	}
	return &t, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
