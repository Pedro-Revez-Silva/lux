package server

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/hoststat"
)

// The control host is the machine this luxd runs on, and its Postgres: what
// operators watch to keep lux itself up. It is sampled with the whole
// system (sampleSystem), into control_samples and control_disk_samples, and
// served only to an operator looking at the whole system (systemHistory).

// DefaultDiskPaths are the directories whose filesystems are tracked when
// history.disk_paths is unset.
var DefaultDiskPaths = []string{"/"}

// controlHost is one reading of this machine. Fields it could not read
// stay nil; disks it could not read are left out.
type controlHost struct {
	cpuSeconds *float64
	cpus       *int
	memUsed    *int64
	memTotal   *int64
	disks      []controlDisk
}

type controlDisk struct {
	path string
	hoststat.Disk
}

// readControlHost reads CPU, memory and every tracked path's filesystem. A
// path that cannot be read is skipped, and logged when it starts and stops
// failing rather than on every tick.
func (s *Server) readControlHost() controlHost {
	var c controlHost
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

// sampleControl writes one control sample in the system sample's
// transaction, so both carry the same `at`. Database size and connections
// come from Postgres itself, which may be on another machine.
func sampleControl(ctx context.Context, tx pgx.Tx, c controlHost) error {
	if _, err := tx.Exec(ctx, `INSERT INTO control_samples (res, at, cpu_seconds, cpus, mem_bytes, mem_total, db_bytes, db_connections)
		SELECT 0, now(), $1, $2, $3, $4, pg_database_size(current_database()),
			(SELECT count(*) FROM pg_stat_activity WHERE datname = current_database())
		ON CONFLICT DO NOTHING`, c.cpuSeconds, c.cpus, c.memUsed, c.memTotal); err != nil {
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
	if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (path, res, at, used_bytes, free_bytes, total_bytes)
		SELECT p, 0, now(), u, f, t FROM unnest($1::text[], $2::int8[], $3::int8[], $4::int8[]) AS d(p, u, f, t)
		ON CONFLICT DO NOTHING`, paths, used, free, total); err != nil {
		return fmt.Errorf("control disk sample: %w", err)
	}
	return nil
}
