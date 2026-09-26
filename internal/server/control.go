package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/hoststat"
)

// The control host is the machine this luxd runs on, and its Postgres. It is
// sampled with the whole system (sampleSystem) and served only to an
// operator reading the whole system (systemHistory).

// DefaultDiskPaths applies when history.disk_paths is unset.
var DefaultDiskPaths = []string{"/"}

// controlHost is one reading; what could not be read stays nil or absent.
type controlHost struct {
	cpuSeconds *float64
	cpus       *int
	memUsed    *int64
	memTotal   *int64
	dbBytes    *int64
	dbConns    *int
	disks      []controlDisk
}

type controlDisk struct {
	path string
	hoststat.Disk
}

// hostname identifies this luxd's control samples. Chosen over
// /etc/machine-id, which cloned images and containers share or lack.
func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "luxd"
}

// readControlHost reads CPU, memory, every tracked path's filesystem and
// the Postgres figures, all before the sampling transaction so a slow read
// never holds it. What cannot be read is left out, and logged when it
// starts and stops failing rather than on every tick.
func (s *Server) readControlHost(ctx context.Context) controlHost {
	var c controlHost
	if b, n, err := s.readPostgres(ctx); err != nil {
		if !s.pgFailing.Swap(true) {
			s.log.Warn("history: postgres size and connections skipped", "err", err)
		}
	} else {
		if s.pgFailing.Swap(false) {
			s.log.Info("history: postgres size and connections readable again")
		}
		c.dbBytes, c.dbConns = &b, &n
	}
	if cpu, err := hoststat.ReadCPU(); err == nil {
		c.cpuSeconds, c.cpus = &cpu.Seconds, &cpu.Cores
	}
	if m, err := hoststat.ReadMemory(); err == nil {
		c.memUsed, c.memTotal = &m.Used, &m.Total
	}
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	for _, p := range s.cfg.DiskPaths {
		d, err := hoststat.ReadDisk(p)
		if err != nil {
			if !s.diskFailing[p] {
				s.log.Warn("history: disk path skipped", "path", p, "err", err)
				s.diskFailing[p] = true
			}
			continue
		}
		if s.diskFailing[p] {
			s.log.Info("history: disk path readable again", "path", p)
			delete(s.diskFailing, p)
		}
		c.disks = append(c.disks, controlDisk{p, d})
	}
	return c
}

// postgresFigures reads through SQL so a Postgres on another machine works.
func (s *Server) postgresFigures(ctx context.Context) (size int64, conns int, err error) {
	err = s.db.Pool.QueryRow(ctx, `SELECT pg_database_size(current_database()),
		(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database())`).Scan(&size, &conns)
	return size, conns, err
}

// sampleControl writes one control sample in the system sample's
// transaction, so both carry the same `at`.
func sampleControl(ctx context.Context, tx pgx.Tx, instance string, c controlHost) error {
	if _, err := tx.Exec(ctx, `INSERT INTO control_samples (instance, res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections)
		VALUES ($1, 0, now(), $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`, instance, c.cpuSeconds, c.cpus, c.memUsed, c.memTotal, c.dbBytes, c.dbConns); err != nil {
		return fmt.Errorf("control sample: %w", err)
	}
	if len(c.disks) == 0 {
		return nil
	}
	paths := make([]string, len(c.disks))
	used, free, total := make([]int64, len(c.disks)), make([]int64, len(c.disks)), make([]int64, len(c.disks))
	for i, d := range c.disks {
		paths[i], used[i], free[i], total[i] = d.path, d.Used, d.Free, d.Total
	}
	if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
		SELECT $1, p, 0, now(), u, f, t FROM unnest($2::text[], $3::int8[], $4::int8[], $5::int8[]) AS d(p, u, f, t)
		ON CONFLICT DO NOTHING`, instance, paths, used, free, total); err != nil {
		return fmt.Errorf("control disk sample: %w", err)
	}
	return nil
}

type ControlSample struct {
	Instance            string       `json:"instance" doc:"The luxd instance's hostname that recorded it."`
	CPUCores            *float64     `json:"cpuCores,omitempty" doc:"CPU in use, in cores: a rate over the previous point."`
	CPUs                *int         `json:"cpus,omitempty" doc:"Cores on the machine."`
	MemoryBytes         *int64       `json:"memoryBytes,omitempty"`
	MemoryTotal         *int64       `json:"memoryTotal,omitempty"`
	DatabaseBytes       *int64       `json:"databaseBytes,omitempty" doc:"Size of lux's Postgres database."`
	DatabaseConnections *int         `json:"databaseConnections,omitempty" doc:"Backends connected to lux's Postgres database."`
	Disks               []DiskSample `json:"disks,omitempty" doc:"Each tracked directory's filesystem (history.disk_paths)."`
}

type DiskSample struct {
	Path       string `json:"path"`
	UsedBytes  int64  `json:"usedBytes"`
	FreeBytes  int64  `json:"freeBytes" doc:"What an unprivileged process can still write."`
	TotalBytes int64  `json:"totalBytes"`
}

// An operator's ?tenant= view carries TenantID too, so it is excluded here.
func controlVisible(p Principal) bool { return p.Operator && p.TenantID == "" }

// addControl attaches the control samples at the system samples' instants.
// Only the instance with the latest sample in the range is served: CPU
// counters of different machines have unrelated baselines.
func addControl(ctx context.Context, tx pgx.Tx, samples []Sample, res int, from, to time.Time) error {
	if len(samples) == 0 {
		return nil
	}
	byAt := make(map[int64]*Sample, len(samples))
	for i := range samples {
		byAt[samples[i].At.UnixNano()] = &samples[i]
	}
	rows, err := tx.Query(ctx, `WITH latest AS (
			SELECT instance FROM control_samples WHERE res = $1 AND at BETWEEN $2 AND $3 ORDER BY at DESC, instance LIMIT 1)
		SELECT c.instance, c.at, c.cpu_seconds, c.cpus, c.mem_bytes, c.mem_total, c.db_bytes, c.db_connections,
			coalesce((SELECT jsonb_agg(jsonb_build_object('path', d.path, 'usedBytes', d.used_bytes, 'freeBytes', d.free_bytes,
				'totalBytes', d.total_bytes) ORDER BY d.path)
				FROM control_disk_samples d WHERE d.instance = c.instance AND d.res = c.res AND d.at = c.at), '[]')
		FROM control_samples c JOIN latest USING (instance) WHERE c.res = $1 AND c.at BETWEEN $2 AND $3 ORDER BY c.at`, res, from, to)
	if err != nil {
		return err
	}
	var cpu rate
	var instance string
	var at time.Time
	var cpuS *float64
	var cpus, conns *int
	var mem, memT, db *int64
	var disks []DiskSample
	_, err = pgx.ForEachRow(rows, []any{&instance, &at, &cpuS, &cpus, &mem, &memT, &db, &conns, &disks}, func() error {
		cores := cpu.next(at, cpuS)
		if sm := byAt[at.UnixNano()]; sm != nil {
			sm.Control = &ControlSample{Instance: instance, CPUCores: cores, CPUs: cpus, MemoryBytes: mem, MemoryTotal: memT,
				DatabaseBytes: db, DatabaseConnections: conns, Disks: disks}
		}
		disks = nil
		return nil
	})
	return err
}
