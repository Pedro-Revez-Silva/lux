package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Error codes of non-preemptible Runs and pools.
const (
	codePoolRequired       = "pool_required"
	codePoolPreemptible    = "pool_preemptible"
	codePoolHasNonPreempt  = "pool_has_non_preemptible_runs"
	nonPreemptibleRunSQL   = `coalesce(r.spec->'placement'->>'preemptible', '') = 'false'`
	errPreemptibleTemplate = "template.spot %v contradicts preemptible %v: an ec2 pool's market is preemptible (spot: true is preemptible: true); leave template.spot out"
	errPreemptibleStored   = "template.spot %v contradicts the pool's stored preemptible %v: an ec2 pool's market is preemptible (spot: true is preemptible: true); set preemptible explicitly or leave template.spot out"
)

// PoolPreemptible is the preemptible a pool set stores: explicit, else
// current (the stored value, on update), else an ec2 template's spot when
// it has one, else true. An ec2 template spot contradicting explicit, or
// with explicit omitted the stored value, is a 422.
func PoolPreemptible(provider string, template map[string]any, explicit, current *bool) (bool, error) {
	want := explicit
	if want == nil {
		want = current
	}
	raw, hasSpot := template["spot"]
	if hasSpot && provider == "ec2" {
		spot, ok := raw.(bool)
		if !ok {
			return false, errf(http.StatusUnprocessableEntity, "invalid_pool", "template.spot must be true or false, got %T", raw)
		}
		if want != nil && *want != spot {
			if explicit == nil {
				return false, errf(http.StatusUnprocessableEntity, "invalid_pool", errPreemptibleStored, spot, *want)
			}
			return false, errf(http.StatusUnprocessableEntity, "invalid_pool", errPreemptibleTemplate, spot, *want)
		}
		return spot, nil
	}
	if want != nil {
		return *want, nil
	}
	return true, nil
}

// ProviderTemplate is the template a pool's provider launches and checks
// with. The pool's preemptible is the one source of an ec2 pool's market:
// luxd sets the template's spot from it here, before every Launch and
// Check, so the ec2 provider reads spot as it always has. The stored
// template (and hosts.launch_template) stay as the operator wrote them.
// Other providers' templates are returned as they are.
func ProviderTemplate(provider string, template json.RawMessage, preemptible bool) (json.RawMessage, error) {
	if provider != "ec2" {
		return template, nil
	}
	var t map[string]any
	if len(template) > 0 {
		if err := json.Unmarshal(template, &t); err != nil {
			return nil, err
		}
	}
	if spot, _ := t["spot"].(bool); spot == preemptible {
		return template, nil
	}
	t = maps.Clone(t)
	if t == nil {
		t = map[string]any{}
	}
	if preemptible {
		t["spot"] = true
	} else {
		delete(t, "spot")
	}
	return json.Marshal(t)
}

// checkPreemptibleFlip refuses (409) making pool poolID preemptible while
// a non-preemptible Run bound to it could still be placed there: queued,
// live, or resumable (every state but terminated). current is its stored
// preemptible; nothing is checked unless it goes from false to true.
func checkPreemptibleFlip(ctx context.Context, tx pgx.Tx, poolID string, current, next bool) error {
	if current || !next {
		return nil
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs r WHERE r.pool_id = $1 AND r.state <> 'terminated' AND `+nonPreemptibleRunSQL+`)`,
		poolID).Scan(&held); err != nil {
		return err
	}
	if held {
		return errf(http.StatusConflict, codePoolHasNonPreempt,
			"a Run with placement.preemptible false is in this pool (queued, live or resumable): terminate or move such Runs before making it preemptible")
	}
	return nil
}

// storedPoolPreemptible is the stored preemptible of the live or retired
// pool name of tenantID ("" the platform's); nil when there is none.
func storedPoolPreemptible(ctx context.Context, tx pgx.Tx, tenantID, name string) (id string, preemptible *bool, err error) {
	var p bool
	err = tx.QueryRow(ctx, `SELECT id, preemptible FROM pools WHERE tenant_id IS NOT DISTINCT FROM nullif($1, '') AND name = $2`,
		tenantID, name).Scan(&id, &p)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return id, &p, err
}

// SavePoolPreemptible resolves a pool set's preemptible against the stored
// pool tenantID/name and refuses a false-to-true change while it holds
// non-preemptible Runs. Call it in the set's transaction, under ChangePool's
// lock (SavePool's upsert).
func SavePoolPreemptible(ctx context.Context, tx pgx.Tx, tenantID, name, provider string, template map[string]any, explicit *bool) (bool, error) {
	id, current, err := storedPoolPreemptible(ctx, tx, tenantID, name)
	if err != nil {
		return false, err
	}
	next, err := PoolPreemptible(provider, template, explicit, current)
	if err != nil || current == nil {
		return next, err
	}
	return next, checkPreemptibleFlip(ctx, tx, id, *current, next)
}

// checkChosenHost refuses (422) an operator's chosen host for a
// non-preemptible Run when the host is spot or its runner lacks
// CapNonPreemptible: hostFit would never place the Run there, and place_on
// would hold it waiting for that host indefinitely.
func checkChosenHost(ctx context.Context, tx pgx.Tx, sp *spec.RunSpec, hostID *string) error {
	if hostID == nil || !sp.NonPreemptible() {
		return nil
	}
	var market string
	var caps []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(market, ''), capabilities FROM hosts WHERE id = $1`, *hostID).Scan(&market, &caps); err != nil {
		return err
	}
	switch {
	case market == MarketSpot:
		return errf(http.StatusUnprocessableEntity, "invalid_spec",
			"the chosen host is a spot instance: a Run with placement.preemptible false runs only on a host that is not spot")
	case !slices.Contains(caps, proto.CapNonPreemptible):
		return errf(http.StatusUnprocessableEntity, "invalid_spec",
			"the chosen host's lux-runner lacks non-preemptible support: a Run with placement.preemptible false cannot run there")
	}
	return nil
}

// resolveSubmitPool applies the same pool resolution and eligibility checks
// to submit and validateOnly, before either normalizes the spec's pool name.
func resolveSubmitPool(ctx context.Context, tx pgx.Tx, tenantID string, sp *spec.RunSpec) (resolvedPool, error) {
	rp, err := resolvePool(ctx, tx, tenantID, sp.Placement.Pool, sp.Placement.PoolID)
	if err != nil || !sp.NonPreemptible() {
		return rp, err
	}
	if sp.Placement.Pool == "" && sp.Placement.PoolID == "" {
		return rp, errf(http.StatusUnprocessableEntity, codePoolRequired,
			"placement.preemptible false needs a pool named by placement.pool or placement.poolId: default pools are not used")
	}
	return rp, requireNonPreemptiblePool(ctx, tx, rp)
}

// requireNonPreemptiblePool refuses a pool that is preemptible, or not
// there (a name no active pool has: a Run bound to none could later bind
// to any pool of that name).
func requireNonPreemptiblePool(ctx context.Context, tx pgx.Tx, rp resolvedPool) error {
	if rp.ID == nil {
		return errf(http.StatusUnprocessableEntity, codePoolPreemptible,
			"no pool %s: placement.preemptible false needs an existing pool with preemptible false", rp.Name)
	}
	var preemptible bool
	if err := tx.QueryRow(ctx, `SELECT preemptible FROM pools WHERE id = $1`, *rp.ID).Scan(&preemptible); err != nil {
		return err
	}
	if preemptible {
		return errf(http.StatusUnprocessableEntity, codePoolPreemptible,
			"pool %s is preemptible: placement.preemptible false needs a pool with preemptible false", rp.Name)
	}
	return nil
}
