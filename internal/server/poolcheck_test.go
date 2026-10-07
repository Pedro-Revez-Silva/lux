package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// checkingProvider answers Check as EC2's dry run would: an error for a
// launch template it does not know (or for every template, with down),
// and counts the checks and keeps the last one's tags. during, when set,
// runs inside the next Check (once), as another writer racing the set.
type checkingProvider struct {
	fakeLaunchProvider
	known    map[string]bool
	down     bool
	checks   int
	lastTags map[string]string
	during   func()
}

func (p *checkingProvider) Check(_ context.Context, template json.RawMessage, tags map[string]string) error {
	p.checks++
	p.lastTags = tags
	if during := p.during; during != nil {
		p.during = nil
		during()
	}
	if p.down {
		return errors.New("ec2 RunInstances: dial tcp: connection refused")
	}
	var t struct {
		LaunchTemplate string `json:"launchTemplate"`
	}
	if err := json.Unmarshal(template, &t); err != nil {
		return err
	}
	if !p.known[t.LaunchTemplate] {
		return fmt.Errorf("InvalidLaunchTemplateName.NotFound: The specified launch template, with template name %s, does not exist.", t.LaunchTemplate)
	}
	return nil
}

func storedTemplate(t *testing.T, s *Server, ctx context.Context, name string) map[string]any {
	t.Helper()
	var tmpl map[string]any
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT template FROM pools WHERE tenant_id = 't1' AND name = $1`, name).Scan(&tmpl)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// poolRowJSON is the whole pools row of t1's pool name, every column.
func poolRowJSON(t *testing.T, s *Server, ctx context.Context, name string) string {
	t.Helper()
	var row string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT to_jsonb(p)::text FROM pools p WHERE tenant_id = 't1' AND name = $1`, name).Scan(&row)
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

// poolEventsJSON is every pool event, in order.
func poolEventsJSON(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	var events string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id), '[]')::text FROM pool_events e`).Scan(&events)
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

// An ec2 pool is set only if its template would launch: a launch template
// EC2 does not know is a 422 naming EC2's code, and the pool keeps what it
// had. An unchanged template is not checked again.
func TestPutPoolChecksTheTemplateLaunches(t *testing.T) {
	s := testServer(t)
	prov := &checkingProvider{known: map[string]bool{"lux-runner": true}}
	s.cfg.Providers = map[string]Provider{"ec2": prov}
	ctx := context.Background()
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	good := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner"}
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 4, Template: good})); err != nil {
		t.Fatalf("a template that launches was refused: %v", err)
	}
	if got := storedTemplate(t, s, ctx, "arm64"); got["launchTemplate"] != "lux-runner" {
		t.Fatalf("stored %v", got)
	}

	gone := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner-arm64"}
	rowBefore, eventsBefore := poolRowJSON(t, s, ctx, "arm64"), poolEventsJSON(t, s, ctx)
	for _, name := range []string{"arm64", "fresh"} {
		_, err := s.putPool(ctx, poolIn(Pool{Name: name, Provider: "ec2", MaxHosts: 10, WarmHosts: 2, Template: gone}))
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity || he.Code != "invalid_pool" ||
			!strings.HasPrefix(he.Message, "template: ec2 cannot launch it: InvalidLaunchTemplateName.NotFound: ") {
			t.Fatalf("%s: err %v, want 422 invalid_pool naming InvalidLaunchTemplateName.NotFound", name, err)
		}
	}
	if got := poolRowJSON(t, s, ctx, "arm64"); got != rowBefore {
		t.Errorf("the refused set changed the pool:\nbefore %s\nafter  %s", rowBefore, got)
	}
	if got := poolEventsJSON(t, s, ctx); got != eventsBefore {
		t.Errorf("the refused set wrote pool events:\nbefore %s\nafter  %s", eventsBefore, got)
	}
	if got := storedTemplate(t, s, ctx, "fresh"); got != nil {
		t.Errorf("the refused set created a pool: %v", got)
	}

	// EC2 unreachable: the same template set again (other fields changed)
	// is not checked; a changed one is refused.
	prov.down = true
	checks := prov.checks
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 6, Template: map[string]any{"launchTemplate": "lux-runner", "region": "eu-north-1"}})); err != nil {
		t.Fatalf("an unchanged template was checked: %v", err)
	}
	if prov.checks != checks {
		t.Errorf("%d checks for an unchanged template", prov.checks-checks)
	}
	changed := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner", "spot": true}
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", Template: changed})); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("a changed template with EC2 down: err %v, want refused", err)
	}

	// A static pool has no check.
	if _, err := s.putPool(ctx, poolIn(Pool{Name: "metal", Provider: "static"})); err != nil {
		t.Errorf("a static pool: %v", err)
	}
}

// The check carries a launch's tags: this deployment's id, the pool's name
// and the id it is stored under (the one a create inserts), and a host's
// Name and lux:host.
func TestPutPoolChecksWithTheLaunchTags(t *testing.T) {
	s := testServer(t)
	prov := &checkingProvider{known: map[string]bool{"lux-runner": true}}
	s.cfg.Providers = map[string]Provider{"ec2": prov}
	ctx := context.Background()
	var deployment string
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&deployment)
	}); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	poolID := func() string {
		var id string
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM pools WHERE tenant_id = 't1' AND name = 'arm64'`).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i, tmpl := range []map[string]any{
		{"region": "eu-north-1", "launchTemplate": "lux-runner"},               // creates
		{"region": "eu-north-1", "launchTemplate": "lux-runner", "spot": true}, // updates
	} {
		if _, err := s.putPool(ctx, poolIn(Pool{Name: "arm64", Provider: "ec2", MaxHosts: 4, Template: tmpl})); err != nil {
			t.Fatal(err)
		}
		got := prov.lastTags
		want := LaunchTags(deployment, poolID(), "arm64", got[tagHost])
		if !maps.Equal(got, want) || !strings.HasPrefix(got[tagHost], "host_") {
			t.Errorf("set %d: check tags %v, want %v", i, got, want)
		}
	}
}

// A set whose pool another write creates, or replaces under a new id,
// while its template is checked would store the pool under an id the
// check's lux:pool-id did not carry: it is refused, 409 pool_changed, and
// the other write's pool and events stay as that write left them.
func TestPutPoolRefusesAPoolChangedDuringItsCheck(t *testing.T) {
	s := testServer(t)
	prov := &checkingProvider{known: map[string]bool{"lux-runner": true}}
	s.cfg.Providers = map[string]Provider{"ec2": prov}
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	ctx = context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	mine := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner"}
	theirs := map[string]any{"region": "eu-north-1", "launchTemplate": "lux-runner", "spot": true}
	set := func(t *testing.T, name string, maxHosts int) {
		t.Helper()
		if _, err := s.putPool(ctx, poolIn(Pool{Name: name, Provider: "ec2", MaxHosts: maxHosts, Template: theirs})); err != nil {
			t.Fatalf("the other set of %s: %v", name, err)
		}
	}

	// A removed pool is retired under its id, so the way a name comes to
	// hold a new id is a rename away and a create.
	cases := []struct {
		pool   string
		before func(t *testing.T) // the pool as the set finds it
		during func(t *testing.T) // the other writes, while the set's check runs
	}{
		{"created", func(*testing.T) {}, func(t *testing.T) { set(t, "created", 2) }},
		{"replaced", func(t *testing.T) { set(t, "replaced", 1) }, func(t *testing.T) {
			in := &renamePoolInput{Name: "replaced"}
			in.Body.Name = "replaced-old"
			if _, err := s.renamePool(ctx, in); err != nil {
				t.Fatalf("renaming the pool away: %v", err)
			}
			set(t, "replaced", 2)
		}},
	}
	for _, c := range cases {
		t.Run(c.pool, func(t *testing.T) {
			c.before(t)
			var checkedID, rowAfterThem, eventsAfterThem string
			prov.during = func() {
				checkedID = prov.lastTags[tagPoolID]
				c.during(t)
				rowAfterThem, eventsAfterThem = poolRowJSON(t, s, ctx, c.pool), poolEventsJSON(t, s, ctx)
			}
			_, err := s.putPool(ctx, poolIn(Pool{Name: c.pool, Provider: "ec2", MaxHosts: 8, Template: mine}))
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "pool_changed" {
				t.Fatalf("err %v, want 409 pool_changed", err)
			}
			if rowAfterThem == "" {
				t.Fatal("the other write did not run during the check")
			}
			if got := poolRowJSON(t, s, ctx, c.pool); got != rowAfterThem {
				t.Errorf("the refused set changed the other write's pool:\nwant %s\ngot  %s", rowAfterThem, got)
			}
			if got := poolEventsJSON(t, s, ctx); got != eventsAfterThem {
				t.Errorf("the refused set wrote pool events:\nwant %s\ngot  %s", eventsAfterThem, got)
			}
			var stored struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(rowAfterThem), &stored); err != nil {
				t.Fatal(err)
			}
			if checkedID == "" || stored.ID == checkedID {
				t.Errorf("checked id %q, the other write's pool's %q: want two ids", checkedID, stored.ID)
			}
		})
	}
}
