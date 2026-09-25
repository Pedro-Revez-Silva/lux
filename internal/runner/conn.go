package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/marcioapm/lux/internal/proto"
)

// conn is the runner's link to luxd: one outbound WebSocket, falling back
// to HTTP polling when the WebSocket cannot be established. Messages from
// luxd are acked after they are handled; reports to luxd are retried until
// luxd replies (ack, or nack).
type conn struct {
	r       *Runner
	log     *slog.Logger
	mu      sync.Mutex
	ws      *websocket.Conn
	polling bool
	nextID  int64
	// replies waiting for a report, by frame id
	waiters map[int64]chan proto.Frame
	// fallback: reports queued for the next poll, acks to send
	pollReports []proto.Frame
	pollAcks    []int64
}

func newConn(r *Runner) *conn {
	return &conn{r: r, log: r.log, waiters: map[int64]chan proto.Frame{}}
}

func (c *conn) wsURL() string {
	u := strings.TrimRight(c.r.cfg.URL, "/")
	u = strings.Replace(u, "http://", "ws://", 1)
	u = strings.Replace(u, "https://", "wss://", 1)
	return u + "/runner/v1/ws"
}

// loop keeps a connection up until ctx ends.
func (c *conn) loop(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		var err error
		if c.r.cfg.ForcePoll {
			err = c.pollLoop(ctx)
		} else {
			err = c.wsSession(ctx)
			if errors.Is(err, errUpgradeFailed) {
				c.log.Warn("websocket unavailable; polling", "err", err)
				err = c.pollLoop(ctx)
			}
		}
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("disconnected from luxd", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
		if err == nil {
			backoff = 500 * time.Millisecond
		}
	}
}

var errUpgradeFailed = errors.New("websocket upgrade failed")

func (c *conn) wsSession(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, c.wsURL(), &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + c.r.cfg.Token}},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized) {
			return fmt.Errorf("luxd rejected the host token")
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode < 500 {
			return fmt.Errorf("%w: %s", errUpgradeFailed, resp.Status)
		}
		return err
	}
	ws.SetReadLimit(64 << 20)
	defer ws.CloseNow()

	hello := c.r.hello(ctx)
	if err := c.writeRaw(ctx, ws, proto.Frame{Type: proto.MsgHello, Data: proto.Marshal(hello)}); err != nil {
		return err
	}
	var welcome proto.Frame
	if err := readFrame(ctx, ws, &welcome); err != nil {
		return err
	}
	if welcome.Type != proto.MsgWelcome {
		return fmt.Errorf("expected welcome, got %s", welcome.Type)
	}
	var w proto.Welcome
	_ = json.Unmarshal(welcome.Data, &w)
	c.mu.Lock()
	c.ws = ws
	c.polling = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.ws == ws {
			c.ws = nil
		}
		for id, ch := range c.waiters {
			close(ch)
			delete(c.waiters, id)
		}
		c.mu.Unlock()
		// Streams live on this connection: luxd ends its side when it
		// drops, and nothing could reach these again.
		c.r.streams.endAll()
	}()
	c.log.Info("connected to luxd", "host", w.HostID, "live", len(w.Live))
	c.r.onWelcome(ctx, w)

	for {
		var f proto.Frame
		if err := readFrame(ctx, ws, &f); err != nil {
			return err
		}
		c.dispatch(ctx, f)
	}
}

func (c *conn) dispatch(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgAck, proto.MsgNack:
		c.mu.Lock()
		ch := c.waiters[f.ID]
		delete(c.waiters, f.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	case proto.MsgStreamOpen, proto.MsgStreamData, proto.MsgStreamClose:
		// Live, not durable: no ack. Handled here, in the order read, so
		// a stream's input stays in order (see stream.go).
		c.r.handleStreamFrame(ctx, f)
	case proto.MsgOutputSubscribe, proto.MsgOutputCancel:
		// Live, not durable: no ack.
		go c.r.handleLive(ctx, f)
	default:
		// Durable control message: handle, then ack. Handling must be
		// idempotent: an unacked message is redelivered on reconnect.
		// Messages about one Run are handled in order (a stop must not
		// overtake its assign); different Runs do not wait for each other.
		c.r.control.enqueue(f.RunID, func() {
			c.r.handleControl(ctx, f)
			c.ack(ctx, f.ID)
		})
	}
}

func (c *conn) ack(ctx context.Context, id int64) {
	if id == 0 {
		return
	}
	c.mu.Lock()
	ws, polling := c.ws, c.polling
	if polling {
		c.pollAcks = append(c.pollAcks, id)
	}
	c.mu.Unlock()
	if ws != nil {
		_ = c.writeRaw(ctx, ws, proto.Frame{Type: proto.MsgAck, ID: id})
	}
}

// Send sends a live frame (output records, stream data). Dropped if not
// connected over WebSocket: live streams need one.
func (c *conn) Send(ctx context.Context, f proto.Frame) error {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return errors.New("not connected")
	}
	return c.writeRaw(ctx, ws, f)
}

// Report delivers a report to luxd and waits for its reply, retrying
// across reconnects until ctx ends. A nack is returned as an error; a
// stale nack as errStale.
func (c *conn) Report(ctx context.Context, f proto.Frame) error {
	for {
		reply, err := c.reportOnce(ctx, f)
		if err == nil {
			if reply.Type == proto.MsgNack {
				var n proto.Nack
				_ = json.Unmarshal(reply.Data, &n)
				if n.Stale {
					return errStale
				}
				return fmt.Errorf("luxd: %s", n.Error)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

var errStale = errors.New("stale epoch")

func (c *conn) reportOnce(ctx context.Context, f proto.Frame) (proto.Frame, error) {
	c.mu.Lock()
	c.nextID++
	f.ID = c.nextID
	ch := make(chan proto.Frame, 1)
	c.waiters[f.ID] = ch
	ws, polling := c.ws, c.polling
	if polling {
		c.pollReports = append(c.pollReports, f)
	}
	c.mu.Unlock()
	cleanup := func() {
		c.mu.Lock()
		delete(c.waiters, f.ID)
		c.mu.Unlock()
	}
	if ws == nil && !polling {
		cleanup()
		return proto.Frame{}, errors.New("not connected")
	}
	if ws != nil {
		if err := c.writeRaw(ctx, ws, f); err != nil {
			cleanup()
			return proto.Frame{}, err
		}
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return proto.Frame{}, errors.New("disconnected")
		}
		return r, nil
	case <-time.After(30 * time.Second):
		cleanup()
		return proto.Frame{}, errors.New("no reply")
	case <-ctx.Done():
		cleanup()
		return proto.Frame{}, ctx.Err()
	}
}

func (c *conn) writeRaw(ctx context.Context, ws *websocket.Conn, f proto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func readFrame(ctx context.Context, ws *websocket.Conn, f *proto.Frame) error {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, f)
}

// pollLoop is the fallback transport: POST /runner/v1/poll every second with
// acks and reports; luxd answers with pending messages and replies. Live
// output is unavailable in this mode (it arrives after exit).
func (c *conn) pollLoop(ctx context.Context) error {
	c.mu.Lock()
	c.polling = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.polling = false
		c.mu.Unlock()
	}()
	hello := c.r.hello(ctx)
	first := true
	seen := map[int64]bool{}
	for ctx.Err() == nil {
		c.mu.Lock()
		req := map[string]any{"acks": c.pollAcks, "reports": c.pollReports}
		c.pollAcks, c.pollReports = nil, nil
		c.mu.Unlock()
		if first {
			req["hello"] = hello
		}
		var resp proto.PollResponse
		if err := c.r.api.postJSON(ctx, "/runner/v1/poll?name="+c.r.cfg.Name, req, &resp); err != nil {
			return err
		}
		for _, f := range resp.Replies {
			if f.Type == proto.MsgWelcome {
				var w proto.Welcome
				_ = json.Unmarshal(f.Data, &w)
				c.log.Info("polling luxd", "host", w.HostID)
				c.r.onWelcome(ctx, w)
				first = false
				continue
			}
			c.dispatch(ctx, f)
		}
		for _, f := range resp.Messages {
			// Pending messages repeat until acked; handle each once.
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			c.dispatch(ctx, f)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

// serialQueues runs functions in order per key, concurrently across keys.
type serialQueues struct {
	mu     sync.Mutex
	queues map[string][]func()
}

func newSerialQueues() *serialQueues { return &serialQueues{queues: map[string][]func(){}} }

func (q *serialQueues) enqueue(key string, fn func()) {
	q.mu.Lock()
	pending, running := q.queues[key]
	q.queues[key] = append(pending, fn)
	q.mu.Unlock()
	if !running {
		go q.drain(key)
	}
}

func (q *serialQueues) drain(key string) {
	for {
		q.mu.Lock()
		fns := q.queues[key]
		if len(fns) == 0 {
			delete(q.queues, key)
			q.mu.Unlock()
			return
		}
		fn := fns[0]
		q.queues[key] = fns[1:]
		q.mu.Unlock()
		fn()
	}
}
