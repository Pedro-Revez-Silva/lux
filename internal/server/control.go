package server

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/hoststat"
	"github.com/marcioapm/lux/internal/proto"
)

// The control host is the machine this luxd runs on, and its Postgres, and
// luxd's own process. It is sampled with the whole system (sampleSystem)
// and served only to an operator reading the whole system (systemHistory).

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
	luxd       *proto.ProcessUsage
}

type controlDisk struct {
	path string
	hoststat.Disk
}

// hostname is the machine this luxd runs on, shown with its control
// samples (which the process's own id, Server.id, keys).
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
	if p, err := s.proc.Read(); err == nil {
		c.luxd = p
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
// transaction, so both carry the same `at`. stored is whether its row went
// in (not a conflict).
func (s *Server) sampleControl(ctx context.Context, tx pgx.Tx, c controlHost) (stored bool, err error) {
	instance := s.id
	tag, err := tx.Exec(ctx, insertControlSample,
		append([]any{instance, s.hostname, c.cpuSeconds, c.cpus, c.memUsed, c.memTotal, c.dbBytes, c.dbConns}, procValues(c.luxd)...)...)
	if err != nil {
		return false, fmt.Errorf("control sample: %w", err)
	}
	if len(c.disks) == 0 {
		return tag.RowsAffected() == 1, nil
	}
	paths := make([]string, len(c.disks))
	used, free, total := make([]int64, len(c.disks)), make([]int64, len(c.disks)), make([]int64, len(c.disks))
	for i, d := range c.disks {
		paths[i], used[i], free[i], total[i] = d.path, d.Used, d.Free, d.Total
	}
	if _, err := tx.Exec(ctx, `INSERT INTO control_disk_samples (instance, path, res, at, used_bytes, free_bytes, total_bytes)
		SELECT $1, p, 0, now(), u, f, t FROM unnest($2::text[], $3::int8[], $4::int8[], $5::int8[]) AS d(p, u, f, t)
		ON CONFLICT DO NOTHING`, instance, paths, used, free, total); err != nil {
		return false, fmt.Errorf("control disk sample: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Control is the control host's history, by what each figure is of: the
// machines luxd ran on, lux's Postgres, and each luxd process. A luxd
// restart starts a new luxd series; a machine's and Postgres's go on.
type Control struct {
	Machines []MachineSeries `json:"machines" doc:"A series per machine luxd ran on (by hostname), oldest first."`
	Postgres []PostgresPoint `json:"postgres" doc:"lux's Postgres database: one series, whichever luxd sampled it."`
	Luxd     []LuxdSeries    `json:"luxd" doc:"A series per luxd process (a restart is a new one), oldest first."`
}

type MachineSeries struct {
	Hostname string         `json:"hostname"`
	Samples  []MachinePoint `json:"samples"`
}

type MachinePoint struct {
	At          time.Time    `json:"at"`
	CPUCores    *float64     `json:"cpuCores,omitempty" doc:"CPU in use, in cores: a rate over the machine's previous point."`
	CPUs        *int         `json:"cpus,omitempty" doc:"Cores on the machine."`
	MemoryBytes *int64       `json:"memoryBytes,omitempty"`
	MemoryTotal *int64       `json:"memoryTotal,omitempty"`
	Disks       []DiskSample `json:"disks,omitempty" doc:"Each tracked directory's filesystem (history.disk_paths)."`
}

type DiskSample struct {
	Path       string `json:"path"`
	UsedBytes  int64  `json:"usedBytes"`
	FreeBytes  int64  `json:"freeBytes" doc:"What an unprivileged process can still write."`
	TotalBytes int64  `json:"totalBytes"`
}

type PostgresPoint struct {
	At          time.Time `json:"at"`
	Bytes       *int64    `json:"bytes,omitempty" doc:"Size of lux's database."`
	Connections *int      `json:"connections,omitempty" doc:"Backends connected to it."`
}

type LuxdSeries struct {
	Instance string       `json:"instance" doc:"The luxd process's id, new at each start (rows from before process ids: its hostname)."`
	Hostname string       `json:"hostname" doc:"The machine it runs on."`
	Samples  []LuxdSample `json:"samples"`
}

type LuxdSample struct {
	At time.Time `json:"at"`
	ProcessSample
}

// An operator's ?tenant= view carries TenantID too, so it is excluded here.
func controlVisible(p Principal) bool { return p.Operator && p.TenantID == "" }

// readControl reads the control samples in the range. Rows come by
// machine, then time: a machine's CPU rate spans its luxd restarts, a luxd
// process's only its own samples.
func readControl(ctx context.Context, tx pgx.Tx, res int, from, to time.Time) (*Control, error) {
	rows, err := tx.Query(ctx, `SELECT c.instance, c.hostname, c.at, c.cpu_seconds, c.cpus, c.mem_bytes, c.mem_total,
			c.db_bytes, c.db_connections,
			coalesce((SELECT jsonb_agg(jsonb_build_object('path', d.path, 'usedBytes', d.used_bytes, 'freeBytes', d.free_bytes,
				'totalBytes', d.total_bytes) ORDER BY d.path)
				FROM control_disk_samples d WHERE d.instance = c.instance AND d.res = c.res AND d.at = c.at), '[]'),
			`+procCols+`
		FROM control_samples c WHERE c.res = $1 AND c.at BETWEEN $2 AND $3
		ORDER BY c.hostname, c.at, c.instance`, res, from, to)
	if err != nil {
		return nil, err
	}
	c := &Control{Machines: []MachineSeries{}, Postgres: []PostgresPoint{}, Luxd: []LuxdSeries{}}
	luxd := map[string]int{} // instance → index in c.Luxd
	luxdCPU := map[string]*procRate{}
	var cpu rate
	var instance, host string
	var m MachinePoint
	var pg PostgresPoint
	var cpuS *float64
	var proc procRow
	_, err = pgx.ForEachRow(rows, append([]any{&instance, &host, &m.At, &cpuS, &m.CPUs, &m.MemoryBytes, &m.MemoryTotal,
		&pg.Bytes, &pg.Connections, &m.Disks}, proc.dest()...), func() error {
		// Two luxd on one machine sample it at their own instants; the
		// machine's series takes both.
		if len(c.Machines) == 0 || c.Machines[len(c.Machines)-1].Hostname != host {
			c.Machines = append(c.Machines, MachineSeries{Hostname: host})
			cpu = rate{}
		}
		m.CPUCores = cpu.next(m.At, cpuS)
		ms := &c.Machines[len(c.Machines)-1]
		ms.Samples = append(ms.Samples, m)
		m.Disks = nil
		pg.At = m.At
		c.Postgres = append(c.Postgres, pg)
		i, ok := luxd[instance]
		if !ok {
			i, luxd[instance], luxdCPU[instance] = len(c.Luxd), len(c.Luxd), &procRate{}
			c.Luxd = append(c.Luxd, LuxdSeries{Instance: instance, Hostname: host})
		}
		if p := proc.sample(luxdCPU[instance], m.At); p != nil {
			c.Luxd[i].Samples = append(c.Luxd[i].Samples, LuxdSample{m.At, *p})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(c.Postgres, func(a, b PostgresPoint) int { return a.At.Compare(b.At) })
	first := func(s []MachinePoint) time.Time { return s[0].At }
	slices.SortStableFunc(c.Machines, func(a, b MachineSeries) int { return first(a.Samples).Compare(first(b.Samples)) })
	c.Luxd = slices.DeleteFunc(c.Luxd, func(l LuxdSeries) bool { return len(l.Samples) == 0 })
	slices.SortStableFunc(c.Luxd, func(a, b LuxdSeries) int { return a.Samples[0].At.Compare(b.Samples[0].At) })
	return c, nil
}
