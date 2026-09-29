package server

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
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
			if err := s.launch(ctx, prov, pl, nil); err != nil {
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
			if err := s.launch(ctx, &fakeLaunchProvider{launched: want}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}, nil); err != nil {
				t.Fatal(err)
			}
			var got Launched
			err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT provider_id, instance_type, zone, market FROM hosts WHERE pool_id = 'pool1'`).
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
	if err := s.launch(ctx, &fakeLaunchProvider{launched: Launched{ProviderID: "i-bare"}}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}, nil); err != nil {
		t.Fatal(err)
	}
	var providerID string
	var instanceType, zone, market *string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_id, instance_type, zone, market FROM hosts WHERE pool_id = 'pool1'`).
			Scan(&providerID, &instanceType, &zone, &market)
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerID != "i-bare" || instanceType != nil || zone != nil || market != nil {
		t.Fatalf("host row: provider_id %q instance_type %v zone %v market %v, want i-bare and three NULLs", providerID, instanceType, zone, market)
	}
}

// GET /v1/hosts and /v1/hosts/{id} return each visible host's instance type,
// zone and market, as they return providerId, and omit the ones the provider
// did not report. A tenant sees the platform's hosts and its own; an operator
// sees every host.
func TestHostAPIReturnsHostFacts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1'), ('t2', 't2')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h-static', 'static-1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state, provider_id, instance_type, zone, market) VALUES
		('h-t1', 't1', 't1-host', 'ready', 'i-t1', 'm7i.large', 'eu-west-1b', 'on-demand'),
		('h-t2', 't2', 't2-host', 'ready', 'i-t2', 'r7g.large', 'eu-west-1a', 'spot')`)
	keys := map[string]string{"tenant": ids.Secret("luxk"), "operator": ids.Secret("luxk")}
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES
		('k1', 't1', 'k', $1, ARRAY['read']), ('ko', NULL, 'o', $2, ARRAY['operator'])`, ids.Hash(keys["tenant"]), ids.Hash(keys["operator"]))

	type facts map[string]any
	want := map[string]facts{
		"h-static": {},
		"h-t1":     {"instanceType": "m7i.large", "zone": "eu-west-1b", "market": "on-demand"},
		"h-t2":     {"instanceType": "r7g.large", "zone": "eu-west-1a", "market": "spot"},
	}
	platform := []string{"h-static"}
	for _, l := range []struct {
		launched Launched
		want     facts
	}{
		{Launched{ProviderID: "i-spot", InstanceType: "c7g.xlarge", Zone: "eu-west-1c", Market: MarketSpot},
			facts{"instanceType": "c7g.xlarge", "zone": "eu-west-1c", "market": "spot"}},
		{Launched{ProviderID: "i-od", InstanceType: "m7i.2xlarge", Zone: "eu-west-1a", Market: MarketOnDemand},
			facts{"instanceType": "m7i.2xlarge", "zone": "eu-west-1a", "market": "on-demand"}},
		{Launched{ProviderID: "i-bare"}, facts{}},
	} {
		if err := s.launch(ctx, &fakeLaunchProvider{launched: l.launched}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2"}, nil); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM hosts WHERE provider_id = $1`, l.launched.ProviderID).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		want[id] = l.want
		platform = append(platform, id)
	}

	// factsOf keeps only the fact keys present in a response host, so an
	// omitted key and one serialized as "" or null differ.
	factsOf := func(h map[string]any) facts {
		f := facts{}
		for _, k := range []string{"instanceType", "zone", "market"} {
			if v, ok := h[k]; ok {
				f[k] = v
			}
		}
		return f
	}
	visible := map[string][]string{
		"tenant":   append([]string{"h-t1"}, platform...),
		"operator": append([]string{"h-t1", "h-t2"}, platform...),
	}
	for who, key := range keys {
		t.Run(who, func(t *testing.T) {
			var list struct {
				Hosts []map[string]any `json:"hosts"`
			}
			if code := getJSON(t, s, key, "/v1/hosts", &list); code != http.StatusOK {
				t.Fatalf("GET /v1/hosts: %d", code)
			}
			got := map[string]facts{}
			for _, h := range list.Hosts {
				got[h["id"].(string)] = factsOf(h)
			}
			wantList := map[string]facts{}
			for _, id := range visible[who] {
				wantList[id] = want[id]
			}
			if !reflect.DeepEqual(got, wantList) {
				t.Errorf("GET /v1/hosts: hosts and facts\n got  %v\n want %v", got, wantList)
			}
			for _, id := range visible[who] {
				var h map[string]any
				if code := getJSON(t, s, key, "/v1/hosts/"+id, &h); code != http.StatusOK {
					t.Fatalf("GET /v1/hosts/%s: %d", id, code)
				}
				if f := factsOf(h); h["id"] != id || !reflect.DeepEqual(f, want[id]) {
					t.Errorf("GET /v1/hosts/%s: id %v facts %v, want %v", id, h["id"], f, want[id])
				}
			}
			if who == "tenant" {
				var h map[string]any
				if code := getJSON(t, s, key, "/v1/hosts/h-t2", &h); code != http.StatusNotFound {
					t.Errorf("GET /v1/hosts/h-t2 as t1: %d, want 404", code)
				}
			}
		})
	}
}
