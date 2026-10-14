package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/ids"
)

// A read with no res picks its resolution by the part of the range its host,
// Run or pool has existed in: one created 40 minutes ago reads raw at 7d,
// one created 8 days ago hourly. An explicit res is kept, and the response's
// from and to stay the range asked for.
func TestHistoryResolutionFromCreation(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	ages := map[string]string{"young": "40 minutes", "minute": "24 hours", "old": "8 days"}
	for id, age := range ages {
		execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, created_at) VALUES ($1, $1, 'ready', now() - $2::interval)`, id, age)
		execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, created_at) VALUES ($1, 't1', '{}', 'running', now() - $2::interval)`, id, age)
		execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, created_at) VALUES ($1, $1, 'static', now() - $2::interval)`, id, age)
		// Raw samples within the young subject's lifetime.
		for _, ago := range []string{"30 minutes", "20 minutes", "10 minutes"} {
			execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds) VALUES ($1, 0, now() - $2::interval, 1)`, id, ago)
			execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds) VALUES ($1, 1, 't1', 0, now() - $2::interval, 1)`, id, ago)
			execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, running) VALUES ($1, '', 0, now() - $2::interval, 1)`, id, ago)
		}
		for _, ago := range []string{"3 hours", "2 hours", "1 hour"} {
			execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds) VALUES ($1, 60, now() - $2::interval, 1)`, id, ago)
			execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds) VALUES ($1, 1, 't1', 60, now() - $2::interval, 1)`, id, ago)
			execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, running) VALUES ($1, '', 60, now() - $2::interval, 1)`, id, ago)
		}
	}
	type answer struct {
		From, To   time.Time
		Resolution int
		Samples    []struct{}
	}
	read := func(path string) answer {
		t.Helper()
		var a answer
		if code := getJSON(t, s, key, path, &a); code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		return a
	}
	// The operator reads the Run as its tenant's.
	paths := map[string]string{"host": "/v1/hosts/%s/history?", "run": "/v1/runs/%s/history?tenant=t1&", "pool": "/v1/pools/%s/metrics?"}
	for kind, path := range paths {
		at := func(id, q string) answer { return read(fmt.Sprintf(path, id) + "since=7d" + q) }
		young := at("young", "")
		if young.Resolution != 0 || len(young.Samples) != 3 {
			t.Errorf("%s created 40m ago at 7d: resolution %d, %d samples; want 0 and 3", kind, young.Resolution, len(young.Samples))
		}
		if d := young.To.Sub(young.From); d != 7*24*time.Hour {
			t.Errorf("%s: from..to %v, want the 7d asked for", kind, d)
		}
		minute := at("minute", "")
		if minute.Resolution != 60 || len(minute.Samples) != 3 {
			t.Errorf("%s created 24h ago at 7d: resolution %d, %d samples; want 60 and 3", kind, minute.Resolution, len(minute.Samples))
		}
		if d := minute.To.Sub(minute.From); d != 7*24*time.Hour {
			t.Errorf("%s minute: from..to %v, want the 7d asked for", kind, d)
		}
		if old := at("old", ""); old.Resolution != 3600 {
			t.Errorf("%s created 8d ago at 7d: resolution %d, want 3600", kind, old.Resolution)
		}
		if explicit := at("young", "&res=3600"); explicit.Resolution != 3600 {
			t.Errorf("%s created 40m ago with res=3600: resolution %d, want 3600", kind, explicit.Resolution)
		}
	}
	tenantKey := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('kt', 't1', 'reader', $1, ARRAY['read'])`, ids.Hash(tenantKey))
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, tenant_id, state, created_at) VALUES ('own', 'own', 't1', 'ready', now() - interval '40 minutes'), ('foreign', 'foreign', 't2', 'ready', now() - interval '40 minutes')`)
	execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds) VALUES ('own', 0, now() - interval '10 minutes', 1)`)
	for _, c := range []struct {
		id   string
		code int
	}{{"own", http.StatusOK}, {"young", http.StatusForbidden}, {"foreign", http.StatusNotFound}} {
		var a answer
		if code := getJSON(t, s, tenantKey, "/v1/hosts/"+c.id+"/history?since=7d", &a); code != c.code {
			t.Errorf("tenant reading %s: status %d, want %d", c.id, code, c.code)
		}
		if c.code == http.StatusOK && (a.Resolution != 0 || len(a.Samples) != 1) {
			t.Errorf("tenant reading own young host: resolution %d, %d samples; want 0 and 1", a.Resolution, len(a.Samples))
		}
	}
}

func TestHistoryResolutionHistoricalWindows(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(key))
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-5 * 24 * time.Hour)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state, created_at) VALUES ('past', 'past', 'ready', $1)`, created)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, created_at) VALUES ('past', 't1', '{}', 'running', $1)`, created)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, created_at) VALUES ('past', 'past', 'static', $1)`, created)
	to := now.Add(-3 * 24 * time.Hour)
	from := to.Add(-time.Hour)
	var sampleTimes []time.Time
	for _, offset := range []time.Duration{10 * time.Minute, 30 * time.Minute, 50 * time.Minute} {
		at := from.Add(offset)
		sampleTimes = append(sampleTimes, at)
		// Only minute samples remain for this window; raw retention is 48h.
		execSQL(t, s, ctx, `INSERT INTO host_samples (host_id, res, at, cpu_seconds) VALUES ('past', 60, $1, 1)`, at)
		execSQL(t, s, ctx, `INSERT INTO placement_samples (run_id, epoch, tenant_id, res, at, cpu_seconds) VALUES ('past', 1, 't1', 60, $1, 1)`, at)
		execSQL(t, s, ctx, `INSERT INTO pool_samples (pool_id, tenant_id, res, at, running) VALUES ('past', '', 60, $1, 1)`, at)
	}
	for kind, path := range map[string]string{
		"host": "/v1/hosts/past/history?",
		"run":  "/v1/runs/past/history?tenant=t1&",
		"pool": "/v1/pools/past/metrics?",
	} {
		for _, window := range []struct {
			name     string
			from, to time.Time
			res      int
			samples  []time.Time
		}{
			{"before creation", created.Add(-2 * time.Hour), created.Add(-time.Hour), 60, nil},
			{"retained minutes", from, to, 60, sampleTimes},
		} {
			t.Run(kind+"/"+window.name, func(t *testing.T) {
				var a struct {
					From, To   time.Time
					Resolution int
					Samples    []struct{ At time.Time }
				}
				query := "from=" + window.from.Format(time.RFC3339) + "&to=" + window.to.Format(time.RFC3339)
				if code := getJSON(t, s, key, path+query, &a); code != http.StatusOK {
					t.Fatalf("status %d, want 200", code)
				}
				if !a.From.Equal(window.from) || !a.To.Equal(window.to) {
					t.Errorf("bounds %v..%v, want %v..%v", a.From, a.To, window.from, window.to)
				}
				if a.Resolution != window.res || len(a.Samples) != len(window.samples) {
					t.Fatalf("resolution %d, %d samples; want %d and %d", a.Resolution, len(a.Samples), window.res, len(window.samples))
				}
				for i, at := range window.samples {
					if !a.Samples[i].At.Equal(at) {
						t.Errorf("sample %d at %v, want %v", i, a.Samples[i].At, at)
					}
				}
			})
		}
	}
}
