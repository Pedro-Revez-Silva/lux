package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func cpConfig(t *testing.T, toml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "luxd.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestControlPlanePortsConfig(t *testing.T) {
	c, err := loadConfig(cpConfig(t, ""))
	if err != nil || c.Runners.ControlPlanePorts != nil {
		t.Fatalf("default: %v %v", c.Runners.ControlPlanePorts, err)
	}
	path := cpConfig(t, "[runners]\ncontrol_plane_ports = [8090, 9000, 8090]\n")
	c, err = loadConfig(path)
	if err != nil || !slices.Equal(c.Runners.ControlPlanePorts, []int{8090, 9000}) {
		t.Fatalf("file: %v %v", c.Runners.ControlPlanePorts, err)
	}
	t.Setenv("LUX_RUNNER_CONTROL_PLANE_PORTS", " 8091, 8091,9001 ")
	c, err = loadConfig(path)
	if err != nil || !slices.Equal(c.Runners.ControlPlanePorts, []int{8091, 9001}) {
		t.Fatalf("env over file: %v %v", c.Runners.ControlPlanePorts, err)
	}
	t.Setenv("LUX_RUNNER_CONTROL_PLANE_PORTS", "8090,http")
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "LUX_RUNNER_CONTROL_PLANE_PORTS") {
		t.Fatalf("not a number: %v", err)
	}
}

func portList(n int) string {
	ports := make([]string, n)
	for i := range ports {
		ports[i] = fmt.Sprint(9000 + i)
	}
	return strings.Join(ports, ", ")
}

func TestControlPlanePortsRefused(t *testing.T) {
	for name, c := range map[string]struct{ ports, want string }{
		"zero":         {"0", "want 1-65535"},
		"too high":     {"65536", "want 1-65535"},
		"negative":     {"-1", "want 1-65535"},
		"more than 16": {portList(17), "at most 16"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(cpConfig(t, "[runners]\ncontrol_plane_ports = ["+c.ports+"]")); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	// Sixteen distinct after collapsing duplicates is within the cap.
	if _, err := loadConfig(cpConfig(t, "[runners]\ncontrol_plane_ports = ["+portList(16)+", 9000]")); err != nil {
		t.Fatal(err)
	}
}

// No configuration can make one of luxd's own ports exempt: its listener,
// the URL runners dial, the public URL (explicit or by scheme) and the
// preview listener are refused at startup.
func TestLuxdOwnPortIsNeverExempt(t *testing.T) {
	const preview = "\n[preview]\ndomain = \"preview.luxd.example\"\n"
	for name, c := range map[string]struct {
		top   string // top-level keys
		port  int
		extra string // tables after [runners]
		want  string
	}{
		"listen":                   {`listen = "0.0.0.0:8090"`, 8090, "", "listen (LUX_LISTEN)"},
		"default listen":           {``, 7070, "", "listen (LUX_LISTEN)"},
		"runner_url":               {`runner_url = "http://10.0.1.10:8090"`, 8090, "", "runner_url"},
		"runner_url scheme":        {`runner_url = "http://10.0.1.10"`, 80, "", "runner_url"},
		"public_url explicit":      {`public_url = "https://luxd.example:8090"`, 8090, "", "public_url"},
		"public_url scheme":        {`public_url = "https://luxd.example"`, 443, "", "public_url"},
		"public_url as runner_url": {`public_url = "http://luxd.example:8090"`, 8090, "", "runner_url"},
		"preview listen":           {`public_url = "https://luxd.example"`, 8090, preview + `listen = "127.0.0.1:8090"`, "preview.listen"},
		"preview default listen":   {`public_url = "https://luxd.example"`, 7071, preview, "preview.listen"},
		"preview public port":      {`public_url = "https://luxd.example"`, 8090, preview + "public_port = 8090", "preview.public_port"},
	} {
		t.Run(name, func(t *testing.T) {
			toml := fmt.Sprintf("%s\n[runners]\ncontrol_plane_ports = [%d]\n%s\n", c.top, c.port, c.extra)
			_, err := loadConfig(cpConfig(t, toml))
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "never opened to Runs") {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
		})
	}
	// The same port is fine when luxd does not use it.
	if _, err := loadConfig(cpConfig(t, "listen = \"127.0.0.1:7070\"\npublic_url = \"https://luxd.example\"\n[runners]\ncontrol_plane_ports = [8090]\n")); err != nil {
		t.Fatal(err)
	}
	// The preview listener's port is luxd's only while previews are on.
	if _, err := loadConfig(cpConfig(t, "[runners]\ncontrol_plane_ports = [7071]\n")); err != nil {
		t.Fatal(err)
	}
}
