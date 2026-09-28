package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// 030 drops lux:* keys from EC2 pool templates stored before putPool
// refused them (production had {"lux:pool": "arm64"} from the Terraform
// module), keeping other tags and every other field. A template left with
// no tags loses "tags"; static pools and tag-free templates are untouched.
func TestPoolTemplateTagsMigration(t *testing.T) {
	owner, _ := emptyDB(t)
	ctx := context.Background()
	if _, err := store.MigrateTo(ctx, owner, "lux_app", "029_process_samples"); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES
		('p1', 't1', 'only-lux', 'ec2', '{"region": "eu-north-1", "spot": true, "tags": {"lux:pool": "arm64"}}'),
		('p2', 't1', 'mixed', 'ec2', '{"region": "eu-north-1", "tags": {"LUX:Host": "x", "team": "platform"}}'),
		('p3', 't1', 'clean', 'ec2', '{"region": "eu-north-1", "tags": {"team": "platform"}}'),
		('p4', 't1', 'static', 'static', '{"tags": {"lux:pool": "keep"}}'),
		('p5', 't1', 'none', 'ec2', '{"region": "eu-north-1"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"p1": `{"spot": true, "region": "eu-north-1"}`,
		"p2": `{"tags": {"team": "platform"}, "region": "eu-north-1"}`,
		"p3": `{"tags": {"team": "platform"}, "region": "eu-north-1"}`,
		"p4": `{"tags": {"lux:pool": "keep"}}`,
		"p5": `{"region": "eu-north-1"}`,
	}
	for id, w := range want {
		var got, exp string
		if err := conn.QueryRow(ctx, `SELECT template::text, $2::jsonb::text FROM pools WHERE id = $1`, id, w).Scan(&got, &exp); err != nil {
			t.Fatal(err)
		}
		if got != exp {
			t.Errorf("%s: template %s, want %s", id, got, exp)
		}
	}
}
