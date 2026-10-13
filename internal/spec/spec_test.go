package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	// The pool is luxd's to choose at submit (the tenant's default), not
	// validation's.
	if s.Placement.Pool != "" {
		t.Fatalf("placement.pool filled with %q", s.Placement.Pool)
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

func TestSpotResumePolicy(t *testing.T) {
	for _, policy := range []string{"", ResumeNever, ResumeAuto, ResumeRestart, ResumeManual, "Never", "always"} {
		t.Run(policy, func(t *testing.T) {
			s := RunSpec{Spot: true, Image: Image{Ref: "alpine"}, Workload: Workload{Command: []string{"true"}}, ResumePolicy: policy}
			err := s.Normalize(BuiltinDefaults)
			if policy != "" && policy != ResumeNever {
				want := fmt.Sprintf("resumePolicy: spot Runs require never (or omit resumePolicy), got %q", policy)
				var ve *ValidationError
				if !errors.As(err, &ve) || !slices.Equal(ve.Problems, []string{want}) {
					t.Fatalf("error = %v, want %q", err, want)
				}
				return
			}
			if err != nil || s.ResumePolicy != ResumeNever {
				t.Fatalf("normalized policy = %q, error %v", s.ResumePolicy, err)
			}
			stored, _, _ := s.SplitSecrets()
			if !stored.Spot || stored.ResumePolicy != ResumeNever {
				t.Fatalf("stored spec = %+v", stored)
			}
			if err := stored.Normalize(BuiltinDefaults); err != nil {
				t.Fatalf("renormalize: %v", err)
			}
		})
	}
}

func TestResumePolicy(t *testing.T) {
	for policy, ok := range map[string]bool{"": true, "auto": true, "restart": true, "manual": true, "never": true, "Never": false, "always": false} {
		s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}}, ResumePolicy: policy}
		err := s.Normalize(BuiltinDefaults)
		if (err == nil) != ok {
			t.Errorf("%q: %v", policy, err)
		}
		if !ok {
			want := fmt.Sprintf("resumePolicy: must be auto, restart, manual or never, got %q", policy)
			var ve *ValidationError
			if !errors.As(err, &ve) || !slices.Equal(ve.Problems, []string{want}) {
				t.Errorf("%q: error %v, want only %q", policy, err, want)
			}
		}
		if ok && s.ResumePolicy != policy {
			t.Errorf("%q stored as %q", policy, s.ResumePolicy)
		}
	}
	var s RunSpec
	if err := yaml.Unmarshal([]byte("resumePolicy: never\n"), &s); err != nil || s.ResumePolicy != ResumeNever {
		t.Fatalf("yaml: %q %v", s.ResumePolicy, err)
	}
	if b, _ := json.Marshal(RunSpec{}); strings.Contains(string(b), "resumePolicy") {
		t.Fatalf("unset resumePolicy marshalled: %s", b)
	}
}

// Which policies fail a moved Run and which refuse a requested resume;
// unset is auto.
func TestResumePolicySets(t *testing.T) {
	for _, c := range []struct {
		policy                     string
		failsOnMove, refusesResume bool
	}{
		{"", false, false},
		{ResumeAuto, false, false},
		{ResumeRestart, false, false},
		{ResumeManual, true, false},
		{ResumeNever, true, true},
	} {
		if got := FailsOnMove(c.policy); got != c.failsOnMove {
			t.Errorf("FailsOnMove(%q) = %v", c.policy, got)
		}
		if got := RefusesResume(c.policy); got != c.refusesResume {
			t.Errorf("RefusesResume(%q) = %v", c.policy, got)
		}
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
		{"wildcard rule", base(Network{Egress: []EgressRule{{Host: "*.example.com"}}}, MCPServer{Name: "a", URL: "https://a.b.example.com/mcp"}), ""},
		{"wildcard excludes the apex", base(Network{Egress: []EgressRule{{Host: "*.example.com"}}}, MCPServer{Name: "a", URL: "https://example.com/mcp"}), "add an egress rule"},
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

func TestEgressWildcardValidation(t *testing.T) {
	const shape = `network.egress[1]: a wildcard is "*." then a domain of at least two labels, e.g. *.example.com`
	const portPath = "network.egress[1]: a wildcard is a domain only, without a port or path"
	for _, c := range []struct {
		host, problem string // "" for valid
	}{
		{"*.example.com", ""},
		{"*.a.b.example.com", ""},
		{"*.Example.COM", ""},
		{"*.example.com.", ""},
		{"*.com", shape},
		{"*.", shape},
		{"*", shape},
		{"a.*.com", shape},
		{"*foo.com", shape},
		{"**.x.com", shape},
		{"*.*.x.com", shape},
		{"x.example.*", shape},
		{"*.exa_mple.com", shape},
		{"*.0.5", shape},
		{"*.example.com:443", portPath},
		{"*.example.com/path", portPath},
	} {
		s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
			Network: Network{Egress: []EgressRule{{Host: "api.example.com"}, {Host: c.host}}}}
		err := s.Normalize(BuiltinDefaults)
		var ve *ValidationError
		switch {
		case c.problem == "" && err != nil:
			t.Errorf("%q: %v", c.host, err)
		case c.problem != "" && (!errors.As(err, &ve) || !slices.Equal(ve.Problems, []string{c.problem})):
			t.Errorf("%q: want %q, got %v", c.host, c.problem, err)
		}
	}
}

func TestEgressRuleMatches(t *testing.T) {
	for _, c := range []struct {
		rule, name string
		want       bool
	}{
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "a.example.org", false},
		{"*.example.com", "evilexample.com", false},
		{"*.example.com", "a.evilexample.com", false},
		{"*.example.com", ".example.com", false},
		{"*.example.com", "A.Example.COM", true},
		{"*.example.com", "a.example.com.", true},
		{"*.Example.com.", "a.example.com", true},
		{"*.example.com", "", false},
		{"*.com", "a.com", false},
		{"api.example.com", "API.example.com.", true},
		{"api.example.com", "x.api.example.com", false},
		{"*.1.1", "1.1.1.1", false},
		{"*.example.com", "a..example.com", false},
		{"*.example.com", "_x.example.com", false},
		{"*.example.com", "-a.example.com", false},
		{"*.example.com", "example.com.evil.com", false},
		{"*.example.com", "a.example.com.evil.com", false},
		{"*.example.com", "bücher.example.com", false},
		{"*.example.com", "xn--bcher-kva.example.com", true},
		{"", "", false},
	} {
		if got := (EgressRule{Host: c.rule}).Matches(c.name); got != c.want {
			t.Errorf("%q matches %q: got %v, want %v", c.rule, c.name, got, c.want)
		}
		// The runner's path for a wildcard agrees with Matches.
		if strings.HasPrefix(c.rule, "*.") {
			s, ok := WildcardSuffix(c.rule)
			if got := ok && SuffixMatches(s, NormalHost(c.name)); got != c.want {
				t.Errorf("%q suffix-matches %q: got %v, want %v", c.rule, c.name, got, c.want)
			}
		}
	}
	// validWildcard already rejects a numeric last label; the suffix
	// match refuses an IP literal on its own too.
	if SuffixMatches(".1.1", "1.1.1.1") {
		t.Error(`".1.1" matches 1.1.1.1`)
	}
}

func TestServiceCoveredByWildcard(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"},
		Services: []Service{{Name: "api", URL: "https://api.svc.example.com/v1"}}},
		Network: Network{Egress: []EgressRule{{Host: "*.svc.example.com"}}}}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	s.Network.Egress = []EgressRule{{Host: "*.other.example.com"}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), `does not allow service "api" at api.svc.example.com`) {
		t.Fatalf("got %v", err)
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
	for _, c := range [][2]string{{"web", "r-9"}, {"api", ""}, {"api", "r-1"}} {
		if dropped, _ := s.DropRepository(c[0], c[1]); dropped {
			t.Fatalf("dropped %s for %q: a repository another request (or the submit) added", c[0], c[1])
		}
	}
	if dropped, cred := s.DropRepository("web", "r-1"); !dropped || cred != "WEB_TOKEN" || len(s.Git.Repositories) != 1 {
		t.Fatalf("not dropped (credential %q): %+v", cred, s.Git.Repositories)
	}
	if err := s.Normalize(BuiltinDefaults); err != nil || slices.ContainsFunc(s.Secrets, func(sec Secret) bool { return sec.Name == "WEB_TOKEN" }) {
		t.Fatalf("its unused credential stays declared: %v %+v", err, s.Secrets)
	}
}

// A dropped repository's credential stays declared, runner-only, while
// anything else uses it: another repository, registry auth, a header.
func TestDropRepositoryKeepsACredentialStillUsed(t *testing.T) {
	base := func() RunSpec {
		s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
			Volumes: []Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}},
			Network: Network{Unrestricted: true}}
		if err := s.Normalize(BuiltinDefaults); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, c := range map[string]struct {
		setup func(*RunSpec)
		kept  bool
	}{
		"unused": {func(*RunSpec) {}, false},
		"another repository": {func(s *RunSpec) {
			s.Git = &Git{Repositories: []Repository{{Name: "a", URL: "https://x/a.git", Credential: "TOK"}}}
			s.Secrets = []Secret{{Name: "TOK"}}
		}, true},
		"registry auth": {func(s *RunSpec) {
			s.Image.RegistryAuth = []RegistryAuth{{Registry: "ghcr.io", Secret: "TOK"}}
			s.Secrets = []Secret{{Name: "TOK"}}
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := base()
			c.setup(&s)
			if err := s.Normalize(BuiltinDefaults); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddRepositories([]Repository{{Name: "web", URL: "https://x/w.git", Credential: "TOK"}}, "r-1", BuiltinDefaults); err != nil {
				t.Fatal(err)
			}
			dropped, cred := s.DropRepository("web", "r-1")
			if !dropped {
				t.Fatal("not dropped")
			}
			if err := s.Normalize(BuiltinDefaults); err != nil {
				t.Fatal(err)
			}
			if c.kept != (cred == "") || c.kept != s.runnerOnly("TOK") {
				t.Fatalf("dropped credential %q, secrets %+v; want kept %v", cred, s.Secrets, c.kept)
			}
		})
	}
	// A header secret is never a git credential (Normalize refuses both),
	// so the header case is the same name used by a service once dropped.
	s := base()
	if _, err := s.AddRepositories([]Repository{{Name: "web", URL: "https://x/w.git", Credential: "TOK"}}, "r-1", BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	s.Workload.Services = []Service{{Name: "api", URL: "https://api.example.com/", Headers: []MCPHeader{{Name: "Authorization", Secret: "TOK"}}}}
	if _, cred := s.DropRepository("web", "r-1"); cred != "" || !slices.ContainsFunc(s.Secrets, func(sec Secret) bool { return sec.Name == "TOK" }) {
		t.Fatalf("dropped %q though a service header uses it: %+v", cred, s.Secrets)
	}
}

// A secret the container sees is never dropped with a repository, even one
// that names it as its credential. AddRepositories refuses that spec, so it
// is built by hand.
func TestDropRepositoryKeepsAnEnvSecret(t *testing.T) {
	s := RunSpec{
		Git:     &Git{Repositories: []Repository{{Name: "web", URL: "https://x/w.git", Credential: "TOK", AddedBy: "r-1"}}},
		Secrets: []Secret{{Name: "TOK", As: "env"}},
	}
	dropped, cred := s.DropRepository("web", "r-1")
	if !dropped || cred != "" {
		t.Fatalf("dropped %v, credential %q; want the repository only", dropped, cred)
	}
	if len(s.Secrets) != 1 || s.Secrets[0].Name != "TOK" || s.Secrets[0].As != "env" {
		t.Errorf("secrets %+v, want TOK as env kept", s.Secrets)
	}
	if len(s.Git.DroppedCredentials) != 0 {
		t.Errorf("droppedCredentials %v, want none", s.Git.DroppedCredentials)
	}
}

// A value re-supplied under a dropped credential's name is never declared:
// not as env by default, refused as env or file; a repository added again
// with it makes it runner-only again.
func TestDroppedCredentialNeverEntersTheContainer(t *testing.T) {
	dropped := func() RunSpec {
		s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"}},
			Volumes: []Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}}}
		if err := s.Normalize(BuiltinDefaults); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddRepositories([]Repository{{Name: "web", URL: "https://x/w.git", Credential: "TOK"}}, "r-1", BuiltinDefaults); err != nil {
			t.Fatal(err)
		}
		if _, cred := s.DropRepository("web", "r-1"); cred != "TOK" {
			t.Fatalf("dropped %q", cred)
		}
		return s
	}
	for _, as := range []string{"", "none"} {
		s := dropped()
		declared, err := s.ResumeSecrets([]Secret{{Name: "TOK", Value: "v", As: as}}, nil, nil, BuiltinDefaults)
		if err != nil || len(declared) != 0 || len(s.Secrets) != 0 {
			t.Fatalf("as %q: declared %v %+v (%v)", as, declared, s.Secrets, err)
		}
	}
	for _, sec := range []Secret{{Name: "TOK", Value: "v", As: "env"}, {Name: "TOK", Value: "v", As: "file", Path: "/t"}} {
		s := dropped()
		if _, err := s.ResumeSecrets([]Secret{sec}, nil, nil, BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "was a git credential") {
			t.Fatalf("as %s: %v", sec.As, err)
		}
	}
	s := dropped()
	repos := []Repository{{Name: "web", URL: "https://x/w.git", Credential: "TOK"}}
	added, err := s.AddRepositories(repos, "r-2", BuiltinDefaults)
	if err != nil || !reflect.DeepEqual(added, []string{"TOK"}) {
		t.Fatalf("re-add: %v %v", added, err)
	}
	if _, err := s.ResumeSecrets([]Secret{{Name: "TOK", Value: "v"}}, nil, repos, BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	if !s.runnerOnly("TOK") || s.Secrets[0].As != "none" {
		t.Fatalf("re-added credential %+v, want runner-only", s.Secrets)
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

// ResumeSecrets declares new names (as at submit, without their values)
// and removes others; a refused change leaves the spec as it was.
func TestResumeSecrets(t *testing.T) {
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
	declared, err := s.ResumeSecrets([]Secret{
		{Name: "ANTHROPIC_API_KEY", Value: "sk-2"}, // the Run has it: a rotation
		{Name: "EXTRA", Value: "x-1"},
		{Name: "CONF", Value: "c-1", As: "file", Path: "/home/agent/.conf"},
	}, nil, nil, BuiltinDefaults)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(declared, []string{"EXTRA", "CONF"}) {
		t.Fatalf("declared %v", declared)
	}
	want := []Secret{{Name: "ANTHROPIC_API_KEY", As: "env"}, {Name: "GITHUB_TOKEN", As: "none", RunnerOnly: true},
		{Name: "EXTRA", As: "env"}, {Name: "CONF", As: "file", Path: "/home/agent/.conf"}}
	if !reflect.DeepEqual(s.Secrets, want) {
		t.Fatalf("secrets %+v", s.Secrets)
	}
	if _, err := s.ResumeSecrets(nil, []string{"EXTRA", "ANTHROPIC_API_KEY"}, nil, BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	want = []Secret{want[1], want[3]}
	if !reflect.DeepEqual(s.Secrets, want) {
		t.Fatalf("after removal %+v", s.Secrets)
	}

	for name, c := range map[string]struct {
		supplied []Secret
		remove   []string
		repos    []Repository
		problem  string
	}{
		"invalid name":         {supplied: []Secret{{Name: "1X", Value: "v"}}, problem: `invalid name "1X"`},
		"reserved":             {supplied: []Secret{{Name: "lux-x", Value: "v", As: "none"}}, problem: "prefix is reserved"},
		"not an env name":      {supplied: []Secret{{Name: "a.b", Value: "v"}}, problem: "not a valid environment variable name"},
		"file without path":    {supplied: []Secret{{Name: "F", Value: "v", As: "file"}}, problem: "absolute path"},
		"bad as":               {supplied: []Secret{{Name: "F", Value: "v", As: "disk"}}, problem: "as must be env, file or none"},
		"duplicate":            {supplied: []Secret{{Name: "N", Value: "v"}, {Name: "N", Value: "w"}}, problem: `duplicate "N"`},
		"remove unknown":       {remove: []string{"NOPE"}, problem: "no such secret"},
		"remove git cred":      {remove: []string{"GITHUB_TOKEN"}, problem: "is a git credential"},
		"remove and supply":    {supplied: []Secret{{Name: "ANTHROPIC_API_KEY", Value: "v"}}, remove: []string{"ANTHROPIC_API_KEY"}, problem: "also in secrets"},
		"remove added's cred":  {remove: []string{"ANTHROPIC_API_KEY"}, repos: []Repository{{Name: "w", Credential: "ANTHROPIC_API_KEY"}}, problem: "repository this resume adds"},
		"remove twice":         {remove: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"}, problem: `removeSecrets: duplicate`},
		"declare and validate": {supplied: []Secret{{Name: "OK", Value: "v"}, {Name: "1X", Value: "v"}}, problem: `invalid name "1X"`},
	} {
		s := stored()
		before := stored()
		if _, err := s.ResumeSecrets(c.supplied, c.remove, c.repos, BuiltinDefaults); err == nil || !strings.Contains(err.Error(), c.problem) {
			t.Errorf("%s: want %q, got %v", name, c.problem, err)
		}
		if !reflect.DeepEqual(s, before) {
			t.Errorf("%s: a refused change changed the spec: %+v", name, s.Secrets)
		}
	}
}

// A direct MCP server's header secret cannot be removed; a declared as: none
// secret, whose name need not be an env name, can be later.
func TestResumeSecretsMCPHeaderAndNone(t *testing.T) {
	s := RunSpec{Image: Image{Ref: "x"}, Workload: Workload{Command: []string{"true"},
		MCPServers: []MCPServer{{Name: "a", URL: "https://m.example/", Headers: []MCPHeader{{Name: "Authorization", Secret: "MCP"}}}}},
		Secrets: []Secret{{Name: "MCP", Value: "v"}}, Network: Network{Unrestricted: true},
		Volumes: []Volume{{Name: "workspace", Path: "/workspace"}}}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	s, _, _ = s.SplitSecrets()
	before := s
	before.Secrets = slices.Clone(s.Secrets)
	if _, err := s.ResumeSecrets(nil, []string{"MCP"}, nil, BuiltinDefaults); err == nil || !strings.Contains(err.Error(), `"MCP" values an MCP server's or service's header`) {
		t.Fatalf("remove a direct MCP header's secret: %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatalf("a refused removal changed the spec: %+v", s.Secrets)
	}

	declared, err := s.ResumeSecrets([]Secret{{Name: "MCP", Value: "v"}, {Name: "a.b", Value: "n-1", As: "none"}}, nil, nil, BuiltinDefaults)
	if err != nil || !reflect.DeepEqual(declared, []string{"a.b"}) {
		t.Fatalf("declare a.b as none: %v %v", declared, err)
	}
	if i := slices.IndexFunc(s.Secrets, func(sec Secret) bool { return sec.Name == "a.b" }); i < 0 || s.Secrets[i].As != "none" || s.Secrets[i].Value != "" {
		t.Fatalf("a.b stored as %+v", s.Secrets)
	}
	if _, err := s.ResumeSecrets([]Secret{{Name: "MCP", Value: "v"}}, []string{"a.b"}, nil, BuiltinDefaults); err != nil {
		t.Fatalf("remove a.b: %v", err)
	}
	if !reflect.DeepEqual(s.Secrets, before.Secrets) {
		t.Fatalf("after removing a.b %+v, want %+v", s.Secrets, before.Secrets)
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
	if got := s.Workload.BeforeStop.Timeout.Duration; got != 2500*time.Millisecond {
		t.Fatalf("default timeout %s, want half the grace (2.5s): the workload keeps the rest", got)
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
	s = spec("-5s", "")
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "workload.grace") {
		t.Fatalf("a negative grace: %v", err)
	}
}

// resources.diskGrace: absent takes the default and is stored set, so an
// explicit 0 is told from absent and survives another default; negative or
// over MaxDiskGrace is refused; JSON and YAML both say "unset" by leaving
// it out.
func TestDiskGrace(t *testing.T) {
	d := BuiltinDefaults
	d.DiskGrace = 5 * time.Minute
	base := func(res string) RunSpec {
		var s RunSpec
		doc := `{"image":{"ref":"x"},"workload":{"command":["true"]},"resources":` + res + `}`
		if err := json.Unmarshal([]byte(doc), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, c := range []struct {
		name, res string
		defaults  Defaults
		want      time.Duration
		problem   string
	}{
		{"absent takes the default", `{}`, d, 5 * time.Minute, ""},
		{"null takes the default", `{"diskGrace":null}`, d, 5 * time.Minute, ""},
		{"absent with the builtin default", `{}`, BuiltinDefaults, 0, ""},
		{"explicit 0 over a non-zero default", `{"diskGrace":"0s"}`, d, 0, ""},
		{"explicit 0 as a number", `{"diskGrace":0}`, d, 0, ""},
		{"explicit value", `{"diskGrace":"90s"}`, BuiltinDefaults, 90 * time.Second, ""},
		{"seconds", `{"diskGrace":120}`, BuiltinDefaults, 2 * time.Minute, ""},
		{"the maximum", `{"diskGrace":"1h"}`, BuiltinDefaults, time.Hour, ""},
		{"negative", `{"diskGrace":"-1s"}`, d, 0, "resources.diskGrace must not be negative"},
		{"too large", `{"diskGrace":"61m"}`, d, 0, "resources.diskGrace must be at most 1h0m0s, got 1h1m0s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := base(c.res)
			err := s.Normalize(c.defaults)
			if c.problem != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || !slices.Equal(ve.Problems, []string{c.problem}) {
					t.Fatalf("%v, want only %q", err, c.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			g := s.Resources.DiskGrace
			if !g.Set || g.Duration.Duration != c.want {
				t.Fatalf("diskGrace %+v, want set to %s", g, c.want)
			}
			// Stored and normalized again under another default: kept.
			b, _ := json.Marshal(s)
			var again RunSpec
			if err := json.Unmarshal(b, &again); err != nil {
				t.Fatal(err)
			}
			if err := again.Normalize(Defaults{CPUs: 1, Memory: 1, Disk: 1, Pids: 1, DiskGrace: 7 * time.Minute}); err != nil {
				t.Fatal(err)
			}
			if again.Resources.DiskGrace != g {
				t.Errorf("after a round trip under another default: %+v, want %+v", again.Resources.DiskGrace, g)
			}
		})
	}

	var unset Resources
	b, _ := json.Marshal(unset)
	if strings.Contains(string(b), "diskGrace") {
		t.Errorf("unset diskGrace marshalled: %s", b)
	}
	var y RunSpec
	if err := yaml.Unmarshal([]byte("image: {ref: x}\nworkload: {command: [\"true\"]}\nresources: {diskGrace: 0s}\n"), &y); err != nil {
		t.Fatal(err)
	}
	if err := y.Normalize(d); err != nil || !y.Resources.DiskGrace.Set || y.Resources.DiskGrace.Duration.Duration != 0 {
		t.Errorf("YAML explicit 0 over a 5m default: %+v %v", y.Resources.DiskGrace, err)
	}
}
