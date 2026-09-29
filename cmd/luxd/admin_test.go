package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/store"
)

// adminDB is a migrated database for the admin commands, and a config
// pointing at it. Needs LUX_TEST_PG, like the store tests.
func adminDB(t *testing.T) (config, *pgx.Conn) {
	t.Helper()
	root := os.Getenv("LUX_TEST_PG")
	if root == "" {
		t.Skip("LUX_TEST_PG not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, root)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	name := "lux_unit_" + ids.New("")[1:]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cc := conn.Config()
	owner := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cc.User, cc.Password, cc.Host, cc.Port, name)
	if _, err := store.Migrate(ctx, owner, "lux_app"); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close(ctx)
		conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		conn.Exec(ctx, "DROP DATABASE "+name)
		conn.Close(ctx)
	})
	var cfg config
	cfg.Database.URL = owner
	return cfg, db
}

// create-pool and create-host-token apply the pool-name rule, keep a name
// the owner already uses, and store nothing when they refuse.
func TestAdminPoolNames(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1');
		INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p_old', 't1', 'Legacy_Pool', 'static');
		INSERT INTO host_tokens (id, tenant_id, pool, token_hash) VALUES ('ht_old', 't1', 'Old_Static', 'x')`); err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	count := func(q string) int {
		var n int
		if err := db.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"create-pool", "--name", "Bad_Name"}, false},
		{[]string{"create-pool", "--name", "bad-", "--tenant", "t1"}, false},
		{[]string{"create-pool", "--name", strings.Repeat("p", server.MaxPoolName+1)}, false},
		{[]string{"create-pool", "--name", "good-1", "--tenant", "t1"}, true},
		{[]string{"create-pool", "--name", "Legacy_Pool", "--tenant", "t1", "--max", "4"}, true},
		{[]string{"create-host-token", "--pool", "Bad_Name"}, false},
		{[]string{"create-host-token", "--pool", "Old_Static", "--tenant", "t1"}, true},
		{[]string{"create-host-token", "--pool", "good-1", "--tenant", "t1"}, true},
	} {
		err := admin(ctx, cfg, c.args)
		var pne *server.PoolNameError
		switch {
		case c.ok && err != nil:
			t.Errorf("%v: refused: %v", c.args, err)
		case !c.ok && !errors.As(err, &pne):
			t.Errorf("%v: err %v, want a *server.PoolNameError", c.args, err)
		}
	}
	if n := count(`SELECT count(*) FROM pools WHERE name IN ('Bad_Name', 'bad-') OR length(name) > 32`); n != 0 {
		t.Errorf("refused pools were stored: %d", n)
	}
	if n := count(`SELECT count(*) FROM host_tokens WHERE pool = 'Bad_Name'`); n != 0 {
		t.Errorf("a refused token was stored: %d", n)
	}
	if n := count(`SELECT max_hosts FROM pools WHERE tenant_id = 't1' AND name = 'Legacy_Pool'`); n != 4 {
		t.Errorf("legacy pool not updated: max_hosts %d", n)
	}
	if n := count(`SELECT count(*) FROM host_tokens WHERE tenant_id = 't1' AND pool = 'Old_Static'`); n != 2 {
		t.Errorf("no replacement token for the legacy static pool: %d tokens", n)
	}
}

// create-pool --default marks the pool its owner's default, moves the mark
// from another pool, and --default=false clears it; omitted leaves it.
func TestAdminPoolDefault(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	defaults := func() []string {
		rows, err := db.Query(ctx, `SELECT coalesce(tenant_id, '-') || '/' || name FROM pools WHERE is_default ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"create-pool", "--name", "a", "--tenant", "t1", "--default"}, []string{"t1/a"}},
		{[]string{"create-pool", "--name", "b", "--tenant", "t1", "--default"}, []string{"t1/b"}},
		{[]string{"create-pool", "--name", "b", "--tenant", "t1", "--max", "2"}, []string{"t1/b"}},
		{[]string{"create-pool", "--name", "shared", "--default"}, []string{"-/shared", "t1/b"}},
		{[]string{"create-pool", "--name", "b", "--tenant", "t1", "--default=false"}, []string{"-/shared"}},
	} {
		if err := admin(ctx, cfg, c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := defaults(); !slices.Equal(got, c.want) {
			t.Errorf("after %v: defaults %v, want %v", c.args, got, c.want)
		}
	}
}

// create-pool --default writes the pool, its mark and their events in one
// transaction: each pool the mark moves between records it, and when an
// event cannot be written neither the settings nor the mark change.
func TestAdminPoolDefaultEvents(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	for _, args := range [][]string{
		{"create-pool", "--name", "a", "--default"},
		{"create-pool", "--name", "b", "--max", "2", "--default"},
	} {
		if err := admin(ctx, cfg, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	var got []string
	rows, err := db.Query(ctx, `SELECT p.name || ' ' || e.type || ' ' || coalesce(e.data->'changes'->'isDefault'->>'old', 'null') || '→' || (e.data->'changes'->'isDefault'->>'new')
		FROM pool_events e JOIN pools p ON p.id = e.pool_id ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"a pool.config_changed null→true", "b pool.config_changed null→true", "a pool.config_changed true→false"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}

	if _, err := db.Exec(ctx, `CREATE FUNCTION refuse_event() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'event refused'; END $$;
		CREATE TRIGGER refuse BEFORE INSERT ON pool_events FOR EACH ROW EXECUTE FUNCTION refuse_event()`); err != nil {
		t.Fatal(err)
	}
	if err := admin(ctx, cfg, []string{"create-pool", "--name", "a", "--max", "5", "--default"}); err == nil {
		t.Fatal("create-pool succeeded with its events refused")
	}
	var marked string
	var maxA int
	if err := db.QueryRow(ctx, `SELECT (SELECT name FROM pools WHERE is_default), (SELECT max_hosts FROM pools WHERE name = 'a')`).Scan(&marked, &maxA); err != nil {
		t.Fatal(err)
	}
	if marked != "b" || maxA != 0 {
		t.Fatalf("after a refused create-pool: default %q, a's max_hosts %d; want b and 0", marked, maxA)
	}
}

// create-pool marks a new pool migrated only while no older luxd (one
// without pool-id-discovery, which launches name-only instances) has been
// seen.
func TestAdminCreatePoolMigratedMark(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	unmarked := func(name string) bool {
		var v bool
		if err := db.QueryRow(ctx, `SELECT id_migrated_at IS NULL FROM pools WHERE name = $1`, name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := admin(ctx, cfg, []string{"create-pool", "--name", "a", "--provider", "ec2"}); err != nil {
		t.Fatal(err)
	}
	if unmarked("a") {
		t.Fatal("a pool created with no older luxd seen was not marked migrated")
	}
	if _, err := db.Exec(ctx, `INSERT INTO control_samples (instance, hostname, res, at) VALUES ('luxd-old', 'old-host', 0, now())`); err != nil {
		t.Fatal(err)
	}
	if err := admin(ctx, cfg, []string{"create-pool", "--name", "b", "--provider", "ec2"}); err != nil {
		t.Fatal(err)
	}
	if !unmarked("b") {
		t.Fatal("a pool created while an older luxd runs was marked migrated")
	}
}
