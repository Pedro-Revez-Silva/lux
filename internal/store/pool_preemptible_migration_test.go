package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 091: an ec2 pool whose template launched spot stays preemptible; one
// without spot (or spot false) launched on-demand and becomes
// non-preemptible; a static pool stays preemptible.
func TestPoolPreemptibleMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "090_run_events_tenant_id"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO pools (id, name, provider, template) VALUES
		('p_spot', 'spot', 'ec2', '{"region": "eu-west-1", "spot": true}'),
		('p_od', 'od', 'ec2', '{"region": "eu-west-1"}'),
		('p_false', 'false', 'ec2', '{"region": "eu-west-1", "spot": false}'),
		('p_static', 'static', 'static', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"p_spot": true, "p_od": false, "p_false": false, "p_static": true}
	for id, w := range want {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT preemptible FROM pools WHERE id = $1`, id).Scan(&got); err != nil || got != w {
			t.Errorf("%s: preemptible %v (%v), want %v", id, got, err, w)
		}
	}
	// A pool created afterwards without the column is preemptible.
	if _, err := conn.Exec(ctx, `INSERT INTO pools (id, name, provider) VALUES ('p_new', 'new', 'ec2')`); err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT preemptible FROM pools WHERE id = 'p_new'`).Scan(&got); err != nil || !got {
		t.Errorf("new pool: preemptible %v (%v), want true", got, err)
	}
}
