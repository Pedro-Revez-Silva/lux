package spec

import (
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func contextSpec() RunSpec {
	return RunSpec{
		Image: Image{Build: &Build{
			Containerfile: "FROM alpine@sha256:" + strings.Repeat("a", 64) + "\nCOPY --from=repo go.mod /seed/\nCOPY setup.sh /opt/\n",
			ContextFiles:  map[string]string{"setup.sh": "echo hi\n", "conf/app.ini": "x=1\n"},
			Contexts:      map[string]BuildContext{"repo": {Git: &GitContext{URL: "https://github.com/acme/web.git", Ref: sha, Credential: "GH"}}},
			Publish:       true,
		}},
		Workload: Workload{Command: []string{"true"}},
		Network:  Network{Egress: []EgressRule{{Host: "github.com"}}},
		Secrets:  []Secret{{Name: "GH", Value: "ghp_x"}, {Name: "OTHER", Value: "o"}},
	}
}

func TestBuildContextValid(t *testing.T) {
	s := contextSpec()
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	// A build context's credential is the runner's, as a git credential.
	if !s.Secrets[0].RunnerOnly || s.Secrets[0].As != "none" || s.Secrets[1].RunnerOnly {
		t.Fatalf("%+v", s.Secrets)
	}
	// And so never a header (it would enter the container).
	s = contextSpec()
	s.Workload.Services = []Service{{Name: "api", URL: "https://github.com/x", Headers: []MCPHeader{{Name: "Authorization", Secret: "GH"}}}}
	if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "git credential") {
		t.Fatalf("header with a context credential: %v", err)
	}
	// Unrestricted network needs no egress rule.
	s = contextSpec()
	s.Network = Network{Unrestricted: true}
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
}

// Paths that could escape the context, or that aren't plain relative
// paths, are refused; so are oversized or non-text contexts.
func TestContextFilesRefused(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/./b", ".", "..", "a//b", "a/", "/", "a\\b", "a b",
		"x/" + strings.Repeat("a", 260), strings.Repeat("a/", 17) + "b", "a\x00b", "é"} {
		s := contextSpec()
		s.Image.Build.ContextFiles = map[string]string{p: "x"}
		if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "contextFiles") {
			t.Errorf("%q: %v", p, err)
		}
	}
	for name, files := range map[string]map[string]string{
		"not text":       {"a": "\xff\xfe"},
		"nul":            {"a": "a\x00"},
		"file and dir":   {"a": "x", "a/b": "y"},
		"too large":      {"a": strings.Repeat("x", MaxContextBytes)},
		"too many files": manyFiles(MaxContextFiles + 1),
	} {
		s := contextSpec()
		s.Image.Build.ContextFiles = files
		if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), "contextFiles") {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := contextSpec()
	s.Image.Build.ContextFiles = manyFiles(MaxContextFiles)
	if err := s.Normalize(BuiltinDefaults); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
}

func manyFiles(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m["f"+strings.Repeat("x", i)] = "x"
	}
	return m
}

// A git context is an https URL the Run may reach, at a full commit.
func TestGitContextRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*RunSpec)
		want string
	}{
		{"branch", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.Ref = "main" }, "git.ref"},
		{"short sha", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.Ref = sha[:12] }, "git.ref"},
		{"upper sha", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.Ref = strings.ToUpper(sha) }, "git.ref"},
		{"option-like", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.Ref = "--upload-pack=x" }, "git.ref"},
		{"file url", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "file:///etc" }, "need an https"},
		{"local path", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "/srv/repo.git" }, "need an https"},
		{"ssh", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "ssh://git@github.com/a/b" }, "need an https"},
		{"ext", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "ext::sh -c touch% /tmp/pwned" }, "need an https"},
		{"git", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "git://github.com/a/b" }, "need an https"},
		{"userinfo", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "https://x:tok@github.com/a/b" }, "credentials"},
		{"query", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "https://github.com/a/b?x=1" }, "no query"},
		{"metadata", func(s *RunSpec) {
			s.Image.Build.Contexts["repo"].Git.URL = "https://169.254.169.254/latest"
			s.Network.Egress = append(s.Network.Egress, EgressRule{CIDR: "169.254.0.0/16"})
		}, "not a host the runner fetches from"},
		{"localhost", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "https://localhost/a" }, "not a host"},
		{"loopback", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "https://127.0.0.1/a" }, "not a host"},
		{"ipv6", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.URL = "https://[::1]/a" }, "not a host"},
		{"egress", func(s *RunSpec) { s.Network.Egress = []EgressRule{{Host: "gitlab.com"}} }, "network.egress does not allow github.com"},
		{"credential", func(s *RunSpec) { s.Image.Build.Contexts["repo"].Git.Credential = "NOPE" }, `no secret named "NOPE"`},
		{"no source", func(s *RunSpec) { s.Image.Build.Contexts["repo"] = BuildContext{} }, "needs a source"},
		{"bad name", func(s *RunSpec) { s.Image.Build.Contexts["Repo"] = s.Image.Build.Contexts["repo"] }, "invalid name"},
		{"name with =", func(s *RunSpec) { s.Image.Build.Contexts["a=b"] = s.Image.Build.Contexts["repo"] }, "invalid name"},
		{"scratch", func(s *RunSpec) { s.Image.Build.Contexts["scratch"] = s.Image.Build.Contexts["repo"] }, "invalid name"},
		{"stage name", func(s *RunSpec) { s.Image.Build.Containerfile = "FROM alpine AS repo\nFROM repo\n" }, "a stage of the Containerfile has that name"},
		{"too many", func(s *RunSpec) {
			for _, n := range []string{"b", "c", "d", "e"} {
				s.Image.Build.Contexts[n] = s.Image.Build.Contexts["repo"]
			}
		}, "at most 4"},
		{"old context field", func(s *RunSpec) { s.Image.Build.Context = "art_x" }, "image.build.context is not supported"},
	} {
		s := contextSpec()
		tc.edit(&s)
		if err := s.Normalize(BuiltinDefaults); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestImageRepository(t *testing.T) {
	for ref, want := range map[string]string{
		"alpine":                          "docker.io/library/alpine",
		"alpine:3":                        "docker.io/library/alpine",
		"acme/app@sha256:abc":             "docker.io/acme/app",
		"ghcr.io/acme/lux/ten_a@sha256:a": "ghcr.io/acme/lux/ten_a",
		"10.0.0.5:5000/team/app:1":        "10.0.0.5:5000/team/app",
		"10.0.0.5:5000/team/app":          "10.0.0.5:5000/team/app",
		"localhost/lux-build:k":           "localhost/lux-build",
	} {
		if got := ImageRepository(ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
	m := ManagedImages{Root: "ghcr.io/acme/lux", Shared: []string{"ghcr.io/acme/base"}}
	for ref, want := range map[string][2]bool{ // under, pullable by ten_a
		"ghcr.io/acme/lux/ten_a@sha256:a":   {true, true},
		"ghcr.io/acme/lux/ten_a:k":          {true, true},
		"ghcr.io/acme/lux/ten_b@sha256:a":   {true, false},
		"ghcr.io/acme/lux/ten_a/x@sha256:a": {true, false},
		"ghcr.io/acme/lux:x":                {true, false},
		"ghcr.io/acme/base:1":               {true, true},
		"ghcr.io/acme/base/x:1":             {false, false},
		"ghcr.io/acme/luxe/ten_a:1":         {false, false},
		"alpine":                            {false, false},
	} {
		if got := [2]bool{m.Under(ref), m.Pullable(ref, "ten_a")}; got != want {
			t.Errorf("%s: %v, want %v", ref, got, want)
		}
	}
}
