package main

import (
	"os"
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
[console]
auth = "cloudflare-access"
[console.cloudflare_access]
team = "acme"
aud = "aud-from-file"
`)
	t.Setenv("LUX_S3_BUCKET", "from-env")
	t.Setenv("LUX_DEFAULT_CPUS", "1.5")
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
	case c.Console.Auth != "cloudflare-access" || c.Console.CloudflareAccess.Team != "acme":
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
		{"[console]\nauth = \"magic\"", "", "want key or cloudflare-access"},
	} {
		os.Unsetenv("LUX_LEASE")
		os.Unsetenv("LUX_DEFAULT_CPUS")
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
// comma-separated in the environment, and takes only absolute paths.
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
	t.Setenv("LUX_HISTORY_DISK_PATHS", "var/lib")
	if _, err := load(""); err == nil || !strings.Contains(err.Error(), "history.disk_paths") {
		t.Fatalf("relative path: %v", err)
	}
}
