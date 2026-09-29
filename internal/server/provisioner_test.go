package server

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// poolState must not put a draining host in terminate while it still has
// a live placement: draining (from scale-down or a plain drain) only
// cordons, and the host's Run runs to completion. Once that placement has
// exited, the host is picked up for termination.
func TestPoolStateHoldsBackADrainingHostWithALivePlacement(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('pool1', 't1', 'burst', 'ec2')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, pool_id, state, draining, provider_id, provision_requested_at)
		VALUES ('h1', 't1', 'h1', 'pool1', 'draining', true, 'i-123', now())`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES ('r1', 't1', '{}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES ('p1', 't1', 'r1', 'h1', 1, 'running')`)

	pl := poolRow{ID: "pool1", Name: "burst", Provider: "ec2", TenantID: new("t1")}

	var st poolState
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(st.terminate, func(h hostRef) bool { return h.ID == "h1" }) {
		t.Error("a draining host with a live placement is in terminate; want it held back until idle")
	}
	if !slices.ContainsFunc(st.existing, func(h hostRef) bool { return h.ID == "h1" && h.Draining }) {
		t.Error("the draining host is missing from existing")
	}

	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited', exited_at = now() WHERE id = 'p1'`)

	st = poolState{}
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return s.poolState(ctx, tx, pl, &st) }); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(st.terminate, func(h hostRef) bool { return h.ID == "h1" }) {
		t.Error("a draining, idle host (its placement exited) is not in terminate")
	}
}
