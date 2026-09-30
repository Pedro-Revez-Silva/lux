package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// --platform asks for the platform's pool of that name.
func TestPoolEventsPlatform(t *testing.T) {
	f := &fakeLuxd{bodies: map[string]any{"/v1/pools/burst/events": map[string]any{"events": []server.LifecycleEvent{}}}}
	for _, c := range []struct {
		args  []string
		owner bool
	}{
		{[]string{"pools", "events", "burst"}, false},
		{[]string{"pools", "events", "burst", "--platform"}, true},
	} {
		f.seen = nil
		if _, err := runCLI(t, f, c.args...); err != nil {
			t.Fatal(err)
		}
		if len(f.seen) != 1 || strings.Contains(f.seen[0], "owner=platform") != c.owner {
			t.Errorf("%v requested %v", c.args, f.seen)
		}
	}
}

// pagedLuxd serves n events of one owner newest first, a page of ?limit=
// at a time before ?before=, as luxd does.
type pagedLuxd struct {
	n    int
	seen []string
}

func (p *pagedLuxd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen = append(p.seen, r.URL.RequestURI())
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before := p.n + 1
	if b := r.URL.Query().Get("before"); b != "" {
		before, _ = strconv.Atoi(b)
	}
	evs := []server.LifecycleEvent{}
	for id := before - 1; id >= 1 && len(evs) < limit; id-- {
		evs = append(evs, server.LifecycleEvent{ID: int64(id), Type: "host.x", Count: 1})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": evs})
}

func TestInfraEventsLimit(t *testing.T) {
	for _, limit := range []string{"0", "1001", "-1"} {
		p := &pagedLuxd{n: 5}
		_, err := runCLI(t, p, "hosts", "events", "h1", "--limit", limit, "--all")
		if err == nil || !strings.Contains(err.Error(), "--limit") {
			t.Errorf("--limit %s: %v, want a --limit error", limit, err)
		}
		if len(p.seen) != 0 {
			t.Errorf("--limit %s requested %v", limit, p.seen)
		}
	}
	// --all pages through every event, newest first, a --limit at a time.
	p := &pagedLuxd{n: 7}
	out, err := runCLI(t, p, "-o", "json", "hosts", "events", "h1", "--limit", "3", "--all")
	if err != nil {
		t.Fatal(err)
	}
	var evs []server.LifecycleEvent
	if err := json.Unmarshal([]byte(out), &evs); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, e := range evs {
		ids = append(ids, e.ID)
	}
	if want := []int64{7, 6, 5, 4, 3, 2, 1}; !slices.Equal(ids, want) || len(p.seen) != 3 {
		t.Fatalf("--all gave %v in %d requests (%v), want %v in 3", ids, len(p.seen), p.seen, want)
	}
}

// eventOf decodes data as the API client does, so numbers are float64.
func eventOf(t *testing.T, typ, data string) server.LifecycleEvent {
	t.Helper()
	e := server.LifecycleEvent{Type: typ, Count: 1}
	if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEventLineCapacity(t *testing.T) {
	for _, c := range []struct {
		name, typ, data, want string
	}{
		{"scale-up before capacity planning", "pool.scale_up",
			`{"hosts":2,"reason":"waiting runs","waiting":2,"warm":0,"min":0,"max":10,"total":1,"idle":0,"provisioning":1}`,
			"+2 host(s) for waiting runs: 2 waiting, warm 0, min 0, max 10; had 1 (0 idle, 1 provisioning)"},
		{"scale-up from observed capacity", "pool.scale_up",
			`{"hosts":1,"reason":"waiting runs","waiting":3,"warm":0,"min":0,"max":10,"total":2,"idle":1,"provisioning":1,
			  "ready":1,"starting":1,"planned":1,"unmet":0,"blocked":1,
			  "expected":{"capacity":{"cpus":8,"memory":34359738368,"disk":0,"runs":4},"observations":3},
			  "deficits":[{"run":"r4","stage":"prerequisite","blockers":[{"reason":"snapshot upload pending"}]}],
			  "exhausted":[{"run":"r3","host":"h1","stage":"ready","blockers":[
			    {"resource":"memory","requested":17179869184,"used":25769803776,"capacity":34359738368,"available":8589934592},
			    {"resource":"cpus","requested":4,"used":6,"capacity":8,"available":2}]}],
			  "ineligible":[{"host":"h2","reason":"draining"}],"omitted":2}`,
			"+1 host(s) for waiting runs: 3 waiting, warm 0, min 0, max 10; had 2 (1 idle, 1 provisioning); " +
				"plan: 1 ready, 1 starting, 1 planned, 0 unmet, 1 blocked; " +
				"new host cpus 8, memory 32.0 GiB, disk unlimited, runs 4 from 3 observation(s); " +
				"deficits: r4 prerequisite [snapshot upload pending]; " +
				"exhausted: h1 (ready) for r3 [memory requested 16.0 GiB, used 24.0 GiB, capacity 32.0 GiB, available 8.0 GiB; cpus requested 4, used 6, capacity 8, available 2]; " +
				"ineligible: h2 draining; 2 more omitted"},
		{"cold pool bootstraps one host", "pool.scale_up",
			`{"hosts":1,"reason":"waiting runs","waiting":1,"warm":0,"min":0,"max":0,"total":0,"idle":0,"provisioning":0,
			  "ready":0,"starting":0,"planned":0,"unmet":1,"blocked":0,"expected":null,
			  "unknown":"no registered host observations for current template",
			  "deficits":[{"run":"r1","stage":"new_host","blockers":[{"reason":"new host capacity unknown"}]}]}`,
			"+1 host(s) for waiting runs: 1 waiting, warm 0, min 0, max 0; had 0 (0 idle, 0 provisioning); " +
				"plan: 0 ready, 0 starting, 0 planned, 1 unmet, 0 blocked; " +
				"new host capacity unknown: no registered host observations for current template; " +
				"deficits: r1 new host [new host capacity unknown]"},
		{"blocked scale-up", "pool.scale_blocked",
			`{"waiting":1,"total":0,"max":2,"ready":0,"starting":0,"planned":0,"unmet":1,"blocked":0,
			  "expected":{"capacity":{"cpus":2,"memory":0,"disk":0,"runs":0},"observations":1},
			  "deficits":[{"run":"r1","stage":"new_host","blockers":[{"resource":"cpus","requested":4,"used":0,"capacity":2,"available":2}]}]}`,
			"no host launched: 1 waiting, had 0, max 2; plan: 0 ready, 0 starting, 0 planned, 1 unmet, 0 blocked; " +
				"new host cpus 2, memory unlimited, disk unlimited, runs unlimited from 1 observation(s); " +
				"deficits: r1 new host [cpus requested 4, used 0, capacity 2, available 2]"},
		{"probe scale-up", "pool.scale_up",
			`{"hosts":1,"reason":"waiting runs","waiting":1,"warm":0,"min":0,"max":0,"total":0,"idle":0,"provisioning":0,
			  "ready":0,"starting":0,"planned":0,"unmet":1,"blocked":0,"probe":true,
			  "expected":{"capacity":{"cpus":2,"memory":0,"disk":0,"runs":0},"observations":1}}`,
			"+1 host(s) for waiting runs: 1 waiting, warm 0, min 0, max 0; had 0 (0 idle, 0 provisioning); " +
				"plan: 0 ready, 0 starting, 0 planned, 1 unmet, 0 blocked; probe: one host to re-observe capacity no expected host fits; " +
				"new host cpus 2, memory unlimited, disk unlimited, runs unlimited from 1 observation(s)"},
		{"exhausted host", "host.capacity_decision",
			`{"pool":"burst","stage":"ready","decision":"exhausted","blockers":[
			  {"resource":"disk","requested":10737418240,"used":107374182400,"capacity":107374182400,"available":0},
			  {"resource":"runs","requested":1,"used":4,"capacity":4,"available":0}]}`,
			"exhausted (ready) in pool burst: disk requested 10.0 GiB, used 100.0 GiB, capacity 100.0 GiB, available 0 B; runs requested 1, used 4, capacity 4, available 0"},
		{"constraint-blocked starting host", "host.capacity_decision",
			`{"pool":"burst","stage":"starting","decision":"blocked","blockers":[{"reason":"required labels do not match"}]}`,
			"blocked (starting) in pool burst: required labels do not match"},
		{"ineligible host", "host.capacity_decision",
			`{"pool":"burst","stage":"ready","decision":"ineligible","reason":"heartbeat stale"}`,
			"ineligible (ready) in pool burst: heartbeat stale"},
		{"reserved host", "host.capacity_decision",
			`{"pool":"burst","stage":"starting","decision":"reserved"}`,
			"reserved (starting) in pool burst"},
		{"placement with resources", "pool.placement",
			`{"run":"r1","epoch":2,"host":"h1","resources":{"cpus":2,"memory":4294967296,"disk":21474836480}}`,
			"r1 epoch 2 on h1 (cpus 2, memory 4.0 GiB, disk 20.0 GiB)"},
		{"placement without resources", "host.placement_assigned",
			`{"run":"r1","epoch":1,"host":"h1"}`,
			"r1 epoch 1 on h1"},
	} {
		if got := eventLine(eventOf(t, c.typ, c.data)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
