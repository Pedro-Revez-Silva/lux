package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

const (
	feedRunA = "run_feedaaaaaaaaaaaa"
	feedRunB = "run_feedbbbbbbbbbbbb"
	feedRunO = "run_feedoooooooooooo" // tenant t2's
)

// feedFixture: wakeFixture plus Runs of t1 with labels (A: team=x env=prod,
// B: team=y, r1: none) and one of t2 (team=x), and the highest event id
// before the test's own.
func feedFixture(t *testing.T) (s *Server, ctx context.Context, key, key2 string, start int64) {
	t.Helper()
	s, ctx, key, key2 = wakeFixture(t)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch, labels) VALUES
		($1, 't1', '{}', 'running', 1, '{"team": "x", "env": "prod", "blank": ""}'),
		($2, 't1', '{}', 'running', 1, '{"team": "y", "env": "prod"}'),
		($3, 't2', '{}', 'running', 1, '{"team": "x", "env": "prod"}')`, feedRunA, feedRunB, feedRunO)
	systemScan(t, s, `SELECT coalesce(max(id), 0) FROM run_events`, nil, &start)
	return s, ctx, key, key2, start
}

// feedEvent inserts an event of a Run (runID) or a server (serverID), at a
// fixed time, and returns its id.
func feedEvent(t *testing.T, s *Server, ctx context.Context, tenant string, runID, serverID *string, typ, data, at string) int64 {
	t.Helper()
	var id int64
	systemScan(t, s, `INSERT INTO run_events (tenant_id, run_id, server_id, epoch, type, data, created_at)
		VALUES ($1, $2, $3, CASE WHEN $3::text IS NULL THEN 1 END, $4, $5::jsonb, $6::timestamptz) RETURNING id`,
		[]any{tenant, runID, serverID, typ, data, at}, &id)
	return id
}

// feedUnmatchedEvents inserts settled state events of Run B (team=y).
func feedUnmatchedEvents(t *testing.T, s *Server, ctx context.Context, n int) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data, created_at)
		SELECT 't1', $1, 1, 'state', '{}', now() - interval '1 hour' FROM generate_series(1, $2::int)`, feedRunB, n)
}

func ptr[T any](v T) *T { return &v }

// feedBody is GET target through the route, follow=false, as key.
func feedBody(t *testing.T, s *Server, key, target string) string {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, target, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, w.Code, w.Body)
	}
	return w.Body.String()
}

// Without label or type the feed's SSE body is exactly what it was: every
// event the tenant sees after the watermark, server events with their
// hostname and url, nothing of another tenant's; last=N the N newest.
func TestFeedUnfilteredBodyIsPinned(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a1 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{"to": "running"}`, at)
	feedEvent(t, s, ctx, "t2", ptr(feedRunO), nil, "state", `{"to": "running"}`, at)
	b1 := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "exited", `{"code": 0}`, at)
	sv := feedEvent(t, s, ctx, "t1", nil, ptr("srv_pinned"), "server.state", `{"host": "web", "name": "web", "runId": null}`, at)
	want := fmt.Sprintf(`id: %d
event: lux
data: {"id":%d,"epoch":1,"type":"state","data":{"to":"running"},"time":"2026-01-02T03:04:05Z","runId":"%s","tenant":"t1"}

id: %d
event: lux
data: {"id":%d,"epoch":1,"type":"exited","data":{"code":0},"time":"2026-01-02T03:04:05Z","runId":"%s","tenant":"t1"}

id: %d
event: lux
data: {"id":%d,"serverId":"srv_pinned","type":"server.state","data":{"host":"web","hostname":"web.lux.example.com","name":"web","runId":null,"url":"https://web.lux.example.com"},"time":"2026-01-02T03:04:05Z","runId":null,"tenant":"t1"}

`, a1, a1, feedRunA, b1, b1, feedRunB, sv, sv)
	if got := feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d", start)); got != want {
		t.Fatalf("after:\n%s\nwant:\n%s", got, want)
	}
	wantLast := want[len(fmt.Sprintf("id: %d\nevent: lux\ndata: {\"id\":%d,\"epoch\":1,\"type\":\"state\",\"data\":{\"to\":\"running\"},\"time\":\"2026-01-02T03:04:05Z\",\"runId\":\"%s\",\"tenant\":\"t1\"}\n\n", a1, a1, feedRunA)):]
	if got := feedBody(t, s, key, "/v1/events?follow=false&last=2"); got != wantLast {
		t.Fatalf("last=2:\n%s\nwant:\n%s", got, wantLast)
	}
	if got := feedResume(t, s, key, a1); got != wantLast {
		t.Fatalf("Last-Event-ID:\n%s\nwant:\n%s", got, wantLast)
	}
	// More than a window: feedPage events of Run A, then a server event,
	// all of them, by after= and by Last-Event-ID.
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data, created_at)
		SELECT 't1', $1, 1, 'state', jsonb_build_object('n', g), $2::timestamptz FROM generate_series(1, $3::int) g`, feedRunA, at, feedPage)
	var first, last, n int64
	systemScan(t, s, `SELECT min(id), max(id), count(*) FROM run_events WHERE id > $1`, []any{sv}, &first, &last, &n)
	if n != feedPage || last-first != feedPage-1 {
		t.Fatalf("ids %d..%d, %d events", first, last, n)
	}
	sv2 := feedEvent(t, s, ctx, "t1", nil, ptr("srv_pinned"), "server.state", `{"host": "web", "name": "web", "runId": null}`, at)
	var big strings.Builder
	for i := range int64(feedPage) {
		fmt.Fprintf(&big, `id: %d
event: lux
data: {"id":%d,"epoch":1,"type":"state","data":{"n":%d},"time":"2026-01-02T03:04:05Z","runId":"%s","tenant":"t1"}

`, first+i, first+i, i+1, feedRunA)
	}
	fmt.Fprintf(&big, `id: %d
event: lux
data: {"id":%d,"serverId":"srv_pinned","type":"server.state","data":{"host":"web","hostname":"web.lux.example.com","name":"web","runId":null,"url":"https://web.lux.example.com"},"time":"2026-01-02T03:04:05Z","runId":null,"tenant":"t1"}

`, sv2, sv2)
	if got := feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d", sv)); got != big.String() {
		t.Fatalf("after, %d events: %d bytes, want %d; ids %v", feedPage+1, len(got), big.Len(), feedIDs(t, got)[max(0, len(feedIDs(t, got))-3):])
	}
	if got := feedResume(t, s, key, sv); got != big.String() {
		t.Fatalf("Last-Event-ID, %d events: %d bytes, want %d; ids %v", feedPage+1, len(got), big.Len(), feedIDs(t, got)[max(0, len(feedIDs(t, got))-3):])
	}
}

// feedResume is the follow=false feed resumed with Last-Event-ID id.
func feedResume(t *testing.T, s *Server, key string, id int64) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/events?follow=false", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Last-Event-ID", fmt.Sprint(id))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Last-Event-ID %d: %d %s", id, w.Code, w.Body)
	}
	return w.Body.String()
}

// feedIDs is the ids of the lux events of an SSE body, in order.
func feedIDs(t *testing.T, body string) []int64 {
	t.Helper()
	ids := []int64{}
	for _, line := range strings.Split(body, "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var e FeedEvent
			if err := json.Unmarshal([]byte(d), &e); err != nil {
				t.Fatalf("%q: %v", d, err)
			}
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// feedQuery is the follow=false feed after start, with query q, as key.
func feedQuery(t *testing.T, s *Server, key string, start int64, q string) []int64 {
	t.Helper()
	return feedIDs(t, feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d&%s", start, q)))
}

func label(kv string) string { return "label=" + url.QueryEscape(kv) }

func wantIDs(t *testing.T, what string, got []int64, want ...int64) {
	t.Helper()
	if want == nil {
		want = []int64{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

// label= matches the event's Run's labels as GET /v1/runs?label= does:
// each label must match; an empty value is a value, even first.
func TestFeedLabelFilter(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	b := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(r1), nil, "state", `{}`, at)
	a2 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), a, a2)
	wantIDs(t, "team=y", feedQuery(t, s, key, start, label("team=y")), b)
	wantIDs(t, "env=prod", feedQuery(t, s, key, start, label("env=prod")), a, b, a2)
	wantIDs(t, "team=y env=prod", feedQuery(t, s, key, start, label("team=y")+"&"+label("env=prod")), b)
	wantIDs(t, "team=x env=dev", feedQuery(t, s, key, start, label("team=x")+"&"+label("env=dev")))
	wantIDs(t, "nope=x", feedQuery(t, s, key, start, label("nope=x")))
	// As on /v1/runs: blank= is the value "", and a first empty value
	// does not drop the others.
	runs := func(q string) []string {
		var out struct{ Runs []Run }
		w := apiCall(t, s, key, http.MethodGet, "/v1/runs?"+q, nil)
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("runs %s: %d %s", q, w.Code, w.Body)
		}
		ids := []string{}
		for _, r := range out.Runs {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := runs(label("blank=")); !slices.Equal(got, []string{feedRunA}) {
		t.Fatalf("/v1/runs blank=: %v", got)
	}
	wantIDs(t, "blank=", feedQuery(t, s, key, start, label("blank=")), a, a2)
	if got := runs(label("blank=") + "&" + label("team=y")); len(got) != 0 {
		t.Fatalf("/v1/runs blank= team=y: %v", got)
	}
	wantIDs(t, "blank= team=y", feedQuery(t, s, key, start, label("blank=")+"&"+label("team=y")))
	wantIDs(t, "team=", feedQuery(t, s, key, start, label("team=")))
	// An empty first label (key "", value "") matches no Run, and is not
	// dropped with those after it.
	if got := runs("label=&" + label("env=prod")); len(got) != 0 {
		t.Fatalf("/v1/runs label= env=prod: %v", got)
	}
	wantIDs(t, "label= env=prod", feedQuery(t, s, key, start, "label=&"+label("env=prod")))
}

// type= is a comma-separated list of exact types; with label=, both apply.
func TestFeedTypeFilter(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	st := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	ex := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "input", `{}`, at)
	ic := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "input.consumed", `{}`, at)
	bst := feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	wantIDs(t, "state", feedQuery(t, s, key, start, "type=state"), st, bst)
	wantIDs(t, "state,exited,input.consumed", feedQuery(t, s, key, start, "type=state,exited,input.consumed"), st, ex, ic, bst)
	wantIDs(t, "input. is not a prefix", feedQuery(t, s, key, start, "type=input."))
	wantIDs(t, "team=x type=state,input.consumed", feedQuery(t, s, key, start, label("team=x")+"&type=state,input.consumed"), st)
	wantIDs(t, "team=y type=state,input.consumed", feedQuery(t, s, key, start, label("team=y")+"&type=state,input.consumed"), ic, bst)
}

// A server's event matches a label filter through its attached Run's
// labels, never while it has none; a type filter alone matches it.
func TestFeedLabelFilterServerEvents(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	loose := feedEvent(t, s, ctx, "t1", nil, ptr("srv_loose"), "server.state", `{"host": "web"}`, at)
	onA := feedEvent(t, s, ctx, "t1", ptr(feedRunA), ptr("srv_a"), "server.state", `{"host": "a"}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunB), ptr("srv_b"), "server.state", `{"host": "b"}`, at)
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), onA)
	wantIDs(t, "team=", feedQuery(t, s, key, start, label("team=")))
	wantIDs(t, "type=server.state team=x", feedQuery(t, s, key, start, "type=server.state&"+label("team=x")), onA)
	wantIDs(t, "last=5 team=x", feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&last=5&"+label("team=x"))), onA)
	body := feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&after=%d&type=server.state", start))
	if got := feedIDs(t, body); len(got) != 3 || got[0] != loose {
		t.Fatalf("type=server.state: %v", got)
	}
	// A sent server event has its detail, as unfiltered.
	if !strings.Contains(body, `"url":"https://web.lux.example.com"`) {
		t.Fatalf("detail: %s", body)
	}
	// Type-only last=N counts events of no Run: the newest match is one.
	newestLoose := feedEvent(t, s, ctx, "t1", nil, ptr("srv_loose"), "server.state", `{"host": "web"}`, at)
	// A newer Run-less event must not count toward last=N under a label filter.
	wantIDs(t, "last=1 team=x ignores newer Run-less events",
		feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&last=1&"+label("team=x"))), onA)
	wantIDs(t, "last=1 type=server.state", feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&last=1&type=server.state")), newestLoose)
}

// More than a window of settled events that do not match, then one that
// does: the feed passes them all, one window after the other, and sends
// it.
func TestFeedWatermarkPassesUnmatchedEvents(t *testing.T) {
	s, ctx, key, _, start := feedFixture(t)
	feedUnmatchedEvents(t, s, ctx, feedPage+100)
	var lastSkipped int64
	systemScan(t, s, `SELECT max(id) FROM run_events`, nil, &lastSkipped)
	match := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, "2026-01-02T03:04:05Z")
	type read struct {
		after, last int64
		n           int
	}
	var reads []read
	feedRead = func(s *Server, ctx context.Context, p Principal, after int64, f feedFilter) ([]feedRow, error) {
		rows, err := s.feedWindow(ctx, p, after, f)
		r := read{after: after, n: len(rows)}
		if len(rows) > 0 {
			r.last = rows[len(rows)-1].ID
		}
		reads = append(reads, r)
		return rows, err
	}
	t.Cleanup(func() { feedRead = (*Server).feedWindow })
	wantIDs(t, "team=x", feedQuery(t, s, key, start, label("team=x")), match)
	// Window one: feedPage unmatched; window two from its last, through
	// the rest and the match.
	if len(reads) != 2 || reads[0].after != start || reads[0].n != feedPage ||
		reads[1].after != reads[0].last || reads[1].n != 101 || reads[1].last != match || match != lastSkipped+1 {
		t.Fatalf("reads: %+v (start %d, last skipped %d, match %d)", reads, start, lastSkipped, match)
	}
}

// last=N with a filter starts at the N newest matching events; fewer
// match: all of them; none: from now.
func TestFeedLastWithFilter(t *testing.T) {
	s, ctx, key, _, _ := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	var a, bx []int64
	for range 3 {
		a = append(a, feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at))
		feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
		bx = append(bx, feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "exited", `{}`, at))
	}
	ax := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	feedEvent(t, s, ctx, "t1", ptr(feedRunB), nil, "state", `{}`, at)
	last := func(q string) []int64 {
		return feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&"+q))
	}
	wantIDs(t, "last=2 team=x", last("last=2&"+label("team=x")), a[2], ax)
	wantIDs(t, "last=1 team=x", last("last=1&"+label("team=x")), ax)
	wantIDs(t, "last=9 team=x", last("last=9&"+label("team=x")), a[0], a[1], a[2], ax)
	wantIDs(t, "last=2 type=state team=x", last("last=2&type=state&"+label("team=x")), a[1], a[2])
	wantIDs(t, "last=2 type=exited", last("last=2&type=exited"), bx[2], ax)
	wantIDs(t, "last=3 type=exited team=y", last("last=3&type=exited&"+label("team=y")), bx...)
	wantIDs(t, "last=5 team=nope", last("last=5&"+label("team=nope")))
	wantIDs(t, "last=0 team=x", last("last=0&"+label("team=x")))
}

// A filter narrows what the caller sees, never widens it: t2's feed, its
// labels matching t1's Runs, has none of t1's events, nor t1's of t2's.
func TestFeedFiltersKeepTenants(t *testing.T) {
	s, ctx, key, key2, start := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	a := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "state", `{}`, at)
	o := feedEvent(t, s, ctx, "t2", ptr(feedRunO), nil, "state", `{}`, at)
	for _, q := range []string{label("team=x"), "type=state", label("team=x") + "&type=state"} {
		wantIDs(t, "t1 "+q, feedQuery(t, s, key, start, q), a)
		wantIDs(t, "t2 "+q, feedQuery(t, s, key2, start, q), o)
		// t2's match is the newest: last=1 counts t1's own.
		target := "/v1/events?follow=false&last=1&" + q
		wantIDs(t, "t1 last "+q, feedIDs(t, feedBody(t, s, key, target)), a)
		wantIDs(t, "t2 last "+q, feedIDs(t, feedBody(t, s, key2, target)), o)
	}
}

// A tenant's filtered last=N finds its own newest old match behind many
// newer events of another tenant that match the same filter, never theirs,
// reading about as many pages as without them; an operator's finds the
// other tenant's, or the narrowed tenant's.
func TestFeedLastSparseTenant(t *testing.T) {
	s, ctx, key, key2, _ := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	a := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	costed := []feedFilter{
		{labels: []string{"team=x"}},
		{types: []string{"exited"}},
		{labels: []string{"team=x"}, types: []string{"exited"}},
		{labels: []string{"team=absent"}},
		{types: []string{"absent"}},
		{labels: []string{"team=x"}, types: []string{"absent"}},
	}
	// lastCost: the buffer pages t1's last=1 watermark query reads for f.
	lastCost := func(f feedFilter) int {
		t.Helper()
		args := []any{1}
		q := feedLastSQL(f, "t1", func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) })
		var blocks int
		if err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
			c := &costTx{Tx: tx, t: t}
			var low int64
			err := c.QueryRow(ctx, q, args...).Scan(&low)
			blocks = c.blocks
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return blocks
	}
	before := make([]int, len(costed))
	for i, f := range costed {
		before[i] = lastCost(f)
	}
	execSQL(t, s, ctx, `INSERT INTO run_events (tenant_id, run_id, epoch, type, data, created_at)
		SELECT 't2', $1, 1, 'exited', '{}', now() - interval '1 hour' FROM generate_series(1, 50000)`, feedRunO)
	ownerExec(t, s, `ANALYZE run_events`)
	// Walking t2's 50,000 events would read hundreds of pages.
	const allowance = 64
	for i, f := range costed {
		if after := lastCost(f); after > before[i]+allowance {
			t.Errorf("t1 last=1 %+v: %d buffer pages behind t2's events, %d without", f, after, before[i])
		}
	}
	var newestO int64
	systemScan(t, s, `SELECT max(id) FROM run_events`, nil, &newestO)
	op := operatorKey(t, s, ctx)
	last := func(key, q string) []int64 {
		return feedIDs(t, feedBody(t, s, key, "/v1/events?follow=false&last=1&"+q))
	}
	for _, q := range []string{label("team=x"), "type=exited", label("team=x") + "&type=exited"} {
		wantIDs(t, "t1 "+q, last(key, q), a)
		wantIDs(t, "t2 "+q, last(key2, q), newestO)
		wantIDs(t, "operator "+q, last(op, q), newestO)
		wantIDs(t, "operator tenant=t1 "+q, last(op, "tenant=t1&"+q), a)
	}
	for _, q := range []string{label("team=absent"), "type=absent", label("team=x") + "&type=absent"} {
		wantIDs(t, "t1 "+q, last(key, q))
	}
}

// last=N with a filter looks among the 20,000 newest events only (the
// documented bound): a match just inside them starts the stream, one just
// outside does not.
func TestFeedLastIsBounded(t *testing.T) {
	const documentedScan = 20_000
	s, ctx, key, _, _ := feedFixture(t)
	const at = "2026-01-02T03:04:05Z"
	old := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	filters := []string{label("team=x"), "type=exited", label("team=x") + "&type=exited"}
	last := func(n int, q string) []int64 {
		return feedIDs(t, feedBody(t, s, key, fmt.Sprintf("/v1/events?follow=false&last=%d&%s", n, q)))
	}
	feedUnmatchedEvents(t, s, ctx, documentedScan-1)
	for _, q := range filters {
		wantIDs(t, "newest documentedScan "+q, last(1, q), old)
	}
	feedUnmatchedEvents(t, s, ctx, 1)
	for _, q := range filters {
		wantIDs(t, "past documentedScan "+q, last(1, q))
	}
	n1 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	feedUnmatchedEvents(t, s, ctx, 3)
	n2 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	n3 := feedEvent(t, s, ctx, "t1", ptr(feedRunA), nil, "exited", `{}`, at)
	for _, q := range filters {
		wantIDs(t, "last=2 "+q, last(2, q), n2, n3)
		wantIDs(t, "last=5 "+q, last(5, q), n1, n2, n3)
	}
}

// feedScript serves target with feedRead replaced by step: step(i, after)
// is read i's rows, given the watermark it reads after. Each read but the
// last wakes the feed, so it reads again at once; the last cancels the
// request. It returns the body and each read's watermark, once the
// handler has returned.
//
// The request has feedScriptBudget, less than feedFallback: a script whose
// next read comes from the fallback, not the wakeup, ends DeadlineExceeded.
func feedScript(t *testing.T, s *Server, key, target string, steps int, step func(i int, after int64) []feedRow) (string, []int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), feedScriptBudget)
	defer cancel()
	var afters []int64
	feedRead = func(s *Server, _ context.Context, _ Principal, after int64, _ feedFilter) ([]feedRow, error) {
		afters = append(afters, after)
		i := len(afters) - 1
		if i >= steps-1 {
			cancel()
		} else {
			s.wakeups.notify("")
		}
		if i >= steps {
			return nil, nil
		}
		return step(i, after), nil
	}
	t.Cleanup(func() { feedRead = (*Server).feedWindow })
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(w, req)
	}()
	bound, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	select {
	case <-done:
	case <-bound.Done():
		t.Fatalf("handler still running after cancel; reads after %v", afters)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("ended by %v, not the last read's cancel; reads after %v", ctx.Err(), afters)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, w.Code, w.Body)
	}
	return w.Body.String(), afters
}

// feedScriptBudget covers a woken script with margin (it takes milliseconds;
// see feedScript), not feedFallback's 5s.
const feedScriptBudget = 2 * time.Second

// feedRows is rows, those above after, in id order.
func feedRows(after int64, rows ...feedRow) []feedRow {
	out := []feedRow{}
	for _, r := range rows {
		if r.ID > after {
			out = append(out, r)
		}
	}
	return out
}

func feedRowAt(id int64, match bool, at time.Time) feedRow {
	e := feedRow{match: match}
	e.ID, e.Type, e.Time, e.Tenant, e.RunID = id, "state", at, "t1", ptr(feedRunA)
	if match {
		e.Data = map[string]any{}
	}
	return e
}

// A young event that does not match holds the watermark, as a young match
// does: a match with a lower id, committed late, is still sent, and the
// older match after it is not sent again.
func TestFeedWatermarkHeldByYoungUnmatched(t *testing.T) {
	s, _, key, _, start := feedFixture(t)
	young, old := time.Now(), time.Now().Add(-time.Hour)
	late, nonmatch, match := start+1, start+2, start+3
	// Read 0 sends nothing, so its next read is the wakeup's, not the 1s
	// re-read of young sent events.
	body, afters := feedScript(t, s, key, fmt.Sprintf("/v1/events?after=%d&%s", start, label("team=x")), 3, func(i int, after int64) []feedRow {
		switch i {
		case 0:
			return feedRows(after, feedRowAt(nonmatch, false, young))
		case 1:
			return feedRows(after, feedRowAt(nonmatch, false, young), feedRowAt(match, true, old))
		}
		return feedRows(after, feedRowAt(late, true, young), feedRowAt(nonmatch, false, young), feedRowAt(match, true, old))
	})
	if len(afters) != 3 || afters[1] != start || afters[2] != start {
		t.Fatalf("passed young nonmatch: reads after %v, want [%d %d %d]", afters, start, start, start)
	}
	wantIDs(t, "late commit delivered, earlier match not duplicated", feedIDs(t, body), match, late)
}

// follow (the default) with a filter keeps streaming past its first read:
// a match that becomes visible later is sent on the same request, once,
// though it is read again while young.
func TestFeedFilteredFollow(t *testing.T) {
	s, _, key, _, start := feedFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	m := feedRowAt(start+2, true, at)
	body, afters := feedScript(t, s, key, fmt.Sprintf("/v1/events?after=%d&%s", start, label("team=x")), 3, func(i int, after int64) []feedRow {
		if i == 0 {
			return feedRows(after, feedRowAt(start+1, false, at))
		}
		return feedRows(after, feedRowAt(start+1, false, at), m)
	})
	if len(afters) != 3 {
		t.Fatalf("reads after %v, want 3", afters)
	}
	want := fmt.Sprintf("id: %d\nevent: lux\ndata: {\"id\":%d,\"type\":\"state\",\"data\":{},\"time\":\"%s\",\"runId\":\"%s\",\"tenant\":\"t1\"}\n\n",
		m.ID, m.ID, at.Format(time.RFC3339), feedRunA)
	if body != want {
		t.Fatalf("body:\n%q\nwant:\n%q", body, want)
	}
}
