package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

// The file sets what it names over the defaults; the environment overrides
// the file; unknown keys and bad values are refused, naming them.
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`
listen = "0.0.0.0:7070"
lease = "45s"
[database]
url = "postgres://file"
[s3]
bucket = "from-file"
[defaults]
memory = "4Gi"
[history]
raw = "24h"
[costs]
every = "5m"
batch = 50
[console]
auth = "cloudflare-access"
[console.cloudflare_access]
team = "acme"
aud = "aud-from-file"
operators = ["ada@example.com"]
default_tenant = "absmartly"
`)
	t.Setenv("LUX_S3_BUCKET", "from-env")
	t.Setenv("LUX_DEFAULT_CPUS", "1.5")
	t.Setenv("LUX_COSTS", "false")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.Listen != "0.0.0.0:7070", c.Lease.Duration != 45*time.Second, c.Database.URL != "postgres://file":
		t.Errorf("file: %+v", c)
	case c.S3.Bucket != "from-env" || c.Defaults.CPUs != 1.5:
		t.Errorf("env over file: bucket %q cpus %v", c.S3.Bucket, c.Defaults.CPUs)
	case c.Defaults.Memory.Bytes != 4<<30 || c.Defaults.Pids != spec.BuiltinDefaults.Pids:
		t.Errorf("defaults: %+v", c.Defaults)
	case c.History.Raw.Duration != 24*time.Hour || c.History.Hours.Duration == 0:
		t.Errorf("history: %+v", c.History)
	case c.Costs.Enabled || c.Costs.Every.Duration != 5*time.Minute || c.Costs.Batch != 50 || c.Costs.DrainEvery.Duration != 2*time.Second:
		t.Errorf("costs: %+v", c.Costs)
	case c.Console.Auth != "cloudflare-access" || c.Console.CloudflareAccess.Team != "acme" ||
		!slices.Equal(c.Console.CloudflareAccess.Operators, []string{"ada@example.com"}) || c.Console.CloudflareAccess.DefaultTenant != "absmartly":
		t.Errorf("console: %+v", c.Console)
	case c.S3.Region != "us-east-1" || c.Tick.Duration != time.Second:
		t.Errorf("defaults kept: region %q tick %v", c.S3.Region, c.Tick)
	}

	for _, bad := range []struct{ file, env, want string }{
		{"lisen = \"x\"", "", "lisen"},
		{"lease = \"soon\"", "", "lease"},
		{"", "LUX_LEASE=soon", "LUX_LEASE"},
		{"[defaults]\ncpus = -1", "", "defaults.cpus"},
		{"[console]\nauth = \"cloudflare-access\"", "", "console.cloudflare_access.team"},
		{"[console]\nauth = \"cloudflare-access\"\n[console.cloudflare_access]\nteam = \"acme\"\naud = \"aud\"", "", "default_tenant"},
		{"[console]\nauth = \"cloudflare-access\"\n[console.cloudflare_access]\nteam = \"acme\"\naud = \"aud\"\ndefault_tenant = \"absmartly\"", "", "operators"},
		{"[console]\nauth = \"cloudflare-access\"\n[console.cloudflare_access]\nteam = \"acme\"\naud = \"aud\"\ndefault_tenant = \"absmartly\"\noperators = [\"ada@example.com\", \"ADA@example.com\"]", "", "operators"},
		{"[console]\nauth = \"magic\"", "", "want key or cloudflare-access"},
		{"[costs]\nbatch = 0", "", "costs.batch"},
		{"", "LUX_COSTS_EVERY=0s", "costs.every"},
	} {
		os.Unsetenv("LUX_LEASE")
		os.Unsetenv("LUX_DEFAULT_CPUS")
		os.Unsetenv("LUX_COSTS_EVERY")
		write(bad.file)
		if bad.env != "" {
			k, v, _ := strings.Cut(bad.env, "=")
			t.Setenv(k, v)
		}
		if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q %q: error %v, want it to name %q", bad.file, bad.env, err, bad.want)
		}
	}
	// A file named but missing is an error; the default path missing is not.
	if _, err := loadConfig(filepath.Join(dir, "none.toml")); err == nil {
		t.Error("a missing named file was not an error")
	}
}

// The host reconciler's rendered TOML must decode with luxd's strict parser
// and pass the same validation as a config loaded at startup.
func TestHostRenderedConfig(t *testing.T) {
	for _, name := range []string{"LUX_CONSOLE_AUTH", "LUX_CF_ACCESS_TEAM", "LUX_CF_ACCESS_AUD", "LUX_CF_ACCESS_OPERATORS", "LUX_CF_ACCESS_DEFAULT_TENANT", "LUX_DEFAULT_MEMORY"} {
		t.Setenv(name, "")
	}
	for _, mode := range []string{"access", "key"} {
		t.Run(mode, func(t *testing.T) {
			fixture := "../../deploy/terraform/examples/aws/host/tests/render_config.py"
			out, err := exec.Command("python3", fixture, mode).CombinedOutput()
			if err != nil {
				t.Fatalf("host render: %v: %s", err, out)
			}
			path := filepath.Join(t.TempDir(), "luxd.toml")
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(path)
			if err != nil {
				t.Fatalf("host-generated %s config: %v", mode, err)
			}
			if c.Database.URL != "postgres://lux_app:test-password@127.0.0.1:5432/lux?sslmode=disable" {
				t.Fatalf("host-generated %s config: unexpected values: %+v", mode, c)
			}
			if mode == "access" {
				if c.Console.Auth != "cloudflare-access" ||
					c.Console.CloudflareAccess.Team != "acme" || c.Console.CloudflareAccess.AUD != "aud-tag" ||
					c.Defaults.Memory.Bytes != 16<<30 ||
					!slices.Equal(c.Console.CloudflareAccess.Operators, []string{"operator@example.com", "second@example.com"}) ||
					c.Console.CloudflareAccess.DefaultTenant != "ten_aaaaaaaaaaaaaaaa" {
					t.Fatalf("host-generated Access config: %+v", c.Console)
				}
			} else if c.Console.Auth != "key" || c.Console.CloudflareAccess.Team != "" ||
				c.Console.CloudflareAccess.AUD != "" || len(c.Console.CloudflareAccess.Operators) != 0 ||
				c.Console.CloudflareAccess.DefaultTenant != "" {
				t.Fatalf("host-generated key config: %+v", c.Console)
			}
		})
	}
}

func TestConfigFlag(t *testing.T) {
	for _, c := range []struct {
		args       []string
		path, rest string
		err        bool
	}{
		{[]string{"serve"}, "", "serve", false},
		{[]string{"--config", "a.toml", "serve"}, "a.toml", "serve", false},
		{[]string{"serve", "--config", "a.toml"}, "a.toml", "serve", false},
		{[]string{"--config=a.toml", "admin", "create-tenant", "--name", "x"}, "a.toml", "admin create-tenant --name x", false},
		{[]string{"admin", "create-tenant", "--config", "a.toml", "--name", "x"}, "a.toml", "admin create-tenant --name x", false},
		{[]string{"serve", "--config"}, "", "", true},
		{[]string{"--config", "--debug", "serve"}, "", "", true},
		{[]string{"--config=", "serve"}, "", "", true},
	} {
		path, rest, err := configFlag(c.args)
		if (err != nil) != c.err || path != c.path || strings.Join(rest, " ") != c.rest {
			t.Errorf("%q: %q %q %v", c.args, path, rest, err)
		}
	}
}

// runner_url defaults to public_url, but can be set separately (a private
// address runners reach that clients cannot).
func TestRunnerURLDefaultsToPublicURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`public_url = "https://luxd.example"`)
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RunnerURL != "" {
		t.Errorf("runner_url defaulted at config load: %q (server.New applies the default)", c.RunnerURL)
	}

	write(`
public_url = "https://luxd.example"
runner_url = "http://10.0.1.10:7070"
`)
	c, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RunnerURL != "http://10.0.1.10:7070" {
		t.Errorf("runner_url from file: %q", c.RunnerURL)
	}

	t.Setenv("LUX_RUNNER_URL", "http://10.0.1.20:7070")
	c, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RunnerURL != "http://10.0.1.20:7070" {
		t.Errorf("LUX_RUNNER_URL over file: %q", c.RunnerURL)
	}
}

// LUX_DEBUG turns debug on with any value but false and 0, as it always has.
func TestDebugFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"1": true, "yes": true, "true": true, "false": false, "0": false} {
		t.Setenv("LUX_DEBUG", v)
		c, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if bool(c.Debug) != want {
			t.Errorf("LUX_DEBUG=%s: debug %v", v, c.Debug)
		}
	}
}

// history.disk_paths defaults to "/", is a list in the file and
// comma-separated in the environment, may be explicitly empty, and takes
// only absolute paths.
func TestDiskPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luxd.toml")
	load := func(file string) (config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadConfig(path)
	}
	c, err := load("")
	if err != nil || !slices.Equal(c.History.DiskPaths, []string{"/"}) {
		t.Fatalf("default: %q %v", c.History.DiskPaths, err)
	}
	c, err = load("[history]\ndisk_paths = [\"/\", \"/var/lib/postgresql/18\"]\n")
	if err != nil || !slices.Equal(c.History.DiskPaths, []string{"/", "/var/lib/postgresql/18"}) {
		t.Fatalf("file: %q %v", c.History.DiskPaths, err)
	}
	t.Setenv("LUX_HISTORY_DISK_PATHS", "/data, /srv,")
	c, err = load("[history]\ndisk_paths = [\"/\"]\n")
	if err != nil || !slices.Equal(c.History.DiskPaths, []string{"/data", "/srv"}) {
		t.Fatalf("env over file: %q %v", c.History.DiskPaths, err)
	}
	// An explicitly empty list, in the file or as only commas, tracks no
	// disks: it stays empty rather than falling back to "/".
	t.Setenv("LUX_HISTORY_DISK_PATHS", ",,")
	c, err = load("")
	if err != nil || c.History.DiskPaths == nil || len(c.History.DiskPaths) != 0 {
		t.Fatalf("env ,,: %#v %v", c.History.DiskPaths, err)
	}
	t.Setenv("LUX_HISTORY_DISK_PATHS", "")
	c, err = load("[history]\ndisk_paths = []\n")
	if err != nil || c.History.DiskPaths == nil || len(c.History.DiskPaths) != 0 {
		t.Fatalf("file []: %#v %v", c.History.DiskPaths, err)
	}
	t.Setenv("LUX_HISTORY_DISK_PATHS", "var/lib")
	if _, err := load(""); err == nil || !strings.Contains(err.Error(), "history.disk_paths") {
		t.Fatalf("relative path: %v", err)
	}
}

func TestCostPluginConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luxd.toml")
	load := func(file string) (config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadConfig(path)
	}
	c, err := load(`[[costs.plugin]]
name = "ledger"
url = "http://10.0.0.5:8080"
token_env = "LEDGER_TOKEN"
timeout = "5s"
max_batch = 10
settle = ["15m", "2h"]
`)
	if err != nil || len(c.Costs.Plugin) != 1 || c.Costs.Plugin[0].Timeout.Duration != 5*time.Second || *c.Costs.Plugin[0].MaxBatch != 10 || len(c.Costs.Plugin[0].Settle) != 2 {
		t.Fatalf("file plugin: %+v %v", c.Costs.Plugin, err)
	}
	t.Setenv("LUX_COSTS_PLUGINS", `[{"name":"env-ledger","url":"https://ledger.example","max_batch":20,"settle":["1h"]}]`)
	t.Setenv("LUX_COSTS_SETTLE", `["20m","3h"]`)
	c, err = load(`[[costs.plugin]]
name = "file-ledger"
url = "https://file.example"
`)
	if err != nil || len(c.Costs.Plugin) != 1 || c.Costs.Plugin[0].Name != "env-ledger" || *c.Costs.Plugin[0].MaxBatch != 20 || len(c.Costs.Settle) != 2 || c.Costs.Settle[0].Duration != 20*time.Minute {
		t.Fatalf("env override: %+v %v", c.Costs, err)
	}
	t.Setenv("LUX_COSTS_PLUGINS", "")
	t.Setenv("LUX_COSTS_SETTLE", "")
	for _, tc := range []struct{ file, want string }{
		{`[[costs.plugin]]
name = "compute"
url = "https://x.example"`, "name"},
		{`[[costs.plugin]]
name = "x"
url = "http://public.example"`, "url"},
		{`[[costs.plugin]]
name = "x"
url = "https://user:password@x.example"`, "url"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
token_file = "/tmp/token"
token_env = "TOKEN"`, "token_file"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
token_file = "relative.token"`, "token_file"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
token_env = "bad-name"`, "token_env"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
[[costs.plugin]]
name = "x"
url = "https://other.example"`, "name"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
timeout = "0s"`, "timeout"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
max_batch = 0`, "max_batch"},
		{`[costs]
settle = ["1h", "10m"]`, "settle"},
		{`[costs]
describe_every = "0s"`, "describe_every"},
		{`[costs]
backoff = "20m"`, "backoff_max"},
		{`[[costs.plugin]]
name = "x"
url = "https://x.example"
surprise = 1`, "surprise"},
	} {
		if _, err := load(tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.file, err, tc.want)
		}
	}
	for _, value := range []string{`not json`, `[{"name":"x","url":"https://x.example","extra":1}]`, `null`, `[{"name":"x","url":"https://x.example","timeout":"bad"}]`} {
		t.Setenv("LUX_COSTS_PLUGINS", value)
		if _, err := load(""); err == nil || !strings.Contains(err.Error(), "LUX_COSTS_PLUGINS") {
			t.Errorf("env %q: %v", value, err)
		}
	}
}
