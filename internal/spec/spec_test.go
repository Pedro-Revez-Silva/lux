package spec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const example = `
name: fix
image: { ref: alpine }
workload:
  adapter: claude-code
  prompt: hi
secrets:
  - { name: ANTHROPIC_API_KEY, value: sk-1 }
  - { name: GITHUB_TOKEN, value: ghp }
git:
  repositories:
    - { name: api, url: https://example.com/a.git, credential: GITHUB_TOKEN }
volumes:
  - { name: workspace, path: /workspace }
  - { name: home, path: /home/agent, kind: state }
resources: { cpus: 1.5, memory: 512Mi }
timeout: 90m
`

func TestNormalizeExample(t *testing.T) {
	var s RunSpec
	if err := yaml.Unmarshal([]byte(example), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if s.Resources.Memory != 512<<20 || s.Timeout.Duration != 90*time.Minute || s.Resources.CPUs != 1.5 {
		t.Fatalf("parsed %+v %v", s.Resources, s.Timeout)
	}
	if s.Git.Repositories[0].Path != "/workspace/repos/api" || s.Git.Repositories[0].Ref != "HEAD" {
		t.Fatalf("repo defaults: %+v", s.Git.Repositories[0])
	}
	if s.Volumes[0].Kind != "state" {
		t.Fatal("volume kind default")
	}
	for _, sec := range s.Secrets {
		if sec.Name == "GITHUB_TOKEN" && !sec.RunnerOnly {
			t.Fatal("git credential must be runner-only")
		}
	}
	stripped, refs, vals := s.SplitSecrets()
	for _, sec := range stripped.Secrets {
		if sec.Value != "" {
			t.Fatal("value kept")
		}
	}
	if len(refs) != 2 || vals["ANTHROPIC_API_KEY"] != "sk-1" {
		t.Fatal("split")
	}
}

func TestAdapterStatePathsMustBeOnStateVolume(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Adapter: "claude-code"}}
	err := s.Normalize(BuiltinDefaults)
	if err == nil || !strings.Contains(err.Error(), "/home/agent/.claude") {
		t.Fatalf("want state path error, got %v", err)
	}
}

func TestAllProblemsAtOnce(t *testing.T) {
	s := RunSpec{Workload: Workload{Adapter: "nope"}, Env: map[string]string{"LUX_X": "1"}}
	err := s.Normalize(BuiltinDefaults)
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) < 3 {
		t.Fatalf("want several problems, got %v", err)
	}
}

func TestBytes(t *testing.T) {
	for in, want := range map[string]int64{"8Gi": 8 << 30, "1G": 1e9, "100": 100, "1.5Mi": 1.5 * (1 << 20)} {
		var b Bytes
		if err := b.parse(in); err != nil || int64(b) != want {
			t.Errorf("%s: %d %v", in, b, err)
		}
	}
}

func TestGitPathsMustBeOnAStateVolume(t *testing.T) {
	base := func(path string) RunSpec {
		return RunSpec{
			Image:    Image{Ref: "x"},
			Workload: Workload{Command: []string{"true"}},
			Volumes: []Volume{
				{Name: "workspace", Path: "/workspace", Kind: "state"},
				{Name: "cache", Path: "/workspace/cache", Kind: "ephemeral"},
			},
			Git: &Git{Repositories: []Repository{{Name: "api", URL: "https://x/a.git", Path: path}}},
		}
	}
	for path, ok := range map[string]bool{
		"/workspace/repos/api": true,
		"/workspace/cache/api": false, // the more specific, ephemeral volume
		"/opt/api":             false, // on no volume
	} {
		s := base(path)
		err := s.Normalize(BuiltinDefaults)
		if (err == nil) != ok {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestGitURLWithCredentialsIsRefused(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
		Volumes: []Volume{{Name: "workspace", Path: "/workspace"}},
		Git:     &Git{Repositories: []Repository{{Name: "a", URL: "https://user:tok@x/a.git"}}}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "must not carry credentials") {
		t.Fatalf("got %v", err)
	}
}

func TestMCPServers(t *testing.T) {
	tok := []MCPHeader{{Name: "Authorization", Secret: "MCP_TOKEN"}}
	base := func(net Network, servers ...MCPServer) RunSpec {
		return RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}, MCPServers: servers},
			Secrets: []Secret{{Name: "MCP_TOKEN", Value: "Bearer t"}}, Network: net}
	}
	byHost := Network{Egress: []EgressRule{{Host: "mcp.example.com"}, {CIDR: "10.1.0.0/16"}}}
	for _, c := range []struct {
		name    string
		spec    RunSpec
		problem string // "" for valid
	}{
		{"host rule", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/mcp", Headers: tok}), ""},
		{"host rule, case and dot", base(byHost, MCPServer{Name: "a", URL: "https://MCP.example.com./mcp"}), ""},
		{"cidr for an address", base(byHost, MCPServer{Name: "a", URL: "http://10.1.2.3:8080/mcp"}), ""},
		{"unrestricted", base(Network{Unrestricted: true}, MCPServer{Name: "a", URL: "https://anywhere.example/mcp"}), ""},
		{"host not allowed", base(byHost, MCPServer{Name: "a", URL: "https://other.example.com/mcp"}), `does not allow MCP server "a" at other.example.com: add an egress rule`},
		{"address outside the cidr", base(byHost, MCPServer{Name: "a", URL: "http://10.2.0.1/mcp"}), "add an egress rule"},
		{"a name is not matched by a cidr", base(Network{Egress: []EgressRule{{CIDR: "0.0.0.0/0"}}}, MCPServer{Name: "a", URL: "https://x.example/mcp"}), "add an egress rule"},
		{"no rules", base(Network{}, MCPServer{Name: "a", URL: "https://mcp.example.com/mcp"}), "add an egress rule"},
		{"duplicate names", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/"}, MCPServer{Name: "a", URL: "https://mcp.example.com/"}), "duplicate name"},
		{"bad name", base(byHost, MCPServer{Name: "Bad Name", URL: "https://mcp.example.com/"}), "invalid name"},
		{"not http", base(byHost, MCPServer{Name: "a", URL: "ftp://mcp.example.com/"}), "http or https"},
		{"no host", base(byHost, MCPServer{Name: "a", URL: "https:///mcp"}), "http or https"},
		{"relative", base(byHost, MCPServer{Name: "a", URL: "/mcp"}), "http or https"},
		{"userinfo", base(byHost, MCPServer{Name: "a", URL: "https://u:p@mcp.example.com/"}), "must not carry credentials"},
		{"unknown secret", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/", Headers: []MCPHeader{{Name: "X-Key", Secret: "NOPE"}}}), `no secret named "NOPE"`},
		{"bad header name", base(byHost, MCPServer{Name: "a", URL: "https://mcp.example.com/", Headers: []MCPHeader{{Name: "Bad Header:", Secret: "MCP_TOKEN"}}}), "invalid header name"},
	} {
		err := c.spec.Normalize(BuiltinDefaults)
		switch {
		case c.problem == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.problem != "" && (err == nil || !strings.Contains(err.Error(), c.problem)):
			t.Errorf("%s: want %q, got %v", c.name, c.problem, err)
		}
	}
}

// An MCP header's secret is not runner-only: it reaches the container
// through the header. So a git credential, which never does, is refused.
func TestMCPHeaderSecretsAndRunnerOnly(t *testing.T) {
	spec := func(secret string) RunSpec {
		return RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"},
			MCPServers: []MCPServer{{Name: "a", URL: "https://m.example/", Headers: []MCPHeader{{Name: "Authorization", Secret: secret}}}}},
			Secrets: []Secret{{Name: "MCP", Value: "v"}, {Name: "GIT", Value: "g"}}, Network: Network{Unrestricted: true},
			Volumes: []Volume{{Name: "workspace", Path: "/workspace"}},
			Git:     &Git{Repositories: []Repository{{Name: "r", URL: "https://x/r.git", Credential: "GIT"}}}}
	}
	s := spec("MCP")
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if s.Secrets[0].RunnerOnly || !s.Secrets[1].RunnerOnly {
		t.Fatalf("runner-only: %+v", s.Secrets)
	}
	// Used only as a header (or a git credential): not in any process's
	// environment unless the spec asks.
	if s.Secrets[0].As != "none" || s.Secrets[1].As != "none" {
		t.Fatalf("as: %+v", s.Secrets)
	}
	s = spec("MCP")
	s.Secrets[0].As = "env"
	if err := s.Normalize(BuiltinDefaults); err != nil || s.Secrets[0].As != "env" {
		t.Fatalf("an explicit as stands: %v %+v", err, s.Secrets)
	}
	s = spec("MCP")
	s.Workload.MCPServers[0].Headers = append(s.Workload.MCPServers[0].Headers, MCPHeader{Name: "authorization", Secret: "MCP"})
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "duplicate header") {
		t.Fatalf("duplicate header: %v", err)
	}
	s = spec("MCP")
	s.Secrets = append(s.Secrets, Secret{Name: "lux-claude-mcp.json", Value: "x", As: "file", Path: "/x"})
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "prefix is reserved") {
		t.Fatalf("reserved name: %v", err)
	}
	s = spec("GIT")
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "is a git credential") {
		t.Fatalf("got %v", err)
	}
}

// A stored spec is normalized again when a resume adds repositories: that
// must change nothing, also after the JSON round trip storage is.
func TestNormalizeIsIdempotent(t *testing.T) {
	var s RunSpec
	if err := yaml.Unmarshal([]byte(example), &s); err != nil {
		t.Fatal(err)
	}
	f := false
	s.Git.Repositories = append(s.Git.Repositories, Repository{Name: "ctx", URL: "https://example.com/c.git", Ref: "v1", Push: &f})
	s.Git.Push = &Push{Branch: "lux/work"}
	s.Secrets = append(s.Secrets, Secret{Name: "CFG", Value: "x", As: "file", Path: "/etc/cfg"},
		Secret{Name: "MCP", Value: "Bearer y"})
	s.Workload.MCPServers = []MCPServer{{Name: "m", URL: "https://m.example/mcp", Headers: []MCPHeader{{Name: "Authorization", Secret: "MCP"}}}}
	s.Network.Egress = []EgressRule{{Host: "m.example"}}
	s.Volumes = append(s.Volumes, Volume{Name: "cache", Path: "/cache/", Kind: "ephemeral"})
	s.Env = map[string]string{"A": "1"}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	once, _, _ := s.SplitSecrets()
	b, _ := json.Marshal(once)
	var twice RunSpec
	if err := json.Unmarshal(b, &twice); err != nil {
		t.Fatal(err)
	}
	if err := twice.Normalize(Defaults{CPUs: 9, Memory: 1, Disk: 1, Pids: 1}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("normalized twice differs:\n%+v\n%+v", once, twice)
	}
}

func TestAddRepositories(t *testing.T) {
	stored := func() RunSpec {
		var s RunSpec
		if err := yaml.Unmarshal([]byte(example), &s); err != nil {
			t.Fatal(err)
		}
		if err := s.Normalize(BuiltinDefaults); err != nil {
			t.Fatal(err)
		}
		s, _, _ = s.SplitSecrets()
		return s
	}
	s := stored()
	added, err := s.AddRepositories([]Repository{
		{Name: "web", URL: "https://example.com/w.git", Credential: "WEB_TOKEN"},
		{Name: "docs", URL: "https://example.com/d.git", Credential: "GITHUB_TOKEN"}, // declared already
		{Name: "pub", URL: "https://example.com/p.git"},
	}, "r-1", BuiltinDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"WEB_TOKEN"}) {
		t.Fatalf("declared %v", added)
	}
	rs := s.Git.Repositories
	if len(rs) != 4 || rs[0].AddedBy != "" || rs[1].AddedBy != "r-1" || rs[1].Path != "/workspace/repos/web" || rs[1].Ref != "HEAD" {
		t.Fatalf("repositories %+v", rs)
	}
	web := s.Secrets[len(s.Secrets)-1]
	if web.Name != "WEB_TOKEN" || web.As != "none" || !web.RunnerOnly || web.Value != "" {
		t.Fatalf("new credential %+v", web)
	}

	for name, c := range map[string]struct {
		repo    Repository
		problem string
	}{
		"existing name":       {Repository{Name: "api", URL: "https://example.com/x.git"}, "duplicate name"},
		"no url":              {Repository{Name: "x"}, "url is required"},
		"credentials in url":  {Repository{Name: "x", URL: "https://u:p@example.com/x.git"}, "must not carry credentials"},
		"not on a state vol":  {Repository{Name: "x", URL: "https://example.com/x.git", Path: "/opt/x"}, "not on a state volume"},
		"a secret it can see": {Repository{Name: "x", URL: "https://example.com/x.git", Credential: "ANTHROPIC_API_KEY"}, "the workload sees"},
	} {
		s := stored()
		if _, err := s.AddRepositories([]Repository{c.repo}, "r-2", BuiltinDefaults); err == nil || !strings.Contains(err.Error(), c.problem) {
			t.Errorf("%s: want %q, got %v", name, c.problem, err)
		}
	}

	// A failed clone drops the repository its request added, and only that.
	s = stored()
	if _, err := s.AddRepositories([]Repository{{Name: "web", URL: "https://example.com/w.git", Credential: "WEB_TOKEN"}}, "r-1", BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if s.DropRepository("web", "r-9") || s.DropRepository("api", "") || s.DropRepository("api", "r-1") {
		t.Fatal("dropped a repository another request (or the submit) added")
	}
	if !s.DropRepository("web", "r-1") || len(s.Git.Repositories) != 1 {
		t.Fatalf("not dropped: %+v", s.Git.Repositories)
	}
	if err := s.Normalize(BuiltinDefaults); err != nil || !s.runnerOnly("WEB_TOKEN") {
		t.Fatalf("its credential stays runner-only: %v %+v", err, s.Secrets)
	}
}

func TestAddRepositoriesSharingANewCredential(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
		Volumes: []Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}}}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	added, err := s.AddRepositories([]Repository{
		{Name: "a", URL: "https://x/a.git", Credential: "TOK"},
		{Name: "b", URL: "https://x/b.git", Credential: "TOK"},
	}, "r-1", BuiltinDefaults)
	if err != nil {
		t.Fatalf("two added repositories may share a new credential: %v", err)
	}
	if len(added) != 1 || added[0] != "TOK" || !s.runnerOnly("TOK") {
		t.Fatalf("added %v, secrets %+v", added, s.Secrets)
	}
}

func TestServiceBackedMCPRules(t *testing.T) {
	base := func() RunSpec {
		return RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"},
			Services: []Service{{Name: "tools", URL: "https://t.example/", Headers: []MCPHeader{{Name: "Authorization", Secret: "TOK"}}, Loopback: true},
				{Name: "plain", URL: "https://p.example/"}},
			MCPServers: []MCPServer{{Name: "tools", Service: "tools"}}},
			Secrets: []Secret{{Name: "TOK", Value: "Bearer t"}}, Network: Network{Unrestricted: true}}
	}
	s := base()
	if err := s.Normalize(BuiltinDefaults); err != nil || s.Workload.MCPServers[0].Path != "" || s.ServicePort("tools") != ServiceBasePort {
		t.Fatalf("%v %+v", err, s.Workload.MCPServers)
	}
	if s.Secrets[0].As != "none" {
		t.Fatalf("a service header's secret is placed nowhere: %+v", s.Secrets)
	}
	for name, mut := range map[string]func(*RunSpec){
		"no loopback":   func(s *RunSpec) { s.Workload.MCPServers[0].Service = "plain" },
		"no service":    func(s *RunSpec) { s.Workload.MCPServers[0].Service = "nope" },
		"url too":       func(s *RunSpec) { s.Workload.MCPServers[0].URL = "https://x/" },
		"headers too":   func(s *RunSpec) { s.Workload.MCPServers[0].Headers = []MCPHeader{{Name: "X", Secret: "TOK"}} },
		"relative path": func(s *RunSpec) { s.Workload.MCPServers[0].Path = "mcp" },
		"dotdot path":   func(s *RunSpec) { s.Workload.MCPServers[0].Path = "/../x" },
		"path, no svc":  func(s *RunSpec) { s.Workload.MCPServers[0] = MCPServer{Name: "m", URL: "https://m/", Path: "/x"} },
		"port clash":    func(s *RunSpec) { s.Network.Ports = []Port{{Name: "web", Port: ServiceBasePort}} },
	} {
		s := base()
		mut(&s)
		if err := s.Normalize(BuiltinDefaults); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// beforeStop's default timeout fits the grace; only one someone set can be
// too long for it.
func TestBeforeStopTimeoutAgainstGrace(t *testing.T) {
	spec := func(grace, timeout string) RunSpec {
		s := RunSpec{Image: Image{Ref: "alpine"}, Workload: Workload{Adapter: "generic", Command: []string{"true"},
			BeforeStop: &BeforeStop{Command: []string{"true"}}}}
		s.Workload.Grace.Duration, _ = time.ParseDuration(grace)
		if timeout != "" {
			s.Workload.BeforeStop.Timeout.Duration, _ = time.ParseDuration(timeout)
		}
		return s
	}
	s := spec("5s", "")
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatalf("a default timeout under a short grace: %v", err)
	}
	if got := s.Workload.BeforeStop.Timeout.Duration; got != 5*time.Second {
		t.Fatalf("default timeout %s, want the grace (5s)", got)
	}
	s = spec("", "")
	_ = s.Normalize(BuiltinDefaults)
	if got := s.Workload.BeforeStop.Timeout.Duration; got != DefaultBeforeStopTimeout {
		t.Fatalf("default timeout %s, want %s", got, DefaultBeforeStopTimeout)
	}
	s = spec("5s", "20s")
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "beforeStop.timeout") {
		t.Fatalf("a timeout past the grace: %v", err)
	}
}
