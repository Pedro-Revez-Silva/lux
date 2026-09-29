package server

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// MaxPoolName is the longest pool name. A pool's name reaches AWS twice:
// in the lux:pool tag ("<tenant id>/<name>", at most 256 characters) and
// in each host's name, "<pool>-<8 characters>", which also becomes the
// runner's LUX_HOST_NAME and may be used as a hostname, so it must fit one
// DNS label (63). 32 leaves room in both.
const MaxPoolName = 32

// poolNameRE: lowercase letters, digits and '-', starting and ending with
// a letter or digit, so a name is safe in a tag, a hostname and a URL.
var poolNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// PoolNameError is a pool name that breaks the rule.
type PoolNameError struct{ Name string }

func (e *PoolNameError) Error() string {
	return fmt.Sprintf("pool name %q: 1-%d characters, lowercase letters, digits and '-', starting and ending with a letter or digit", e.Name, MaxPoolName)
}

// ValidPoolName reports whether name may be given to a new pool: nil, or
// a *PoolNameError.
func ValidPoolName(name string) error {
	if len(name) > MaxPoolName || !poolNameRE.MatchString(name) {
		return &PoolNameError{Name: name}
	}
	return nil
}

// CheckPoolName is ValidPoolName for a name about to be written, except
// that a name the same owner already uses may be kept: a pool stored
// under it (live or retired), or, for a static pool that has no pool row,
// a host token or host that joined it. Updating such a pool or minting a
// replacement token must not start failing on upgrade. A *PoolNameError
// is the rule; any other error is the database's.
func CheckPoolName(ctx context.Context, tx pgx.Tx, tenantID *string, name string) error {
	err := ValidPoolName(name)
	if err == nil {
		return nil
	}
	var exists bool
	if qerr := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM pools WHERE coalesce(tenant_id, '') = coalesce($1, '') AND name = $2)
			OR EXISTS (SELECT 1 FROM host_tokens WHERE coalesce(tenant_id, '') = coalesce($1, '') AND pool = $2)
			OR EXISTS (SELECT 1 FROM hosts WHERE coalesce(tenant_id, '') = coalesce($1, '') AND pool = $2)`,
		tenantID, name).Scan(&exists); qerr != nil {
		return qerr
	}
	if exists {
		return nil
	}
	return err
}
