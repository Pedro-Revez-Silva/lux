package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
		INSERT INTO pools (id, tenant_id, name, provider) VALUES ('p_old', 't1', 'Legacy_Pool', 'static'), ('p_static', 't1', 'Old_Static', 'static');
		INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ('ht_old', 't1', 'p_static', 'x')`); err != nil {
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
	if n := count(`SELECT count(*) FROM host_tokens t JOIN pools p ON p.id = t.pool_id WHERE p.name = 'Bad_Name'`); n != 0 {
		t.Errorf("a refused token was stored: %d", n)
	}
	if n := count(`SELECT max_hosts FROM pools WHERE tenant_id = 't1' AND name = 'Legacy_Pool'`); n != 4 {
		t.Errorf("legacy pool not updated: max_hosts %d", n)
	}
	if n := count(`SELECT count(*) FROM host_tokens WHERE tenant_id = 't1' AND pool_id = 'p_static'`); n != 2 {
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

// create-tenant sets expire_after_days (default 90); set-quota changes it,
// 0 meaning never, and leaves it when the flag is absent.
func TestAdminExpireAfterDays(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	days := func(name string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT expire_after_days FROM tenants WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, args := range [][]string{{"create-tenant", "--name", "dflt"}, {"create-tenant", "--name", "short", "--expire-after-days", "7"}} {
		if err := admin(ctx, cfg, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if days("dflt") != 90 || days("short") != 7 {
		t.Fatalf("created: %d and %d, want 90 and 7", days("dflt"), days("short"))
	}
	if err := admin(ctx, cfg, []string{"create-tenant", "--name", "neg", "--expire-after-days", "-1"}); err == nil {
		t.Fatal("negative --expire-after-days accepted")
	}
	var id string
	if err := db.QueryRow(ctx, `SELECT id FROM tenants WHERE name = 'short'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"set-quota", "--tenant", id, "--expire-after-days", "0"}, 0},
		{[]string{"set-quota", "--tenant", id, "--retention-days", "3"}, 0},
		{[]string{"set-quota", "--tenant", id, "--expire-after-days", "30"}, 30},
	} {
		if err := admin(ctx, cfg, c.args); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := days("short"); got != c.want {
			t.Errorf("after %v: %d, want %d", c.args, got, c.want)
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

// create-pool --provider ec2 asks EC2 (a dry run) whether the template
// would launch, and stores nothing when it would not.
func TestAdminCreatePoolChecksTheTemplateLaunches(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var dryRuns atomic.Int64
	var mu sync.Mutex
	var successfulTags map[string]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("DryRun") == "true" {
			dryRuns.Add(1)
		}
		if lt := r.PostForm.Get("LaunchTemplate.LaunchTemplateName"); lt != "lux-runner" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<Response><Errors><Error><Code>InvalidLaunchTemplateName.NotFound</Code><Message>The specified launch template, with template name %s, does not exist.</Message></Error></Errors><RequestID>1</RequestID></Response>`, lt)
			return
		}
		tags := map[string]string{}
		for i := 1; r.PostForm.Has(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i)); i++ {
			tags[r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))] = r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))
		}
		mu.Lock()
		successfulTags = tags
		mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>DryRunOperation</Code><Message>Request would have succeeded, but DryRun flag is set.</Message></Error></Errors><RequestID>1</RequestID></Response>`)
	}))
	defer fake.Close()
	cfg.EC2.Endpoint = fake.URL
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	err = admin(ctx, cfg, []string{"create-pool", "--name", "arm64", "--provider", "ec2", "--max", "4",
		"--template", `{"region": "eu-north-1", "launchTemplate": "lux-runner-arm64"}`})
	if err == nil || !strings.Contains(err.Error(), "template: ec2 cannot launch it: InvalidLaunchTemplateName.NotFound") {
		t.Fatalf("err %v, want the check's refusal", err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pools`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d pools stored after a refused create-pool", n)
	}
	if err := admin(ctx, cfg, []string{"create-pool", "--name", "arm64", "--provider", "ec2", "--max", "4",
		"--template", `{"region": "eu-north-1", "launchTemplate": "lux-runner"}`}); err != nil {
		t.Fatalf("a template that launches: %v", err)
	}
	if dryRuns.Load() != 2 {
		t.Errorf("%d dry runs, want one per create-pool", dryRuns.Load())
	}
	// The successful check's tags are a launch's in the stored pool.
	var storedID, deployment string
	if err := db.QueryRow(ctx, `SELECT id FROM pools WHERE name = 'arm64'`).Scan(&storedID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&deployment); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := successfulTags
	mu.Unlock()
	if got["lux:pool-id"] != storedID || got["lux:deployment"] != deployment || got["lux:pool"] != "arm64" {
		t.Fatalf("checked tags %v do not identify stored pool %s (deployment %s)", got, storedID, deployment)
	}
	host := got["lux:host"]
	if want := server.LaunchTags(deployment, storedID, "arm64", host); !maps.Equal(got, want) ||
		!strings.HasPrefix(host, "host_") || got["Name"] != "arm64-"+host[len(host)-8:] {
		t.Errorf("checked tags %v, want a launch's %v with a host_ id", got, want)
	}
}

// A create-pool whose pool another create-pool makes while its template is
// checked would store the pool under an id its check did not carry: it
// fails with pool_changed, and the other create's pool stays as it was.
func TestAdminCreatePoolRefusesAPoolCreatedDuringItsCheck(t *testing.T) {
	cfg, db := adminDB(t)
	ctx := context.Background()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var interleave atomic.Bool
	var otherErr error
	interleave.Store(true)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first dry run creates the pool by another create-pool
		// (a static one: no check of its own) before it answers.
		if interleave.Swap(false) {
			otherErr = admin(ctx, cfg, []string{"create-pool", "--name", "arm64", "--max", "2"})
		}
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>DryRunOperation</Code><Message>Request would have succeeded, but DryRun flag is set.</Message></Error></Errors><RequestID>1</RequestID></Response>`)
	}))
	defer fake.Close()
	cfg.EC2.Endpoint = fake.URL
	stdout := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })

	err = admin(ctx, cfg, []string{"create-pool", "--name", "arm64", "--provider", "ec2", "--max", "4",
		"--template", `{"region": "eu-north-1", "launchTemplate": "lux-runner"}`})
	if otherErr != nil {
		t.Fatalf("the other create-pool: %v", otherErr)
	}
	var he *server.HTTPError
	if !errors.As(err, &he) || he.Code != "pool_changed" ||
		!strings.Contains(err.Error(), "pool arm64 was created or replaced by another write while its template was being checked") {
		t.Fatalf("err %v, want pool_changed", err)
	}
	var provider string
	var maxHosts, n, events int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM pools), provider, max_hosts, (SELECT count(*) FROM pool_events)
		FROM pools WHERE name = 'arm64'`).Scan(&n, &provider, &maxHosts, &events); err != nil {
		t.Fatal(err)
	}
	if n != 1 || provider != "static" || maxHosts != 2 || events != 1 {
		t.Errorf("%d pools, arm64 %s max %d, %d events; want the other create's static max 2 and its one event", n, provider, maxHosts, events)
	}
}
