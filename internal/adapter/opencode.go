package adapter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
)

// opencodeBus is OpenCode's own HTTP server, run by the same process as
// its ACP (`opencode acp --port <p>`, on 127.0.0.1): lux sends steers
// through it and reads its event bus to learn when the agent read them.
//
// ACP stdio says nothing when a prompt sent during a turn is read. The
// legacy prompt_async endpoint takes a client message id, stores the
// message under it, and joins the running agent loop (never the v2
// /api/session/{id}/prompt: mixed with ACP it starts a second, concurrent
// loop). Each model step is an assistant message.updated whose parentID is
// the newest user message when the step began; the step has every user
// message up to its parent in context. When that parent is a steer lux
// sent, the step read it and every steer lux sent before it (answered).
type opencodeBus struct {
	port int
	dir  string
	hc   *http.Client
	// stream: for the event stream, which has no timeout (ctx ends it).
	stream *http.Client

	mu        sync.Mutex
	connected bool
	lastErr   error
	// expect maps a steer's message id to its lux request id, and the
	// adapter's cancel generation when it was sent.
	expect map[string]busSteer
	// last: the largest message id value (the 12 hex digits) lux made or
	// saw OpenCode store.
	last int64
	// seeded: the session whose stored messages last was raised to.
	seeded string
}

// freeLoopbackPort is a port nothing listens on at 127.0.0.1 now.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func newOpencodeBus(port int, dir string) *opencodeBus {
	// Straight to loopback: never through a proxy the Run's environment
	// names (HTTP_PROXY). A request OpenCode accepts while it is still
	// starting gets no response headers at all (1.18.31): time it out and
	// try again.
	direct := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 3 * time.Second}
	return &opencodeBus{port: port, dir: dir, expect: map[string]busSteer{},
		hc: &http.Client{Timeout: 30 * time.Second, Transport: direct}, stream: &http.Client{Transport: direct}}
}

func (b *opencodeBus) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(b.port) + path
}

func (b *opencodeBus) isConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// waitConnected waits up to d for the event stream; OpenCode's server
// comes up a little after its ACP answers.
func (b *opencodeBus) waitConnected(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !b.isConnected() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return false
		case <-tick.C:
		}
	}
	return true
}

// err is why the event stream is not connected, if it failed.
func (b *opencodeBus) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

// busEvent is what lux reads of a bus event.
type busEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			ParentID  string `json:"parentID"`
			SessionID string `json:"sessionID"`
		} `json:"info"`
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	} `json:"properties"`
}

// follow reads GET /event until ctx ends, reconnecting while the server is
// not up yet or the stream drops, and hands each event to on. connected
// runs each time the stream is (re)established, before its first event:
// what happened while it was down is not replayed.
//
// Every end of the stream, clean or not, is followed by a wait: capped
// exponential backoff with jitter, back to its start after a stream that
// stayed up for healthyStream.
func (b *opencodeBus) follow(ctx context.Context, on func(busEvent), connected func()) {
	wait := followMin
	for ctx.Err() == nil {
		began := time.Now()
		err := b.followOnce(ctx, on, connected)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("GET /event: the stream ended")
		}
		b.mu.Lock()
		b.lastErr = err
		b.mu.Unlock()
		if time.Since(began) >= healthyStream {
			wait = followMin
		}
		// Jitter: between half and all of wait.
		d := wait/2 + time.Duration(mrand.Int64N(int64(wait/2)+1))
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, followMax)
	}
}

// Reconnect backoff of the event stream.
const (
	followMin     = 100 * time.Millisecond
	followMax     = 5 * time.Second
	healthyStream = 10 * time.Second
)

func (b *opencodeBus) followOnce(ctx context.Context, on func(busEvent), connected func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url("/event"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-opencode-directory", b.dir)
	resp, err := b.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /event: %s", resp.Status)
	}
	b.mu.Lock()
	b.connected = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.connected = false
		b.mu.Unlock()
	}()
	if connected != nil {
		connected()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		var ev busEvent
		if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil && ev.Type != "" {
			on(ev)
		}
	}
	return sc.Err()
}

type busSteer struct {
	requestID string
	gen       int
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// messageID is a new OpenCode message id: "msg_", 12 hex digits of (unix
// ms × 0x1000 + a counter), then 14 random base62 characters. OpenCode
// orders messages by id, so it must sort after every message already
// stored: the value is strictly above both the last one lux made and the
// largest OpenCode id lux has seen (observe), whatever the clock does.
func (b *opencodeBus) messageID(now time.Time) string {
	b.mu.Lock()
	v := max((now.UnixMilli()*0x1000+1)&idMask, b.last+1)
	b.last = v
	b.mu.Unlock()
	suffix := make([]byte, 14)
	for i := range suffix {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(base62))))
		suffix[i] = base62[n.Int64()]
	}
	return fmt.Sprintf("msg_%012x%s", v, suffix)
}

const idMask = 1<<48 - 1

// observe raises the floor of messageID to a message id OpenCode stored.
func (b *opencodeBus) observe(id string) {
	v, ok := idValue(id)
	if !ok {
		return
	}
	b.mu.Lock()
	b.last = max(b.last, v)
	b.mu.Unlock()
}

// idValue is the 12-hex-digit value of a message id: unix ms × 0x1000 plus
// its generator's counter.
func idValue(id string) (int64, bool) {
	if len(id) < 16 || id[:4] != "msg_" {
		return 0, false
	}
	v, err := strconv.ParseInt(id[4:16], 16, 64)
	if err != nil || v >= idMask {
		return 0, false
	}
	return v, true
}

// seed observes, once per session, its newest stored message, so the next
// messageID sorts after it: a resumed session may hold ids made by a clock
// ahead of this one. After that, observe keeps up from the bus and the
// stored messages lux reads. Best effort: lux's own ids ascend without it.
func (b *opencodeBus) seed(ctx context.Context, session string) {
	b.mu.Lock()
	done := b.seeded == session
	b.mu.Unlock()
	if done {
		return
	}
	var page []storedMessage
	if _, err := b.get(ctx, "/session/"+session+"/message?limit=1", &page); err != nil {
		return
	}
	for _, m := range page {
		b.observe(m.Info.ID)
	}
	b.mu.Lock()
	b.seeded = session
	b.mu.Unlock()
}

// errNotSent: the steer certainly did not reach OpenCode; another path
// may deliver it.
var errNotSent = errors.New("not sent")

// promptAsync stores text as the user message msgID of session and joins
// it to the running loop (starting one if none runs). An error wrapping
// errNotSent means OpenCode did not take it.
func (b *opencodeBus) promptAsync(ctx context.Context, session, msgID, text string) error {
	body, _ := json.Marshal(map[string]any{"messageID": msgID, "parts": textInput(text)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url("/session/"+session+"/prompt_async"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errNotSent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-directory", b.dir)
	resp, err := b.hc.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: %v", errNotSent, err)
		}
		return fmt.Errorf("prompt_async: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: prompt_async: %s", errNotSent, resp.Status)
	}
	return nil
}

// storedMessage is what lux reads of a message GET /session/{id}/message
// lists.
type storedMessage struct {
	Info struct {
		ID       string `json:"id"`
		Role     string `json:"role"`
		ParentID string `json:"parentID"`
	} `json:"info"`
}

// messagesSince lists the session's stored messages, newest page first, back
// to the first page holding a message with an id before since (OpenCode
// pages newest-first; x-next-cursor is the older page's cursor).
func (b *opencodeBus) messagesSince(ctx context.Context, session, since string) ([]storedMessage, error) {
	var all []storedMessage
	cursor := ""
	for {
		q := url.Values{"limit": {"100"}}
		if cursor != "" {
			q.Set("before", cursor)
		}
		var page []storedMessage
		next, err := b.get(ctx, "/session/"+session+"/message?"+q.Encode(), &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" || len(page) == 0 || page[0].Info.ID < since {
			return all, nil
		}
		cursor = next
	}
}

// sessionBusy reports whether OpenCode runs a loop for the session: GET
// /session/status lists only sessions that are not idle.
func (b *opencodeBus) sessionBusy(ctx context.Context, session string) (bool, error) {
	var st map[string]struct {
		Type string `json:"type"`
	}
	if _, err := b.get(ctx, "/session/status", &st); err != nil {
		return false, err
	}
	s, ok := st[session]
	return ok && s.Type != "idle", nil
}

// get decodes a JSON GET and returns its x-next-cursor header.
func (b *opencodeBus) get(ctx context.Context, path string, v any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url(path), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-opencode-directory", b.dir)
	resp, err := b.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return resp.Header.Get("x-next-cursor"), json.NewDecoder(resp.Body).Decode(v)
}

func (b *opencodeBus) track(msgID, requestID string, gen int) {
	b.mu.Lock()
	b.expect[msgID] = busSteer{requestID, gen}
	b.mu.Unlock()
}

func (b *opencodeBus) untrack(msgID string) {
	b.mu.Lock()
	delete(b.expect, msgID)
	b.mu.Unlock()
}

// untrackRequest forgets the message ids of a request.
func (b *opencodeBus) untrackRequest(requestID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for m, st := range b.expect {
		if st.requestID == requestID {
			delete(b.expect, m)
		}
	}
}

// messageOf is the message id a request was last sent under ("" if none),
// and the cancel generation it was sent in.
func (b *opencodeBus) messageOf(requestID string) (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for m, st := range b.expect {
		if st.requestID == requestID {
			return m, st.gen
		}
	}
	return "", 0
}

// oldest is the smallest message id tracked, "" if none.
func (b *opencodeBus) oldest() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	o := ""
	for m := range b.expect {
		if o == "" || m < o {
			o = m
		}
	}
	return o
}

// answered returns, once, the request ids of the steers a model step whose
// parentID is parentID has in context. Only a parent lux sent says so: the
// step then has every user message up to it, and lux's own ids ascend, so
// every tracked message at or before it, in id order. A parent OpenCode
// made (the turn's prompt, a resumed transcript) reads none: its id and
// lux's come from separate generators and do not order each other.
func (b *opencodeBus) answered(parentID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.expect[parentID]; !ok {
		return nil
	}
	var msgs []string
	for m := range b.expect {
		if m <= parentID {
			msgs = append(msgs, m)
		}
	}
	slices.Sort(msgs)
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = b.expect[m].requestID
		delete(b.expect, m)
	}
	return ids
}
