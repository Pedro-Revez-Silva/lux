package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marcioapm/lux/internal/server"
)

func (a *app) historyCmd() *cobra.Command {
	var since, res string
	var host bool
	cmd := &cobra.Command{
		Use:   "history [run | --host host]",
		Short: "Resource use and system state over time",
		Long: `Without arguments, the system's history: Runs, queue, time to start,
hosts and capacity (an operator's whole system; --tenant narrows it), and
for an operator's whole system the control host: each machine luxd runs
on, its Postgres, and each luxd process. With a Run, its resource use across
placements; with --host, a host's, and its runner process's.

--since is how far back (1h, 24h, 7d; default 1h). The resolution is the
finest kept for that range (raw, 60 or 3600 seconds), or --res.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"since": {since}}
			if res != "" {
				q.Set("res", res)
			}
			path := "/v1/history"
			switch {
			case host && len(args) == 1:
				path = "/v1/hosts/" + args[0] + "/history"
			case host:
				return fmt.Errorf("--host needs a host")
			case len(args) == 1:
				path = "/v1/runs/" + args[0] + "/history"
			}
			var h server.History
			if err := a.c.Do(ctxOf(cmd), "GET", path+"?"+q.Encode(), nil, &h); err != nil {
				return err
			}
			if a.output == "json" {
				return a.json(h)
			}
			if len(h.Samples) == 0 {
				fmt.Fprintln(a.stdout, "no samples in this range")
				return nil
			}
			if len(args) == 0 {
				a.systemHistory(h)
			} else {
				a.usageHistory(h, !host)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "1h", "how far back (e.g. 30m, 24h, 7d)")
	cmd.Flags().StringVar(&res, "res", "", "resolution in seconds: 0 (raw), 60, 3600")
	cmd.Flags().BoolVar(&host, "host", false, "the argument is a host (id or name)")
	return cmd
}

// The text form: a line per series with its sparkline, then the latest
// value and the range.

// series is one line of history: how to read it from a sample (a
// server.Sample, or a control host point) and print it.
type series[S any] struct {
	name   string
	get    func(S) *float64
	format func(float64) string
}

func (a *app) usageHistory(h server.History, run bool) {
	all := []series[server.Sample]{
		{"cpu", func(s server.Sample) *float64 { return s.CPUCores }, cores},
		{"memory", func(s server.Sample) *float64 { return server.Float(s.MemoryBytes) }, bytesF},
		{"disk", func(s server.Sample) *float64 { return server.Float(s.DiskBytes) }, bytesF},
	}
	if run {
		all = append(all, []series[server.Sample]{
			{"pids", func(s server.Sample) *float64 { return server.Float(s.Pids) }, plain},
			{"net rx", func(s server.Sample) *float64 { return s.NetRxRate }, rateF},
			{"net tx", func(s server.Sample) *float64 { return s.NetTxRate }, rateF},
		}...)
	} else {
		all = append(all, []series[server.Sample]{
			{"placements", func(s server.Sample) *float64 { return server.Float(s.Placements) }, plain},
			{"alloc cpu", func(s server.Sample) *float64 { return s.AllocCPUs }, cores},
		}...)
		all = append(all, processSeries("runner", func(s server.Sample) *server.ProcessSample { return s.Runner })...)
	}
	a.header(h)
	for _, s := range all {
		spark(a, s, h.Samples)
	}
}

// processSeries are one of lux's processes' lines: a runner, or luxd.
func processSeries[S any](name string, get func(S) *server.ProcessSample) []series[S] {
	of := func(f func(*server.ProcessSample) *float64) func(S) *float64 {
		return func(s S) *float64 {
			if p := get(s); p != nil {
				return f(p)
			}
			return nil
		}
	}
	return []series[S]{
		{name + " cpu", of(func(p *server.ProcessSample) *float64 { return p.CPUCores }), cores},
		{name + " rss", of(func(p *server.ProcessSample) *float64 { return server.Float(p.RSSBytes) }), bytesF},
		{name + " peak", of(func(p *server.ProcessSample) *float64 { return server.Float(p.PeakRSSBytes) }), bytesF},
		{name + " heap", of(func(p *server.ProcessSample) *float64 { return server.Float(p.HeapBytes) }), bytesF},
		{name + " goroutines", of(func(p *server.ProcessSample) *float64 { return server.Float(p.Goroutines) }), plain},
	}
}

func (a *app) systemHistory(h server.History) {
	a.header(h)
	all := []series[server.Sample]{
		{"running", func(s server.Sample) *float64 { return server.Float(ptr(s.Runs["running"])) }, plain},
		{"busy", func(s server.Sample) *float64 { return server.Float(s.Busy) }, plain},
		{"queued", func(s server.Sample) *float64 { return server.Float(s.Queued) }, plain},
		{"started", func(s server.Sample) *float64 { return server.Float(s.Started) }, plain},
		{"finished", func(s server.Sample) *float64 { return server.Float(s.Finished) }, plain},
		{"start p95", func(s server.Sample) *float64 { return s.StartP95 }, secs},
		{"hosts ready", func(s server.Sample) *float64 { return server.Float(ptr(s.Hosts["ready"])) }, plain},
		{"alloc cpu", func(s server.Sample) *float64 { return s.SysAllocC }, cores},
		{"capacity cpu", func(s server.Sample) *float64 { return s.CapCPUs }, cores},
	}
	for _, s := range all {
		spark(a, s, h.Samples)
	}
	// An operator reading the whole system: the control host.
	if c := h.Control; c != nil {
		for _, m := range c.Machines {
			fmt.Fprintf(a.stdout, "machine %s\n", m.Hostname)
			for _, s := range []series[server.MachinePoint]{
				{"cpu", func(p server.MachinePoint) *float64 { return p.CPUCores }, cores},
				{"memory", func(p server.MachinePoint) *float64 { return server.Float(p.MemoryBytes) }, bytesF},
			} {
				spark(a, s, m.Samples)
			}
		}
		if len(c.Postgres) > 0 {
			fmt.Fprintln(a.stdout, "postgres")
			for _, s := range []series[server.PostgresPoint]{
				{"size", func(p server.PostgresPoint) *float64 { return server.Float(p.Bytes) }, bytesF},
				{"connections", func(p server.PostgresPoint) *float64 { return server.Float(p.Connections) }, plain},
			} {
				spark(a, s, c.Postgres)
			}
		}
		for _, l := range c.Luxd {
			fmt.Fprintf(a.stdout, "luxd %s on %s\n", l.Instance, l.Hostname)
			for _, s := range processSeries("luxd", func(s server.LuxdSample) *server.ProcessSample { return &s.ProcessSample }) {
				spark(a, s, l.Samples)
			}
		}
	}
}

func (a *app) header(h server.History) {
	res := "raw"
	if h.Resolution > 0 {
		res = (time.Duration(h.Resolution) * time.Second).String()
	}
	fmt.Fprintf(a.stdout, "%s → %s, %d samples (%s)\n", h.From.Local().Format("Jan 2 15:04"), h.To.Local().Format("Jan 2 15:04"), len(h.Samples), res)
}

var sparks = []rune("▁▂▃▄▅▆▇█")

// spark prints one series: name, sparkline (at most 60 wide), last, min–max.
func spark[S any](a *app, sr series[S], samples []S) {
	var vs []float64
	for _, s := range samples {
		if v := sr.get(s); v != nil {
			vs = append(vs, *v)
		}
	}
	if len(vs) == 0 {
		return
	}
	// Downsample to 60 columns by taking each column's maximum.
	cols := min(len(vs), 60)
	col := make([]float64, cols)
	for i := range col {
		lo, hi := i*len(vs)/cols, (i+1)*len(vs)/cols
		col[i] = vs[lo]
		for _, v := range vs[lo:hi] {
			col[i] = max(col[i], v)
		}
	}
	lo, hi := vs[0], vs[0]
	for _, v := range vs {
		lo, hi = min(lo, v), max(hi, v)
	}
	var b strings.Builder
	for _, v := range col {
		i := 0
		if hi > lo {
			i = int((v - lo) / (hi - lo) * float64(len(sparks)-1))
		}
		b.WriteRune(sparks[i])
	}
	fmt.Fprintf(a.stdout, "%-17s %-60s  %s  (%s–%s)\n", sr.name, b.String(), sr.format(vs[len(vs)-1]), sr.format(lo), sr.format(hi))
}

func ptr(v int) *int { return &v }

// cores is CPU as the console shows it: cores from one, else millicores,
// with a decimal under 10m (lux's own processes idle at a few).
func cores(v float64) string {
	trim := func(s string) string { return strings.TrimSuffix(strings.TrimRight(s, "0"), ".") }
	switch m := v * 1000; {
	case v == 1:
		return "1 core"
	case v > 1:
		return trim(fmt.Sprintf("%.2f", v)) + " cores"
	case v == 0:
		return "0"
	case m < 10:
		return trim(fmt.Sprintf("%.1f", m)) + "m"
	default:
		return fmt.Sprintf("%.0fm", m)
	}
}

func bytesF(v float64) string { return bytesHuman(int64(v)) }
func rateF(v float64) string  { return bytesHuman(int64(v)) + "/s" }
func plain(v float64) string  { return fmt.Sprintf("%g", v) }
func secs(v float64) string   { return fmt.Sprintf("%.1fs", v) }
