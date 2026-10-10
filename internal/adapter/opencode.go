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
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/proto"
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

	// observer: a server the workload's own command runs (not lux's
	// `opencode acp --port`): followed for activity only, never sent to.
	// user and password: OpenCode's server Basic auth, if set.
	observer       bool
	user, password string
	// refusals (under mu): an observer's requests refused since the server
	// last accepted a status read; gaveUp: the refusal that reached
	// refusalLimit, after which the observer sends nothing more. drop ends
	// the open event stream, nil when none is.
	refusals int
	gaveUp   error
	drop     context.CancelFunc

	// pause waits out one reconnect backoff d, false if ctx ends first; a
	// timer unless a test sets it before Run.
	pause func(ctx context.Context, d time.Duration) bool
	// beforeRegister, nil in production, lets tests schedule a refusal before
	// the terminal check and stream cancellation registration.
	beforeRegister func()
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
	// A redirect is never followed: it could lead off 127.0.0.1:port, or to
	// another port there with the Basic auth still attached. The 3xx itself
	// is the answer, an error to every caller (none takes it as success).
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &opencodeBus{port: port, dir: dir, expect: map[string]busSteer{},
		hc:     &http.Client{Timeout: 30 * time.Second, Transport: direct, CheckRedirect: noRedirect},
		stream: &http.Client{Transport: direct, CheckRedirect: noRedirect}}
}

// newOpencodeObserver follows the server a command lux did not build runs
// on 127.0.0.1:port, with OpenCode's Basic auth if password is set.
func newOpencodeObserver(port int, dir, user, password string) *opencodeBus {
	b := newOpencodeBus(port, dir)
	b.observer, b.user, b.password = true, user, password
	return b
}

// authorize adds the server's Basic auth to a request, if it has a password.
func (b *opencodeBus) authorize(req *http.Request) {
	if b.password != "" {
		req.SetBasicAuth(b.user, b.password)
	}
}

// errRefused: OpenCode's server refused lux's credentials (401 or 403).
var errRefused = errors.New("refused")

// refusal is an error wrapping errRefused if resp is a 401 or 403.
func refusal(what string, resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s: %s", errRefused, what, resp.Status)
	}
	return nil
}

// refused counts err, a refusal of an observer's request, and ends the open
// event stream: lux connects again after a backoff, or not at all once the
// count reaches refusalLimit.
func (b *opencodeBus) refused(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gaveUp != nil {
		return
	}
	if b.refusals++; b.refusals >= refusalLimit {
		b.gaveUp = err
	}
	if b.drop != nil {
		b.drop()
	}
}

// accepted clears the refusals: the server answered a status read.
func (b *opencodeBus) accepted() {
	b.mu.Lock()
	if b.gaveUp == nil {
		b.refusals = 0
	}
	b.mu.Unlock()
}

// refusing: the server refused the observer since it last accepted a
// status read. Nothing it says then shows OpenCode busy.
func (b *opencodeBus) refusing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refusals > 0 || b.gaveUp != nil
}

// givenUp is the refusal that made the observer stop, nil if none did.
func (b *opencodeBus) givenUp() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gaveUp
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
// what happened while it was down is not replayed. disconnected runs each
// time an established stream ends, unless ctx has ended.
//
// Every end of the stream, clean or not, is followed by a wait: capped
// exponential backoff with jitter, back to its start after a stream that
// stayed up for healthyStream. An observer stops once its requests have
// been refused refusalLimit times with no status read accepted between
// (refused), returning the refusal that reached it; otherwise follow
// returns nil.
func (b *opencodeBus) follow(ctx context.Context, on func(busEvent), connected, disconnected func()) error {
	wait := followMin
	for ctx.Err() == nil {
		began := time.Now()
		stream, drop := context.WithCancel(ctx)
		if b.beforeRegister != nil {
			b.beforeRegister()
		}
		b.mu.Lock()
		if err := b.gaveUp; err != nil {
			b.mu.Unlock()
			drop()
			return err
		}
		b.drop = drop
		b.mu.Unlock()
		err := b.followOnce(ctx, stream, on, connected, disconnected)
		b.mu.Lock()
		b.drop = nil
		b.mu.Unlock()
		dropped := stream.Err() != nil
		drop()
		if ctx.Err() != nil {
			return nil
		}
		if err == nil || dropped {
			err = errors.New("GET /event: the stream ended")
		}
		b.mu.Lock()
		b.lastErr = err
		b.mu.Unlock()
		if b.observer && errors.Is(err, errRefused) {
			b.refused(err)
		}
		if err := b.givenUp(); err != nil {
			return err
		}
		if time.Since(began) >= healthyStream {
			wait = followMin
		}
		// Jitter: between half and all of wait.
		if !b.wait(ctx, wait/2+time.Duration(mrand.Int64N(int64(wait/2)+1))) {
			return nil
		}
		wait = min(wait*2, followMax)
	}
	return nil
}

// wait is one reconnect backoff of d; false if ctx ended first.
func (b *opencodeBus) wait(ctx context.Context, d time.Duration) bool {
	if b.pause != nil {
		return b.pause(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Reconnect backoff of the event stream.
const (
	followMin     = 100 * time.Millisecond
	followMax     = 5 * time.Second
	healthyStream = 10 * time.Second
	// refusalLimit: an observer's requests refused in a row before it gives
	// up; with the backoff above, 60 to 120 s of refusals.
	refusalLimit = 30
)

// followOnce reads one GET /event stream until it or stream (ctx, or a
// refused status read) ends. disconnected is skipped only when ctx ends.
func (b *opencodeBus) followOnce(ctx, stream context.Context, on func(busEvent), connected, disconnected func()) error {
	req, err := http.NewRequestWithContext(stream, http.MethodGet, b.url("/event"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-opencode-directory", b.dir)
	b.authorize(req)
	resp, err := b.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := refusal("GET /event", resp); err != nil {
		return err
	}
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
		if disconnected != nil && ctx.Err() == nil {
			disconnected()
		}
	}()
	if connected != nil {
		connected()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		if data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
			var ev busEvent
			if json.Unmarshal(bytes.TrimSpace(data), &ev) == nil && ev.Type != "" {
				on(ev)
			}
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

// promptAsync stores an input as the user message msgID of session (its
// images as file parts, then its text) and joins it to the running loop
// (starting one if none runs). An error wrapping errNotSent means OpenCode
// did not take it.
func (b *opencodeBus) promptAsync(ctx context.Context, session, msgID string, in proto.Input) error {
	if b.observer {
		return fmt.Errorf("%w: an observed server is not steered through", errNotSent)
	}
	body, _ := json.Marshal(map[string]any{"messageID": msgID, "parts": inputContent(dialectOpenCode, in)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url("/session/"+session+"/prompt_async"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errNotSent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-directory", b.dir)
	b.authorize(req)
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

// compactionMessage is what lux reads of a stored message to find a
// compaction: the summary is an assistant message with info.summary true
// (a user message's summary is an object), whose text parts are the
// summary; its parent is the user message holding the compaction part
// ({"type":"compaction","auto","overflow"?}). opencode 1.18.35.
type compactionMessage struct {
	Info struct {
		ID       string          `json:"id"`
		Role     string          `json:"role"`
		ParentID string          `json:"parentID"`
		Summary  json.RawMessage `json:"summary"`
	} `json:"info"`
	Parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Auto     bool   `json:"auto"`
		Overflow bool   `json:"overflow"`
	} `json:"parts"`
}

func (m compactionMessage) isSummary() bool {
	return m.Info.Role == "assistant" && string(bytes.TrimSpace(m.Info.Summary)) == "true"
}

// compactionWindow: the newest messages read for a compaction's summary;
// by the time session.compacted is handled the loop may have stored a few
// more after it (an auto compaction's synthetic "continue" and its reply).
const compactionWindow = 20

// errCompactionReported: the session's newest summary is one already
// reported; session.compacted came again for the same compaction.
var errCompactionReported = errors.New("no summary newer than the one already reported")

// compaction reads a compaction of the session: the oldest summary message
// with an id above after (the summary last reported; ids ascend, whatever
// order the page has), its text and its trigger, and the summary's id. So
// two compactions close together each get their own summary.
// errCompactionReported if the page holds summaries, none newer than after.
func (b *opencodeBus) compaction(ctx context.Context, session, after string) (summary, trigger, id string, err error) {
	var page []compactionMessage
	if _, err := b.get(ctx, "/session/"+session+"/message?limit="+strconv.Itoa(compactionWindow), &page); err != nil {
		return "", "", "", err
	}
	at, older := -1, false
	for i, m := range page {
		if !m.isSummary() {
			continue
		}
		if m.Info.ID <= after {
			older = true
		} else if at < 0 || m.Info.ID < page[at].Info.ID {
			at = i
		}
	}
	switch {
	case at < 0 && older:
		return "", "", "", errCompactionReported
	case at < 0:
		return "", "", "", fmt.Errorf("no summary message among the session's newest %d", compactionWindow)
	}
	m := page[at]
	var text strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
	}
	for _, u := range page {
		if u.Info.ID != m.Info.ParentID {
			continue
		}
		for _, p := range u.Parts {
			if p.Type == "compaction" {
				trigger = map[bool]string{true: "auto", false: "manual"}[p.Auto]
				if p.Overflow {
					trigger = "overflow"
				}
			}
		}
	}
	if text.Len() == 0 {
		return "", trigger, m.Info.ID, errors.New("its summary message has no text")
	}
	return text.String(), trigger, m.Info.ID, nil
}

// newestSummary is the id of the newest summary message among the
// session's newest compactionWindow, "" if there is none.
func (b *opencodeBus) newestSummary(ctx context.Context, session string) (string, error) {
	var page []compactionMessage
	if _, err := b.get(ctx, "/session/"+session+"/message?limit="+strconv.Itoa(compactionWindow), &page); err != nil {
		return "", err
	}
	id := ""
	for _, m := range page {
		if m.isSummary() && m.Info.ID > id {
			id = m.Info.ID
		}
	}
	return id, nil
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

// busyStatus reads a session.status type: busy and retry are a running
// loop, idle is none; known is false for any other type.
func busyStatus(typ string) (busy, known bool) {
	switch typ {
	case "busy", "retry":
		return true, true
	case "idle":
		return false, true
	}
	return false, false
}

// get decodes a JSON GET and returns its x-next-cursor header.
func (b *opencodeBus) get(ctx context.Context, path string, v any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url(path), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-opencode-directory", b.dir)
	b.authorize(req)
	resp, err := b.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := refusal("GET "+path, resp); err != nil {
		return "", err
	}
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
