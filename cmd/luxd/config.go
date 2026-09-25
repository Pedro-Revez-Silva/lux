package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/marcioapm/lux/internal/server"
	"github.com/marcioapm/lux/internal/spec"
)

// config is luxd's configuration: a TOML file, then environment variables,
// which override it. Every setting has both: the TOML key (a table and a
// key: [s3] bucket) and the variable in its env tag. docs/operations.md
// lists them.
type config struct {
	Database struct {
		URL         string `toml:"url" env:"LUX_DATABASE_URL"`
		AppPassword string `toml:"app_password" env:"LUX_APP_PASSWORD"`
	} `toml:"database"`
	Listen    string `toml:"listen" env:"LUX_LISTEN"`
	PublicURL string `toml:"public_url" env:"LUX_PUBLIC_URL"`
	// RunnerURL is what runners dial (LUX_URL in a provisioned host's
	// env); it may be a private address unreachable from clients. Empty:
	// PublicURL.
	RunnerURL string `toml:"runner_url" env:"LUX_RUNNER_URL"`
	// RunnerBinDir holds runner binaries luxd serves for self-update:
	// <dir>/linux-{arm64,amd64}/{lux-runner,lux-shim}.
	RunnerBinDir string `toml:"runner_bin_dir" env:"LUX_RUNNER_BIN_DIR"`
	Debug        onFlag `toml:"debug" env:"LUX_DEBUG"`
	S3           struct {
		Bucket         string `toml:"bucket" env:"LUX_S3_BUCKET"`
		Endpoint       string `toml:"endpoint" env:"LUX_S3_ENDPOINT"`
		PublicEndpoint string `toml:"public_endpoint" env:"LUX_S3_PUBLIC_ENDPOINT"`
		Region         string `toml:"region" env:"LUX_S3_REGION"`
		AccessKey      string `toml:"access_key" env:"LUX_S3_ACCESS_KEY"`
		SecretKey      string `toml:"secret_key" env:"LUX_S3_SECRET_KEY"`
	} `toml:"s3"`
	Lease          duration `toml:"lease" env:"LUX_LEASE"`
	Tick           duration `toml:"tick" env:"LUX_TICK"`
	ScaleDownAfter duration `toml:"scale_down_after" env:"LUX_SCALE_DOWN_AFTER"`
	LaunchTimeout  duration `toml:"launch_timeout" env:"LUX_LAUNCH_TIMEOUT"`
	// How often each pool's instances are listed with the provider, and how
	// long a lost provisioned host keeps its instance.
	ProviderCheckEvery duration `toml:"provider_check_every" env:"LUX_PROVIDER_CHECK_EVERY"`
	LostGrace          duration `toml:"lost_grace" env:"LUX_LOST_GRACE"`
	// OutdatedDrainPercent caps concurrent outdated-binaries drains per
	// pool, as a percentage of its live hosts (at least 1). Default 10.
	OutdatedDrainPercent int `toml:"outdated_drain_percent" env:"LUX_OUTDATED_DRAIN_PERCENT"`
	Defaults             struct {
		CPUs   float64 `toml:"cpus" env:"LUX_DEFAULT_CPUS"`
		Memory size    `toml:"memory" env:"LUX_DEFAULT_MEMORY"`
		Disk   size    `toml:"disk" env:"LUX_DEFAULT_DISK"`
		Pids   int     `toml:"pids" env:"LUX_DEFAULT_PIDS"`
	} `toml:"defaults"`
	History struct {
		SampleEvery duration `toml:"sample_every" env:"LUX_SAMPLE_EVERY"`
		Raw         duration `toml:"raw" env:"LUX_HISTORY_RAW"`
		Minutes     duration `toml:"minutes" env:"LUX_HISTORY_MINUTES"`
		Hours       duration `toml:"hours" env:"LUX_HISTORY_HOURS"`
	} `toml:"history"`
	EC2 struct {
		Endpoint string `toml:"endpoint" env:"LUX_EC2_ENDPOINT"`
	} `toml:"ec2"`
	Console struct {
		// Auth: "key" (paste an API key) or "cloudflare-access".
		Auth             string `toml:"auth" env:"LUX_CONSOLE_AUTH"`
		CloudflareAccess struct {
			Team string `toml:"team" env:"LUX_CF_ACCESS_TEAM"`
			AUD  string `toml:"aud" env:"LUX_CF_ACCESS_AUD"`
		} `toml:"cloudflare_access"`
	} `toml:"console"`
}

// onFlag is a bool the environment sets with any non-empty value
// (LUX_DEBUG=1, =yes), as it always has; in the file, true or false.
type onFlag bool

func (f *onFlag) UnmarshalText(b []byte) error {
	*f = onFlag(len(b) > 0 && string(b) != "false" && string(b) != "0")
	return nil
}

// duration is a time.Duration written as a Go duration string ("30s").
type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// size is a number of bytes written as a string: a size ("8Gi") or digits.
type size struct{ spec.Bytes }

func (s *size) UnmarshalText(b []byte) error {
	return s.UnmarshalJSON(strconv.AppendQuote(nil, string(b)))
}

// configFlag takes --config FILE (or --config=FILE) out of luxd's
// arguments, wherever it is.
func configFlag(args []string) (path string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config":
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", nil, errors.New("--config needs a file")
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
			continue
		}
		if path == "" {
			return "", nil, errors.New("--config needs a file")
		}
	}
	return path, rest, nil
}

// defaultConfigPath is read when it exists and no other file is named.
const defaultConfigPath = "/etc/lux/luxd.toml"

func defaultConfig() config {
	var c config
	c.Database.AppPassword = "lux_app"
	c.Listen = "127.0.0.1:7070"
	c.S3.Region = "us-east-1"
	c.RunnerBinDir = "/usr/local/lib/lux/runner"
	c.Lease.Duration = 30 * time.Second
	c.Tick.Duration = time.Second
	c.ScaleDownAfter.Duration = server.DefaultScaleDownAfter
	c.LaunchTimeout.Duration = server.DefaultLaunchTimeout
	c.ProviderCheckEvery.Duration = server.DefaultProviderCheckEvery
	c.LostGrace.Duration = server.DefaultLostGrace
	c.OutdatedDrainPercent = server.DefaultOutdatedDrainPercent
	d := spec.BuiltinDefaults
	c.Defaults.CPUs, c.Defaults.Memory.Bytes, c.Defaults.Disk.Bytes, c.Defaults.Pids = d.CPUs, d.Memory, d.Disk, d.Pids
	c.History.SampleEvery.Duration = 10 * time.Second
	c.History.Raw.Duration = server.DefaultHistoryRaw
	c.History.Minutes.Duration = server.DefaultHistoryMinutes
	c.History.Hours.Duration = server.DefaultHistoryHours
	c.Console.Auth = "key"
	return c
}

// loadConfig reads path (or LUX_CONFIG, or defaultConfigPath if it exists)
// over the defaults, then the environment over that, and checks the result.
func loadConfig(path string) (config, error) {
	c := defaultConfig()
	named := path != ""
	if !named {
		path, named = os.LookupEnv("LUX_CONFIG")
	}
	if !named {
		path = defaultConfigPath
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		warnReadable(path)
		dec := toml.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("%s: %s", path, tomlError(err))
		}
	case named || !errors.Is(err, fs.ErrNotExist):
		return c, err
	}
	if err := applyEnv(reflect.ValueOf(&c).Elem()); err != nil {
		return c, err
	}
	return c, c.check()
}

// applyEnv sets every field whose env variable is set (and not empty).
func applyEnv(v reflect.Value) error {
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		name := f.Tag.Get("env")
		if name == "" {
			if fv.Kind() == reflect.Struct {
				if err := applyEnv(fv); err != nil {
					return err
				}
			}
			continue
		}
		s := os.Getenv(name)
		if s == "" {
			continue
		}
		if err := setField(fv, s); err != nil {
			return fmt.Errorf("%s: %q: %w", name, s, err)
		}
	}
	return nil
}

func setField(v reflect.Value, s string) error {
	if u, ok := v.Addr().Interface().(interface{ UnmarshalText([]byte) error }); ok {
		return u.UnmarshalText([]byte(s))
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(n))
	case reflect.Float64:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		v.SetFloat(n)
	default:
		return fmt.Errorf("unsupported kind %s", v.Kind())
	}
	return nil
}

func (c config) check() error {
	var problems []string
	if !(c.Defaults.CPUs > 0) || math.IsInf(c.Defaults.CPUs, 0) {
		problems = append(problems, "defaults.cpus (LUX_DEFAULT_CPUS) must be a positive number")
	}
	if c.Defaults.Memory.Bytes <= 0 {
		problems = append(problems, "defaults.memory (LUX_DEFAULT_MEMORY) must be a positive size")
	}
	if c.Defaults.Disk.Bytes <= 0 {
		problems = append(problems, "defaults.disk (LUX_DEFAULT_DISK) must be a positive size")
	}
	if c.Defaults.Pids <= 0 {
		problems = append(problems, "defaults.pids (LUX_DEFAULT_PIDS) must be a positive number")
	}
	switch c.Console.Auth {
	case "key":
	case "cloudflare-access":
		if c.Console.CloudflareAccess.Team == "" || c.Console.CloudflareAccess.AUD == "" {
			problems = append(problems, "console.auth cloudflare-access needs console.cloudflare_access.team and .aud (LUX_CF_ACCESS_TEAM, LUX_CF_ACCESS_AUD)")
		}
	default:
		problems = append(problems, fmt.Sprintf("console.auth (LUX_CONSOLE_AUTH) %q: want key or cloudflare-access", c.Console.Auth))
	}
	if len(problems) > 0 {
		return errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return nil
}

// tomlError says which key a TOML error is about, where go-toml knows.
func tomlError(err error) string {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		var keys []string
		for _, e := range strict.Errors {
			keys = append(keys, strings.Join(e.Key(), "."))
		}
		return "unknown keys: " + strings.Join(keys, ", ")
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Sprintf("line %d column %d: %s: %v", row, col, strings.Join(de.Key(), "."), de)
	}
	return err.Error()
}

// warnReadable warns when others may read the file: it may hold the
// database password or S3 secret (both better in the environment).
func warnReadable(path string) {
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "luxd: warning: others can read %s (mode %v); if it holds secrets, chmod 600 it or set them in the environment\n", path, st.Mode().Perm())
	}
}

// require is the value, or an error naming its key and variable.
func require(v, key, env string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("%s is required (%s in the config file, or %s)", key, key, env)
	}
	return v, nil
}
