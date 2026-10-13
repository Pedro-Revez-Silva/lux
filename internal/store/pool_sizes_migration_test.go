package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 071: pools and Runs from before it read back unchanged, with no shape,
// request or size; the new columns round-trip what luxd writes.
func TestPoolSizesMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "070_instance_types"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const legacySpec = `{"resources": {"cpus": 2, "disk": 21474836480, "memory": 8589934592, "pids": 4096}}`
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('t1', 't1')`,
		`INSERT INTO pools (id, name, provider, template) VALUES ('pool_old', 'old', 'ec2', '{"region": "eu-west-1"}')`,
		`INSERT INTO runs (id, tenant_id, spec, state, pool_id) VALUES ('r_old', 't1', '` + legacySpec + `', 'stopped', 'pool_old')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}

	var shape, requested, size *string
	var template string
	var specSame bool
	if err := conn.QueryRow(ctx, `SELECT shape::text, template::text FROM pools WHERE id = 'pool_old'`).Scan(&shape, &template); err != nil {
		t.Fatal(err)
	}
	if shape != nil || template != `{"region": "eu-west-1"}` {
		t.Errorf("legacy pool: shape %v template %s", shape, template)
	}
	if err := conn.QueryRow(ctx, `SELECT requested_resources::text, resolved_size, spec = $1::jsonb FROM runs WHERE id = 'r_old'`, legacySpec).
		Scan(&requested, &size, &specSame); err != nil {
		t.Fatal(err)
	}
	if requested != nil || size != nil || !specSame {
		t.Errorf("legacy Run: requested %v size %v, spec unchanged %v", requested, size, specSame)
	}

	const wantShape = `{"cpus": 8, "arch": "arm64", "disk": 171798691840, "family": "m", "memory": 34359738368, "minCpus": 0.5, "defaultCpus": 2}`
	const wantRequest = `{"cpus": 3, "memory": 10737418240}`
	for _, q := range []string{
		`INSERT INTO pools (id, name, provider, shape) VALUES ('pool_m', 'm', 'static', '` + wantShape + `')`,
		`INSERT INTO runs (id, tenant_id, spec, state, pool_id, requested_resources, resolved_size)
			VALUES ('r_new', 't1', '{}', 'submitted', 'pool_m', '` + wantRequest + `', 'm-4-arm64')`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var gotShape, gotRequest, gotSize string
	if err := conn.QueryRow(ctx, `SELECT p.shape->>'family', r.requested_resources->>'memory', r.resolved_size
		FROM runs r JOIN pools p ON p.id = r.pool_id WHERE r.id = 'r_new'`).Scan(&gotShape, &gotRequest, &gotSize); err != nil {
		t.Fatal(err)
	}
	if gotShape != "m" || gotRequest != "10737418240" || gotSize != "m-4-arm64" {
		t.Errorf("round trip: family %q memory %q size %q", gotShape, gotRequest, gotSize)
	}
}
