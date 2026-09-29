package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

func httpErr(err error) (int, string, string) {
	var he *HTTPError
	if !errors.As(err, &he) {
		return 0, "", ""
	}
	return he.Status, he.Code, he.Error()
}

// A pool that just finished a rename is not renamed again until a late
// re-tag from an expired provisioner can no longer land; the refusal
// says how long remains.
func TestRenamePoolCooldown(t *testing.T) {
	f := newRenameFixture(t, false)
	f.rename(t, "burst", "burst-eu")
	execSQL(t, f.s, f.ctx, `UPDATE pools SET renamed_from = NULL, retagged_at = NULL, rename_finished_at = now()`)
	_, err := renameConfirmed(f.ctx, f.s, "t1", "burst-eu", "burst-us", false)
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "rename_cooldown" || !strings.Contains(msg, "renamed again in") {
		t.Fatalf("rename right after the last finished: %v", err)
	}
	execSQL(t, f.s, f.ctx, `UPDATE pools SET rename_finished_at = now() - $1::interval`, interval(f.s.renameCooldown()))
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "burst-eu", "burst-us", false); err != nil {
		t.Fatalf("rename after the cooldown: %v", err)
	}
}

// The provisioner finishing a rename records when, which starts the
// cooldown.
func TestRenamePoolFinishStartsCooldown(t *testing.T) {
	f := newRenameFixture(t, false)
	f.s.cfg.ListingLag = 0
	f.rename(t, "burst", "burst-eu")
	f.check(t, f.pool(t))
	f.check(t, f.pool(t))
	if f.pool(t).RenamedFrom != nil {
		t.Fatal("rename never finished")
	}
	_, err := renameConfirmed(f.ctx, f.s, "t1", "burst-eu", "burst-us", false)
	if _, code, _ := httpErr(err); code != "rename_cooldown" {
		t.Fatalf("rename right after the provisioner finished one: %v", err)
	}
}

// A provisioned pool is not renamed while a luxd that cannot follow a
// rename (it writes control samples, never checks in) is running; a
// static pool is. The refusal names the luxd.
func TestRenamePoolRefusedWithAnOlderLuxd(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `INSERT INTO control_samples (instance, hostname, res, at) VALUES ('luxd-old', 'old-host', 0, now())`)
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ps', 't1', 'lab', 'static')`)
	_, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", false)
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "rename_unsupported_by_deployment" || !strings.Contains(msg, "luxd-old") {
		t.Fatalf("ec2 pool rename with an old luxd running: %v", err)
	}
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "lab", "lab2", false); err != nil {
		t.Fatalf("static pool rename with an old luxd running: %v", err)
	}
	// Its samples age out of the window once it has stopped.
	execSQL(t, f.s, f.ctx, `UPDATE control_samples SET at = now() - $1::interval`, interval(3*f.s.provisionLeaseDuration()))
	if _, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", false); err != nil {
		t.Fatalf("rename once the old luxd stopped: %v", err)
	}
}

// A luxd holding the provisioner lease that never checked in (an older
// binary, before its first sample) blocks a rename too.
func TestRenamePoolRefusedWhileAnOlderLuxdHoldsTheLease(t *testing.T) {
	f := newRenameFixture(t, false)
	execSQL(t, f.s, f.ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ('provisioner', 'luxd-old', now() + interval '30 seconds')`)
	_, err := renameConfirmed(f.ctx, f.s, "t1", "burst", "burst-eu", false)
	if _, code, _ := httpErr(err); code != "rename_unsupported_by_deployment" {
		t.Fatalf("rename with an old luxd holding the lease: %v", err)
	}
}

// Renaming a platform pool moves the Runs of tenants without a pool of the
// old name; one of them owning a pool of the new name would find its Runs
// resolved to that pool. Refused, naming the tenant.
func TestRenamePlatformPoolOntoATenantsPoolName(t *testing.T) {
	s := testServer(t)
	ctx := t.Context()
	if err := s.checkIn(ctx); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 'acme'), ('t2', 'other')`)
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES
		('plat', NULL, 'shared', 'static'), ('mine', 't1', 'gpu', 'static'), ('theirs', 't2', 'big', 'static')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES
		('r1', 't1', '{"placement":{"pool":"shared"}}', 'provisioning'),
		('r2', 't2', '{"placement":{"pool":"shared"}}', 'succeeded')`)
	_, err := renameConfirmed(ctx, s, "", "shared", "gpu", false)
	if st, code, msg := httpErr(err); st != http.StatusConflict || code != "pool_exists" || !strings.Contains(msg, "acme") {
		t.Fatalf("platform rename onto a moved tenant's pool name: %v", err)
	}
	// t2's only Run is final: it does not move, and its pool "big" is no
	// obstacle.
	if _, err := renameConfirmed(ctx, s, "", "shared", "big", false); err != nil {
		t.Fatalf("platform rename onto a name only an unaffected tenant has: %v", err)
	}
}

// A pool with hosts is renamed only with its current name as confirmation,
// whatever a client counted before.
func TestRenamePoolNeedsConfirmationWithHosts(t *testing.T) {
	f := newRenameFixture(t, false)
	for _, confirm := range []string{"", "burst-eu"} {
		_, err := f.s.rename(f.ctx, "t1", renameArgs{From: "burst", To: "burst-eu", Confirm: confirm})
		if st, code, _ := httpErr(err); st != http.StatusConflict || code != "confirm_required" {
			t.Errorf("confirm %q: %v", confirm, err)
		}
	}
	if f.pool(t).Name != "burst" {
		t.Fatal("renamed without confirmation")
	}
	execSQL(t, f.s, f.ctx, `INSERT INTO pools (id, tenant_id, name, provider) VALUES ('ps', 't1', 'empty', 'static')`)
	if _, err := f.s.rename(f.ctx, "t1", renameArgs{From: "empty", To: "empty2"}); err != nil {
		t.Errorf("a pool without hosts needs no confirmation: %v", err)
	}
}

// A pool's provider does not change while its rename is unfinished, by
// the API or luxd admin create-pool (CheckPoolProviderChange); its other
// settings do.
func TestPoolProviderFixedWhileRenaming(t *testing.T) {
	f := newRenameFixture(t, false)
	f.rename(t, "burst", "burst-eu")
	ctx := context.WithValue(f.ctx, principalKey, Principal{TenantID: "t1", Scopes: []string{"admin"}})
	_, err := f.s.putPool(ctx, &poolBody{Body: Pool{Name: "burst-eu", Provider: "static"}})
	if st, code, _ := httpErr(err); st != http.StatusConflict || code != "rename_in_progress" {
		t.Errorf("pools set --provider static mid-rename: %v", err)
	}
	if _, err := f.s.putPool(ctx, &poolBody{Body: Pool{Name: "burst-eu", Provider: "ec2", MaxHosts: 5}}); err != nil {
		t.Errorf("pools set, same provider, mid-rename: %v", err)
	}
	err = f.s.db.Tx(f.ctx, store.System(), func(tx pgx.Tx) error {
		return CheckPoolProviderChange(f.ctx, tx, "t1", "burst-eu", "static")
	})
	if _, code, _ := httpErr(err); code != "rename_in_progress" {
		t.Errorf("create-pool --provider static mid-rename: %v", err)
	}
	if f.pool(t).Provider != "ec2" {
		t.Fatal("provider changed")
	}
}

// A cost hour written under the old name after the rename committed (its
// writer read the host row before) is moved by the provisioner's rename
// steps: the re-tagging pass, and the one that finishes the rename.
// Another pool's hours under that name are not.
func TestRenamePoolMovesLateCostHours(t *testing.T) {
	f := newRenameFixture(t, false)
	f.s.cfg.ListingLag = 0
	f.rename(t, "burst", "burst-eu")
	execSQL(t, f.s, f.ctx, `INSERT INTO hosts (id, name, pool, state) VALUES ('hx', 'x', 'burst', 'ready')`)
	late := func(host string) {
		execSQL(t, f.s, f.ctx, `INSERT INTO cost_hourly (hour, source, family, currency, host_id, pool, allocated) VALUES
			(date_trunc('hour', now()), 'compute', 'compute', 'USD', $1, 'burst', 1)`, host)
	}
	pools := func() string {
		return f.query(t, `SELECT string_agg(host_id || '=' || pool, ',' ORDER BY host_id) FROM cost_hourly`)
	}
	late("h1")
	late("hx")
	f.check(t, f.pool(t)) // re-tags
	if got := pools(); got != "h1=burst-eu,hx=burst" {
		t.Errorf("after the re-tagging pass: %s, want h1=burst-eu,hx=burst", got)
	}
	late("h2")
	f.check(t, f.pool(t)) // finishes
	if f.pool(t).RenamedFrom != nil {
		t.Fatal("rename never finished")
	}
	if got := pools(); got != "h1=burst-eu,h2=burst-eu,hx=burst" {
		t.Errorf("after the finishing pass: %s, want h1=burst-eu,h2=burst-eu,hx=burst", got)
	}
}
