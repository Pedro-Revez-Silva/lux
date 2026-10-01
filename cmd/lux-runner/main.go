// Command lux-runner runs on every host: it connects to luxd and runs the
// placements it is given in Podman containers.
//
//	LUX_URL=https://luxd.example LUX_HOST_TOKEN=luxh_… lux-runner --name host-a
//
// See docs/operations.md for host requirements.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/hostboot"
	"github.com/marcioapm/lux/internal/runner"
	"github.com/marcioapm/lux/internal/version"
)

type labels map[string]string

func (l labels) String() string { return "" }
func (l labels) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want key=value")
	}
	l[k] = val
	return nil
}

// envBytes is a flag's default from the environment: name's value as a
// byte count, or def when unset. A value that is not one stops the runner.
func envBytes(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lux-runner: %s=%q: want a number of bytes\n", name, v)
		os.Exit(2)
	}
	return n
}

func main() {
	cfg := runner.Config{Labels: labels{}}
	flag.StringVar(&cfg.Name, "name", "", "host name (default: hostname)")
	flag.StringVar(&cfg.DataDir, "data-dir", "/var/lib/lux", "where the runner keeps its state and local snapshots")
	flag.StringVar(&cfg.Shim, "shim", hostboot.InstallDir+"/lux-shim", "path to the lux-shim binary")
	flag.Var(labels(cfg.Labels), "label", "host label key=value (repeatable)")
	flag.IntVar(&cfg.MaxRuns, "max-runs", 0, "maximum concurrent Runs (default 16)")
	flag.Float64Var(&cfg.CPUs, "cpus", 0, "CPUs to offer (default: all)")
	flag.Int64Var(&cfg.Memory, "memory", envBytes("LUX_RUNNER_MEMORY", 0), "bytes of memory to offer: the machine's gross memory, in which Runs ask; each gets its share of MemTotal less --memory-headroom (default: MemTotal). Also LUX_RUNNER_MEMORY")
	flag.Int64Var(&cfg.MemoryHeadroom, "memory-headroom", envBytes("LUX_RUNNER_MEMORY_HEADROOM", runner.DefaultMemoryHeadroom), "bytes of MemTotal kept from Runs for the kernel and the host. Also LUX_RUNNER_MEMORY_HEADROOM")
	flag.DurationVar(&cfg.UsageEvery, "usage-every", 0, "how often disk use is sampled and the disk limit checked (default 15s)")
	flag.Int64Var(&cfg.Disk, "disk", 0, "bytes of disk to reserve for Runs, from their resources.disk (default: not reserved)")
	flag.StringVar(&cfg.ProviderID, "provider-id", os.Getenv("LUX_PROVIDER_ID"), "cloud instance id, for provisioned hosts")
	flag.BoolVar(&cfg.ForcePoll, "poll", false, "use HTTP polling instead of a WebSocket")
	flag.StringVar(&cfg.EC2IMDS, "ec2-imds", os.Getenv("LUX_EC2_IMDS"), "EC2 instance metadata URL to watch for spot interruptions (http://169.254.169.254 on EC2; empty: off)")
	flag.BoolVar(&cfg.Nested, "nested", false, "offer nested containers (needs /dev/fuse and /dev/net/tun)")
	flag.DurationVar(&cfg.HostTTL, "host-ttl", 24*time.Hour, "how long to keep local snapshot copies after upload, and images lux pulled or built after their last use")
	flag.Float64Var(&cfg.ImageDiskHigh, "image-disk-high", 80, "percent use of the disk holding Podman's storage over which lux's unused images are removed, least recently used first (0: off)")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	if cfg.UsageEvery < 0 || cfg.Disk < 0 || cfg.Memory < 0 || cfg.MemoryHeadroom < 0 || cfg.CPUs < 0 {
		fmt.Fprintln(os.Stderr, "lux-runner: --usage-every, --disk, --memory, --memory-headroom and --cpus must not be negative")
		os.Exit(2)
	}
	// A provisioned host gets its name from luxd (user data).
	if cfg.Name == "" {
		cfg.Name = os.Getenv("LUX_HOST_NAME")
	}
	cfg.URL = os.Getenv("LUX_URL")
	cfg.Token = os.Getenv("LUX_HOST_TOKEN")
	if cfg.URL == "" || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "lux-runner: LUX_URL and LUX_HOST_TOKEN are required")
		os.Exit(2)
	}
	for _, kv := range strings.Split(os.Getenv("LUX_LABELS"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			cfg.Labels[k] = v
		}
	}
	level := slog.LevelInfo
	if os.Getenv("LUX_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := runner.New(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-runner:", err)
		os.Exit(1)
	}
	log.Info("lux-runner starting", "name", cfg.Name, "version", version.Version, "url", cfg.URL)
	if err := r.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "lux-runner:", err)
		os.Exit(1)
	}
}
