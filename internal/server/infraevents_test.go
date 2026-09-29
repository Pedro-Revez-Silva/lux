package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// refuseEvent makes every write of an event of type typ fail, as a full
// disk or a bad row would: whatever it records must then not happen.
func refuseEvent(t *testing.T, s *Server, typ string) {
	t.Helper()
	ownerExec(t, s, `CREATE OR REPLACE FUNCTION refuse_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.type = TG_ARGV[0] THEN RAISE EXCEPTION 'event % refused', NEW.type; END IF;
			RETURN NEW;
		END $$`)
	for _, table := range []string{"pool_events", "host_events"} {
		ownerExec(t, s, fmt.Sprintf(`CREATE TRIGGER refuse BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION refuse_event(%s)`,
			table, "'"+typ+"'"))
	}
}

type recorded struct {
	Owner string
	Data  map[string]any
	Count int
}

// events lists the events of type typ (pool.* or host.*), oldest first.
func events(t *testing.T, s *Server, typ string) []recorded {
	t.Helper()
	tbl := poolEvents
	if typ[:5] == "host." {
		tbl = hostEvents
	}
	var out []recorded
	err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT `+tbl.owner+`, data, count FROM `+tbl.table+` WHERE type = $1 ORDER BY id`, typ)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[recorded])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func queryOne[T any](t *testing.T, s *Server, q string, args ...any) T {
	t.Helper()
	var v T
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), q, args...).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	return v
}

// failingProvider refuses every launch and terminate with err.
type failingProvider struct{ err error }

func (p *failingProvider) Launch(context.Context, json.RawMessage, map[string]string, map[string]string) (Launched, error) {
	return Launched{}, p.err
}

func (p *failingProvider) Terminate(context.Context, json.RawMessage, string) error { return p.err }

func (p *failingProvider) Instances(context.Context, json.RawMessage, map[string]string) (map[string]Instance, error) {
	return nil, p.err
}

// infraFixture: tenant t1 with an ec2 pool "burst" (pool1), a ready host h1
// in it, and a Run r1 waiting for that pool.
func infraFixture(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('tok1', 't1', 'burst', 'hash1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, capacity, last_heartbeat, provision_requested_at, provider_id, registered_at, token_id)
		VALUES ('h1', 't1', 'h1', 'burst', 'ready', '{"runs": 2}', now(), now(), 'i-1', now() - interval '1 hour', 'tok1')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{"placement": {"pool": "burst"}}', 'provisioning')`)
}

func running(t *testing.T, s *Server, ctx context.Context) {
	t.Helper()
	execSQL(t, s, ctx, `UPDATE runs SET state = 'running', current_epoch = 1 WHERE id = 'r1'`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, lease_expires_at)
		VALUES ('p1', 't1', 'r1', 'h1', 1, 'running', now() + interval '1 hour')`)
}

// Every pool and host event is written in the transaction of the change it
// records: when the event cannot be written, the change does not happen
// either; when it can, both are there.
func TestInfraEventsAreWrittenWithTheirChange(t *testing.T) {
	tenant := context.WithValue(context.Background(), principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	cases := []struct {
		typ   string
		setup func(t *testing.T, s *Server, ctx context.Context)
		act   func(s *Server, ctx context.Context) error
		// changed: whether the state change the event records happened.
		changed func(t *testing.T, s *Server) bool
		check   func(t *testing.T, ev recorded)
	}{
		{
			typ: evScaleUp,
			act: func(s *Server, ctx context.Context) error {
				return s.launch(ctx, &fakeLaunchProvider{}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")},
					map[string]any{"hosts": 1, "reason": "waiting runs", "waiting": 1})
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM hosts WHERE provider_id = 'i-fake'`) == 1
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Data["reason"] != "waiting runs" || ev.Data["waiting"] != 1.0 {
					t.Errorf("scale_up data %v", ev.Data)
				}
			},
		},
		{
			typ: evLaunchRequested,
			act: func(s *Server, ctx context.Context) error {
				return s.launch(ctx, &fakeLaunchProvider{}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}, nil)
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM hosts WHERE provider_id = 'i-fake'`) == 1
			},
		},
		{
			typ: evHostLaunched,
			act: func(s *Server, ctx context.Context) error {
				_ = s.launch(ctx, &fakeLaunchProvider{launched: Launched{ProviderID: "i-9", InstanceType: "m7i.large"}},
					poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}, nil)
				return nil
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM hosts WHERE provider_id = 'i-9'`) == 1
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Data["providerId"] != "i-9" || ev.Data["instanceType"] != "m7i.large" {
					t.Errorf("host_launched data %v", ev.Data)
				}
			},
		},
		{
			typ: evLaunchFailed,
			act: func(s *Server, ctx context.Context) error {
				_ = s.launch(ctx, &failingProvider{errors.New("InvalidParameterValue: duplicate tag")},
					poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}, nil)
				return nil
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM hosts WHERE id <> 'h1' AND state = 'terminated'`) == 1
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Data["error"] != "InvalidParameterValue: duplicate tag" {
					t.Errorf("launch_failed data %v", ev.Data)
				}
			},
		},
		{
			typ: evPlacement,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				s.hub.polled("h1")
			},
			act: func(s *Server, ctx context.Context) error {
				_, _, err := s.scheduleBatch(ctx, cursorPos{})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id = 'r1'`) == 1
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Owner != "pool1" || ev.Data["run"] != "r1" || ev.Data["host"] != "h1" || ev.Data["epoch"] != 1.0 {
					t.Errorf("placement event %+v", ev)
				}
			},
		},
		{
			typ: evPlacementAssign,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				s.hub.polled("h1")
			},
			act: func(s *Server, ctx context.Context) error {
				_, _, err := s.scheduleBatch(ctx, cursorPos{})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT count(*) FROM placements WHERE run_id = 'r1'`) == 1
			},
		},
		{
			typ:   evPlacementEnded,
			setup: running,
			act: func(s *Server, ctx context.Context) error {
				code := 0
				return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
					return s.placementExited(ctx, tx, "t1", "r1", 1, proto.Status{State: "exited", ExitCode: &code}, StateRunning, false)
				})
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[string](t, s, `SELECT state FROM placements WHERE id = 'p1'`) == "exited"
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Owner != "h1" || ev.Data["outcome"] != StateSucceeded || ev.Data["run"] != "r1" {
					t.Errorf("placement_ended %+v", ev)
				}
			},
		},
		{
			typ: evDrainRequested,
			act: func(s *Server, ctx context.Context) error {
				_, err := s.drainHost(tenant, &drainHostInput{HostPath: HostPath{ID: "h1"}})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[bool](t, s, `SELECT draining FROM hosts WHERE id = 'h1'`)
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Data["cause"] != causeManual {
					t.Errorf("drain_requested %v", ev.Data)
				}
			},
		},
		{
			typ: evSpotInterrupted,
			act: func(s *Server, ctx context.Context) error {
				return s.hostEvicting(ctx, "h1", proto.Evicting{Reason: "spot interruption"})
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[bool](t, s, `SELECT draining FROM hosts WHERE id = 'h1'`)
			},
		},
		{
			typ: evLost,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				s.cfg.LeaseDuration = time.Minute
				execSQL(t, s, ctx, `UPDATE hosts SET last_heartbeat = now() - interval '1 hour' WHERE id = 'h1'`)
			},
			act: func(s *Server, ctx context.Context) error { return s.reapHosts(ctx) },
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h1'`) == "lost"
			},
		},
		{
			typ: evTerminateRequest,
			act: func(s *Server, ctx context.Context) error {
				s.terminateRequested(ctx, "h1", "never registered")
				return nil
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[bool](t, s, `SELECT terminate_requested_at IS NOT NULL FROM hosts WHERE id = 'h1'`)
			},
		},
		{
			typ: evTerminated,
			act: func(s *Server, ctx context.Context) error {
				s.markTerminated(ctx, "h1", "drained: terminated")
				return nil
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h1'`) == "terminated"
			},
		},
		{
			typ: evHostReleased,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				execSQL(t, s, ctx, `UPDATE hosts SET state = 'draining', draining = true, drain_causes = '{scale-down}',
					last_placement_ended_at = now() - interval '700 seconds', drain_requested_at = now() - interval '100 seconds' WHERE id = 'h1'`)
			},
			act: func(s *Server, ctx context.Context) error {
				s.markTerminated(ctx, "h1", "drained: terminated")
				return nil
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h1'`) == "terminated"
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Owner != "pool1" || ev.Data["host"] != "h1" || ev.Data["reason"] != "idle" || ev.Data["idleSeconds"] != 600.0 {
					t.Errorf("host_released %+v", ev)
				}
			},
		},
		{
			typ: evRegistered,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provision_requested_at, provider_id, token_id)
					VALUES ('h2', 't1', 'burst-h2', 'burst', 'provisioning', now(), 'i-2', 'tok1')`)
			},
			act: func(s *Server, ctx context.Context) error {
				_, err := s.registerHost(ctx, &hostToken{ID: "tok1", TenantID: new("t1"), Pool: "burst"},
					proto.Hello{Name: "burst-h2", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-2"})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[bool](t, s, `SELECT registered_at IS NOT NULL FROM hosts WHERE id = 'h2'`)
			},
		},
		{
			typ: evHostRegistered,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provision_requested_at, provider_id, token_id)
					VALUES ('h2', 't1', 'burst-h2', 'burst', 'provisioning', now(), 'i-2', 'tok1')`)
			},
			act: func(s *Server, ctx context.Context) error {
				_, err := s.registerHost(ctx, &hostToken{ID: "tok1", TenantID: new("t1"), Pool: "burst"},
					proto.Hello{Name: "burst-h2", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-2"})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[bool](t, s, `SELECT registered_at IS NOT NULL FROM hosts WHERE id = 'h2'`)
			},
		},
		{
			typ: evReady,
			setup: func(t *testing.T, s *Server, ctx context.Context) {
				execSQL(t, s, ctx, `UPDATE hosts SET state = 'lost', lost_at = now() WHERE id = 'h1'`)
			},
			act: func(s *Server, ctx context.Context) error { return s.heartbeat(ctx, "h1", proto.Heartbeat{}) },
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[string](t, s, `SELECT state FROM hosts WHERE id = 'h1'`) == "ready"
			},
			check: func(t *testing.T, ev recorded) {
				if ev.Data["from"] != "lost" {
					t.Errorf("ready %v", ev.Data)
				}
			},
		},
		{
			typ: evConfigChanged,
			act: func(s *Server, ctx context.Context) error {
				_, err := s.putPool(tenant, &poolBody{Body: Pool{Name: "burst", Provider: "ec2", MaxHosts: 4, Template: map[string]any{"region": "eu-west-1"}}})
				return err
			},
			changed: func(t *testing.T, s *Server) bool {
				return queryOne[int](t, s, `SELECT max_hosts FROM pools WHERE id = 'pool1'`) == 4
			},
			check: func(t *testing.T, ev recorded) {
				changes, _ := ev.Data["changes"].(map[string]any)
				want := map[string]any{
					"maxHosts":        map[string]any{"old": 0.0, "new": 4.0},
					"template.region": map[string]any{"old": nil, "new": "eu-west-1"},
				}
				if fmt.Sprint(changes) != fmt.Sprint(want) {
					t.Errorf("config_changed %v, want %v", changes, want)
				}
			},
		},
	}
	for _, c := range cases {
		for _, refused := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/refused=%v", c.typ, refused), func(t *testing.T) {
				s := testServer(t)
				ctx := context.Background()
				infraFixture(t, s, ctx)
				if c.setup != nil {
					c.setup(t, s, ctx)
				}
				if refused {
					refuseEvent(t, s, c.typ)
				}
				err := c.act(s, ctx)
				if !refused && err != nil {
					t.Fatal(err)
				}
				changed := c.changed(t, s)
				evs := events(t, s, c.typ)
				switch {
				case refused && changed:
					t.Fatalf("the change was committed without its %s event", c.typ)
				case !refused && !changed:
					t.Fatal("the change did not happen")
				case !refused && len(evs) != 1:
					t.Fatalf("%d %s events, want 1", len(evs), c.typ)
				case !refused && c.check != nil:
					c.check(t, evs[0])
				}
			})
		}
	}
}

// A launch that fails every pass (a duplicate tag in production, once a
// second) is one pool.launch_failed event, counted, and the scale-up and
// launch request before it are folded the same way; a different error, or
// a launch that succeeds in between, starts a new one.
func TestRepeatedLaunchFailuresCollapse(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}
	up := map[string]any{"hosts": 1, "reason": "waiting runs", "waiting": 1}
	dup := &failingProvider{errors.New("operation error EC2: RunInstances, https response error StatusCode: 400, RequestID: 5f1c-aa, api error InvalidParameterValue: duplicate tag")}
	for i := range 5 {
		// Each pass its own request id: still the same failure.
		dup.err = fmt.Errorf("operation error EC2: RunInstances, https response error StatusCode: 400, RequestID: %d0c-aa, api error InvalidParameterValue: duplicate tag", i)
		if err := s.launch(ctx, dup, pl, up); err == nil {
			t.Fatal("launch succeeded")
		}
	}
	failed := events(t, s, evLaunchFailed)
	if len(failed) != 1 || failed[0].Count != 5 {
		t.Fatalf("launch_failed events %+v, want one with count 5", failed)
	}
	for _, typ := range []string{evScaleUp, evLaunchRequested} {
		if evs := events(t, s, typ); len(evs) != 1 || evs[0].Count != 5 {
			t.Errorf("%s events %+v, want one with count 5", typ, evs)
		}
	}
	lastAt := queryOne[time.Time](t, s, `SELECT last_at FROM pool_events WHERE type = $1`, evLaunchFailed)
	firstAt := queryOne[time.Time](t, s, `SELECT created_at FROM pool_events WHERE type = $1`, evLaunchFailed)
	if !lastAt.After(firstAt) {
		t.Errorf("last_at %v not after created_at %v", lastAt, firstAt)
	}

	// Another error: a new event.
	if err := s.launch(ctx, &failingProvider{errors.New("InsufficientInstanceCapacity")}, pl, up); err == nil {
		t.Fatal("launch succeeded")
	}
	if n := len(events(t, s, evLaunchFailed)); n != 2 {
		t.Fatalf("%d launch_failed events after a different error, want 2", n)
	}
	// A success in between: the next failure is new too.
	if err := s.launch(ctx, &fakeLaunchProvider{}, pl, up); err != nil {
		t.Fatal(err)
	}
	if err := s.launch(ctx, &failingProvider{errors.New("InsufficientInstanceCapacity")}, pl, up); err == nil {
		t.Fatal("launch succeeded")
	}
	if n := len(events(t, s, evLaunchFailed)); n != 3 {
		t.Fatalf("%d launch_failed events after a success, want 3", n)
	}
	// Two launches in one pass (want 2) are two requests.
	if err := s.launch(ctx, &fakeLaunchProvider{}, pl, nil); err != nil {
		t.Fatal(err)
	}
	// Every attempt is counted once, whatever it folded into: 5 + 1 + 1 + 1 + 1.
	total := 0
	for _, e := range events(t, s, evLaunchRequested) {
		total += e.Count
	}
	if total != 9 {
		t.Errorf("launch_requested counts add up to %d, want 9", total)
	}
	if n := len(events(t, s, evHostLaunched)); n != 2 {
		t.Errorf("%d host_launched events, want 2", n)
	}
}

// A terminate the provider keeps refusing is one host.provider_error.
func TestRepeatedProviderErrorsCollapse(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	for range 3 {
		s.providerError(ctx, hostEvents, "h1", "terminate", "i-1", errors.New("UnauthorizedOperation"))
	}
	if evs := events(t, s, evHostProviderErr); len(evs) != 1 || evs[0].Count != 3 {
		t.Fatalf("provider_error events %+v, want one with count 3", evs)
	}
}

// The Run's own scheduled event names the pool it was placed from.
func TestScheduledRunEventNamesThePool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	s.hub.polled("h1")
	if _, _, err := s.scheduleBatch(ctx, cursorPos{}); err != nil {
		t.Fatal(err)
	}
	data := queryOne[map[string]any](t, s, `SELECT data FROM run_events WHERE run_id = 'r1' AND data->>'state' = 'scheduled'`)
	if data["pool"] != "burst" || data["host"] != "h1" {
		t.Fatalf("scheduled event %v", data)
	}
}

func getEvents(t *testing.T, s *Server, key, path string) (int, []LifecycleEvent) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var body struct {
		Events []LifecycleEvent `json:"events"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, body.Events
}

func apiKey(t *testing.T, s *Server, tenant *string, scopes ...string) string {
	t.Helper()
	key := ids.Secret("lux")
	execSQL(t, s, context.Background(), `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'k', $3, $4)`,
		ids.New(ids.APIKey), tenant, ids.Hash(key), scopes)
	return key
}

// Newest first, a page at a time with ?before=.
func TestInfraEventsPagination(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	for i := range 7 {
		execSQL(t, s, ctx, `INSERT INTO host_events (tenant_id, host_id, type, data) VALUES ('t1', 'h1', 'host.x', $1)`, map[string]any{"i": i})
	}
	key := apiKey(t, s, new("t1"), "read")
	// Two repeated events, each with its own last time.
	execSQL(t, s, ctx, `INSERT INTO host_events (tenant_id, host_id, type, count, created_at, last_at) VALUES
		('t1', 'h1', 'host.old', 2, now() - interval '2 hours', now() - interval '1 hour'),
		('t1', 'h1', 'host.new', 3, now() - interval '30 minutes', now() - interval '1 minute')`)
	_, repeated := getEvents(t, s, key, "/v1/hosts/h1/events?limit=2")
	if len(repeated) != 2 || repeated[0].LastTime == nil || repeated[1].LastTime == nil ||
		!repeated[0].LastTime.After(repeated[0].Time) || !repeated[0].LastTime.After(*repeated[1].LastTime) {
		t.Fatalf("repeated events %+v: each wants its own lastTime", repeated)
	}
	ownerExec(t, s, `DELETE FROM host_events WHERE type IN ('host.old', 'host.new')`)
	var seen []float64
	before := ""
	for page := 0; ; page++ {
		code, evs := getEvents(t, s, key, "/v1/hosts/h1/events?limit=3"+before)
		if code != http.StatusOK {
			t.Fatalf("page %d: %d", page, code)
		}
		for _, e := range evs {
			seen = append(seen, e.Data["i"].(float64))
		}
		if len(evs) < 3 {
			break
		}
		before = fmt.Sprintf("&before=%d", evs[len(evs)-1].ID)
	}
	if want := []float64{6, 5, 4, 3, 2, 1, 0}; !slices.Equal(seen, want) {
		t.Fatalf("pages gave %v, want %v", seen, want)
	}
}

// A tenant reads its own pool's and hosts' events, never another tenant's
// nor the platform's; an operator reads any. ?owner= picks the platform's
// pool or a tenant's of a shared name; an operator narrowed with ?tenant=
// sees what that tenant would.
func TestInfraEventsVisibility(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
	// "burst" is t1's, t2's and the platform's; "shared" only the platform's.
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool2', 't2', 'burst', 'ec2'),
		('pool0', NULL, 'shared', 'ec2'), ('poolP', NULL, 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('h2', 't2', 'h2', 'burst', 'ready'), ('h0', NULL, 'h0', 'shared', 'ready')`)
	for _, q := range []string{
		`INSERT INTO pool_events (tenant_id, pool_id, type) VALUES ('t1', 'pool1', 'pool.t1'), ('t2', 'pool2', 'pool.t2'),
			(NULL, 'pool0', 'pool.platform'), (NULL, 'poolP', 'pool.platform-burst')`,
		`INSERT INTO host_events (tenant_id, host_id, type) VALUES ('t1', 'h1', 'host.t1'), ('t2', 'h2', 'host.t2'), (NULL, 'h0', 'host.platform')`,
	} {
		execSQL(t, s, ctx, q)
	}
	t1 := apiKey(t, s, new("t1"), "read")
	op := apiKey(t, s, nil, "operator")
	for _, c := range []struct {
		key, path string
		code      int
		typ       string
	}{
		{t1, "/v1/pools/burst/events", 200, "pool.t1"},
		{t1, "/v1/pools/burst/events?owner=tenant", 200, "pool.t1"},
		{t1, "/v1/pools/burst/events?owner=platform", 403, ""},
		{t1, "/v1/hosts/h1/events", 200, "host.t1"},
		{t1, "/v1/hosts/h2/events", 404, ""},
		{t1, "/v1/hosts/h0/events", 403, ""},
		{t1, "/v1/pools/shared/events", 403, ""},
		{t1, "/v1/pools/shared/events?owner=tenant", 404, ""},
		{op, "/v1/pools/burst/events", 409, ""},
		{op, "/v1/pools/burst/events?owner=tenant", 409, ""},
		{op, "/v1/pools/burst/events?owner=platform", 200, "pool.platform-burst"},
		{op, "/v1/pools/burst/events?tenant=t2", 200, "pool.t2"},
		{op, "/v1/pools/burst/events?tenant=t2&owner=tenant", 200, "pool.t2"},
		{op, "/v1/pools/shared/events", 200, "pool.platform"},
		{op, "/v1/pools/nothing/events", 404, ""},
		{op, "/v1/pools/burst/events?owner=someone", 422, ""},
		{op, "/v1/hosts/h2/events", 200, "host.t2"},
		{op, "/v1/hosts/h0/events", 200, "host.platform"},
		// Narrowed: what t2 would see, and t2 sees no platform events.
		{op, "/v1/pools/shared/events?tenant=t2", 403, ""},
		{op, "/v1/pools/burst/events?tenant=t2&owner=platform", 403, ""},
		{op, "/v1/hosts/h0/events?tenant=t2", 403, ""},
		{op, "/v1/hosts/h2/events?tenant=t2", 200, "host.t2"},
		{op, "/v1/hosts/h1/events?tenant=t2", 404, ""},
	} {
		code, evs := getEvents(t, s, c.key, c.path)
		if code != c.code {
			t.Errorf("%s: %d, want %d", c.path, code, c.code)
			continue
		}
		if c.typ != "" && (len(evs) != 1 || evs[0].Type != c.typ) {
			t.Errorf("%s: %+v, want one %s", c.path, evs, c.typ)
		}
	}
}

// A host's pool events go to the pool of its owner and its pool's name: a
// platform host in "burst" to the platform's burst, not a tenant's.
func TestHostPoolEventsGoToItsOwnersPool(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('poolP', NULL, 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state) VALUES ('hP', NULL, 'hP', 'burst', 'ready')`)
	for _, h := range []string{"h1", "hP"} {
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return hostPoolEvent(ctx, tx, h, evHostRegistered, map[string]any{"host": h})
		}); err != nil {
			t.Fatal(err)
		}
	}
	evs := events(t, s, evHostRegistered)
	if len(evs) != 2 || evs[0].Owner != "pool1" || evs[1].Owner != "poolP" {
		t.Fatalf("host_registered events %+v: want h1's on pool1, hP's on poolP", evs)
	}
}

// listingProvider lists the given instances; nothing else is called.
type listingProvider struct {
	fakeLaunchProvider
	instances map[string]Instance
}

func (p *listingProvider) Instances(context.Context, json.RawMessage, map[string]string) (map[string]Instance, error) {
	return p.instances, nil
}

// A launch whose reply was lost is recovered from the provider's listing
// by its tag: its host gets the instance, and the pool records the launch,
// marked recovered. Once only: the next listing finds the host claimed.
func TestRecoveredLaunchIsRecorded(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool, state, provision_requested_at, token_id, tagged)
		VALUES ('h2', 't1', 'burst-h2', 'burst', 'provisioning', now(), 'tok1', true)`)
	pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}
	prov := &listingProvider{instances: map[string]Instance{"i-lost": {State: "running", Tags: map[string]string{tagHost: "h2"}}}}
	for range 2 {
		var st poolState
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
			t.Fatal(err)
		}
		s.reconcileWithProvider(ctx, prov, pl, &st)
	}
	if pid := queryOne[string](t, s, `SELECT provider_id FROM hosts WHERE id = 'h2'`); pid != "i-lost" {
		t.Fatalf("provider_id %q", pid)
	}
	evs := events(t, s, evHostLaunched)
	if len(evs) != 1 {
		t.Fatalf("host_launched events %+v, want one", evs)
	}
	if d := evs[0].Data; evs[0].Owner != "pool1" || d["host"] != "h2" || d["name"] != "burst-h2" || d["providerId"] != "i-lost" || d["recovered"] != true {
		t.Fatalf("host_launched %+v", evs[0])
	}
}

// A template key removed, and nothing else changed, is a change: old→null.
func TestConfigChangedRecordsARemovedTemplateKey(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	tenant := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	for _, tmpl := range []map[string]any{{"region": "eu-west-1", "subnet": "subnet-1"}, {"region": "eu-west-1"}} {
		if _, err := s.putPool(tenant, &poolBody{Body: Pool{Name: "burst", Provider: "ec2", Template: tmpl}}); err != nil {
			t.Fatal(err)
		}
	}
	evs := events(t, s, evConfigChanged)
	if len(evs) != 2 {
		t.Fatalf("config_changed events %+v, want two", evs)
	}
	want := map[string]any{"template.subnet": map[string]any{"old": "subnet-1", "new": nil}}
	if got := evs[1].Data["changes"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("changes %v, want %v", got, want)
	}
}

// Draining a drained host again for the same cause records nothing new; a
// new cause does, and so does evicting Runs not yet asked to stop, which
// are stopped as ever.
func TestDrainRequestedOnlyForANewCauseOrEviction(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	running(t, s, ctx)
	tenant := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	drain := func(force bool) {
		t.Helper()
		if _, err := s.drainHost(tenant, &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: force}}); err != nil {
			t.Fatal(err)
		}
	}
	drain(false)
	drain(false)
	if evs := events(t, s, evDrainRequested); len(evs) != 1 {
		t.Fatalf("drain_requested events after draining twice: %+v, want one", evs)
	}
	drain(true)
	evs := events(t, s, evDrainRequested)
	if len(evs) != 2 || evs[1].Data["evict"] != true {
		t.Fatalf("drain_requested events after an eviction: %+v, want a second, evicting", evs)
	}
	if reason := queryOne[string](t, s, `SELECT stop_reason FROM placements WHERE id = 'p1'`); reason != "drain" {
		t.Fatalf("placement stop_reason %q, want drain", reason)
	}
	// Evicting again: the Run is already stopping, nothing new.
	drain(true)
	if evs := events(t, s, evDrainRequested); len(evs) != 2 {
		t.Fatalf("drain_requested events after evicting twice: %+v, want two", evs)
	}
	// Another cause is news.
	if _, err := s.drainForScaleDown(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := s.drainHosts(ctx, tx, outdatedBinariesReason, causeOutdated, "", "id = $1", "h1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if evs := events(t, s, evDrainRequested); len(evs) != 3 || evs[2].Data["cause"] != causeOutdated {
		t.Fatalf("drain_requested events after a new cause: %+v, want a third, outdated", evs)
	}
}

// A static host registering for the first time is registered and ready.
func TestStaticHostFirstRegistrationIsReady(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	w, err := s.registerHost(ctx, &hostToken{ID: "tok1", TenantID: new("t1"), Pool: "burst"},
		proto.Hello{Name: "static-1", ProtocolVersion: proto.Version, Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	types := queryOne[[]string](t, s, `SELECT array_agg(type ORDER BY id) FROM host_events WHERE host_id = $1`, w.HostID)
	if !slices.Equal(types, []string{evRegistered, evReady}) {
		t.Fatalf("events %v, want registered then ready", types)
	}
	if from := events(t, s, evReady)[0].Data["from"]; from != "new" {
		t.Fatalf("ready from %v, want new", from)
	}
}

// A pool removed and set again keeps its id and its events; the history
// shows the boundary: pool.retired, then pool.restored.
func TestPoolRetiredAndRestored(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	infraFixture(t, s, ctx)
	tenant := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	if _, err := s.deletePool(tenant, &deletePoolInput{Name: "burst"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.putPool(tenant, &poolBody{Body: Pool{Name: "burst", Provider: "ec2", MaxHosts: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.putPool(tenant, &poolBody{Body: Pool{Name: "burst", Provider: "ec2", MaxHosts: 3}}); err != nil {
		t.Fatal(err)
	}
	types := queryOne[[]string](t, s, `SELECT array_agg(type ORDER BY id) FROM pool_events WHERE pool_id = 'pool1' AND type NOT LIKE 'pool.host%'`)
	if !slices.Equal(types, []string{evRetired, evRestored, evConfigChanged}) {
		t.Fatalf("pool1's events %v, want retired, restored, config_changed", types)
	}
	restored := events(t, s, evRestored)[0].Data["changes"].(map[string]any)
	if fmt.Sprint(restored["retired"]) != fmt.Sprint(map[string]any{"old": true, "new": false}) || restored["maxHosts"] == nil {
		t.Fatalf("restored changes %v", restored)
	}
}
