package spec

import (
	"strings"
	"testing"
)

func TestServers(t *testing.T) {
	base := func(servers ...Server) RunSpec {
		return RunSpec{Image: Image{Ref: "alpine"}, Workload: Workload{Command: []string{"sleep", "infinity"}, Workdir: "/work", Servers: servers,
			Services: []Service{{Name: "api", URL: "https://api.example.com", Loopback: true}}},
			Network: Network{Egress: []EgressRule{{Host: "api.example.com"}}}}
	}
	ok := base(
		Server{Name: "web", Port: 3000, Command: []string{"npm", "run", "dev"}, Workdir: "apps/web", Env: map[string]string{"VITE_X": "1"}},
		Server{Name: "a", Port: 1},
		Server{Name: "db-2", Port: 65535},
		Server{Name: strings.Repeat("a", 30), Port: 8080},
	)
	if err := ok.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sv   []Server
		want string
	}{
		{[]Server{{Name: "Web", Port: 1}}, "invalid name"},
		{[]Server{{Name: "1web", Port: 1}}, "invalid name"},
		{[]Server{{Name: "web-", Port: 1}}, "invalid name"},
		{[]Server{{Name: "we_b", Port: 1}}, "invalid name"},
		{[]Server{{Name: "", Port: 1}}, "invalid name"},
		{[]Server{{Name: strings.Repeat("a", 31), Port: 1}}, "invalid name"},
		{[]Server{{Name: "web", Port: 0}}, "port: need 1-65535"},
		{[]Server{{Name: "web", Port: 65536}}, "port: need 1-65535"},
		{[]Server{{Name: "web", Port: ServiceBasePort}}, "loopback port"},
		{[]Server{{Name: "web", Port: 1, Command: []string{}}}, "empty command"},
		{[]Server{{Name: "web", Port: 1, Command: []string{""}}}, "empty command"},
		{[]Server{{Name: "web", Port: 1, Workdir: "../etc"}}, "workdir"},
		{[]Server{{Name: "web", Port: 1, Env: map[string]string{"LUX_X": "1"}}}, "reserved"},
		{[]Server{{Name: "web", Port: 1, Env: map[string]string{"A-B": "1"}}}, "invalid name"},
		{[]Server{{Name: "web", Port: 1}, {Name: "web", Port: 2}}, "duplicate name"},
	} {
		sp := base(c.sv...)
		err := sp.Normalize(BuiltinDefaults)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want %q", c.sv, err, c.want)
		}
	}
}

func TestServerWorkdir(t *testing.T) {
	for _, c := range []struct{ workload, dir, want string }{
		{"/work", "", "/work"},
		{"/work", "apps/web", "/work/apps/web"},
		{"/work", "/srv", "/srv"},
		{"/work", "/srv/../x", "/x"},
		{"", "apps", "apps"},
		{"", "", ""},
	} {
		if got := ServerWorkdir(c.workload, c.dir); got != c.want {
			t.Errorf("ServerWorkdir(%q, %q) = %q, want %q", c.workload, c.dir, got, c.want)
		}
	}
}
