package server

import (
	"cmp"
	"context"
	"encoding/base64"
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
// query names the sort, dir and limit), calling between(i, p) after page i
// so a test can change the data mid-walk. It checks, for every page, that
// at re-reads it unchanged after between ran (changes a test makes must be
// ahead of or behind the page), and, without between, that prev leads back
// through the same pages. A page read again at its cursor has a prev
// exactly when it had one (with between: keeps one it had). Returns every
// id in page order.
func walkPages(t *testing.T, s *Server, key, base, field string, between func(i int, p page)) []string {
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
		if between != nil {
			between(i, p)
		}
		if p.Self != "" {
			// A last page has room: rows added behind it may follow.
			again := fetchPage(t, s, key, base+sep+"at="+url.QueryEscape(p.Self), field)
			same := slices.Equal(again.IDs, p.IDs)
			if p.Next == "" && between != nil {
				same = len(again.IDs) >= len(p.IDs) && slices.Equal(again.IDs[:len(p.IDs)], p.IDs)
			}
			if !same {
				t.Fatalf("%s: page %d read again at its cursor: %v, was %v", base, i, again.IDs, p.IDs)
			}
			// A re-read keeps the page's prev. Rows between adds ahead of a
			// page give it one it did not have, so with between only a
			// prev once there must stay.
			if (again.Prev == "") != (p.Prev == "") && (between == nil || p.Prev != "") {
				t.Fatalf("%s: page %d read again at its cursor: prev %q, was %q", base, i, again.Prev, p.Prev)
			}
		}
		if p.Next == "" {
			break
		}
		if i > 200 {
			t.Fatalf("%s: no end of pages", base)
		}
		p = fetchPage(t, s, key, base+sep+"next="+url.QueryEscape(p.Next), field)
	}
	// Back through prev: the same pages, in reverse. Only without changes:
	// rows inserted between pages already read show up on the way back.
	if between != nil {
		return all
	}
	q := pages[len(pages)-1]
	for i := len(pages) - 2; i >= 0; i-- {
		q = fetchPage(t, s, key, base+sep+"prev="+url.QueryEscape(q.Prev), field)
		if !slices.Equal(q.IDs, pages[i].IDs) {
			t.Fatalf("%s: prev of page %d: %v, want %v", base, i+1, q.IDs, pages[i].IDs)
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
	// One pool's hosts, by the pool's id: another pool's are not counted.
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool2', 'other', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state) VALUES ('hz', 'hz', 'pool2', 'ready')`)
	if p := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&limit=100&poolId=pool1", "hosts"); *p.Total != 57 || slices.Contains(p.IDs, "hz") {
		t.Fatalf("poolId=pool1: total %d", *p.Total)
	}
	if p := fetchPage(t, s, key, "/v1/hosts?all=true&sort=created&poolId=pool2", "hosts"); *p.Total != 1 || p.IDs[0] != "hz" {
		t.Fatalf("poolId=pool2: %v", p.IDs)
	}
	execSQL(t, s, ctx, `DELETE FROM hosts WHERE id = 'hz'`)
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

// GET /v1/hosts with limit and a dir but no sort pages as limit alone does:
// by created, newest first, the dir ignored.
func TestHostsLimitWithDirWithoutSortIsNewestFirst(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	hosts := hostsFixture(t, s, ctx)
	var ids []string
	for id := range hosts {
		ids = append(ids, id)
	}
	newest := sorted(ids, func(id string) *int64 { v := hosts[id].created.UnixMicro(); return &v }, "desc")
	for _, q := range []string{"limit=5&dir=asc", "limit=5&dir=desc", "limit=5"} {
		p := fetchPage(t, s, key, "/v1/hosts?all=true&"+q, "hosts")
		if p.Total == nil {
			t.Fatalf("%s: no total", q)
		}
		if !slices.Equal(p.IDs, newest[:5]) || *p.Total != len(ids) || p.Next == "" {
			t.Errorf("%s: %v (total %d), want %v", q, p.IDs, *p.Total, newest[:5])
		}
		c, err := decodeCursor(p.Next)
		if err != nil || c.Sort != "created" || c.Dir != "desc" {
			t.Errorf("%s: next cursor %+v, want sort=created dir=desc", q, c)
		}
	}
	// Its cursors carry the sort: followed without dir, the whole list.
	if got := walkPages(t, s, key, "/v1/hosts?all=true&limit=5", "hosts", nil); !slices.Equal(got, newest) {
		t.Errorf("limit=5 walk:\n got %v\nwant %v", got, newest)
	}
}

// GET /v1/hosts/summary: the live hosts each caller's unfiltered host list
// shows, and the capacity and allocation of its ready and draining ones,
// exactly, and equal to the sums over that list's rows. A tenant counts
// platform hosts of shared pools but only its own placements on them.
func TestHostSummary(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	keys := poolFixture(t, s, ctx)
	poolFixtureMore(t, s, ctx)
	// A draining host of a's with one of a's Runs; a provisioning platform
	// host (not counted in capacity or allocation, though a placement names
	// it); an ended placement on hp (not live); a lost host of a's and a
	// lost platform host with a placement of each tenant's (counted live,
	// not in capacity or allocation).
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, capacity) VALUES
		('hd', 'ta', 'hd', 'p-a', 'draining', '{"cpus": 4, "memory": 400}'),
		('hprov', NULL, 'hprov', 'p-shared', 'provisioning', '{"cpus": 32, "memory": 3200}'),
		('hlost-a', 'ta', 'hlost-a', 'p-a', 'lost', '{"cpus": 128, "memory": 12800}'),
		('hlost-p', NULL, 'hlost-p', 'p-shared', 'lost', '{"cpus": 64, "memory": 6400}')`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES
		('pd', 'ta', 'ra-own', 'hd', 2, 'running', '{"cpus": 1, "memory": 50}'),
		('pprov', 'ta', 'ra', 'hprov', 2, 'assigned', '{"cpus": 5, "memory": 500}'),
		('pend', 'tb', 'rb', 'hp', 3, 'exited', '{"cpus": 7, "memory": 700}'),
		('plost-a', 'ta', 'ra', 'hlost-p', 3, 'running', '{"cpus": 9, "memory": 900}'),
		('plost-b', 'tb', 'rb', 'hlost-p', 4, 'running', '{"cpus": 11, "memory": 1100}')`)
	type sum struct {
		Live                int
		Capacity, Allocated HostResources
	}
	for who, want := range map[string]sum{
		"a":           {6, HostResources{14, 1500}, HostResources{3, 60}},
		"op?tenant=a": {6, HostResources{14, 1500}, HostResources{3, 60}},
		"b":           {5, HostResources{28, 2200}, HostResources{5, 30}},
		"op":          {8, HostResources{34, 2700}, HostResources{8, 90}},
	} {
		key, narrow, _ := strings.Cut(who, "?")
		q := ""
		if narrow != "" {
			q = "?" + narrow
		}
		var got sum
		if code := getJSON(t, s, keys[key], "/v1/hosts/summary"+q, &got); code != http.StatusOK {
			t.Fatalf("%s: GET /v1/hosts/summary: %d", who, code)
		}
		if got != want {
			t.Errorf("%s: summary %+v, want %+v", who, got, want)
		}
		var list struct{ Hosts []Host }
		if code := getJSON(t, s, keys[key], "/v1/hosts"+q, &list); code != http.StatusOK {
			t.Fatalf("%s: GET /v1/hosts: %d", who, code)
		}
		var rows sum
		for _, h := range list.Hosts {
			rows.Live++
			if h.State == "ready" || h.State == "draining" {
				rows.Capacity.CPUs += h.Capacity.CPUs
				rows.Capacity.Memory += int64(h.Capacity.Memory)
				rows.Allocated.CPUs += h.Allocated.CPUs
				rows.Allocated.Memory += int64(h.Allocated.Memory)
			}
		}
		if rows != got {
			t.Errorf("%s: summary %+v, the list's rows sum to %+v", who, got, rows)
		}
	}
	// A host named summary is still read by its id.
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state) VALUES ('hs', 'ta', 'summary', 'p-a', 'ready')`)
	var h Host
	if code := getJSON(t, s, keys["a"], "/v1/hosts/hs", &h); code != http.StatusOK || h.Name != "summary" {
		t.Errorf("GET /v1/hosts/hs: %d %q", code, h.Name)
	}
}

// GET /v1/hosts pages by the keys computed from a host's placements, its
// owner and pool, and its state, both ways, and filters by lifecycle.
func TestHostsPagedSortByLoadOwnerAndState(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'alpha'), ('t2', 'beta')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pa', 'burst', 'ec2'), ('pb', 'alpine', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running')`)
	type fh struct {
		id, tenant, pool, state, outcome string
		cpus                             float64
		held                             []float64 // live placements' cpus
		ended                            bool      // with an ended placement too
	}
	hosts := []fh{
		{"h1", "", "pa", "ready", "launched", 8, []float64{2, 2}, true},
		{"h2", "t1", "pa", "ready", "", 4, []float64{2}, false},
		{"h3", "t2", "pb", "draining", "", 4, []float64{1, 1, 1}, false},
		{"h4", "", "", "ready", "", 0, nil, false},
		{"h5", "t1", "pb", "provisioning", "requested", 8, nil, false},
		// Not ready: its placements are not counted in its sort values.
		{"h6", "", "pa", "lost", "launched", 8, []float64{4}, false},
		{"h7", "t2", "", "terminated", "launched", 8, nil, true},
		{"h8", "", "pa", "terminated", "failed", 8, nil, false},
		{"h9", "t1", "pb", "ready", "", 16, nil, true},
		{"h10", "", "pb", "terminated", "failed", 2, nil, false},
		{"h11", "t2", "pa", "ready", "", 8, []float64{8}, false},
	}
	n := 0
	for _, h := range hosts {
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, pool_id, state, capacity, launch_outcome)
			VALUES ($1, $1, nullif($2, ''), nullif($3, ''), $4, jsonb_build_object('cpus', $5::float8), nullif($6, ''))`, h.id, h.tenant, h.pool, h.state, h.cpus, h.outcome)
		for _, c := range h.held {
			n++
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES ($1, 't1', 'r1', $2, $3, 'running', jsonb_build_object('cpus', $4::float8))`,
				fmt.Sprintf("p%d", n), h.id, n, c)
		}
		if h.ended {
			n++
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources) VALUES ($1, 't1', 'r1', $2, $3, 'exited', '{"cpus": 99}')`,
				fmt.Sprintf("p%d", n), h.id, n)
		}
	}
	byID := map[string]fh{}
	var ids []string
	for _, h := range hosts {
		byID[h.id] = h
		ids = append(ids, h.id)
	}
	slices.Sort(ids)
	serving := func(h fh) bool { return h.state == "ready" || h.state == "draining" }
	num := func(v float64) *float64 { return &v }
	stateRank := map[string]float64{"provisioning": 1, "ready": 2, "draining": 3, "lost": 4, "terminated": 5}
	tenants := map[string]string{"t1": "alpha", "t2": "beta"}
	pools := map[string]string{"pa": "burst", "pb": "alpine"}
	for _, dir := range []string{"asc", "desc"} {
		for k, v := range map[string]func(string) *float64{
			"state": func(id string) *float64 {
				if h := byID[id]; h.outcome == "failed" && h.state == "terminated" {
					return num(6)
				}
				return num(stateRank[byID[id].state])
			},
			"runs": func(id string) *float64 {
				if h := byID[id]; serving(h) {
					return num(float64(len(h.held)))
				}
				return nil
			},
			"cpu": func(id string) *float64 {
				h := byID[id]
				if !serving(h) || h.cpus == 0 {
					return nil
				}
				var sum float64
				for _, c := range h.held {
					sum += c
				}
				return num(sum / h.cpus)
			},
		} {
			got := walkPages(t, s, key, "/v1/hosts?all=true&limit=3&sort="+k+"&dir="+dir, "hosts", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
		for k, v := range map[string]func(string) *string{
			"tenant": func(id string) *string {
				if n, ok := tenants[byID[id].tenant]; ok {
					return &n
				}
				return nil
			},
			"pool": func(id string) *string {
				if n, ok := pools[byID[id].pool]; ok {
					return &n
				}
				return nil
			},
		} {
			got := walkPages(t, s, key, "/v1/hosts?all=true&limit=3&sort="+k+"&dir="+dir, "hosts", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
	}
	// Lifecycle: exact sets, launch failures among the ended; the count
	// and a pool filter by name agree.
	for q, want := range map[string][]string{
		"lifecycle=live":                      {"h1", "h11", "h2", "h3", "h4", "h5", "h6", "h9"},
		"lifecycle=ended":                     {"h10", "h7", "h8"},
		"lifecycle=ended&state=launch_failed": {"h10", "h8"},
		"lifecycle=live&pool=alpine":          {"h3", "h5", "h9"},
		"all=true&pool=burst&sort=cpu":        {"h1", "h11", "h2", "h6", "h8"},
	} {
		p := fetchPage(t, s, key, "/v1/hosts?limit=100&"+q, "hosts")
		got := slices.Sorted(slices.Values(p.IDs))
		if !slices.Equal(got, want) || *p.Total != len(want) {
			t.Errorf("%s: %v (total %d), want %v", q, got, *p.Total, want)
		}
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
	got := walkPages(t, s, key, "/v1/hosts?all=true&limit=6&sort=created", "hosts", func(i int, _ page) {
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
	got := walkPages(t, s, key, "/v1/runs?limit=4&sort=created", "runs", func(i int, _ page) {
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ($1, 't1', '{}', 'submitted')`, fmt.Sprintf("new%02d", i))
		execSQL(t, s, ctx, `UPDATE runs SET state = 'failed' WHERE id = (SELECT id FROM runs WHERE state = 'succeeded' ORDER BY id LIMIT 1)`)
	})
	if len(got) != len(ids) || len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(ids) {
		t.Fatalf("walked %v, want each of %d once", got, len(ids))
	}
}

// fixtureRun is one Run of TestRunsPagedSortEveryKey, with its expected
// sort values; the clock-dependent ones as a function of the page clock.
type fixtureRun struct {
	id, tenant, name, state, pool, adapter, host string
	epoch                                        int64
	cost                                         *string
	runtime, placement                           func(clock time.Time) *float64
	placing                                      bool
}

// GET /v1/runs pages in every sort key's order, both ways, with running
// and still-starting placements whose values grow with the clock. The
// clock-dependent walks wait between pages, so a key read at the server's
// now() instead of the cursor's clock would reorder the list mid-walk.
func TestRunsPagedSortEveryKey(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'alpha'), ('t2', 'beta')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('p1', 'burst', 'ec2'), ('p2', 'alpine', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('ha', 'h-a', 'ready'), ('hb', 'h-b', 'ready'), ('hc', 'h-c', 'ready')`)
	T := time.Now().UTC().Truncate(time.Microsecond)
	at := func(sec float64) time.Time { return T.Add(time.Duration(sec * float64(time.Second))) }
	fixed := func(v float64) func(time.Time) *float64 { return func(time.Time) *float64 { return &v } }
	// Values that grow with the clock count from L, which reanchor moves
	// to just before each clock-dependent walk.
	L := T
	since := func(base, from float64) func(time.Time) *float64 {
		return func(c time.Time) *float64 {
			v := base + c.Sub(L.Add(time.Duration(from*float64(time.Second)))).Seconds()
			return &v
		}
	}
	none := func(time.Time) *float64 { return nil }
	str := func(s string) *string { return &s }
	type pl struct {
		host, state                      string
		created                          float64
		needed, workload, started, ended *float64
	}
	f := func(v float64) *float64 { return &v }
	runs := []struct {
		fixtureRun
		created   float64
		needsHost *float64
		poolID    string
		pls       []pl
		costs     [][2]string
	}{
		{fixtureRun{id: "ra", tenant: "t1", name: "zeta", state: "succeeded", pool: "burst", adapter: "generic", host: "h-b", epoch: 1,
			cost: str("3"), runtime: fixed(100), placement: fixed(15)}, -1010, nil, "p1",
			[]pl{{"hb", "exited", -1000, f(-1010), f(-995), f(-995), f(-895)}}, [][2]string{{"3", "USD"}}},
		// Running: its runtime passes ra's a second into the walk.
		{fixtureRun{id: "rb", tenant: "t2", state: "running", pool: "burst", adapter: "acp", host: "h-a", epoch: 1,
			cost: str("1"), runtime: since(0, -99.5), placement: fixed(10)}, -130, nil, "p1",
			[]pl{{"ha", "running", -120, f(-120), f(-110), f(-99.5), nil}}, [][2]string{{"1", "EUR"}, {"50", "USD"}}},
		// Queued, never ran: waiting since 5s ago.
		{fixtureRun{id: "rc", tenant: "t1", state: "submitted", runtime: none, placement: since(0, -5), placing: true}, -5, f(-5), "", nil, nil},
		// Still starting: its start passes rj's 30s a second into the walk.
		{fixtureRun{id: "rd", tenant: "t2", name: "mid", state: "starting", pool: "alpine", adapter: "generic", host: "h-a", epoch: 1,
			runtime: none, placement: since(0, -29.5), placing: true}, -40, nil, "p2",
			[]pl{{"ha", "starting", -29.5, f(-29.5), nil, nil, nil}}, nil},
		// No needed_since: the Run's creation, then the previous end. The
		// first ended without starting (start to its end); the second
		// never reported its workload's start (start to started_at).
		{fixtureRun{id: "re", tenant: "t1", state: "failed", pool: "burst", adapter: "codex", host: "h-c", epoch: 2,
			cost: str("3"), runtime: fixed(90), placement: fixed(100 + 30 + 70 + 10)}, -600, nil, "p1",
			[]pl{{"hb", "exited", -500, nil, nil, nil, f(-470)}, {"hc", "exited", -400, nil, nil, f(-390), f(-300)}},
			[][2]string{{"1", "USD"}, {"2", "USD"}}},
		// Its runner's clock is ahead: started_at in the future counts 0.
		{fixtureRun{id: "rf", tenant: "t2", state: "running", pool: "alpine", adapter: "generic", host: "h-c", epoch: 1,
			runtime: fixed(0), placement: fixed(10)}, -60, nil, "p2",
			[]pl{{"hc", "running", -60, f(-60), f(-50), f(60), nil}}, nil},
		{fixtureRun{id: "rg", tenant: "t1", state: "cancelled", adapter: "generic", cost: str("2"), runtime: none, placement: fixed(0)}, -50, nil, "",
			nil, [][2]string{{"2", "EUR"}}},
		{fixtureRun{id: "rh", tenant: "t1", name: "aardvark", state: "lost", pool: "burst", host: "h-a", epoch: 1,
			cost: str("0.75"), runtime: fixed(90), placement: fixed(10)}, -300, nil, "p1",
			[]pl{{"ha", "lost", -300, f(-300), f(-290), f(-290), f(-200)}}, [][2]string{{"0.5", "USD"}, {"0.25", "USD"}}},
		// Queued again after a placement: its wait counts since needs_host_since.
		{fixtureRun{id: "ri", tenant: "t2", state: "provisioning", pool: "alpine", adapter: "acp", host: "h-b", epoch: 1,
			runtime: fixed(90), placement: since(10, -25), placing: true}, -800, f(-25), "p2",
			[]pl{{"hb", "exited", -800, f(-800), f(-790), f(-790), f(-700)}}, nil},
		{fixtureRun{id: "rj", tenant: "t2", state: "succeeded", pool: "burst", adapter: "generic", host: "h-c", epoch: 1,
			runtime: fixed(70), placement: fixed(30)}, -900, nil, "p1",
			[]pl{{"hc", "exited", -900, f(-900), f(-870), f(-870), f(-800)}}, nil},
		// needed_since after its assignment (clock skew): waited 0, not -10.
		{fixtureRun{id: "rk", tenant: "t1", state: "stopped", adapter: "acp", host: "h-a", epoch: 1,
			cost: str("9"), runtime: fixed(85), placement: fixed(15)}, -700, nil, "",
			[]pl{{"ha", "exited", -700, f(-690), f(-685), f(-685), f(-600)}}, [][2]string{{"10", "USD"}, {"-1", "USD"}}},
		// Running, its runner never reported the workload's start: placed
		// once luxd saw it running, not still placing.
		{fixtureRun{id: "rl", tenant: "t2", state: "running", pool: "alpine", adapter: "generic", host: "h-b", epoch: 1,
			runtime: since(0, -3600), placement: fixed(60)}, -3700, nil, "p2",
			[]pl{{"hb", "running", -3660, f(-3660), nil, f(-3600), nil}}, nil},
		// Scheduled, no placement yet: ranks between submitted and provisioning.
		{fixtureRun{id: "rm", tenant: "t1", state: "scheduled", adapter: "acp", runtime: none, placement: fixed(0)}, -2, nil, "", nil, nil},
	}
	opt := func(v *float64) any {
		if v == nil {
			return nil
		}
		return at(*v)
	}
	for _, r := range runs {
		spec := `{}`
		if r.adapter != "" {
			spec = `{"workload":{"adapter":"` + r.adapter + `"}}`
		}
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, created_at, current_epoch, name, pool_id, needs_host_since)
			VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9)`, r.id, r.tenant, spec, r.state, at(r.created), r.epoch, r.name, r.poolID, opt(r.needsHost))
		for i, p := range r.pls {
			execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, created_at, needed_since, workload_started_at, started_at, ended_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, fmt.Sprintf("%s-%d", r.id, i+1), r.tenant, r.id, p.host, i+1, p.state,
				at(p.created), opt(p.needed), opt(p.workload), opt(p.started), opt(p.ended))
		}
		for _, c := range r.costs {
			execSQL(t, s, ctx, `INSERT INTO cost_lines (tenant_id, run_id, source, family, item, amount, currency, period_from, period_to, final)
				VALUES ($1, $2, 'compute', 'compute', $3, $4, $5, $6, $6, true)`, r.tenant, r.id, c[0]+c[1], c[0], c[1], T)
		}
	}
	byID := map[string]fixtureRun{}
	var ids []string
	for _, r := range runs {
		byID[r.id] = r.fixtureRun
		ids = append(ids, r.id)
	}
	slices.Sort(ids)

	// Each Run's own values, as the list shows them.
	var list struct {
		Runs []*Run `json:"runs"`
	}
	getJSON(t, s, key, "/v1/runs?limit=100", &list)
	for _, r := range list.Runs {
		want := byID[r.ID]
		if r.Placing != want.placing {
			t.Errorf("%s: placing %v, want %v", r.ID, r.Placing, want.placing)
		}
		if v := want.placement(time.Now()); v != nil && !want.placing && r.PlacementSeconds != *v {
			t.Errorf("%s: placementSeconds %v, want %v", r.ID, r.PlacementSeconds, *v)
		}
		if v := want.runtime(time.Now()); r.RuntimeSeconds < 0 || v != nil && *v == 0 && r.RuntimeSeconds != 0 {
			t.Errorf("%s: runtimeSeconds %v", r.ID, r.RuntimeSeconds)
		}
	}

	text := func(get func(r fixtureRun) string) func(string) *string {
		return func(id string) *string {
			if v := get(byID[id]); v != "" {
				return &v
			}
			return nil
		}
	}
	num := func(v float64) *float64 { return &v }
	stateOrder := []string{"submitted", "scheduled", "provisioning", "starting", "running", "stopping", "stopped", "resuming", "succeeded", "failed", "cancelled", "lost"}
	tenants := map[string]string{"t1": "alpha", "t2": "beta"}
	for _, dir := range []string{"asc", "desc"} {
		for k, v := range map[string]func(string) *string{
			"id":      func(id string) *string { return &id },
			"name":    func(id string) *string { n := cmp.Or(byID[id].name, id); return &n },
			"tenant":  text(func(r fixtureRun) string { return tenants[r.tenant] }),
			"host":    text(func(r fixtureRun) string { return r.host }),
			"pool":    text(func(r fixtureRun) string { return r.pool }),
			"adapter": text(func(r fixtureRun) string { return r.adapter }),
		} {
			got := walkPages(t, s, key, "/v1/runs?limit=3&sort="+k+"&dir="+dir, "runs", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
		for k, v := range map[string]func(string) *float64{
			"state":      func(id string) *float64 { return num(float64(slices.Index(stateOrder, byID[id].state))) },
			"placements": func(id string) *float64 { return num(float64(byID[id].epoch)) },
			"cost": func(id string) *float64 {
				if c := byID[id].cost; c != nil {
					n, _ := strconv.ParseFloat(*c, 64)
					return &n
				}
				return nil
			},
		} {
			got := walkPages(t, s, key, "/v1/runs?limit=3&sort="+k+"&dir="+dir, "runs", nil)
			if want := sorted(ids, v, dir); !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
		for _, k := range []string{"runtime", "placement"} {
			// rb's runtime and rd's start cross ra's and rj's half a second
			// after the first page.
			L = time.Now().UTC().Truncate(time.Microsecond)
			l := func(sec float64) time.Time { return L.Add(time.Duration(sec * float64(time.Second))) }
			execSQL(t, s, ctx, `UPDATE placements SET started_at = $1 WHERE id = 'rb-1'`, l(-99.5))
			execSQL(t, s, ctx, `UPDATE placements SET created_at = $1, needed_since = $1 WHERE id = 'rd-1'`, l(-29.5))
			execSQL(t, s, ctx, `UPDATE placements SET started_at = $1 WHERE id = 'rl-1'`, l(-3600))
			execSQL(t, s, ctx, `UPDATE runs SET needs_host_since = $1 WHERE id = 'rc'`, l(-5))
			execSQL(t, s, ctx, `UPDATE runs SET needs_host_since = $1 WHERE id = 'ri'`, l(-25))
			base := "/v1/runs?limit=1&sort=" + k + "&dir=" + dir
			first := fetchPage(t, s, key, base, "runs")
			c, err := decodeCursor(first.Self)
			if err != nil {
				t.Fatal(err)
			}
			clock, _ := time.Parse(time.RFC3339Nano, c.At)
			got := walkPages(t, s, key, base, "runs", func(i int, _ page) {
				if i == 0 {
					time.Sleep(1200 * time.Millisecond)
				}
			})
			want := sorted(ids, func(id string) *float64 {
				if k == "runtime" {
					return byID[id].runtime(clock)
				}
				return byID[id].placement(clock)
			}, dir)
			if !slices.Equal(got, want) {
				t.Errorf("sort=%s dir=%s:\n got %v\nwant %v", k, dir, got, want)
			}
		}
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

// A pool's events page by time (many sharing one instant) and by type, on a
// database whose default collation is linguistic (as glibc's en_US is;
// musl's compares bytes).
func TestPoolEventsPagedSort(t *testing.T) {
	s := testServerWith(t, `TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE 'en-US'`)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Types where one prefixes others, the next character '_' or a space,
	// which a linguistic collation and bytes order differently.
	types := []string{"pool.scale", "pool.scale_up", "pool.launch_failed", "pool.scale x"}
	for i := range 31 {
		execSQL(t, s, ctx, `INSERT INTO pool_events (pool_id, type, data, created_at) VALUES ('pool1', $1, '{}', $2)`,
			types[i%4], at.Add(time.Duration(i%3)*time.Second))
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

// Pool events by time while new ones arrive: now, and at the instant of a
// row already read (a tie broken by id). Every event there was is read once
// and in order, each page re-reads in place, and none repeats.
func TestPoolEventsPagedStableUnderInserts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	pad := func(x string) string { return fmt.Sprintf("%06s", x) }
	// One pool per direction, each with 40 events over 5 instants.
	for _, dir := range []string{"desc", "asc"} {
		execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ($1, $1, 'ec2')`, dir)
		for i := range 40 {
			execSQL(t, s, ctx, `INSERT INTO pool_events (pool_id, type, created_at) VALUES ($1, 'pool.scale_up', $2)`, dir, at.Add(time.Duration(i%5)*time.Second))
		}
		when := map[string]time.Time{}
		var ids []string
		for _, id := range queryOne[[]int64](t, s, `SELECT array_agg(id) FROM pool_events WHERE pool_id = $1`, dir) {
			k := fmt.Sprint(id)
			when[k] = queryOne[time.Time](t, s, `SELECT created_at FROM pool_events WHERE id = $1`, id)
			ids = append(ids, k)
		}
		var added []string
		got := walkPages(t, s, key, "/v1/pools/"+dir+"/events?owner=platform&limit=6&sort=time&dir="+dir, "events", func(i int, p page) {
			// A tie outside the page: in desc order a larger id at the
			// first row's instant is ahead of it; in asc, at the last
			// row's, behind it.
			edge := p.IDs[0]
			if dir == "asc" {
				edge = p.IDs[len(p.IDs)-1]
			}
			for _, tie := range []any{when[edge], nil} {
				id := queryOne[int64](t, s, `INSERT INTO pool_events (pool_id, type, created_at) VALUES ($1, 'pool.scale_up', coalesce($2, now())) RETURNING id`, dir, tie)
				k := fmt.Sprint(id)
				when[k] = queryOne[time.Time](t, s, `SELECT created_at FROM pool_events WHERE id = $1`, id)
				added = append(added, k)
			}
		})
		seen := map[string]bool{}
		var old []string
		for _, id := range got {
			if seen[id] {
				t.Fatalf("dir=%s: event %s read twice", dir, id)
			}
			seen[id] = true
			if !slices.Contains(added, id) {
				old = append(old, pad(id))
			}
		}
		var padded []string
		for _, id := range ids {
			padded = append(padded, pad(id))
		}
		want := sorted(padded, func(id string) *int64 { v := when[fmt.Sprint(mustAtoi(id))].UnixMicro(); return &v }, dir)
		if !slices.Equal(old, want) {
			t.Errorf("dir=%s: events there were:\n got %v\nwant %v", dir, old, want)
		}
		if dir == "desc" && len(got) != len(ids) {
			t.Errorf("desc: new events ahead of the cursor were read: %v", got)
		}
	}
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(s, "0"))
	return n
}

// getError GETs path with key: the status and the error message.
func getError(t *testing.T, s *Server, key, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var body struct {
		Error struct{ Message string }
		// huma's own validation errors.
		Detail string
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Message != "" {
		return w.Code, body.Error.Message
	}
	return w.Code, body.Detail
}

// Every way a paging request can be wrong is a 400 naming the parameter,
// never a 500 from a failed cast and never a silently different page.
func TestPagingRejects(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := operatorKey(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'running')`)
	execSQL(t, s, ctx, `INSERT INTO pool_events (pool_id, type) VALUES ('pool1', 'pool.scale_up')`)
	b64 := func(s string) string { return url.QueryEscape(base64.RawURLEncoding.EncodeToString([]byte(s))) }
	cur := func(c pageCursor) string { return url.QueryEscape(c.encode()) }
	v := func(s string) *string { return &s }
	now := time.Now().UTC().Format(time.RFC3339Nano)
	created := pageCursor{Sort: "created", Dir: "desc", V: v("2026-09-01 12:00:00+00"), ID: "h1", At: now}
	cases := []struct{ path, want string }{
		{"/v1/hosts?all=true&sort=nope", "sort: one of cpu, created, heartbeat, id, memory, name, pool, runs, state, tenant, terminated, uptime"},
		{"/v1/hosts?all=true&sort=created&limit=0", "limit: 1 to 500"},
		{"/v1/hosts?all=true&sort=created&limit=501", "limit: 1 to 500"},
		{"/v1/hosts?all=true&sort=created&limit=x", "limit: 1 to 500"},
		{"/v1/hosts?all=true&next=" + url.QueryEscape("!!!"), "next: not a cursor"},
		{"/v1/hosts?all=true&next=" + b64(`{}`), "next: not a cursor"},
		{"/v1/hosts?all=true&prev=" + b64(`nonsense`), "prev: not a cursor"},
		{"/v1/hosts?all=true&next=" + b64(`{"s":"created","d":"desc","i":""}`), "next: not a cursor"},
		{"/v1/hosts?all=true&sort=name&next=" + cur(created), "next: a cursor of another sort"},
		{"/v1/hosts?all=true&dir=asc&at=" + cur(created), "at: a cursor of another sort"},
		{"/v1/hosts?all=true&next=" + cur(created) + "&prev=" + cur(created), "at most one of next, prev and at"},
		{"/v1/hosts?all=true&sort=created&offset=-1", "offset: a count of hosts"},
		{"/v1/hosts?all=true&sort=created&offset=x", "offset: a count of hosts"},
		{"/v1/hosts?all=true&offset=5&next=" + cur(created), "offset does not go with next, prev or at"},
		{"/v1/hosts?all=true&next=" + cur(pageCursor{Sort: "zzz", Dir: "desc", ID: "h1"}), "sort: one of cpu, created, heartbeat, id, memory, name, pool, runs, state, tenant, terminated, uptime"},
		{"/v1/hosts?all=true&next=" + cur(pageCursor{Sort: "created", Dir: "up", V: v("2026-09-01 12:00:00+00"), ID: "h1"}), "dir: asc or desc"},
		// A value that is not of its sort key's type, and a bad clock.
		{"/v1/hosts?all=true&next=" + cur(pageCursor{Sort: "created", Dir: "desc", V: v("not-a-time"), ID: "h1"}), "next: not a cursor"},
		{"/v1/hosts?all=true&prev=" + cur(pageCursor{Sort: "uptime", Dir: "desc", V: v("1.5x"), ID: "h1"}), "prev: not a cursor"},
		{"/v1/hosts?all=true&at=" + cur(pageCursor{Sort: "state", Dir: "asc", V: v("2.5"), ID: "h1"}), "at: not a cursor"},
		{"/v1/hosts?all=true&next=" + cur(pageCursor{Sort: "uptime", Dir: "desc", V: v("1.5"), ID: "h1", At: "yesterday"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "created", Dir: "desc", V: v("xx"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "cost", Dir: "desc", V: v("abc"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "placements", Dir: "desc", V: v("1e3"), ID: "r1"}), "next: not a cursor"},
		// Go reads these as numbers; Postgres does not, or overflows.
		{"/v1/hosts?all=true&next=" + cur(pageCursor{Sort: "uptime", Dir: "desc", V: v("1_000.5"), ID: "h1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "cost", Dir: "desc", V: v("1/2"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "cost", Dir: "desc", V: v("1e200000"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "cost", Dir: "desc", V: v("10e131071"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?next=" + cur(pageCursor{Sort: "cost", Dir: "desc", V: v("0.01e-16382"), ID: "r1"}), "next: not a cursor"},
		{"/v1/runs?sort=created&before=2020-01-01T00:00:00Z", "before does not go with sort and cursors"},
		{"/v1/runs?sort=created&limit=201", "limit: 1 to 200"},
		{"/v1/pools/burst/events?owner=platform&next=" + cur(pageCursor{Sort: "time", Dir: "desc", V: v("x"), ID: "1"}), "next: not a cursor"},
		{"/v1/pools/burst/events?owner=platform&next=" + cur(pageCursor{Sort: "time", Dir: "desc", V: v("2026-09-01 12:00:00+00"), ID: "one"}), "next: not a cursor"},
		{"/v1/pools/burst/events?owner=platform&sort=time&before=5", "before and after (event ids) do not go with sort and cursors"},
		{"/v1/pools/burst/events?owner=platform&sort=detail", "sort: one of id, time, type"},
	}
	for _, c := range cases {
		if code, msg := getError(t, s, key, c.path); code != http.StatusBadRequest || msg != c.want {
			t.Errorf("GET %s: %d %q, want 400 %q", c.path, code, msg, c.want)
		}
	}
	// Values of each cast as Postgres prints them are cursors.
	for _, c := range []pageCursor{
		{Sort: "created", Dir: "desc", V: v("2026-09-01 12:00:00.123456+00"), ID: "h1"},
		{Sort: "created", Dir: "desc", V: v("2026-09-01 12:00:00+05:30"), ID: "h1"},
		{Sort: "uptime", Dir: "desc", V: v("1.5e+06"), ID: "h1", At: now},
		{Sort: "state", Dir: "asc", V: v("2"), ID: "h1"},
		{Sort: "state", Dir: "asc", ID: "h1"},
		// Postgres' specials cast too.
		{Sort: "created", Dir: "desc", V: v("infinity"), ID: "h1"},
	} {
		if code, msg := getError(t, s, key, "/v1/hosts?all=true&next="+cur(c)); code != http.StatusOK {
			t.Errorf("cursor %+v: %d %s", c, code, msg)
		}
	}
	for _, c := range []string{"12.500", "NaN", "-Infinity", ".5", "1e99999", "0.01e-16381"} {
		if code, msg := getError(t, s, key, "/v1/runs?next="+cur(pageCursor{Sort: "cost", Dir: "desc", V: v(c), ID: "r1"})); code != http.StatusOK {
			t.Errorf("cost cursor %s: %d %s", c, code, msg)
		}
	}
	// huma validates the enums before the handler.
	for _, path := range []string{"/v1/hosts?all=true&sort=created&dir=up", "/v1/hosts?lifecycle=gone"} {
		if code, _ := getError(t, s, key, path); code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s: %d, want 422", path, code)
		}
	}
}
