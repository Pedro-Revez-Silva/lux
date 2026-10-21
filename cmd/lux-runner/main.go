// Command lux-runner runs on every host: it connects to luxd and runs the
// placements it is given in Podman containers.
//
//	LUX_URL=https://luxd.example LUX_HOST_TOKEN=luxh_… lux-runner --name host-a
//
// See docs/operations.md for host requirements.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
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

// envBytes is a flag's default from the environment: name's value in
// getenv as a byte count, or def when unset. set says whether it was set.
func envBytes(getenv func(string) string, name string, def int64) (n int64, set bool, err error) {
	v := getenv(name)
	if v == "" {
		return def, false, nil
	}
	n, err = strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q: want a number of bytes", name, v)
	}
	return n, true, nil
}

// envCPUs is --cpus's default: LUX_RUNNER_CPUS, or 0 (all) when unset.
func envCPUs(getenv func(string) string) (float64, error) {
	v := getenv("LUX_RUNNER_CPUS")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("LUX_RUNNER_CPUS=%q: want a number of CPUs", v)
	}
	return n, nil
}

// config parses args into fs and returns the runner's Config, each flag
// defaulting from its variable in getenv; a flag on the command line wins.
// An unusable variable or a negative size is an error.
func config(fs *flag.FlagSet, args []string, getenv func(string) string) (cfg runner.Config, showVersion bool, err error) {
	cfg = runner.Config{Labels: labels{}}
	var envErr error
	bytesOf := func(name string, def int64) int64 {
		n, _, err := envBytes(getenv, name, def)
		envErr = errors.Join(envErr, err)
		return n
	}
	// Set from the environment or the flag, 0 included: it turns scaling on.
	headroomUsage := "bytes of the data directory's filesystem kept from Runs; given with --disk (0 allowed), each Run's disk limit is resources.disk × min(1, (filesystem capacity − this) / --disk). Also LUX_RUNNER_DISK_HEADROOM"
	if n, set, err := envBytes(getenv, "LUX_RUNNER_DISK_HEADROOM", 0); err != nil {
		envErr = err
	} else if set {
		cfg.DiskHeadroom = &n
		// flag.Func has no default for -h to print, so name the env's here.
		headroomUsage += fmt.Sprintf(" (default %d)", n)
	}
	cpus, err := envCPUs(getenv)
	envErr = errors.Join(envErr, err)
	// The low-free-space guard's watermarks: a percentage of the data
	// directory's filesystem, or a size.
	markOf := func(name string, into *runner.FreeMark) {
		if v := getenv(name); v != "" {
			m, err := runner.ParseFreeMark(v)
			if err != nil {
				err = fmt.Errorf("%s=%q: %w", name, v, err)
			}
			envErr = errors.Join(envErr, err)
			*into = m
		}
	}
	markOf("LUX_RUNNER_DISK_LOW_FREE", &cfg.DiskLowFree)
	markOf("LUX_RUNNER_DISK_RECOVER_FREE", &cfg.DiskRecoverFree)
	// flag names the flag and the value itself.
	markFlag := func(into *runner.FreeMark) func(string) error {
		return func(s string) error {
			m, err := runner.ParseFreeMark(s)
			if err != nil {
				return err
			}
			*into = m
			return nil
		}
	}
	nestedDefault, err := hostboot.NestedFromEnv(getenv("LUX_NESTED"))
	envErr = errors.Join(envErr, err)
	fs.StringVar(&cfg.Name, "name", "", "host name (default: hostname)")
	fs.StringVar(&cfg.DataDir, "data-dir", "/var/lib/lux", "where the runner keeps its state and local snapshots")
	fs.StringVar(&cfg.Shim, "shim", hostboot.InstallDir+"/lux-shim", "path to the lux-shim binary")
	fs.Var(labels(cfg.Labels), "label", "host label key=value (repeatable)")
	fs.IntVar(&cfg.MaxRuns, "max-runs", 0, "maximum concurrent Runs (default 16)")
	fs.Float64Var(&cfg.CPUs, "cpus", cpus, "CPUs to offer (default: all). Also LUX_RUNNER_CPUS")
	fs.Int64Var(&cfg.Memory, "memory", bytesOf("LUX_RUNNER_MEMORY", 0), "bytes of memory to offer: the machine's gross memory, in which Runs ask; each gets its share of MemTotal less --memory-headroom (default: MemTotal). Also LUX_RUNNER_MEMORY")
	fs.Int64Var(&cfg.MemoryHeadroom, "memory-headroom", bytesOf("LUX_RUNNER_MEMORY_HEADROOM", runner.DefaultMemoryHeadroom), "bytes of MemTotal kept from Runs for the kernel and the host. Also LUX_RUNNER_MEMORY_HEADROOM")
	fs.DurationVar(&cfg.UsageEvery, "usage-every", 0, "how often disk use is sampled and the disk limit checked (default 15s)")
	fs.Int64Var(&cfg.Disk, "disk", bytesOf("LUX_RUNNER_DISK", 0), "bytes of disk to reserve for Runs, from their resources.disk (default: not reserved). With --disk-headroom, the host's nominal disk. Also LUX_RUNNER_DISK")
	fs.Func("disk-headroom", headroomUsage, func(s string) error {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return errors.New("want a number of bytes")
		}
		cfg.DiskHeadroom = &n
		return nil
	})
	fs.Func("disk-low-free", fmt.Sprintf("with disk scaling (--disk-headroom): free space of the data directory's filesystem under which the host is under disk pressure: it takes no new Run, removes unused images, and stops Runs over their disk limit. A percentage of the filesystem's capacity (5%%) or a size (8GiB) (default %s). Also LUX_RUNNER_DISK_LOW_FREE", runner.DefaultDiskLowFree), markFlag(&cfg.DiskLowFree))
	fs.Func("disk-recover-free", fmt.Sprintf("with disk scaling: free space at which disk pressure ends; above --disk-low-free (default %s). Also LUX_RUNNER_DISK_RECOVER_FREE", runner.DefaultDiskRecoverFree), markFlag(&cfg.DiskRecoverFree))
	fs.StringVar(&cfg.ProviderID, "provider-id", getenv("LUX_PROVIDER_ID"), "cloud instance id, for provisioned hosts")
	fs.BoolVar(&cfg.ForcePoll, "poll", false, "use HTTP polling instead of a WebSocket")
	fs.StringVar(&cfg.EC2IMDS, "ec2-imds", getenv("LUX_EC2_IMDS"), "EC2 instance metadata URL to watch for spot interruptions (http://169.254.169.254 on EC2; empty: off)")
	fs.BoolVar(&cfg.Cpuset, "cpuset", false, "pin each Run to ceil(resources.cpus) of this host's CPUs (--cpuset-cpus): disjoint while CPUs are free, the least used after; fractions packed")
	fs.BoolVar(&cfg.Nested, "nested", nestedDefault, "offer nested containers (needs /dev/fuse and /dev/net/tun; default from LUX_NESTED=true)")
	fs.DurationVar(&cfg.HostTTL, "host-ttl", 24*time.Hour, "how long to keep local snapshot copies after upload, and images lux pulled or built after their last use")
	fs.Float64Var(&cfg.ImageDiskHigh, "image-disk-high", 80, "percent use of the disk holding Podman's storage over which lux's unused images are removed, least recently used first (0: off)")
	fs.BoolVar(&showVersion, "version", false, "print the version")
	if envErr != nil {
		return cfg, false, envErr
	}
	if err := fs.Parse(args); err != nil {
		return cfg, false, err
	}
	if showVersion {
		return cfg, true, nil
	}
	if cfg.UsageEvery < 0 || cfg.Disk < 0 || cfg.Memory < 0 || cfg.MemoryHeadroom < 0 || cfg.CPUs < 0 ||
		cfg.DiskHeadroom != nil && *cfg.DiskHeadroom < 0 {
		return cfg, false, errors.New("--usage-every, --disk, --disk-headroom, --memory, --memory-headroom and --cpus must not be negative")
	}
	low := cmp.Or(cfg.DiskLowFree, runner.DefaultDiskLowFree)
	rec := cmp.Or(cfg.DiskRecoverFree, runner.DefaultDiskRecoverFree)
	if err := runner.CheckFreeMarks(low, rec); err != nil {
		return cfg, false, err
	}
	return cfg, false, nil
}

// reportErr writes err to w with every line prefixed, as config joins one
// error per unusable variable.
func reportErr(w io.Writer, err error) {
	for _, l := range strings.Split(err.Error(), "\n") {
		fmt.Fprintln(w, "lux-runner:", l)
	}
}

func main() {
	cfg, showVersion, err := config(flag.CommandLine, os.Args[1:], os.Getenv)
	if err != nil {
		reportErr(os.Stderr, err)
		os.Exit(2)
	}
	if showVersion {
		fmt.Println(version.Version)
		return
	}
	if cfg.DiskHeadroom != nil && cfg.Disk == 0 {
		fmt.Fprintln(os.Stderr, "lux-runner: --disk-headroom without --disk: the disk is not scaled")
	}
	if (!cfg.DiskLowFree.IsZero() || !cfg.DiskRecoverFree.IsZero()) && (cfg.DiskHeadroom == nil || cfg.Disk == 0) {
		fmt.Fprintln(os.Stderr, "lux-runner: --disk-low-free and --disk-recover-free need disk scaling (--disk with --disk-headroom): no low-free-space guard")
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
	// Test-only (tests/suites): not a flag, so it stays out of --help.
	cfg.AssignHold = os.Getenv("LUX_TEST_ASSIGN_HOLD")
	if v := os.Getenv("LUX_TEST_WITHOUT_CAPABILITIES"); v != "" {
		cfg.WithoutCapabilities = strings.Split(v, ",")
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
