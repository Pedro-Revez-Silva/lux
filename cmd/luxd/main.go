// Command luxd is the lux control plane.
//
//	luxd migrate                                  apply migrations (as the database owner)
//	luxd admin create-tenant --name N             → {"tenantId", "apiKey"}
//	luxd admin create-key --tenant T [--scopes run,read]
//	luxd admin create-operator-key [--name N]    → {"apiKey"}: every tenant
//	luxd admin create-host-token [--tenant T] [--pool P] [--label k=v]  → {"token"}
//	luxd admin create-pool --name N --provider static|ec2 [--tenant T] [--shared] ...
//	luxd admin set-quota --tenant T [--max-runs N] [--max-hosts N] [--retention-days N]
//	luxd serve                                    run the API, scheduler and reapers
//	luxd openapi                                  print the tenant API's OpenAPI spec (YAML)
//
// Configuration is a TOML file (--config, LUX_CONFIG, or /etc/lux/luxd.toml),
// overridden by environment variables; see docs/operations.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
	"github.com/marcioapm/lux/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path, rest, err := configFlag(os.Args[1:])
	if err != nil || len(rest) == 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "luxd:", err)
		}
		usage()
	}
	cmd, args := rest[0], rest[1:]
	var cfg config
	switch cmd {
	case "migrate", "admin", "serve":
		if cfg, err = loadConfig(path); err != nil {
			break
		}
		if cmd != "admin" && len(args) > 0 {
			err = fmt.Errorf("%s takes no arguments: %s", cmd, strings.Join(args, " "))
			break
		}
		switch cmd {
		case "migrate":
			err = migrate(ctx, cfg)
		case "admin":
			err = admin(ctx, cfg, args)
		case "serve":
			err = serve(ctx, cfg)
		}
	case "openapi":
		var doc []byte
		if doc, err = server.OpenAPI(); err == nil {
			_, err = os.Stdout.Write(doc)
		}
	case "version", "--version":
		fmt.Println(version.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "luxd:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: luxd [--config FILE] migrate | admin <command> | serve | openapi | version

Configuration: FILE (TOML), else LUX_CONFIG, else /etc/lux/luxd.toml if it
exists; environment variables override it (docs/operations.md).

admin commands:
  create-tenant --name N [--max-runs N] [--max-hosts N] [--retention-days N]
  create-key --tenant T [--name N] [--scopes read,run,admin]
  create-operator-key [--name N]
  create-host-token [--tenant T] [--pool P] [--label k=v ...]
  create-pool --name N --provider static|ec2 [--tenant T] [--shared]
              [--min N] [--max N] [--warm N] [--template JSON]
  set-quota --tenant T [--max-runs N] [--max-hosts N] [--max-storage BYTES] [--retention-days N]`)
	os.Exit(2)
}

func migrate(ctx context.Context, cfg config) error {
	dsn, err := require(cfg.Database.URL, "database.url", "LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	applied, err := store.Migrate(ctx, dsn, cfg.Database.AppPassword)
	for _, v := range applied {
		fmt.Fprintln(os.Stderr, "applied", v)
	}
	return err
}

func serve(ctx context.Context, c config) error {
	level := slog.LevelInfo
	if bool(c.Debug) {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	dsn, err := require(c.Database.URL, "database.url", "LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	bucket, err := require(c.S3.Bucket, "s3.bucket", "LUX_S3_BUCKET")
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	blobs, err := blob.New(ctx, blob.Config{
		Endpoint:       c.S3.Endpoint,
		PublicEndpoint: c.S3.PublicEndpoint,
		Region:         c.S3.Region,
		Bucket:         bucket,
		AccessKey:      c.S3.AccessKey,
		SecretKey:      c.S3.SecretKey,
	})
	if err != nil {
		return err
	}
	if err := blobs.Check(ctx); err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	srv := server.New(server.Config{
		Listen:               c.Listen,
		PublicURL:            c.PublicURL,
		RunnerURL:            c.RunnerURL,
		RunnerBinDir:         c.RunnerBinDir,
		LeaseDuration:        c.Lease.Duration,
		Tick:                 c.Tick.Duration,
		ScaleDownAfter:       c.ScaleDownAfter.Duration,
		LaunchTimeout:        c.LaunchTimeout.Duration,
		ProviderCheckEvery:   c.ProviderCheckEvery.Duration,
		LostGrace:            c.LostGrace.Duration,
		ListingLag:           c.ListingLag.Duration,
		OutdatedDrainPercent: c.OutdatedDrainPercent,
		Defaults:             spec.Defaults{CPUs: c.Defaults.CPUs, Memory: c.Defaults.Memory.Bytes, Disk: c.Defaults.Disk.Bytes, Pids: c.Defaults.Pids},
		SampleEvery:          c.History.SampleEvery.Duration,
		HistoryRaw:           c.History.Raw.Duration,
		HistoryMinutes:       c.History.Minutes.Duration,
		HistoryHours:         c.History.Hours.Duration,
		Providers:            providers(c),
		ConsoleAuth: server.ConsoleAuth{
			Mode:   c.Console.Auth,
			CFTeam: c.Console.CloudflareAccess.Team,
			CFAud:  c.Console.CloudflareAccess.AUD,
		},
	}, db, blobs, log)
	return srv.Run(ctx)
}

type labelsFlag map[string]string

func (l labelsFlag) String() string { return "" }
func (l labelsFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("want key=value")
	}
	l[k] = val
	return nil
}

func admin(ctx context.Context, cfg config, args []string) error {
	if len(args) == 0 {
		usage()
	}
	dsn, err := require(cfg.Database.URL, "database.url", "LUX_DATABASE_URL")
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	out := json.NewEncoder(os.Stdout)
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	sys := store.System()

	switch args[0] {
	case "create-tenant":
		name := fs.String("name", "", "tenant name")
		maxRuns := fs.Int("max-runs", 0, "max concurrent runs (0: unlimited)")
		maxHosts := fs.Int("max-hosts", 0, "max hosts (0: unlimited)")
		retention := fs.Int("retention-days", 30, "days to keep finished runs' blobs")
		fs.Parse(args[1:])
		if *name == "" {
			return errors.New("--name is required")
		}
		tenantID := ids.New(ids.Tenant)
		key := ids.Secret("lux")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, retention_days, max_concurrent_runs, max_hosts)
				VALUES ($1, $2, $3, nullif($4, 0), nullif($5, 0))`, tenantID, *name, *retention, *maxRuns, *maxHosts); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'bootstrap', $3, $4)`,
				ids.New(ids.APIKey), tenantID, ids.Hash(key), []string{"admin"})
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"tenantId": tenantID, "apiKey": key})

	case "create-key":
		tenant := fs.String("tenant", "", "tenant id")
		name := fs.String("name", "key", "key name")
		scopes := fs.String("scopes", "run", "comma-separated: read, run, admin")
		fs.Parse(args[1:])
		if *tenant == "" {
			return errors.New("--tenant is required")
		}
		sc := strings.Split(*scopes, ",")
		for _, s := range sc {
			if s != "read" && s != "run" && s != "admin" {
				return fmt.Errorf("unknown scope %q", s)
			}
		}
		key := ids.Secret("lux")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, $3, $4, $5)`,
				ids.New(ids.APIKey), *tenant, *name, ids.Hash(key), sc)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"apiKey": key})

	case "create-operator-key":
		// An operator key belongs to no tenant: it reads and acts on every
		// tenant's Runs and hosts. Only ever made here, never over the API.
		name := fs.String("name", "operator", "key name")
		fs.Parse(args[1:])
		key := ids.Secret("lux")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, NULL, $2, $3, $4)`,
				ids.New(ids.APIKey), *name, ids.Hash(key), []string{"operator"})
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"apiKey": key})

	case "create-host-token":
		tenant := fs.String("tenant", "", "tenant id (empty: a platform host)")
		pool := fs.String("pool", "default", "pool the hosts join")
		labels := labelsFlag{}
		fs.Var(labels, "label", "host label key=value (repeatable)")
		fs.Parse(args[1:])
		token := ids.Secret("luxh")
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO host_tokens (id, tenant_id, pool, labels, token_hash) VALUES ($1, nullif($2, ''), $3, $4, $5)`,
				ids.New(ids.HostToken), *tenant, *pool, map[string]string(labels), ids.Hash(token))
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"token": token})

	case "create-pool":
		name := fs.String("name", "", "pool name")
		provider := fs.String("provider", "static", "static | ec2")
		tenant := fs.String("tenant", "", "tenant id (empty: a platform pool)")
		shared := fs.Bool("shared", false, "platform pool whose hosts may run several tenants' Runs")
		minH := fs.Int("min", 0, "minimum hosts")
		maxH := fs.Int("max", 0, "maximum hosts")
		warm := fs.Int("warm", 0, "idle hosts to keep ready")
		scaleDown := fs.Duration("scale-down-after", 0, "how long a host stays idle before it is released (default: scale_down_after)")
		warmActive := fs.Bool("warm-while-active", false, "keep --warm hosts only while the pool is in use")
		template := fs.String("template", "{}", "provider template (JSON)")
		fs.Parse(args[1:])
		var tmpl map[string]any
		if err := json.Unmarshal([]byte(*template), &tmpl); err != nil {
			return fmt.Errorf("--template: %w", err)
		}
		if *shared && *tenant != "" {
			return errors.New("only platform pools (no --tenant) can be shared")
		}
		id := ids.New(ids.Pool)
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO pools (id, tenant_id, name, provider, template, min_hosts, max_hosts, warm_hosts, shared,
					scale_down_after_s, warm_while_active)
				VALUES ($1, nullif($2, ''), $3, $4, $5, $6, $7, $8, $9, nullif($10, 0), $11)
				ON CONFLICT (coalesce(tenant_id, ''), name) DO UPDATE SET provider = EXCLUDED.provider, template = EXCLUDED.template,
					min_hosts = EXCLUDED.min_hosts, max_hosts = EXCLUDED.max_hosts, warm_hosts = EXCLUDED.warm_hosts, shared = EXCLUDED.shared,
					scale_down_after_s = EXCLUDED.scale_down_after_s, warm_while_active = EXCLUDED.warm_while_active,
					retired = false`,
				id, *tenant, *name, *provider, tmpl, *minH, *maxH, *warm, *shared, int(*scaleDown/time.Second), *warmActive)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"pool": *name})

	case "set-quota":
		tenant := fs.String("tenant", "", "tenant id")
		maxRuns := fs.Int("max-runs", -1, "max concurrent runs (0: unlimited)")
		maxHosts := fs.Int("max-hosts", -1, "max hosts (0: unlimited)")
		maxStorage := fs.Int64("max-storage", -1, "max stored bytes (0: unlimited)")
		retention := fs.Int("retention-days", -1, "days to keep finished runs' blobs")
		fs.Parse(args[1:])
		err := db.Tx(ctx, sys, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET
					max_concurrent_runs = CASE WHEN $2 < 0 THEN max_concurrent_runs ELSE nullif($2, 0) END,
					max_hosts = CASE WHEN $3 < 0 THEN max_hosts ELSE nullif($3, 0) END,
					max_storage_bytes = CASE WHEN $4 < 0 THEN max_storage_bytes ELSE nullif($4, 0) END,
					retention_days = CASE WHEN $5 < 0 THEN retention_days ELSE $5 END
				WHERE id = $1`, *tenant, *maxRuns, *maxHosts, *maxStorage, *retention)
			return err
		})
		if err != nil {
			return err
		}
		return out.Encode(map[string]bool{"ok": true})
	}
	usage()
	return nil
}
