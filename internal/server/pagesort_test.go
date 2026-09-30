package server

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// page is one paged list response, whatever the list.
type page struct {
	IDs              []string
	Next, Prev, Self string
	Total, Offset    *int
}

// fetchPage GETs one page of a list: ids from its rows under field.
func fetchPage(t *testing.T, s *Server, key, path, field string) page {
	t.Helper()
	var body map[string]any
	if code := getJSON(t, s, key, path, &body); code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, code)
	}
	var p page
	for _, r := range body[field].([]any) {
		p.IDs = append(p.IDs, fmt.Sprint(r.(map[string]any)["id"]))
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	p.Next, p.Prev, p.Self = str("next"), str("prev"), str("page")
	num := func(k string) *int {
		if v, ok := body[k].(float64); ok {
			n := int(v)
			return &n
		}
		return nil
	}
	p.Total, p.Offset = num("total"), num("offset")
	return p
}

// walkPages reads a list's pages in order through next, from base (whose
// query names the sort, dir and limit), calling between(i) after page i so
// a test can change the data mid-walk. It checks, for every page, that its
// prev leads back to the page before and at re-reads it unchanged, and
// returns every id in page order.
func walkPages(t *testing.T, s *Server, key, base, field string, between func(i int)) []string {
	t.Helper()
	sep := "&"
	var all []string
	var pages []page
	p := fetchPage(t, s, key, base, field)
	for i := 0; ; i++ {
		pages = append(pages, p)
		all = append(all, p.IDs...)
		if i > 0 && p.Prev == "" {
			t.Fatalf("%s: page %d has no prev", base, i)
		}
		if i == 0 && p.Prev != "" {
			t.Fatalf("%s: the first page has a prev", base)
		}
		if again := fetchPage(t, s, key, base+sep+"at="+url.QueryEscape(p.Self), field); p.Self != "" && !slices.Equal(again.IDs, p.IDs) && between == nil {
			t.Fatalf("%s: page %d read again at its cursor: %v, was %v", base, i, again.IDs, p.IDs)
		}
		if between != nil {
			between(i)
		}
		if p.Next == "" {
			break
		}
		if i > 200 {
			t.Fatalf("%s: no end of pages", base)
		}
		p = fetchPage(t, s, key, base+sep+"next="+url.QueryEscape(p.Next), field)
	}
	// Back through prev, without changes: the same pages, in reverse.
	if between == nil {
		q := pages[len(pages)-1]
		for i := len(pages) - 2; i >= 0; i-- {
			q = fetchPage(t, s, key, base+sep+"prev="+url.QueryEscape(q.Prev), field)
			if !slices.Equal(q.IDs, pages[i].IDs) {
				t.Fatalf("%s: prev of page %d: %v, want %v", base, i+1, q.IDs, pages[i].IDs)
			}
		}
	}
	return all
}

// sorted is ids ordered by v in dir, missing values (nil) last either way,
// then by id in dir: the order every paged list promises.
func sorted[V cmp.Ordered](ids []string, v func(id string) *V, dir string) []string {
	out := slices.Clone(ids)
	slices.SortStableFunc(out, func(a, b string) int {
		va, vb := v(a), v(b)
		switch {
		case va == nil && vb == nil:
		case va == nil:
			return 1
		case vb == nil:
			return -1
		default:
			if c := cmp.Compare(*va, *vb); c != 0 {
				if dir == "desc" {
					return -c
				}
				return c
			}
		}
		if dir == "desc" {
			return cmp.Compare(b, a)
		}
		return cmp.Compare(a, b)
	})
	return out
}

// hostsFixture: 57 hosts over three creation instants (so most sort values
// tie), some terminated, some whose launch failed (no terminated time, no
// uptime), some without a heartbeat. Returns each host's facts.
type fixtureHost struct {
	name                string
	created, terminated *time.Time
	failed, live        bool
	heartbeat           *time.Time
}

func hostsFixture(t *testing.T, s *Server, ctx context.Context) map[string]fixtureHost {
	t.Helper()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	hosts := map[string]fixtureHost{}
	for i := range 57 {
		id := fmt.Sprintf("h%03d", (i*37)%57) // ids not in creation order
		created := base.Add(time.Duration(i%3) * time.Hour)
		h := fixtureHost{name: fmt.Sprintf("n%02d", (i*11)%23), created: &created, live: true}
		state, outcome := "ready", "launched"
		switch i % 6 {
		case 1:
			end := created.Add(time.Duration(i%4) * time.Minute)
			h.terminated, h.live, state = &end, false, "terminated"
		case 4:
			end := created.Add(time.Second)
			h.failed, h.live, state, outcome = true, false, "terminated", "failed"
			execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, created_at, terminated_at, launch_outcome, provision_requested_at)
				VALUES ($1, $2, 'pool1', $3, $4, $5, $6, $4)`, id, h.name, state, created, end, outcome)
			hosts[id] = h
			continue
		}
		if i%5 != 0 {
			hb := base.Add(time.Duration(i%7) * time.Minute)
			h.heartbeat = &hb
		}
		// Terminated names are free again, and live ones must be unique.
		name := h.name
		if h.live {
			name = fmt.Sprintf("%s-%s", h.name, id)
			h.name = name
		}
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, created_at, terminated_at, launch_outcome, provision_requested_at, last_heartbeat)
			VALUES ($1, $2, 'pool1', $3, $4, $5, $6, $4, $7)`, id, name, state, created, h.terminated, outcome, h.heartbeat)
		hosts[id] = h
	}
	return hosts
}

// Every sort key family of GET /v1/hosts pages through the whole list in
// order: text (name), a time that never is missing (created, mostly tied),
// a time that often is (terminated, missing for live and launch-failed
// hosts), and a duration computed at the clock (uptime); each ascending
// and descending, with missing values last both ways.
func TestHostsPagedSortEveryFamily(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	hosts := hostsFixture(t, s, ctx)
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range hosts {
			if !yield(id) {
				return
			}
		}
	})
	first := fetchPage(t, s, key, "/v1/hosts?all=true&sort=uptime&limit=5", "hosts")
	c, err := decodeCursor(first.Self)
	if err != nil {
		t.Fatal(err)
	}
	clock, _ := time.Parse(time.RFC3339Nano, c.At)
	values := map[string]func(id string) *float64{
		"created": func(id string) *float64 { v := float64(hosts[id].created.UnixMicro()); return &v },
		"terminated": func(id string) *float64 {
			h := hosts[id]
			if h.terminated == nil || h.failed {
				return nil
			}
			v := float64(h.terminated.UnixMicro())
			return &v
		},
		"uptime": func(id string) *float64 {
			h := hosts[id]
			if h.failed {
				return nil
			}
			end := clock
			if h.terminated != nil {
				end = *h.terminated
			}
			v := end.Sub(*h.created).Seconds()
			return &v
		},
		"heartbeat": func(id string) *float64 {
			if hosts[id].heartbeat == nil {
				return nil
			}
			v := float64(hosts[id].heartbeat.UnixMicro())
			return &v
		},
	}
	for _, dir := range []string{"asc", "desc"} {
		for k, v := range values {
			got := walkPages(t, s, key, "/v1/hosts?all=true&limit=7&sort="+k+"&dir="+dir, "hosts", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
		got := walkPages(t, s, key, "/v1/hosts?all=true&limit=7&sort=name&dir="+dir, "hosts", nil)
		if want := sorted(ids, func(id string) *string { n := hosts[id].name; return &n }, dir); !slices.Equal(got, want) {
			t.Errorf("sort=name dir=%s:\n got %v\nwant %v", dir, got, want)
		}
	}
	// Numbered pages: the count, and an offset that lands where next does.
	p3 := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&limit=7&offset=14", "hosts")
	if *p3.Total != 57 || *p3.Offset != 14 {
		t.Fatalf("offset page: total %v offset %v", *p3.Total, *p3.Offset)
	}
	p1 := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&limit=7", "hosts")
	p2 := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&limit=7&next="+url.QueryEscape(p1.Next), "hosts")
	via := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&limit=7&next="+url.QueryEscape(p2.Next), "hosts")
	if !slices.Equal(via.IDs, p3.IDs) || *via.Offset != 14 {
		t.Fatalf("page 3 by cursor %v (offset %v), by offset %v", via.IDs, *via.Offset, p3.IDs)
	}
	// Unpaged, as it always was: every live host, by name.
	var list struct {
		Hosts []struct{ ID, Name string }
	}
	getJSON(t, s, key, "/v1/hosts", &list)
	live := 0
	for _, h := range hosts {
		if h.live {
			live++
		}
	}
	if len(list.Hosts) != live || !slices.IsSortedFunc(list.Hosts, func(a, b struct{ ID, Name string }) int { return cmp.Compare(a.Name, b.Name) }) {
		t.Fatalf("unpaged list: %d hosts, want %d by name", len(list.Hosts), live)
	}
}

// Paging while the list changes: a host created meanwhile (newest, so
// ahead of the cursor) and one that ends meanwhile neither repeat nor
// drop a host, and equal creation times never skip one.
func TestHostsPagedStableUnderChanges(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	hosts := hostsFixture(t, s, ctx)
	got := walkPages(t, s, key, "/v1/hosts?all=true&limit=6&sort=created", "hosts", func(i int) {
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, created_at) VALUES ($1, $1, 'pool1', 'ready', now())`, fmt.Sprintf("new%02d", i))
		execSQL(t, s, ctx, `UPDATE hosts SET state = 'terminated', terminated_at = now() WHERE id = (
			SELECT id FROM hosts WHERE state = 'ready' AND id LIKE 'h%' ORDER BY id LIMIT 1)`)
	})
	if len(got) != len(hosts) || len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(hosts) {
		t.Fatalf("walked %d hosts (%d distinct), want each of %d once", len(got), len(slices.Compact(slices.Sorted(slices.Values(got)))), len(hosts))
	}
}

// runsFixture: 23 runs sharing four creation instants, with placements
// that give each a placement time (waiting, then starting) and cost lines
// in USD on some (the others have none: missing, last).
func runsFixture(t *testing.T, s *Server, ctx context.Context) (map[string]float64, map[string]*float64) {
	t.Helper()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('hx', 'hx', 'ready')`)
	placement := map[string]float64{}
	cost := map[string]*float64{}
	for i := range 23 {
		id := fmt.Sprintf("r%02d", (i*7)%23)
		created := base.Add(time.Duration(i%4) * time.Minute)
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, created_at, current_epoch, name) VALUES ($1, 't1', '{"workload":{"adapter":"generic"}}', 'succeeded', $2, 1, $1)`, id, created)
		wait := time.Duration(i%5) * time.Second
		start := time.Duration(i%3) * time.Second
		assigned := created.Add(wait)
		execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, workload_started_at, started_at, ended_at)
			VALUES ($1, 't1', $2, 'hx', 1, 'exited', $3, $4, $4, $5)`, "p"+id, id, assigned, assigned.Add(start), assigned.Add(time.Hour))
		placement[id] = (wait + start).Seconds()
		if i%3 != 0 {
			v := float64(i%4) / 4
			cost[id] = &v
			execSQL(t, s, ctx, `INSERT INTO cost_lines (tenant_id, run_id, source, family, item, amount, currency, period_from, period_to, final)
				VALUES ('t1', $1, 'compute', 'compute', 'x', $2, 'USD', $3, $3, true)`, id, fmt.Sprint(v), created)
		}
	}
	return placement, cost
}

// GET /v1/runs pages in every server-sorted family: creation (tied), the
// computed placement time, and cost (missing for Runs with none, last both
// ways). Each page's Runs carry their placement time.
func TestRunsPagedSortEveryFamily(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	placement, cost := runsFixture(t, s, ctx)
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range placement {
			if !yield(id) {
				return
			}
		}
	})
	created := map[string]time.Time{}
	var list struct {
		Runs []*Run `json:"runs"`
	}
	getJSON(t, s, key, "/v1/runs?limit=100", &list)
	for _, r := range list.Runs {
		created[r.ID] = r.CreatedAt
		if r.PlacementSeconds != placement[r.ID] {
			t.Errorf("%s: placementSeconds %v, want %v", r.ID, r.PlacementSeconds, placement[r.ID])
		}
	}
	for _, dir := range []string{"asc", "desc"} {
		checks := map[string]func(id string) *float64{
			"created":   func(id string) *float64 { v := float64(created[id].UnixMicro()); return &v },
			"placement": func(id string) *float64 { v := placement[id]; return &v },
			"cost":      func(id string) *float64 { return cost[id] },
		}
		for k, v := range checks {
			got := walkPages(t, s, key, "/v1/runs?limit=4&sort="+k+"&dir="+dir, "runs", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
	}
	// Created, newest first, while Runs arrive and change state: each once.
	got := walkPages(t, s, key, "/v1/runs?limit=4&sort=created", "runs", func(i int) {
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, 't1', '{}', 'submitted')`, fmt.Sprintf("new%02d", i))
		execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id = (SELECT id FROM runs WHERE state = 'succeeded' ORDER BY id LIMIT 1)`)
	})
	if len(got) != len(ids) || len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(ids) {
		t.Fatalf("walked %v, want each of %d once", got, len(ids))
	}
}

// Placement time counts each placement from when the Run needed a host to
// its workload starting, sums placements, and counts up while one waits.
func TestPlacementTime(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	c := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('hx', 'hx', 'ready')`)
	// Two placements: 10s waiting + 5s starting; then, after the first
	// ended, 20s waiting + 7s starting. Then it waits again, since 30s ago.
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, created_at, current_epoch, needs_host_since)
		VALUES ('r', 't1', '{}', 'provisioning', $1, 2, now() - interval '30 seconds')`, c)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, workload_started_at, ended_at) VALUES
		('p1', 't1', 'r', 'hx', 1, 'exited', $1::timestamptz + interval '10 seconds', $1::timestamptz + interval '15 seconds', $1::timestamptz + interval '100 seconds'),
		('p2', 't1', 'r', 'hx', 2, 'exited', $1::timestamptz + interval '120 seconds', $1::timestamptz + interval '127 seconds', $1::timestamptz + interval '200 seconds')`, c)
	var r Run
	if code := getJSON(t, s, key, "/v1/runs/r", &r); code != http.StatusOK {
		t.Fatal(code)
	}
	if !r.Placing || r.PlacementStartSeconds != 12 || r.PlacementWaitSeconds < 60 || r.PlacementWaitSeconds > 62 {
		t.Fatalf("placing %v wait %v start %v, want placing, 10+20+~30 waiting, 5+7 starting", r.Placing, r.PlacementWaitSeconds, r.PlacementStartSeconds)
	}
	// Placed and started: no longer counting.
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running', current_epoch = 3, needs_host_since = NULL WHERE id = 'r'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, workload_started_at, needed_since)
		VALUES ('p3', 't1', 'r', 'hx', 3, 'running', now() - interval '5 seconds', now() - interval '2 seconds', now() - interval '35 seconds')`)
	r = Run{}
	getJSON(t, s, key, "/v1/runs/r", &r)
	if r.Placing || r.PlacementStartSeconds < 14.9 || r.PlacementStartSeconds > 15.1 || r.PlacementWaitSeconds < 59.9 || r.PlacementWaitSeconds > 60.1 {
		t.Fatalf("placed: placing %v wait %v start %v, want 60 and 15", r.Placing, r.PlacementWaitSeconds, r.PlacementStartSeconds)
	}
}

// A pool's events page by time (many sharing one instant) and by type.
func TestPoolEventsPagedSort(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	types := []string{"pool.scale_up", "pool.launch_failed", "pool.placement"}
	for i := range 31 {
		execSQL(t, s, ctx, `INSERT INTO pool_events (pool_id, type, created_at) VALUES ('pool1', $1, $2)`, types[i%3], at.Add(time.Duration(i%4)*time.Second))
	}
	type ev struct {
		ID   int64
		Type string
		Time time.Time
	}
	var all struct{ Events []ev }
	getJSON(t, s, key, "/v1/pools/burst/events?owner=platform&limit=1000", &all)
	byID := map[string]ev{}
	var ids []string
	for _, e := range all.Events {
		id := fmt.Sprint(e.ID)
		byID[id] = e
		ids = append(ids, id)
	}
	// Ids compare as numbers: pad them for the expected order.
	pad := func(xs []string) []string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = fmt.Sprintf("%06s", x)
		}
		return out
	}
	for _, dir := range []string{"asc", "desc"} {
		got := walkPages(t, s, key, "/v1/pools/burst/events?owner=platform&limit=4&sort=time&dir="+dir, "events", nil)
		want := sorted(pad(ids), func(id string) *int64 { v := byID[fmt.Sprint(mustAtoi(id))].Time.UnixMicro(); return &v }, dir)
		if !slices.Equal(pad(got), want) {
			t.Errorf("sort=time dir=%s:\n got %v\nwant %v", dir, pad(got), want)
		}
		got = walkPages(t, s, key, "/v1/pools/burst/events?owner=platform&limit=4&sort=type&dir="+dir, "events", nil)
		want = sorted(pad(ids), func(id string) *string { v := byID[fmt.Sprint(mustAtoi(id))].Type; return &v }, dir)
		if !slices.Equal(pad(got), want) {
			t.Errorf("sort=type dir=%s:\n got %v\nwant %v", dir, pad(got), want)
		}
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(s, "0"))
	return n
}
