package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	// How often each pool's instances are listed with the provider, how
	// long a lost provisioned host keeps its instance, and how long after a
	// launch the listings may still miss it.
	ProviderCheckEvery duration `toml:"provider_check_every" env:"LUX_PROVIDER_CHECK_EVERY"`
	LostGrace          duration `toml:"lost_grace" env:"LUX_LOST_GRACE"`
	ListingLag         duration `toml:"listing_lag" env:"LUX_LISTING_LAG"`
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
		// DiskPaths: directories whose filesystems the control host's
		// history tracks; in the environment, comma-separated. Empty
		// ([] or only commas): no disk samples.
		DiskPaths []string `toml:"disk_paths" env:"LUX_HISTORY_DISK_PATHS"`
	} `toml:"history"`
	// Costs: the cost tick and the drainer of the cost queue
	// (docs/costs.md, section 5). Off: neither runs.
	Costs struct {
		Enabled       bool               `toml:"enabled" env:"LUX_COSTS"`
		Every         duration           `toml:"every" env:"LUX_COSTS_EVERY"`
		DrainEvery    duration           `toml:"drain_every" env:"LUX_COSTS_DRAIN_EVERY"`
		Batch         int                `toml:"batch" env:"LUX_COSTS_BATCH"`
		Settle        []duration         `toml:"settle" env:"LUX_COSTS_SETTLE"`
		SettleGiveUp  duration           `toml:"settle_give_up" env:"LUX_COSTS_SETTLE_GIVE_UP"`
		Backoff       duration           `toml:"backoff" env:"LUX_COSTS_BACKOFF"`
		BackoffMax    duration           `toml:"backoff_max" env:"LUX_COSTS_BACKOFF_MAX"`
		DescribeEvery duration           `toml:"describe_every" env:"LUX_COSTS_DESCRIBE_EVERY"`
		Hourly        duration           `toml:"hourly" env:"LUX_COSTS_HOURLY"`
		Plugin        []costPluginConfig `toml:"plugin" env:"LUX_COSTS_PLUGINS"`
		Compute       struct {
			EC2             bool     `toml:"ec2" env:"LUX_COSTS_COMPUTE_EC2"`
			PricesRefresh   duration `toml:"prices_refresh" env:"LUX_COSTS_PRICES_REFRESH"`
			PricingRegion   string   `toml:"pricing_region" env:"LUX_COSTS_PRICING_REGION"`
			PricingEndpoint string   `toml:"pricing_endpoint" env:"LUX_PRICING_ENDPOINT"`
		} `toml:"compute"`
	} `toml:"costs"`
	EC2 struct {
		Endpoint string `toml:"endpoint" env:"LUX_EC2_ENDPOINT"`
	} `toml:"ec2"`
	Console struct {
		// Auth: "key" (paste an API key) or "cloudflare-access".
		Auth             string `toml:"auth" env:"LUX_CONSOLE_AUTH"`
		CloudflareAccess struct {
			Team          string   `toml:"team" env:"LUX_CF_ACCESS_TEAM"`
			AUD           string   `toml:"aud" env:"LUX_CF_ACCESS_AUD"`
			Operators     []string `toml:"operators" env:"LUX_CF_ACCESS_OPERATORS"`
			DefaultTenant string   `toml:"default_tenant" env:"LUX_CF_ACCESS_DEFAULT_TENANT"`
		} `toml:"cloudflare_access"`
		// AllowedOrigins: origins (besides public_url's and a request's
		// own) whose pages may open interactive streams.
		AllowedOrigins []string `toml:"allowed_origins" env:"LUX_CONSOLE_ALLOWED_ORIGINS"`
	} `toml:"console"`
	// Preview: the preview listener (docs/operators.md#previews).
	Preview struct {
		Domain           string   `toml:"domain" env:"LUX_PREVIEW_DOMAIN"`
		Listen           string   `toml:"listen" env:"LUX_PREVIEW_LISTEN"`
		Auth             string   `toml:"auth" env:"LUX_PREVIEW_AUTH"`
		HoldFor          duration `toml:"hold_for" env:"LUX_PREVIEW_HOLD_FOR"`
		CloudflareAccess struct {
			AUD string `toml:"aud" env:"LUX_PREVIEW_CF_ACCESS_AUD"`
		} `toml:"cloudflare_access"`
	} `toml:"preview"`
}

// Nil timeout and max_batch use the transport defaults; explicit zero is invalid.
type costPluginConfig struct {
	Name      string     `toml:"name" json:"name"`
	URL       string     `toml:"url" json:"url"`
	TokenFile string     `toml:"token_file" json:"token_file"`
	TokenEnv  string     `toml:"token_env" json:"token_env"`
	Timeout   *duration  `toml:"timeout" json:"timeout"`
	MaxBatch  *int       `toml:"max_batch" json:"max_batch"`
	Settle    []duration `toml:"settle" json:"settle"`
	Insecure  bool       `toml:"insecure" json:"insecure"`
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

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
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
	c.ListingLag.Duration = server.DefaultListingLag
	c.OutdatedDrainPercent = server.DefaultOutdatedDrainPercent
	d := spec.BuiltinDefaults
	c.Defaults.CPUs, c.Defaults.Memory.Bytes, c.Defaults.Disk.Bytes, c.Defaults.Pids = d.CPUs, d.Memory, d.Disk, d.Pids
	c.History.SampleEvery.Duration = 10 * time.Second
	c.History.Raw.Duration = server.DefaultHistoryRaw
	c.History.Minutes.Duration = server.DefaultHistoryMinutes
	c.History.Hours.Duration = server.DefaultHistoryHours
	c.History.DiskPaths = slices.Clone(server.DefaultDiskPaths)
	c.Costs.Enabled = true
	c.Costs.Every.Duration = server.DefaultCostsEvery
	c.Costs.DrainEvery.Duration = server.DefaultCostsDrainEvery
	c.Costs.Batch = server.DefaultCostsBatch
	c.Costs.Settle = []duration{{10 * time.Minute}, {time.Hour}}
	c.Costs.SettleGiveUp.Duration = 168 * time.Hour
	c.Costs.Backoff.Duration = 10 * time.Second
	c.Costs.BackoffMax.Duration = 10 * time.Minute
	c.Costs.DescribeEvery.Duration = time.Hour
	c.Costs.Hourly.Duration = 400 * 24 * time.Hour
	c.Costs.Compute.EC2 = true
	c.Costs.Compute.PricesRefresh.Duration = server.DefaultPricesRefresh
	c.Costs.Compute.PricingRegion = "us-east-1"
	c.Console.Auth = "key"
	c.Preview.Listen = "127.0.0.1:7071"
	c.Preview.HoldFor.Duration = 20 * time.Second
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
	case reflect.Slice:
		if v.Type() == reflect.TypeFor[[]duration]() || v.Type() == reflect.TypeFor[[]costPluginConfig]() {
			dec := json.NewDecoder(strings.NewReader(s))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v.Addr().Interface()); err != nil {
				return err
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				return errors.New("expected one JSON array")
			}
			if v.IsNil() {
				return errors.New("expected a JSON array, not null")
			}
			return nil
		}
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported kind %s", v.Kind())
		}
		// Non-nil even when empty: only commas is an explicit empty list.
		list := []string{}
		for _, e := range strings.Split(s, ",") {
			if e = strings.TrimSpace(e); e != "" {
				list = append(list, e)
			}
		}
		v.Set(reflect.ValueOf(list))
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
	// Runners heartbeat every third of the lease, at least every second.
	if c.Lease.Duration < 3*time.Second {
		problems = append(problems, "lease (LUX_LEASE) must be at least 3s")
	}
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
	for _, p := range c.History.DiskPaths {
		if !filepath.IsAbs(p) {
			problems = append(problems, fmt.Sprintf("history.disk_paths (LUX_HISTORY_DISK_PATHS) %q: want an absolute path", p))
		}
	}
	if c.Costs.Every.Duration <= 0 {
		problems = append(problems, "costs.every (LUX_COSTS_EVERY) must be positive")
	}
	if c.Costs.DrainEvery.Duration <= 0 {
		problems = append(problems, "costs.drain_every (LUX_COSTS_DRAIN_EVERY) must be positive")
	}
	if c.Costs.Batch <= 0 {
		problems = append(problems, "costs.batch (LUX_COSTS_BATCH) must be a positive number")
	}
	for key, value := range map[string]time.Duration{
		"settle_give_up": c.Costs.SettleGiveUp.Duration,
		"backoff":        c.Costs.Backoff.Duration,
		"backoff_max":    c.Costs.BackoffMax.Duration,
		"describe_every": c.Costs.DescribeEvery.Duration,
		"hourly":         c.Costs.Hourly.Duration,
	} {
		if value <= 0 {
			problems = append(problems, "costs."+key+" must be positive")
		}
	}
	if c.Costs.BackoffMax.Duration < c.Costs.Backoff.Duration {
		problems = append(problems, "costs.backoff_max must be at least costs.backoff")
	}
	checkSettle := func(key string, settle []duration) {
		var previous time.Duration
		for _, d := range settle {
			if d.Duration <= previous {
				problems = append(problems, key+" must contain positive, increasing durations")
				break
			}
			previous = d.Duration
		}
	}
	checkSettle("costs.settle", c.Costs.Settle)
	names := map[string]bool{"compute": true}
	for i, p := range c.Costs.Plugin {
		key := fmt.Sprintf("costs.plugin[%d]", i)
		if p.Name == "" || strings.TrimSpace(p.Name) != p.Name || strings.ContainsAny(p.Name, " \t\r\n") || names[p.Name] {
			problems = append(problems, key+".name must be nonempty, unique and not compute")
		}
		names[p.Name] = true
		u, err := url.Parse(p.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			problems = append(problems, key+".url must be an http(s) base URL without credentials, query or fragment")
		} else if u.Scheme == "http" && !p.Insecure {
			ip, ipErr := netip.ParseAddr(u.Hostname())
			if u.Hostname() != "localhost" && (ipErr != nil || !ip.IsValid() || !(ip.IsPrivate() || ip.IsLoopback())) {
				problems = append(problems, key+".url plain HTTP requires a loopback/private IP or insecure = true")
			}
		}
		if p.TokenFile != "" && p.TokenEnv != "" {
			problems = append(problems, key+" token_file and token_env are mutually exclusive")
		}
		if p.TokenFile != "" && !filepath.IsAbs(p.TokenFile) {
			problems = append(problems, key+".token_file must be an absolute path")
		}
		if p.TokenEnv != "" {
			for j, r := range p.TokenEnv {
				if !(r == '_' || r >= 'A' && r <= 'Z' || j > 0 && r >= '0' && r <= '9') {
					problems = append(problems, key+".token_env must name an environment variable (A-Z, 0-9, _)")
					break
				}
			}
		}
		if p.Timeout != nil && p.Timeout.Duration <= 0 {
			problems = append(problems, key+".timeout must be positive")
		}
		if p.MaxBatch != nil && *p.MaxBatch <= 0 {
			problems = append(problems, key+".max_batch must be positive")
		}
		checkSettle(key+".settle", p.Settle)
	}
	if c.Costs.Compute.PricesRefresh.Duration <= 0 {
		problems = append(problems, "costs.compute.prices_refresh (LUX_COSTS_PRICES_REFRESH) must be positive")
	}
	if c.Costs.Compute.PricingRegion == "" {
		problems = append(problems, "costs.compute.pricing_region (LUX_COSTS_PRICING_REGION) is required")
	}
	switch c.Console.Auth {
	case "key":
	case "cloudflare-access":
		cf := c.Console.CloudflareAccess
		if cf.Team == "" || cf.AUD == "" {
			problems = append(problems, "console.auth cloudflare-access needs console.cloudflare_access.team and .aud (LUX_CF_ACCESS_TEAM, LUX_CF_ACCESS_AUD)")
		}
		if cf.DefaultTenant == "" || cf.DefaultTenant != strings.TrimSpace(cf.DefaultTenant) {
			problems = append(problems, "console.cloudflare_access.default_tenant (LUX_CF_ACCESS_DEFAULT_TENANT) must name a tenant")
		}
		if len(cf.Operators) == 0 {
			problems = append(problems, "console.cloudflare_access.operators (LUX_CF_ACCESS_OPERATORS) needs at least one email")
		}
		seen := map[string]bool{}
		for _, email := range cf.Operators {
			if !server.ValidAccessOperatorEmail(email) || seen[strings.ToLower(email)] {
				problems = append(problems, "console.cloudflare_access.operators (LUX_CF_ACCESS_OPERATORS) must contain distinct plain email addresses")
				break
			}
			seen[strings.ToLower(email)] = true
		}
	default:
		problems = append(problems, fmt.Sprintf("console.auth (LUX_CONSOLE_AUTH) %q: want key or cloudflare-access", c.Console.Auth))
	}
	for _, o := range c.Console.AllowedOrigins {
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.TrimRight(u.Path, "/") != "" || u.RawQuery != "" {
			problems = append(problems, fmt.Sprintf("console.allowed_origins (LUX_CONSOLE_ALLOWED_ORIGINS) %q: want an origin, scheme://host[:port]", o))
		}
	}
	if p := c.Preview; p.Domain != "" {
		if strings.HasPrefix(p.Domain, ".") || strings.HasPrefix(p.Domain, "*") || strings.Contains(p.Domain, "/") || !strings.Contains(p.Domain, ".") {
			problems = append(problems, fmt.Sprintf("preview.domain (LUX_PREVIEW_DOMAIN) %q: want a domain, e.g. lux.example.com", p.Domain))
		}
		if _, _, err := net.SplitHostPort(p.Listen); err != nil {
			problems = append(problems, fmt.Sprintf("preview.listen (LUX_PREVIEW_LISTEN) %q: want host:port", p.Listen))
		}
		if p.Listen == c.Listen {
			problems = append(problems, "preview.listen must differ from listen: the preview listener only ever proxies")
		}
		auth := p.Auth
		if auth == "" {
			auth = map[string]string{"key": "ticket"}[c.Console.Auth]
			if auth == "" {
				auth = c.Console.Auth
			}
		}
		switch auth {
		case "ticket":
			if c.PublicURL == "" {
				problems = append(problems, "preview.auth ticket needs public_url (LUX_PUBLIC_URL): previews send people there to sign in")
			}
		case "cloudflare-access":
			if p.CloudflareAccess.AUD == "" || c.Console.CloudflareAccess.Team == "" {
				problems = append(problems, "preview.auth cloudflare-access needs preview.cloudflare_access.aud (LUX_PREVIEW_CF_ACCESS_AUD) and console.cloudflare_access.team")
			}
			if c.Console.Auth != "cloudflare-access" {
				problems = append(problems, "preview.auth cloudflare-access needs console.auth cloudflare-access (its operators and default tenant say who may read a Run)")
			}
		default:
			problems = append(problems, fmt.Sprintf("preview.auth (LUX_PREVIEW_AUTH) %q: want cloudflare-access or ticket", p.Auth))
		}
		if p.HoldFor.Duration < 0 {
			problems = append(problems, "preview.hold_for (LUX_PREVIEW_HOLD_FOR) must not be negative")
		}
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
