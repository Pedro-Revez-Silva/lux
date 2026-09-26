// Package store is luxd's access to Postgres.
//
// Every query runs inside a scope that sets the row-level-security context
// for its transaction: Tenant(id) for work on behalf of one tenant, System()
// for the scheduler, reapers and runner endpoints. luxd connects as a role
// that cannot bypass RLS, so a query without a scope sees nothing.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Scope is the RLS context of a transaction.
type Scope struct {
	tenant string
	system bool
}

func Tenant(id string) Scope { return Scope{tenant: id} }
func System() Scope          { return Scope{system: true} }

// Tx runs fn in a transaction with the scope applied. The settings are
// transaction-local, so a pooled connection never carries one request's
// tenant into the next.
func (s *Store) Tx(ctx context.Context, sc Scope, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if sc.system {
			if _, err := tx.Exec(ctx, "SELECT set_config('lux.system', 'on', true)"); err != nil {
				return err
			}
		} else {
			if sc.tenant == "" {
				return errors.New("store: empty tenant scope")
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('lux.tenant_id', $1, true)", sc.tenant); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

// Migrate applies pending migrations as the database owner, then makes sure
// the application role exists with exactly the grants it needs.
func Migrate(ctx context.Context, dsn, appPassword string) ([]string, error) {
	return migrate(ctx, dsn, appPassword, "")
}

// migrate applies pending migrations up to and including version upTo
// ("" for all of them).
func migrate(ctx context.Context, dsn, appPassword, upTo string) ([]string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// One migrator at a time.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(7431)"); err != nil {
		return nil, err
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock(7431)")

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	rows.Close()

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var done []string
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		if upTo != "" && version > upTo {
			break
		}
		if applied[version] {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return nil, err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("%s: %w", version, err)
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			return done, err
		}
		done = append(done, version)
	}
	return done, ensureAppRole(ctx, conn, appPassword)
}

// ensureAppRole creates lux_app. It has neither SUPERUSER nor BYPASSRLS:
// either would silently switch row-level security off, and every isolation
// test would pass for the wrong reason.
func ensureAppRole(ctx context.Context, conn *pgx.Conn, password string) error {
	// Roles are the cluster's, not the database's, and advisory locks are
	// per database: migrations of two databases at once (parallel test
	// packages) can collide on the role. It is idempotent: try again.
	var err error
	for range 5 {
		var pe *pgconn.PgError
		// XX000 "tuple concurrently updated", 23505 a role created at once.
		if err = ensureAppRoleOnce(ctx, conn, password); err == nil || !errors.As(err, &pe) || (pe.Code != "XX000" && pe.Code != "23505") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func ensureAppRoleOnce(ctx context.Context, conn *pgx.Conn, password string) error {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lux_app')").Scan(&exists); err != nil {
		return err
	}
	quoted := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	stmts := []string{}
	if !exists {
		stmts = append(stmts, "CREATE ROLE lux_app LOGIN PASSWORD "+quoted)
	} else if password != "" {
		stmts = append(stmts, "ALTER ROLE lux_app PASSWORD "+quoted)
	}
	stmts = append(stmts,
		"ALTER ROLE lux_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE",
		"GRANT USAGE ON SCHEMA public TO lux_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO lux_app",
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO lux_app",
		"REVOKE ALL ON schema_migrations FROM lux_app",
		// Events are an audit trail: the app appends, never rewrites.
		"REVOKE UPDATE, DELETE ON run_events FROM lux_app",
	)
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("app role: %s: %w", strings.SplitN(s, " PASSWORD", 2)[0], err)
		}
	}
	return nil
}
