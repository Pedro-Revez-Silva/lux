package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/runner"
)

// parse runs config over args with env as the environment, keeping the
// fields provisioning sets.
func parse(t *testing.T, env map[string]string, args ...string) (configView, error) {
	t.Helper()
	fs := flag.NewFlagSet("lux-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg, _, err := config(fs, args, func(k string) string { return env[k] })
	v := configView{CPUs: cfg.CPUs, Memory: cfg.Memory, Disk: cfg.Disk, HeadroomSet: cfg.DiskHeadroom != nil}
	if cfg.DiskHeadroom != nil {
		v.Headroom = *cfg.DiskHeadroom
	}
	return v, err
}

type configView struct {
	CPUs        float64
	Memory      int64
	Disk        int64
	HeadroomSet bool
	Headroom    int64
}

// A provisioned host is configured only through its environment
// (/etc/lux/runner.env): each variable reaches the Config, and a headroom
// of 0 is a headroom, which turns disk scaling on.
func TestConfigFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want configView
	}{
		{"none", nil, nil, configView{}},
		{"shaped host", map[string]string{"LUX_RUNNER_CPUS": "0.5", "LUX_RUNNER_MEMORY": "2147483648",
			"LUX_RUNNER_DISK": "21474836480", "LUX_RUNNER_DISK_HEADROOM": "10737418240"}, nil,
			configView{CPUs: 0.5, Memory: 2 << 30, Disk: 20 << 30, HeadroomSet: true, Headroom: 10 << 30}},
		{"zero headroom", map[string]string{"LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": "0"}, nil,
			configView{Disk: 100, HeadroomSet: true}},
		{"empty headroom is unset", map[string]string{"LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": ""}, nil,
			configView{Disk: 100}},
		{"flags win", map[string]string{"LUX_RUNNER_CPUS": "8", "LUX_RUNNER_DISK": "100", "LUX_RUNNER_DISK_HEADROOM": "7"},
			[]string{"--cpus", "16", "--disk", "200", "--disk-headroom", "0"},
			configView{CPUs: 16, Disk: 200, HeadroomSet: true}},
		{"zero headroom flag", nil, []string{"--disk", "100", "--disk-headroom", "0"}, configView{Disk: 100, HeadroomSet: true}},
	} {
		got, err := parse(t, c.env, c.args...)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v (err %v), want %+v", c.name, got, err, c.want)
		}
	}
}

// Sizes are plain bytes; an unusable variable or a negative size stops
// the runner rather than leaving a host unshaped.
func TestConfigRefuses(t *testing.T) {
	const negative = "--usage-every, --disk, --disk-headroom, --memory, --memory-headroom and --cpus must not be negative"
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"suffixed disk", map[string]string{"LUX_RUNNER_DISK": "20Gi"}, nil, `LUX_RUNNER_DISK="20Gi": want a number of bytes`},
		{"suffixed headroom", map[string]string{"LUX_RUNNER_DISK_HEADROOM": "10Gi"}, nil, `LUX_RUNNER_DISK_HEADROOM="10Gi": want a number of bytes`},
		{"cpus not a number", map[string]string{"LUX_RUNNER_CPUS": "eight"}, nil, `LUX_RUNNER_CPUS="eight": want a number of CPUs`},
		{"cpus NaN", map[string]string{"LUX_RUNNER_CPUS": "NaN"}, nil, `LUX_RUNNER_CPUS="NaN": want a number of CPUs`},
		{"suffixed headroom flag", nil, []string{"--disk-headroom", "10Gi"}, `invalid value "10Gi" for flag -disk-headroom: want a number of bytes`},
		{"negative headroom", map[string]string{"LUX_RUNNER_DISK_HEADROOM": "-1"}, nil, negative},
		{"negative cpus", nil, []string{"--cpus", "-1"}, negative},
	} {
		got, err := parse(t, c.env, c.args...)
		if err == nil {
			t.Errorf("%s: accepted, %+v", c.name, got)
		} else if err.Error() != c.want {
			t.Errorf("%s: %q, want %q", c.name, err, c.want)
		}
	}
}

// Every unusable variable is reported, each on its own prefixed line.
func TestReportErrPrefixesEveryLine(t *testing.T) {
	_, err := parse(t, map[string]string{"LUX_RUNNER_CPUS": "x", "LUX_RUNNER_DISK": "20Gi"})
	if err == nil {
		t.Fatal("accepted")
	}
	var out strings.Builder
	reportErr(&out, err)
	want := "lux-runner: LUX_RUNNER_CPUS=\"x\": want a number of CPUs\n" +
		"lux-runner: LUX_RUNNER_DISK=\"20Gi\": want a number of bytes\n"
	if out.String() != want {
		t.Errorf("%q, want %q", out.String(), want)
	}
}

// The low-free-space guard's watermarks come from their flags or
// variables, as a percentage or a size; unset, they are the defaults.
func TestConfigDiskFreeMarks(t *testing.T) {
	marks := func(env map[string]string, args ...string) (runner.FreeMark, runner.FreeMark, error) {
		fs := flag.NewFlagSet("lux-runner", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		cfg, _, err := config(fs, args, func(k string) string { return env[k] })
		return cfg.DiskLowFree, cfg.DiskRecoverFree, err
	}
	low, rec, err := marks(nil)
	if err != nil || !low.IsZero() || !rec.IsZero() {
		t.Fatalf("unset: %+v %+v %v, want zero (the defaults)", low, rec, err)
	}
	if runner.DefaultDiskLowFree != (runner.FreeMark{Percent: 5}) || runner.DefaultDiskRecoverFree != (runner.FreeMark{Percent: 10}) {
		t.Fatalf("defaults %+v %+v, want 5%% and 10%%", runner.DefaultDiskLowFree, runner.DefaultDiskRecoverFree)
	}
	low, rec, err = marks(map[string]string{"LUX_RUNNER_DISK_LOW_FREE": "8GiB", "LUX_RUNNER_DISK_RECOVER_FREE": "12%"})
	if err != nil || low != (runner.FreeMark{Bytes: 8 << 30}) || rec != (runner.FreeMark{Percent: 12}) {
		t.Fatalf("env: %+v %+v %v", low, rec, err)
	}
	low, rec, err = marks(map[string]string{"LUX_RUNNER_DISK_LOW_FREE": "8GiB"}, "--disk-low-free", "2%", "--disk-recover-free", "4%")
	if err != nil || low != (runner.FreeMark{Percent: 2}) || rec != (runner.FreeMark{Percent: 4}) {
		t.Fatalf("flags win: %+v %+v %v", low, rec, err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"low above recover", nil, []string{"--disk-low-free", "20%"}, "--disk-low-free 20% must be below --disk-recover-free 10%"},
		{"low equals recover", nil, []string{"--disk-low-free", "8GiB", "--disk-recover-free", "8GiB"}, "--disk-low-free 8.0 GiB must be below --disk-recover-free 8.0 GiB"},
		{"recover at 100%", nil, []string{"--disk-recover-free", "100%"}, `invalid value "100%" for flag -disk-recover-free: want a percentage above 0% and below 100%`},
		{"zero low", nil, []string{"--disk-low-free", "0"}, `invalid value "0" for flag -disk-low-free: want a percentage (5%) or a size above 0 (8GiB)`},
		{"bad env", map[string]string{"LUX_RUNNER_DISK_LOW_FREE": "lots"}, nil, `LUX_RUNNER_DISK_LOW_FREE="lots": want a percentage (5%) or a size above 0 (8GiB)`},
	} {
		if _, _, err := marks(c.env, c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// -version prints the version whatever the sizes given with it.
func TestConfigVersionIgnoresSizes(t *testing.T) {
	fs := flag.NewFlagSet("lux-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_, showVersion, err := config(fs, []string{"--cpus", "-1", "-version"}, func(string) string { return "" })
	if err != nil || !showVersion {
		t.Errorf("showVersion %v, err %v; want true, nil", showVersion, err)
	}
}
