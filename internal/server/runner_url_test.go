package server

import (
	"cmp"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

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
