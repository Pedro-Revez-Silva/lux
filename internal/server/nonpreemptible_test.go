package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func nonPreemptibleSpec(pool, poolID string, res spec.Resources) spec.RunSpec {
	sp := baseSpec(pool, res)
	sp.Placement.PoolID = poolID
	no := false
	sp.Placement.Preemptible = &no
	return sp
}

// nonPreemptibleFixture is tenant t1 with static pools "steady"
// (preemptible false) and "spot" (preemptible true), and a tenant default
// pool "def" that is non-preemptible: rule 1 must not fall back to it.
func nonPreemptibleFixture(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "steady", Provider: "static", Preemptible: mark(false)})
	mustPut(t, s, "t1", Pool{Name: "spot", Provider: "static"})
	mustPut(t, s, "t1", Pool{Name: "def", Provider: "static", Preemptible: mark(false), IsDefault: mark(true)})
	return s
}

func submitBoth(t *testing.T, s *Server, sp spec.RunSpec) (submitErr, validateErr error) {
	t.Helper()
	_, validateErr = s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	_, submitErr = s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	return
}

// Submit and validateOnly hold a non-preemptible Run to the same rules:
// a pool named explicitly (no default), non-preemptible, and no spot.
func TestSubmitNonPreemptibleRefusals(t *testing.T) {
	s := nonPreemptibleFixture(t)
	spotID := poolID(t, s, "spot", "t1")
	for _, c := range []struct {
		name string
		sp   spec.RunSpec
		code string
	}{
		{"no pool named, a non-preemptible default exists", nonPreemptibleSpec("", "", spec.Resources{}), codePoolRequired},
		{"preemptible pool by name", nonPreemptibleSpec("spot", "", spec.Resources{}), codePoolPreemptible},
		{"preemptible pool by id", nonPreemptibleSpec("", spotID, spec.Resources{}), codePoolPreemptible},
		{"no such pool name", nonPreemptibleSpec("nope", "", spec.Resources{}), codePoolPreemptible},
		{"with spot", func() spec.RunSpec {
			sp := nonPreemptibleSpec("steady", "", spec.Resources{})
			sp.Spot = true
			return sp
		}(), "invalid_spec"},
	} {
		t.Run(c.name, func(t *testing.T) {
			submitErr, validateErr := submitBoth(t, s, c.sp)
			wantRefused(t, submitErr, c.code)
			wantRefused(t, validateErr, c.code)
		})
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d Runs created by refused submits", n)
	}
}

// A non-preemptible Run naming a shaped non-preemptible pool gets its
// size as any Run there; preemptible true (explicit) on a default pool is
// an ordinary Run.
func TestSubmitNonPreemptibleSizeOnShapedPool(t *testing.T) {
	s := nonPreemptibleFixture(t)
	mustPut(t, s, "t1", Pool{Name: "shaped", Provider: "static", Preemptible: mark(false), Shape: &testShape})
	sp := nonPreemptibleSpec("shaped", "", spec.Resources{Size: "m-4-arm64"})
	v, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if v.Body.EffectivePool != "shaped" || v.Body.EffectiveResources.CPUs != 4 {
		t.Fatalf("validateOnly answer %+v", v.Body)
	}
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if _, size := runSizing(t, s, out.Body.ID); size != "m-4-arm64" || out.Body.Spec.Resources.CPUs != 4 || !out.Body.Spec.NonPreemptible() {
		t.Fatalf("size %q resources %+v placement %+v", size, out.Body.Spec.Resources, out.Body.Spec.Placement)
	}
	yes := true
	ordinary := baseSpec("", spec.Resources{})
	ordinary.Placement.Preemptible = &yes
	if _, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: ordinary}); err != nil {
		t.Fatalf("preemptible true on the default pool: %v", err)
	}
}

// A resume moving a non-preemptible Run to another pool keeps the rule:
// the target must be non-preemptible; an ordinary Run may move anywhere.
func TestResumeNonPreemptibleIntoPreemptiblePool(t *testing.T) {
	s := testServer(t)
	id, big := movedRun(t, s)
	setSpec(t, s, id, "{placement,preemptible}", `false`)
	before := binding(t, s, id)
	_, err := resumeTo(s, id, big, nil)
	wantRefused(t, err, codePoolPreemptible)
	if after := binding(t, s, id); after.PoolID != before.PoolID || after.State != before.State {
		t.Fatalf("refused resume changed the Run: %+v → %+v", before, after)
	}
	req := &resumeRequest{ValidateOnly: true, Secrets: []spec.Secret{{Name: "TOKEN", Value: "v"}}, Placement: &resumeTarget{PoolID: big}}
	_, err = s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}, Body: req})
	wantRefused(t, err, codePoolPreemptible)

	execSQL(t, s, context.Background(), `UPDATE pools SET preemptible = false WHERE id = $1`, big)
	if _, err := resumeTo(s, id, big, nil); err != nil {
		t.Fatalf("resume into a non-preemptible pool: %v", err)
	}
}

// Pool preemptible: omitted on create follows an ec2 template's spot, else
// true; omitted on update keeps it; a template spot contradicting it is a
// 422.
func TestPutPoolPreemptible(t *testing.T) {
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	got := func(name string) bool {
		return queryOne[bool](t, s, `SELECT preemptible FROM pools WHERE name = $1`, name)
	}
	mustPut(t, s, "t1", Pool{Name: "plain", Provider: "static"})
	mustPut(t, s, "t1", Pool{Name: "od", Provider: "ec2", Template: map[string]any{"spot": false}})
	mustPut(t, s, "t1", Pool{Name: "sp", Provider: "ec2", Template: map[string]any{"spot": true}})
	mustPut(t, s, "t1", Pool{Name: "bare", Provider: "ec2"})
	for name, want := range map[string]bool{"plain": true, "od": false, "sp": true, "bare": true} {
		if got(name) != want {
			t.Errorf("%s: preemptible %v, want %v", name, got(name), want)
		}
	}
	out := mustPut(t, s, "t1", Pool{Name: "plain", Provider: "static", Preemptible: mark(false)})
	if out.Preemptible == nil || *out.Preemptible {
		t.Fatalf("answer's preemptible %v, want false", out.Preemptible)
	}
	mustPut(t, s, "t1", Pool{Name: "plain", Provider: "static", MaxHosts: 3})
	if got("plain") {
		t.Error("an update without preemptible changed it")
	}
	for _, c := range []struct {
		spot, preemptible bool
	}{{true, false}, {false, true}} {
		_, err := putPoolAs(t, s, "t1", Pool{Name: "x", Provider: "ec2", Template: map[string]any{"spot": c.spot}, Preemptible: mark(c.preemptible)})
		wantRefused(t, err, "invalid_pool")
	}
	// Omitted on update, the stored value stands: a template spot
	// contradicting it is a 422, one without spot keeps it.
	mustPut(t, s, "t1", Pool{Name: "kept", Provider: "ec2", Preemptible: mark(false)})
	_, err := putPoolAs(t, s, "t1", Pool{Name: "kept", Provider: "ec2", Template: map[string]any{"spot": true}})
	wantRefused(t, err, "invalid_pool")
	if got("kept") {
		t.Error("an update with template spot true and no preemptible made the pool preemptible")
	}
	mustPut(t, s, "t1", Pool{Name: "kept", Provider: "ec2", Template: map[string]any{"region": "r"}})
	if got("kept") {
		t.Error("an update with a template without spot changed preemptible")
	}
	list, err := s.listPools(tenantCtx("t1"), &TenantQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Body.Pools {
		if p.Preemptible == nil {
			t.Errorf("GET /v1/pools: %s has no preemptible", p.Name)
		}
	}
}

// Making a pool preemptible while a non-preemptible Run is queued, live or
// resumable in it is a 409; once the Run is terminated it is allowed.
func TestPutPoolPreemptibleFlipRefusedWithNonPreemptibleRuns(t *testing.T) {
	for _, state := range []string{StateSubmitted, StateRunning, StateStopped} {
		t.Run(state, func(t *testing.T) {
			s := nonPreemptibleFixture(t)
			out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: nonPreemptibleSpec("steady", "", spec.Resources{})})
			if err != nil {
				t.Fatal(err)
			}
			execSQL(t, s, context.Background(), `UPDATE runs SET state = $2 WHERE id = $1`, out.Body.ID, state)
			_, err = putPoolAs(t, s, "t1", Pool{Name: "steady", Provider: "static", Preemptible: mark(true)})
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != codePoolHasNonPreempt {
				t.Fatalf("flip with a %s non-preemptible Run: %v, want 409 %s", state, err, codePoolHasNonPreempt)
			}
			if queryOne[bool](t, s, `SELECT preemptible FROM pools WHERE name = 'steady'`) {
				t.Fatal("refused flip was stored")
			}
			execSQL(t, s, context.Background(), `UPDATE runs SET state = 'terminated' WHERE id = $1`, out.Body.ID)
			mustPut(t, s, "t1", Pool{Name: "steady", Provider: "static", Preemptible: mark(true)})
		})
	}
}

// A non-preemptible Run is placed only on a host that is not spot and
// whose runner advertises CapNonPreemptible, even when the others have
// room; an ordinary Run still takes any of them.
func TestPlacementNonPreemptibleHosts(t *testing.T) {
	r, _ := fitFixture()
	no := false
	r.Spec.Placement.Preemptible = &no
	host := func(id, market string, caps ...string) *candidateHost {
		_, h := fitFixture()
		h.ID, h.Market, h.Caps = id, market, caps
		return h
	}
	spot := host("a-spot", MarketSpot, proto.CapNonPreemptible)
	old := host("b-old", MarketOnDemand)
	good := host("c-good", MarketOnDemand, proto.CapNonPreemptible)
	static := host("d-static", "", proto.CapNonPreemptible)
	for _, h := range []*candidateHost{spot, old} {
		if b := hostFit(r, h); len(b) != 1 || b[0].Kind != kindNonPreemptible {
			t.Errorf("%s: blockers %+v, want one non-preemptible", h.ID, b)
		}
	}
	for _, h := range []*candidateHost{good, static} {
		if b := hostFit(r, h); len(b) != 0 {
			t.Errorf("%s: blockers %+v, want none", h.ID, b)
		}
	}
	chosen := r
	chosen.PlaceOn = spot.ID
	if b := hostFit(chosen, spot); len(b) != 1 || b[0].Kind != kindNonPreemptible {
		t.Errorf("chosen spot host: blockers %+v", b)
	}
	if adm := normalAdmission(r, []*candidateHost{spot, old}); adm.Host != nil ||
		!slices.ContainsFunc(adm.Blockers, func(b AdmissionBlocker) bool { return b.Code == blockCapability && b.Hosts == 2 }) {
		t.Errorf("admission over spot and old: %+v, want capability on 2 hosts", adm)
	}
	ordinary, _ := fitFixture()
	for _, h := range []*candidateHost{spot, old} {
		if b := hostFit(ordinary, h); len(b) != 0 {
			t.Errorf("ordinary Run on %s: blockers %+v", h.ID, b)
		}
	}
}

// Through the scheduler: with a spot host and an old runner both free in
// a non-preemptible pool, the Run waits with the reason saying why;
// validateOnly reports the capability blocker; once a capable on-demand
// host is ready it is placed there.
func TestScheduleNonPreemptibleSkipsSpotAndOldRunners(t *testing.T) {
	s := nonPreemptibleFixture(t)
	ctx := context.Background()
	readyHost(t, s, "h-spot", "steady", "t1", true)
	readyHost(t, s, "h-old", "steady", "t1", true)
	execSQL(t, s, ctx, `UPDATE hosts SET market = 'spot', capabilities = $1 WHERE id = 'h-spot'`, []string{proto.CapNonPreemptible})
	execSQL(t, s, ctx, `UPDATE hosts SET market = 'on-demand', capabilities = '{}' WHERE id = 'h-old'`)
	sp := nonPreemptibleSpec("steady", "", spec.Resources{CPUs: 1, Memory: 1, Disk: 1})
	v, err := s.submitRun(tenantCtx("t1"), &submitRunInput{ValidateOnly: true, Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	if v.Body.WouldStartNow || len(v.Body.Blockers) != 1 || v.Body.Blockers[0].Code != blockCapability || v.Body.Blockers[0].Hosts != 2 {
		t.Fatalf("validateOnly %+v, want a capability blocker on 2 hosts", v.Body)
	}
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	if host, state, reason := placedOn(t, s, out.Body.ID); host != "" {
		t.Fatalf("placed on %s (%s %s), want waiting", host, state, reason)
	}
	readyHost(t, s, "h-good", "steady", "t1", false)
	execSQL(t, s, ctx, `UPDATE hosts SET market = 'on-demand', capabilities = $1 WHERE id = 'h-good'`, []string{proto.CapNonPreemptible})
	schedule(t, s)
	if host, _, reason := placedOn(t, s, out.Body.ID); host != "h-good" {
		t.Fatalf("placed on %q (%s), want h-good", host, reason)
	}
	// An ordinary Run in the same pool takes the spot host.
	ord, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: baseSpec("steady", spec.Resources{CPUs: 1, Memory: 1, Disk: 1})})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET draining = true WHERE id IN ('h-good', 'h-old')`)
	schedule(t, s)
	if host, _, reason := placedOn(t, s, ord.Body.ID); host != "h-spot" {
		t.Fatalf("ordinary Run placed on %q (%s), want h-spot", host, reason)
	}
}

// launch hands the provider the template with spot set from the pool's
// preemptible, whatever the stored template says; the rest is unchanged.
func TestLaunchUsesThePoolsMarket(t *testing.T) {
	for _, c := range []struct {
		preemptible bool
		template    string
		wantSpot    any
	}{
		{false, `{"region":"r","spot":true}`, nil},
		{true, `{"region":"r"}`, true},
		{false, `{"region":"r"}`, nil},
	} {
		s := testServer(t)
		ctx := context.Background()
		execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
		prov := &fakeLaunchProvider{}
		pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", Template: json.RawMessage(c.template), Preemptible: c.preemptible}
		if err := s.launch(ctx, prov, pl, nil); err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(prov.template, &sent); err != nil {
			t.Fatal(err)
		}
		if sent["spot"] != c.wantSpot || sent["region"] != "r" {
			t.Errorf("preemptible %v, template %s: launched with %s", c.preemptible, c.template, prov.template)
		}
	}
}

// A force-evicting drain still stops a non-preemptible Run: an
// operator's forced action is not lux's own initiative.
func TestForceDrainStopsNonPreemptibleRun(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	drainFixture(t, s, ctx)
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"placement":{"preemptible":false}}' WHERE id = 'r1'`)
	actx := context.WithValue(ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	if _, err := s.drainHost(actx, &drainHostInput{HostPath: HostPath{ID: "h1"}, Body: &drainHostRequest{ForceEvict: true}}); err != nil {
		t.Fatal(err)
	}
	if _, stopRequested, reason := drainState(t, s, ctx, "h1", "r1"); !stopRequested || reason != "drain" {
		t.Fatalf("stopRequested=%v reason=%q, want a drain stop", stopRequested, reason)
	}
}

// runPlaceOn is the Run's state and place_on ("" for none).
func runPlaceOn(t *testing.T, s *Server, id string) (state, placeOn string) {
	t.Helper()
	systemScan(t, s, `SELECT state, coalesce(place_on, '') FROM runs WHERE id = $1`, []any{id}, &state, &placeOn)
	return state, placeOn
}

// An operator's migrate --to a spot host, or one whose runner lacks
// CapNonPreemptible, is a 422 for a non-preemptible Run and changes
// nothing (no stop requested); a capable on-demand or static (no market)
// host is accepted.
func TestMigrateNonPreemptibleToIncapableHost(t *testing.T) {
	for _, c := range []struct {
		name, market string
		caps         []string
		ok           bool
	}{
		{"spot", MarketSpot, []string{proto.CapNonPreemptible}, false},
		{"old runner", MarketOnDemand, []string{}, false},
		{"capable on-demand", MarketOnDemand, []string{proto.CapNonPreemptible}, true},
		{"capable static", "", []string{proto.CapNonPreemptible}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx := policyFixture(t, "")
			execSQL(t, s, ctx, `UPDATE runs SET spec = jsonb_set(spec, '{placement,preemptible}', 'false') WHERE id = 'r1'`)
			execSQL(t, s, ctx, `UPDATE hosts SET market = nullif($1, ''), capabilities = $2 WHERE id = 'h2'`, c.market, c.caps)
			_, err := s.migrateRun(operatorCtx(), &migrateRunInput{RunPath: RunPath{ID: "r1"}, Body: &migrateRequest{To: "h2"}})
			state, placeOn := runPlaceOn(t, s, "r1")
			if c.ok {
				if err != nil || placeOn != "h2" {
					t.Fatalf("migrate --to h2: %v, place_on %q", err, placeOn)
				}
				return
			}
			wantRefused(t, err, "invalid_spec")
			if state != StateRunning || placeOn != "" {
				t.Errorf("after the refusal: state %s place_on %q", state, placeOn)
			}
			if _, stopRequested, _ := drainState(t, s, ctx, "h1", "r1"); stopRequested {
				t.Error("a refused migrate requested a stop")
			}
		})
	}
}

// resume --to holds a non-preemptible Run to the same rule.
func TestResumeNonPreemptibleToIncapableHost(t *testing.T) {
	for _, c := range []struct {
		name, market string
		caps         []string
		ok           bool
	}{
		{"spot", MarketSpot, []string{proto.CapNonPreemptible}, false},
		{"old runner", MarketOnDemand, []string{}, false},
		{"capable on-demand", MarketOnDemand, []string{proto.CapNonPreemptible}, true},
		{"capable static", "", []string{proto.CapNonPreemptible}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id, _ := movedRun(t, s)
			setSpec(t, s, id, "{placement,preemptible}", `false`)
			execSQL(t, s, context.Background(), `UPDATE hosts SET market = nullif($1, ''), capabilities = $2 WHERE id = 'hc'`, c.market, c.caps)
			_, err := s.resumeRun(operatorCtx(), &resumeRunInput{RunPath: RunPath{ID: id}, Body: &resumeRequest{
				To: "hc", Secrets: []spec.Secret{{Name: "TOKEN", Value: "v"}}}})
			state, placeOn := runPlaceOn(t, s, id)
			if c.ok {
				if err != nil || placeOn != "hc" || state != StateResuming {
					t.Fatalf("resume --to hc: %v, state %s place_on %q", err, state, placeOn)
				}
				return
			}
			wantRefused(t, err, "invalid_spec")
			if state != StateStopped || placeOn != "" {
				t.Errorf("after the refusal: state %s place_on %q", state, placeOn)
			}
		})
	}
}
