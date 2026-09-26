package server

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// fakeLaunchProvider records the env launch passed it and answers with
// launched (i-fake when unset); Terminate and Instances are unused here.
type fakeLaunchProvider struct {
	env      map[string]string
	launched Launched
}

func (p *fakeLaunchProvider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (Launched, error) {
	p.env = env
	if p.launched.ProviderID == "" {
		return Launched{ProviderID: "i-fake"}, nil
	}
	return p.launched, nil
}

func (p *fakeLaunchProvider) Terminate(ctx context.Context, template json.RawMessage, providerID string) error {
	return nil
}

func (p *fakeLaunchProvider) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]Instance, error) {
	return nil, nil
}

// launch puts RunnerURL into LUX_URL for the runner's env, defaulting to
// PublicURL when RunnerURL is unset (server.go's cfg.RunnerURL =
// cmp.Or(cfg.RunnerURL, cfg.PublicURL), applied in New).
func TestLaunchSetsLuxURLFromRunnerURL(t *testing.T) {
	cases := []struct {
		name                 string
		publicURL, runnerURL string
		wantLuxURL           string
	}{
		{"RunnerURL set: used over PublicURL", "https://public.example", "http://10.0.1.10:7070", "http://10.0.1.10:7070"},
		{"RunnerURL unset: falls back to PublicURL", "https://public.example", "", "https://public.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			s.cfg.PublicURL = c.publicURL
			s.cfg.RunnerURL = cmp.Or(c.runnerURL, c.publicURL)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
			pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}
			prov := &fakeLaunchProvider{}
			if err := s.launch(ctx, prov, pl); err != nil {
				t.Fatal(err)
			}
			if prov.env["LUX_URL"] != c.wantLuxURL {
				t.Errorf("LUX_URL = %q, want %q", prov.env["LUX_URL"], c.wantLuxURL)
			}
		})
	}
}

// launch stores what the provider says it started: id, instance type, zone
// and market, on the host row.
func TestLaunchStoresHostFacts(t *testing.T) {
	for _, want := range []Launched{
		{ProviderID: "i-od", InstanceType: "m7i.2xlarge", Zone: "eu-west-1a", Market: MarketOnDemand},
		{ProviderID: "i-spot", InstanceType: "c7g.xlarge", Zone: "eu-west-1c", Market: MarketSpot},
	} {
		t.Run(want.Market, func(t *testing.T) {
			s := testServer(t)
			ctx := context.Background()
			execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
			if err := s.launch(ctx, &fakeLaunchProvider{launched: want}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}); err != nil {
				t.Fatal(err)
			}
			var got Launched
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT provider_id, instance_type, zone, market FROM hosts WHERE pool = 'burst'`).
					Scan(&got.ProviderID, &got.InstanceType, &got.Zone, &got.Market)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("host row: got %+v, want %+v", got, want)
			}
		})
	}
}

// A provider that reports only the instance id leaves the other host facts
// NULL, not empty strings.
func TestLaunchMissingHostFactsAreNull(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	if err := s.launch(ctx, &fakeLaunchProvider{launched: Launched{ProviderID: "i-bare"}}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}); err != nil {
		t.Fatal(err)
	}
	var providerID string
	var instanceType, zone, market *string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_id, instance_type, zone, market FROM hosts WHERE pool = 'burst'`).
			Scan(&providerID, &instanceType, &zone, &market)
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerID != "i-bare" || instanceType != nil || zone != nil || market != nil {
		t.Fatalf("host row: provider_id %q instance_type %v zone %v market %v, want i-bare and three NULLs", providerID, instanceType, zone, market)
	}
}

// GET /v1/hosts and /v1/hosts/{id} return a launched platform host's facts
// to a tenant and to an operator, as they return providerId; a host that
// registered itself omits all three.
func TestHostAPIReturnsHostFacts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h-static', 'static-1', 'ready')`)
	keys := map[string]string{"tenant": ids.Secret("luxk"), "operator": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'k', $1, ARRAY['read']), ('ko', NULL, 'o', $2, ARRAY['operator'])`, ids.Hash(keys["tenant"]), ids.Hash(keys["operator"]))
	launched := Launched{ProviderID: "i-spot", InstanceType: "c7g.xlarge", Zone: "eu-west-1c", Market: MarketSpot}
	if err := s.launch(ctx, &fakeLaunchProvider{launched: launched}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}); err != nil {
		t.Fatal(err)
	}
	var launchedID string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM hosts WHERE pool = 'burst'`).Scan(&launchedID)
	}); err != nil {
		t.Fatal(err)
	}

	get := func(t *testing.T, key, path string, out any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, where string, h map[string]any) {
		t.Helper()
		switch h["id"] {
		case launchedID:
			if h["instanceType"] != "c7g.xlarge" || h["zone"] != "eu-west-1c" || h["market"] != "spot" {
				t.Errorf("%s: launched host facts %v/%v/%v, want c7g.xlarge/eu-west-1c/spot", where, h["instanceType"], h["zone"], h["market"])
			}
		case "h-static":
			for _, k := range []string{"instanceType", "zone", "market"} {
				if v, ok := h[k]; ok {
					t.Errorf("%s: static host has %s = %v, want it omitted", where, k, v)
				}
			}
		default:
			t.Errorf("%s: unexpected host %v", where, h["id"])
		}
	}
	for who, key := range keys {
		t.Run(who, func(t *testing.T) {
			var list struct {
				Hosts []map[string]any `json:"hosts"`
			}
			get(t, key, "/v1/hosts", &list)
			if len(list.Hosts) != 2 {
				t.Fatalf("GET /v1/hosts: %d hosts, want 2", len(list.Hosts))
			}
			for _, h := range list.Hosts {
				check(t, "list", h)
			}
			for _, id := range []string{launchedID, "h-static"} {
				var h map[string]any
				get(t, key, "/v1/hosts/"+id, &h)
				check(t, "get", h)
			}
		})
	}
}
