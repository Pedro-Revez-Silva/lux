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
		case proto.MsgOutputRecords, proto.MsgOutputEnd:
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
	var out []proto.Frame
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE host_messages SET delivered_at = now()
			WHERE id IN (SELECT id FROM host_messages
				WHERE host_id = $1 AND acked_at IS NULL AND (NOT $2 OR delivered_at IS NULL)
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
		if out[i].Type == proto.MsgAssign {
			out[i].Data = s.attachSecrets(out[i].RunID, out[i].Data)
		}
	}
	return out, nil
}

func (s *Server) attachSecrets(runID string, data json.RawMessage) json.RawMessage {
	vals, ok := s.secrets.get(runID)
	if !ok || len(vals) == 0 {
		return data
	}
	var a proto.Assign
	if err := json.Unmarshal(data, &a); err != nil {
		return data
	}
	a.Secrets = vals
	return proto.Marshal(a)
}

func (s *Server) ackMessage(ctx context.Context, hostID string, id int64) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var typ, runID string
		var epoch int
		err := tx.QueryRow(ctx, `UPDATE host_messages SET acked_at = now()
			WHERE id = $1 AND host_id = $2 AND acked_at IS NULL
			RETURNING type, coalesce(run_id, ''), coalesce(epoch, 0)`, id, hostID).Scan(&typ, &runID, &epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if typ == proto.MsgAssign {
			_, err = tx.Exec(ctx, `UPDATE placements SET accepted_at = coalesce(accepted_at, now())
				WHERE run_id = $1 AND epoch = $2`, runID, epoch)
		}
		return err
	})
}

// enqueue writes a durable control message for a host. Call inside a
// transaction; notify after commit.
func enqueue(ctx context.Context, tx pgx.Tx, hostID, runID string, epoch int, typ string, payload any) error {
	_, err := tx.Exec(ctx, `INSERT INTO host_messages (host_id, run_id, epoch, type, payload)
		VALUES ($1, nullif($2, ''), nullif($3, 0), $4, $5)`, hostID, runID, epoch, typ, proto.Marshal(payload))
	return err
}

type hostToken struct {
	ID       string
	TenantID *string
	Pool     string
	Labels   map[string]string
}

func (s *Server) authHostToken(r *http.Request) (*hostToken, error) {
	raw := bearer(r)
	if raw == "" {
		return nil, errf(http.StatusUnauthorized, "unauthorized", "missing host token")
	}
	var t hostToken
	err := s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT id, tenant_id, pool, labels FROM host_tokens
			WHERE token_hash = $1 AND revoked_at IS NULL`, ids.Hash(raw)).Scan(&t.ID, &t.TenantID, &t.Pool, &t.Labels)
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
