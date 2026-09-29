package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain keeps every test in the package off the developer's own config
// and LUX_* variables, which would add a ?tenant= to every request.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "lux-cli-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, k := range []string{"LUX_URL", "LUX_API_KEY", "LUX_TENANT"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// isolateConfig points the user config dir at a temp dir, so the real
// ~/.config/lux/config.toml is never read, and writes config there.
func isolateConfig(t *testing.T, config string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, k := range []string{"LUX_URL", "LUX_API_KEY", "LUX_TENANT"} {
		t.Setenv(k, "") // restored after the test
		os.Unsetenv(k)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lux"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lux", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTenantPrecedence(t *testing.T) {
	unset := "\x00"
	for _, c := range []struct {
		name   string
		config string
		env    string   // unset: not in the environment
		flag   []string // nil: not given
		want   string   // "" : no ?tenant=
		absent bool
	}{
		{name: "flag", config: `tenant = "cfg"`, env: "env", flag: []string{"--tenant", "flag"}, want: "flag"},
		{name: "env", config: `tenant = "cfg"`, env: "env", want: "env"},
		{name: "config", config: `tenant = "cfg"`, env: unset, want: "cfg"},
		{name: "env set empty", config: `tenant = "cfg"`, env: "", absent: true},
		{name: "flag set empty", config: `tenant = "cfg"`, env: "env", flag: []string{"--tenant", ""}, absent: true},
		{name: "flag set empty with =", config: `tenant = "cfg"`, env: unset, flag: []string{"--tenant="}, absent: true},
		{name: "none", config: `url = "http://unused"`, env: unset, absent: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolateConfig(t, c.config)
			if c.env != unset {
				t.Setenv("LUX_TENANT", c.env)
			}
			f := &fakeLuxd{bodies: map[string]any{"/v1/runs/run_1/cost": fixtureRunCost()}}
			if _, err := runCLI(t, f, append(c.flag, "cost", "run_1", "-o", "json")...); err != nil {
				t.Fatal(err)
			}
			if len(f.seen) != 1 {
				t.Fatalf("requests: %v", f.seen)
			}
			u, err := url.Parse(f.seen[0])
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if c.absent {
				if q.Has("tenant") {
					t.Errorf("sent tenant=%q, want none (%s)", q.Get("tenant"), f.seen[0])
				}
			} else if !q.Has("tenant") || q.Get("tenant") != c.want {
				t.Errorf("sent %s, want tenant=%s", f.seen[0], c.want)
			}
		})
	}
}

func TestTenantRequiredHint(t *testing.T) {
	isolateConfig(t, "")
	f := &fakeLuxd{
		status: map[string]int{"/v1/runs": http.StatusBadRequest},
		errors: map[string][2]string{"/v1/runs": {"tenant_required", "an operator key must name a tenant: ?tenant=<id or name>"}},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	spec := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(spec, []byte("workload:\n  type: generic\n  image: alpine\n  command: [\"true\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	if code := a.main([]string{"--url", srv.URL, "--api-key", "k", "run", "-f", spec}); code != 1 {
		t.Fatalf("exit %d, want 1; stderr:\n%s", code, errOut.String())
	}
	want := "lux: an operator key must name a tenant: ?tenant=<id or name>\n" +
		"lux: name one with --tenant, LUX_TENANT, or tenant in ~/.config/lux/config.toml\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", got, want)
	}
}
