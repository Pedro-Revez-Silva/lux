package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

const maxCheckError = 300

// CheckPoolTemplate checks before persistence, outside the write transaction.
// Providers without Checker and identical live provider/templates skip the check.
// Launch tags use the stored pool id (newID on create) and a representative host.
// The returned checked id, or "" for a skip, must be passed to ConfirmCheckedPool.
func CheckPoolTemplate(ctx context.Context, db *store.Store, providers map[string]Provider, tenantID, name, newID, provider string, template map[string]any) (string, error) {
	checker, ok := providers[provider].(Checker)
	if !ok {
		return "", nil
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	stored, err := readCheckedPool(ctx, db, tenantID, name)
	if err != nil {
		return "", err
	}
	if stored.live && stored.provider == provider {
		same, err := sameTemplate(stored.template, raw)
		if err != nil || same {
			return "", err
		}
	}
	poolID := newID
	if stored.id != "" {
		poolID = stored.id
	}
	tags := LaunchTags(stored.deployment, poolID, name, ids.New(ids.Host))
	if err := checker.Check(ctx, raw, tags); err != nil {
		return "", errf(http.StatusUnprocessableEntity, "invalid_pool", "template: %s cannot launch it: %s",
			provider, truncate(providerErrorText(err), maxCheckError))
	}
	return poolID, nil
}

// ConfirmCheckedPool compares the upsert's RETURNING id with the checked id.
// Return its error inside the write transaction to roll back a concurrent
// creation or replacement. An empty checkedID means no check ran.
func ConfirmCheckedPool(name, storedID, checkedID string) error {
	if checkedID == "" || storedID == checkedID {
		return nil
	}
	return errf(http.StatusConflict, "pool_changed",
		"pool %s was created or replaced by another write while its template was being checked: nothing was stored; set it again", name)
}

// Retired pools are included: reviving one preserves its id.
type checkedPool struct {
	deployment, id, provider string
	live                     bool
	template                 map[string]any
}

func readCheckedPool(ctx context.Context, db *store.Store, tenantID, name string) (checkedPool, error) {
	var out checkedPool
	err := db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'deployment'`).Scan(&out.deployment); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id, provider, template, NOT retired FROM pools
			WHERE tenant_id IS NOT DISTINCT FROM nullif($1, '') AND name = $2`, tenantID, name).Scan(&out.id, &out.provider, &out.template, &out.live)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}

// sameTemplate: stored and raw are equal as JSON values (key order and
// number spelling aside).
func sameTemplate(stored map[string]any, raw []byte) (bool, error) {
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		return false, err
	}
	if want == nil {
		want = map[string]any{}
	}
	if stored == nil {
		stored = map[string]any{}
	}
	return reflect.DeepEqual(stored, want), nil
}
