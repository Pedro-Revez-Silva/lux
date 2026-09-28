package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Interactive access: GET /v1/runs/{id}/exec, /attach and /ports/{name}
// upgrade to a WebSocket, relayed to the Run's host.
//
// Client protocol (JSON text messages, proto.StreamData):
//   - exec: the client sends one proto.StreamOpen first ({"command":[...],
//     "tty":true,"rows":..,"cols":..}); attach and ports need none.
//   - then StreamData both ways: {"data":<base64>} for bytes, {"eof":true}
//     to close input, {"rows","cols"} to resize;
//   - the stream ends with one {"exitCode":N} (exec) or {"error":"..."}, or
//     just the socket closing (a tunnel whose connection ended), and luxd
//     closes.
//
// Without the WebSocket upgrade, the same URL answers 200 if the stream
// could be opened and the error it would get otherwise, so a client can
// check first (lux port-forward does, before listening).
//
// The preview listener opens tunnels the same way (hubStream), as the
// connections of its reverse proxy (hubConn).

type streamTarget struct {
	hostID string
	epoch  int
	port   int
}

// resolveStream checks a stream can be opened and finds where it goes.
func (s *Server) resolveStream(r *http.Request, kind, runID, port string) (streamTarget, error) {
	return s.resolveTarget(r.Context(), store.Tenant(principal(r.Context()).TenantID), kind, runID, port)
}

// resolveTarget is resolveStream in a given scope: port names a port in
// the spec's network.ports, or else one of the Run's servers.
func (s *Server) resolveTarget(ctx context.Context, scope store.Scope, kind, runID, port string) (streamTarget, error) {
	var t streamTarget
	err := s.db.Tx(ctx, scope, func(tx pgx.Tx) error {
		var state string
		var sp spec.RunSpec
		err := tx.QueryRow(ctx, `SELECT r.state, r.spec, r.current_epoch, coalesce(p.host_id, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, runID).Scan(&state, &sp, &t.epoch, &t.hostID)
		if err != nil {
			return err
		}
		if state != StateRunning {
			return errf(http.StatusConflict, "not_running", "run is %s: interactive access needs it running", state)
		}
		switch kind {
		case "attach":
			if !sp.Workload.TTY {
				return errf(http.StatusConflict, "no_terminal", "attach needs a generic workload with workload.tty")
			}
		case "tunnel":
			for _, dp := range sp.Network.Ports {
				if dp.Name == port {
					t.port = dp.Port
				}
			}
			if t.port == 0 {
				p, ok, err := serverPort(ctx, tx, runID, port)
				if err != nil {
					return err
				}
				if !ok {
					return errf(http.StatusNotFound, "not_found", "the Run declares no port named %q and has no server of that name", port)
				}
				t.port = p
			}
		}
		return nil
	})
	if err != nil {
		return t, err
	}
	if !s.hub.Streaming(t.hostID) {
		return t, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
	}
	return t, nil
}

type portInput struct {
	RunPath
	Name string `path:"name" doc:"The port's name in the spec's network.ports, or a server's name."`
	// Ticket is read by streamAuth; here to be documented.
	Ticket string `query:"ticket" doc:"A stream ticket (kind exec), instead of an Authorization header."`
}

// runStreamInput is exec's or attach's: portInput without the port.
type runStreamInput struct {
	RunPath
	Ticket string `query:"ticket" doc:"A stream ticket (kind exec), instead of an Authorization header."`
}

// runStream is exec or attach.
func (s *Server) runStream(kind string) func(http.ResponseWriter, *http.Request, *runStreamInput) error {
	h := s.streamHandler(kind)
	return func(w http.ResponseWriter, r *http.Request, in *runStreamInput) error {
		return h(w, r, &portInput{RunPath: in.RunPath})
	}
}

func (s *Server) streamHandler(kind string) func(http.ResponseWriter, *http.Request, *portInput) error {
	return func(w http.ResponseWriter, r *http.Request, in *portInput) error {
		t, err := s.resolveStream(r, kind, in.ID, in.Name)
		if err != nil {
			return err
		}
		if r.Header.Get("Upgrade") == "" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return nil
		}
		// The origin was checked already (streamAuth), against more than
		// the request's own host, which is all Accept's check allows.
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return nil
		}
		ws.SetReadLimit(4 << 20)
		defer ws.CloseNow()
		s.relayStream(r.Context(), ws, in.ID, kind, t)
		return nil
	}
}

// hubStream is one stream to a Run's host, through the runner's
// WebSocket: frames out with send, frames in on ch until the host ends it
// (a stream.close), drops it (ch closed: its reader fell behind) or goes
// away (gone).
type hubStream struct {
	s      *Server
	hostID string
	runID  string
	epoch  int
	id     string
	ch     <-chan proto.Frame
	gone   <-chan struct{}
	unsub  func()
	once   sync.Once
	// window: flow control (a tunnel's); taken counts the output frames
	// read since the last grant. Only the stream's reader touches taken.
	window int
	taken  int
}

// tunnelWindow is how many output frames (up to 32 KiB each: 4 MiB) a
// tunnel's runner may send ahead of its reader here. Well under the hub's
// 256-frame subscription buffer, with room for the EOF and close after.
const tunnelWindow = 128

// openStream opens a stream of open.Kind to t. A tunnel is flow
// controlled: its runner sends output only as fast as it is read here
// (see took).
func (s *Server) openStream(runID string, t streamTarget, open proto.StreamOpen) (*hubStream, error) {
	h := &hubStream{s: s, hostID: t.hostID, runID: runID, epoch: t.epoch, id: ids.New("st"), gone: s.hub.Gone(t.hostID)}
	if open.Kind == "tunnel" {
		open.Window, h.window = tunnelWindow, tunnelWindow
	}
	h.ch, h.unsub = s.hub.Subscribe(h.id)
	if err := h.send(proto.MsgStreamOpen, proto.Marshal(open)); err != nil {
		h.unsub()
		return nil, err
	}
	return h, nil
}

// took notes that the reader has taken an output frame off the stream,
// and grants the runner more once it has taken half the window. A runner
// from before flow control reads the grant as empty input.
func (h *hubStream) took() {
	if h.window == 0 {
		return
	}
	h.taken++
	// A grant that could not be sent stays owed: the next frame taken
	// tries again, so a busy host never loses its window for good.
	if h.taken >= h.window/2 && h.send(proto.MsgStreamData, proto.Marshal(proto.StreamData{Credit: h.taken})) == nil {
		h.taken = 0
	}
}

func (h *hubStream) send(typ string, data []byte) error {
	return h.s.hub.SendLive(h.hostID, proto.Frame{Type: typ, RunID: h.runID, Epoch: h.epoch, Stream: h.id, Data: data})
}

// close ends the stream on the host's side too; idempotent.
func (h *hubStream) close() {
	h.once.Do(func() {
		_ = h.send(proto.MsgStreamClose, nil)
		h.unsub()
	})
}

func (s *Server) relayStream(ctx context.Context, ws *websocket.Conn, runID, kind string, t streamTarget) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	open := proto.StreamOpen{Kind: kind, Port: t.port}
	if kind == "exec" {
		if err := wsjson.Read(ctx, ws, &open); err != nil || len(open.Command) == 0 {
			closeWith(ctx, ws, []byte(`{"error":"exec needs a command"}`))
			return
		}
		open.Kind, open.Port, open.Window = kind, 0, 0
	}
	st, err := s.openStream(runID, t, open)
	if err != nil {
		closeWith(ctx, ws, proto.Marshal(proto.StreamData{Error: err.Error()}))
		return
	}
	defer st.close()

	// Client → host: checked to be StreamData, forwarded as sent.
	go func() {
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var d proto.StreamData
			if json.Unmarshal(b, &d) != nil || d.ExitCode != nil || d.Error != "" || d.Credit != 0 {
				return
			}
			if err := st.send(proto.MsgStreamData, b); err != nil {
				return
			}
		}
	}()
	// Host → client, forwarded as they come.
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.gone:
			closeWith(ctx, ws, []byte(`{"error":"the Run's host disconnected"}`))
			return
		case f, ok := <-st.ch:
			if !ok {
				closeWith(ctx, ws, []byte(`{"error":"the stream fell behind and was dropped"}`))
				return
			}
			if f.Type == proto.MsgStreamClose {
				closeWith(ctx, ws, f.Data)
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
			err := ws.Write(wctx, websocket.MessageText, f.Data)
			wcancel()
			if err != nil {
				return
			}
			st.took()
		}
	}
}

// closeWith sends the stream's last message, if it has one, and closes.
func closeWith(ctx context.Context, ws *websocket.Conn, last []byte) {
	var d proto.StreamData
	if json.Unmarshal(last, &d) == nil && (d.ExitCode != nil || d.Error != "") {
		_ = ws.Write(ctx, websocket.MessageText, last)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// hubConn is a tunnel stream as a net.Conn: what the preview proxy's
// transport dials. Writes go out as StreamData; reads take the host's
// StreamData until its EOF or the stream's end.
type hubConn struct {
	st      *hubStream
	buf     []byte
	eof     bool
	err     error
	closed  chan struct{}
	closeMu sync.Once

	dmu      sync.Mutex
	deadline time.Time
	dchange  chan struct{}
}

// dialTunnel opens a tunnel to t.port as a connection.
func (s *Server) dialTunnel(runID string, t streamTarget) (net.Conn, error) {
	st, err := s.openStream(runID, t, proto.StreamOpen{Kind: "tunnel", Port: t.port})
	if err != nil {
		return nil, err
	}
	return &hubConn{st: st, closed: make(chan struct{}), dchange: make(chan struct{})}, nil
}

// maxTunnelChunk bounds one StreamData a tunnel writes.
const maxTunnelChunk = 32 << 10

var errDeadline = &deadlineError{}

type deadlineError struct{}

func (*deadlineError) Error() string   { return "i/o timeout" }
func (*deadlineError) Timeout() bool   { return true }
func (*deadlineError) Temporary() bool { return true }

func (c *hubConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		if c.err != nil {
			return 0, c.err
		}
		c.dmu.Lock()
		d, changed := c.deadline, c.dchange
		c.dmu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !d.IsZero() {
			if time.Until(d) <= 0 {
				return 0, errDeadline
			}
			timer = time.NewTimer(time.Until(d))
			expired = timer.C
		}
		f, ok, err := c.next(changed, expired)
		if timer != nil {
			timer.Stop()
		}
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		switch {
		case f.Type == proto.MsgStreamClose:
			var d proto.StreamData
			if json.Unmarshal(f.Data, &d) == nil && d.Error != "" {
				c.err = errors.New(d.Error)
			} else {
				c.eof = true
			}
		default:
			c.st.took()
			var d proto.StreamData
			if json.Unmarshal(f.Data, &d) != nil {
				continue
			}
			if d.EOF {
				c.eof = true
			}
			c.buf = d.Data
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// next waits for the stream's next frame (ok), a deadline change (not
// ok, no error), or an end: the deadline, Close, the host or the stream
// going away (recorded in c.err for the reads after).
func (c *hubConn) next(changed <-chan struct{}, expired <-chan time.Time) (proto.Frame, bool, error) {
	select {
	case <-c.closed:
		return proto.Frame{}, false, net.ErrClosed
	case <-c.st.gone:
		c.err = errors.New("the Run's host disconnected")
		return proto.Frame{}, false, c.err
	case <-expired:
		return proto.Frame{}, false, errDeadline
	case <-changed:
		return proto.Frame{}, false, nil
	case f, ok := <-c.st.ch:
		if !ok {
			c.err = errors.New("the stream fell behind and was dropped")
			return proto.Frame{}, false, c.err
		}
		return f, true, nil
	}
}

func (c *hubConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxTunnelChunk)]
		if err := c.st.send(proto.MsgStreamData, proto.Marshal(proto.StreamData{Data: chunk})); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// CloseWrite passes a half-close on: the container sees EOF.
func (c *hubConn) CloseWrite() error {
	return c.st.send(proto.MsgStreamData, proto.Marshal(proto.StreamData{EOF: true}))
}

func (c *hubConn) Close() error {
	c.closeMu.Do(func() {
		close(c.closed)
		c.st.close()
	})
	return nil
}

func (c *hubConn) LocalAddr() net.Addr  { return tunnelAddr(c.st.runID) }
func (c *hubConn) RemoteAddr() net.Addr { return tunnelAddr(c.st.runID) }

// SetDeadline and SetReadDeadline bound reads; writes go straight to the
// runner's connection, which has its own bound.
func (c *hubConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }
func (c *hubConn) SetReadDeadline(t time.Time) error {
	c.dmu.Lock()
	c.deadline = t
	close(c.dchange)
	c.dchange = make(chan struct{})
	c.dmu.Unlock()
	return nil
}
func (c *hubConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelAddr string

func (a tunnelAddr) Network() string { return "lux" }
func (a tunnelAddr) String() string  { return string(a) }
