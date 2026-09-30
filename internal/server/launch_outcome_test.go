package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/ids"
)

// hostView is a host as GET /v1/hosts/{id} returns it: its state, the
// times a launch failure must not report, and its launch outcome.
type hostView struct {
	State       string                `json:"state"`
	StateReason string                `json:"stateReason"`
	Times       map[string]*time.Time `json:"times"`
	Launch      *HostLaunch           `json:"launch"`
}

func operatorKey(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('ko', NULL, 'o', $1, ARRAY['operator'])`, ids.Hash(key))
	return key
}

// A launch the provider refuses leaves a host that is operationally
// terminated (its token revoked, gone from the live list) with outcome
// failed, its error, and no terminated time; a launch that succeeds and
// whose host is later terminated is launched, with its terminated time.
func TestLaunchOutcomeFailedVersusLaunchedThenTerminated(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	key := operatorKey(t, s, ctx)
	pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}

	refused := errors.New("api error InsufficientInstanceCapacity: no c7a.4xlarge capacity in eu-west-1b")
	if err := s.launch(ctx, &failingProvider{err: refused}, pl, nil); err == nil {
		t.Fatal("a refused launch returned no error")
	}
	failed := queryOne[string](t, s, `SELECT id FROM hosts WHERE provider_id IS NULL`)
	if err := s.launch(ctx, &fakeLaunchProvider{launched: Launched{ProviderID: "i-ok"}}, pl, nil); err != nil {
		t.Fatal(err)
	}
	ok := queryOne[string](t, s, `SELECT id FROM hosts WHERE provider_id = 'i-ok'`)
	var mid hostView
	if code := getJSON(t, s, key, "/v1/hosts/"+ok, &mid); code != http.StatusOK || mid.Launch == nil || mid.Launch.Outcome != "launched" || mid.Launch.FinishedAt == nil {
		t.Fatalf("launched host before its end: %d %+v", code, mid.Launch)
	}
	// The launched host ends the usual way: the provider terminated it.
	s.markTerminated(ctx, ok, "the provider terminated this host")

	// Operational cleanup is the same for both: terminated, token revoked.
	for _, id := range []string{failed, ok} {
		if st := queryOne[string](t, s, `SELECT state FROM hosts WHERE id = $1`, id); st != "terminated" {
			t.Errorf("%s: state %s, want terminated", id, st)
		}
		if !queryOne[bool](t, s, `SELECT revoked_at IS NOT NULL FROM host_tokens WHERE id = (SELECT token_id FROM hosts WHERE id = $1)`, id) {
			t.Errorf("%s: its host token is not revoked", id)
		}
	}

	var f, l hostView
	if code := getJSON(t, s, key, "/v1/hosts/"+failed, &f); code != http.StatusOK {
		t.Fatalf("GET failed host: %d", code)
	}
	if code := getJSON(t, s, key, "/v1/hosts/"+ok, &l); code != http.StatusOK {
		t.Fatalf("GET launched host: %d", code)
	}
	if f.State != "terminated" || f.Launch == nil || f.Launch.Outcome != "failed" || f.Launch.FinishedAt == nil ||
		f.Launch.Error != providerErrorText(refused) || f.Launch.Error != refused.Error() || f.Launch.RequestedAt == nil {
		t.Errorf("failed launch: %+v %+v", f, f.Launch)
	}
	if f.Times["terminated"] != nil || f.Times["terminateRequested"] != nil {
		t.Errorf("a launch that never ran reports a terminate time: %v", f.Times)
	}
	if want := "launch failed: " + refused.Error(); f.StateReason != want {
		t.Errorf("reason %q, want %q", f.StateReason, want)
	}
	if l.State != "terminated" || l.Launch == nil || l.Launch.Outcome != "launched" || l.Times["terminated"] == nil {
		t.Errorf("launched then terminated: %+v %+v", l, l.Launch)
	}

	// The list filters them apart; launch_failed is not "terminated".
	var list struct {
		Hosts []struct {
			ID string `json:"id"`
		} `json:"hosts"`
	}
	for q, want := range map[string]string{"state=launch_failed": failed, "state=terminated": ok} {
		if code := getJSON(t, s, key, "/v1/hosts?"+q, &list); code != http.StatusOK || len(list.Hosts) != 1 || list.Hosts[0].ID != want {
			t.Errorf("GET /v1/hosts?%s: %d %+v, want only %s", q, code, list.Hosts, want)
		}
	}
	if code := getJSON(t, s, key, "/v1/hosts", &list); code != http.StatusOK || len(list.Hosts) != 0 {
		t.Errorf("live hosts: %+v, want none", list.Hosts)
	}
}

// blockingProvider's Launch waits for release, after telling started.
type blockingProvider struct {
	fakeLaunchProvider
	started, release chan struct{}
}

func (p *blockingProvider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (Launched, error) {
	close(p.started)
	<-p.release
	return p.fakeLaunchProvider.Launch(ctx, template, tags, env)
}

// While the provider has not answered, the launch is requested (with its
// request time and no answer); then launched.
func TestLaunchOutcomeRequestedUntilAnswered(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	key := operatorKey(t, s, ctx)
	pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}
	p := &blockingProvider{fakeLaunchProvider{launched: Launched{ProviderID: "i-slow"}}, make(chan struct{}), make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.launch(ctx, p, pl, nil) }()
	<-p.started
	id := queryOne[string](t, s, `SELECT id FROM hosts`)
	var mid hostView
	if code := getJSON(t, s, key, "/v1/hosts/"+id, &mid); code != http.StatusOK || mid.Launch == nil ||
		mid.Launch.Outcome != "requested" || mid.Launch.RequestedAt == nil || mid.Launch.FinishedAt != nil {
		close(p.release)
		t.Fatalf("in flight: %d %+v", code, mid.Launch)
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if o := queryOne[string](t, s, `SELECT launch_outcome FROM hosts WHERE id = $1`, id); o != "launched" {
		t.Fatalf("answered: %s", o)
	}
}

// A launch whose answer was never recorded is written off as abandoned,
// not as a provider refusal.
func TestLaunchOutcomeAbandoned(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, provision_requested_at, tagged, launch_outcome)
		VALUES ('h1', 'h1', 'pool1', 'provisioning', now() - interval '1 hour', true, 'requested')`)
	s.markAbandoned(ctx, "h1")
	if got := queryOne[string](t, s, `SELECT state || '/' || launch_outcome FROM hosts WHERE id = 'h1'`); got != "terminated/abandoned" {
		t.Fatalf("abandoned launch: %s", got)
	}
}
