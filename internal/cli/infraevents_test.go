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

// A rename's event says from and to, what followed, and a kept default.
func TestPoolRenamedEventLine(t *testing.T) {
	e := server.LifecycleEvent{Type: "pool.renamed", Count: 1, Data: map[string]any{
		"from": "burst", "to": "burst-eu", "hosts": 2, "runs": 1, "instances": 2, "isDefault": true}}
	line := eventLine(e)
	if !strings.Contains(line, "burst → burst-eu: 2 hosts, 1 Runs, 2 instances followed; still the default") {
		t.Fatalf("pool.renamed line %q", line)
	}
	e.Data["isDefault"] = false
	if line := eventLine(e); strings.Contains(line, "default") {
		t.Fatalf("a non-default rename's line %q", line)
	}
}
