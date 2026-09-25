// Package spec is the RunSpec: the immutable description of what a Run
// executes. It is validated and normalized on submission; the normalized form,
// minus secret values, is what luxd stores and what runners receive.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type RunSpec struct {
	Name      string            `json:"name,omitempty" yaml:"name,omitempty"`
	Labels    map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Image     Image             `json:"image" yaml:"image"`
	Workload  Workload          `json:"workload" yaml:"workload"`
	Init      *Init             `json:"init,omitempty" yaml:"init,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets   []Secret          `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Git       *Git              `json:"git,omitempty" yaml:"git,omitempty"`
	Volumes   []Volume          `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Resources Resources         `json:"resources" yaml:"resources" doc:"What the Run gets, and the scheduler reserves on its host. Unset fields take luxd's defaults: cpus 2, memory 8Gi, disk 20Gi, pids 1024, unless its operator changed them (LUX_DEFAULT_CPUS, LUX_DEFAULT_MEMORY, LUX_DEFAULT_DISK, LUX_DEFAULT_PIDS). disk bounds the writable layer plus state volumes: a Run over it is stopped and fails."`
	Timeout   Duration          `json:"timeout,omitempty" yaml:"timeout,omitempty" doc:"The most time the Run may spend running, summed over its placements (time stopped, lost, or waiting for a host does not count). Past it, the Run is stopped and fails. Unset: no limit."`
	Placement Placement         `json:"placement" yaml:"placement"`
	Network   Network           `json:"network" yaml:"network"`
	Sandbox   Sandbox           `json:"sandbox" yaml:"sandbox"`
	Artifacts Artifacts         `json:"artifacts" yaml:"artifacts"`
}

// Image is exactly one of a pinned reference or a build.
type Image struct {
	Ref          string         `json:"ref,omitempty" yaml:"ref,omitempty"`
	Build        *Build         `json:"build,omitempty" yaml:"build,omitempty"`
	RegistryAuth []RegistryAuth `json:"registryAuth,omitempty" yaml:"registryAuth,omitempty" doc:"Credentials for private registries, used by the runner for every pull and push of the placement (image.ref, FROM bases, the build cache). Never in the container."`
}

// RegistryAuth logs the runner in to a registry. The secret stays the
// runner's: it never enters the container.
type RegistryAuth struct {
	Registry string `json:"registry" yaml:"registry" doc:"The registry's host, with an optional port: ghcr.io, 123.dkr.ecr.eu-west-1.amazonaws.com, 10.0.0.5:5000. No scheme or path."`
	Secret   string `json:"secret" yaml:"secret" doc:"The secret (in secrets) holding user:password, or a bare token (sent as the password, with the user lux)."`
}

type Build struct {
	Containerfile string `json:"containerfile" yaml:"containerfile"`
	// Context is an optional artifact id whose contents are the build context.
	Context string            `json:"context,omitempty" yaml:"context,omitempty"`
	Args    map[string]string `json:"args,omitempty" yaml:"args,omitempty"`
	Cache   string            `json:"cache,omitempty" yaml:"cache,omitempty" doc:"A registry repository (no tag), e.g. registry.example.com/team/lux-cache, shared by hosts: a host pulls <cache>:<build key> instead of building, and pushes what it builds there. The key covers the tenant, egress rules, pinned Containerfile and args."`
}

type Workload struct {
	// generic | acp | claude-code | codex | opencode
	Adapter string   `json:"adapter" yaml:"adapter"`
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
	Prompt  string   `json:"prompt,omitempty" yaml:"prompt,omitempty"`
	Workdir string   `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	// User the workload runs as; defaults to the image's user.
	User   string  `json:"user,omitempty" yaml:"user,omitempty"`
	TTY    bool    `json:"tty,omitempty" yaml:"tty,omitempty"`
	Resume *Resume `json:"resume,omitempty" yaml:"resume,omitempty"`
	// Grace is how long a graceful stop waits before SIGKILL.
	Grace      Duration    `json:"grace,omitempty" yaml:"grace,omitempty"`
	MCPServers []MCPServer `json:"mcpServers,omitempty" yaml:"mcpServers,omitempty" doc:"MCP servers (streamable HTTP) the agent connects to, through its adapter. Each URL's host must be allowed by network.egress (unless unrestricted), and may not be the control plane's."`
	Services   []Service   `json:"services,omitempty" yaml:"services,omitempty" doc:"HTTP services the workload calls through a local socket (/.lux/services/<name>.sock, named in LUX_SERVICE_<NAME>), which adds their headers: the workload never holds the credentials. Same URL rules as mcpServers."`
}

// Service is an HTTP service proxied into the container. Header values come
// only from secrets and stay in lux-shim's memory: the workload never has
// them.
type Service struct {
	Name    string      `json:"name" yaml:"name" doc:"Unique name: the socket is /.lux/services/<name>.sock. Lowercase letters, digits, - and _."`
	URL     string      `json:"url" yaml:"url" doc:"Where requests go: http or https, without credentials. A request's path is appended to this URL's."`
	Headers []MCPHeader `json:"headers,omitempty" yaml:"headers,omitempty" doc:"Headers set on every request (replacing the workload's own of the same name), each valued from a secret."`
	// Loopback also serves it on 127.0.0.1 in the Run (for clients that
	// take a URL, not a socket: an agent's MCP client).
	Loopback bool `json:"loopback,omitempty" yaml:"loopback,omitempty" doc:"Also serve it on http://127.0.0.1:<port> inside the Run (LUX_SERVICE_<NAME>_URL), for clients that need a URL; an mcpServers entry can then name it."`
}

// ServiceBasePort: a loopback service's port is this plus its index in
// workload.services, the same on every placement.
const ServiceBasePort = 41000

// ServicePort is the loopback port of the named service, or 0.
func (s *RunSpec) ServicePort(name string) int {
	for i, v := range s.Workload.Services {
		if v.Name == name && v.Loopback {
			return ServiceBasePort + i
		}
	}
	return 0
}

// MCPServer is a remote MCP server the agent is given. Header values come
// only from secrets, so none is ever stored in the spec.
type MCPServer struct {
	Name    string      `json:"name" yaml:"name" doc:"Unique name; the agent sees its tools under it. Lowercase letters, digits, - and _."`
	URL     string      `json:"url,omitempty" yaml:"url,omitempty" doc:"The server's streamable HTTP endpoint: http or https, without credentials. Or service instead."`
	Headers []MCPHeader `json:"headers,omitempty" yaml:"headers,omitempty" doc:"Headers sent on every request, each valued from a secret (the agent then holds them; prefer service)."`
	Service string      `json:"service,omitempty" yaml:"service,omitempty" doc:"A workload.services entry with loopback: the agent is given its loopback address, and the service adds the headers, so the agent never holds them. Instead of url and headers."`
	Path    string      `json:"path,omitempty" yaml:"path,omitempty" doc:"With service: the MCP endpoint's path, appended to the service's URL (default: none, the URL itself)."`
}

type MCPHeader struct {
	Name   string `json:"name" yaml:"name" doc:"The HTTP header's name, e.g. Authorization."`
	Secret string `json:"secret" yaml:"secret" doc:"The secret (in secrets) whose value is the header's whole value, e.g. \"Bearer …\"."`
}

type Resume struct {
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`
}

type Init struct {
	Script string `json:"script" yaml:"script"`
}

type Secret struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
	As    string `json:"as,omitempty" yaml:"as,omitempty"` // env | file | none
	Path  string `json:"path,omitempty" yaml:"path,omitempty"`
	// Git marks a secret used only by the runner (a git credential): it is
	// never placed in the container.
	RunnerOnly bool `json:"runnerOnly,omitempty" yaml:"-"`
}

type Git struct {
	Repositories []Repository `json:"repositories" yaml:"repositories"`
	Push         *Push        `json:"push,omitempty" yaml:"push,omitempty"`
}

type Repository struct {
	Name       string `json:"name" yaml:"name"`
	URL        string `json:"url" yaml:"url"`
	Ref        string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Credential string `json:"credential,omitempty" yaml:"credential,omitempty"`
	// Path inside the container; defaults to <workspace volume>/repos/<name>.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// Push: false keeps a repository out of pushes (one cloned for context;
	// its credential may be read-only). Default true.
	Push *bool `json:"push,omitempty" yaml:"push,omitempty"`
	// AddedBy: added to a Run on resume, not submitted with it. A clone
	// that fails drops it rather than failing the placement.
	AddedBy string `json:"addedBy,omitempty" yaml:"-" doc:"Set by luxd: the resume request that added it."`
}

// Pushed reports whether pushes include the repository.
func (r Repository) Pushed() bool { return r.Push == nil || *r.Push }

type Push struct {
	Branch string `json:"branch" yaml:"branch"`
}

type Volume struct {
	Name string `json:"name" yaml:"name"`
	Path string `json:"path" yaml:"path"`
	Kind string `json:"kind" yaml:"kind"` // state | ephemeral
}

type Resources struct {
	CPUs   float64 `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	Memory Bytes   `json:"memory,omitempty" yaml:"memory,omitempty"`
	Disk   Bytes   `json:"disk,omitempty" yaml:"disk,omitempty"`
	Pids   int64   `json:"pids,omitempty" yaml:"pids,omitempty"`
}

type Placement struct {
	Pool     string            `json:"pool,omitempty" yaml:"pool,omitempty"`
	Requires map[string]string `json:"requires,omitempty" yaml:"requires,omitempty"`
	Prefers  map[string]string `json:"prefers,omitempty" yaml:"prefers,omitempty"`
}

type Network struct {
	Egress []EgressRule `json:"egress,omitempty" yaml:"egress,omitempty"`
	Ports  []Port       `json:"ports,omitempty" yaml:"ports,omitempty"`
	// Unrestricted switches egress filtering off. Default deny otherwise.
	Unrestricted bool `json:"unrestricted,omitempty" yaml:"unrestricted,omitempty"`
}

type EgressRule struct {
	Host string `json:"host,omitempty" yaml:"host,omitempty"`
	CIDR string `json:"cidr,omitempty" yaml:"cidr,omitempty"`
}

type Port struct {
	Port int    `json:"port" yaml:"port"`
	Name string `json:"name" yaml:"name"`
}

type Sandbox struct {
	NestedContainers bool `json:"nestedContainers,omitempty" yaml:"nestedContainers,omitempty"`
	ReadOnlyRoot     bool `json:"readOnlyRoot,omitempty" yaml:"readOnlyRoot,omitempty"`
}

type Artifacts struct {
	Paths []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

// Adapters lux knows. Each is a way to start, resume, steer and stop a
// workload; `generic` is a plain process.
var Adapters = map[string]AdapterInfo{
	"generic": {},
	"acp":     {},
	"claude-code": {
		DefaultCommand: []string{"claude"},
		StatePaths:     []string{"$HOME/.claude"},
	},
	"codex": {
		DefaultCommand: []string{"codex"},
		StatePaths:     []string{"$HOME/.codex"},
	},
	"opencode": {
		DefaultCommand: []string{"opencode", "acp"},
		StatePaths:     []string{"$HOME/.local/share/opencode"},
	},
}

type AdapterInfo struct {
	DefaultCommand []string
	// Paths the agent keeps its session in. A spec whose state volumes do not
	// cover them is rejected: it would break on the first resume.
	StatePaths []string
}

// Duration marshals as a Go duration string ("4h", "90s").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	return d.parse(s)
}
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return d.parse(s)
}
func (d *Duration) parse(s string) error {
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = v
	return nil
}

// Bytes accepts "8Gi", "512Mi", "1G", or a plain number of bytes.
type Bytes int64

func (b Bytes) MarshalJSON() ([]byte, error) { return json.Marshal(int64(b)) }
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = Bytes(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	return b.parse(s)
}
func (b *Bytes) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return b.parse(s)
}

var bytesRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?)?B?$`)

func (b *Bytes) parse(s string) error {
	m := bytesRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return fmt.Errorf("invalid size %q", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{
		"": 1, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
	}[m[2]]
	*b = Bytes(f * mult)
	return nil
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	volumeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// DefaultGrace is a graceful stop's wait before SIGKILL, when unset.
const DefaultGrace = 30 * time.Second

// Defaults are the resources a Run gets when its spec leaves them unset.
// An operator sets them per luxd.
type Defaults struct {
	CPUs   float64
	Memory Bytes
	Disk   Bytes
	Pids   int
}

var BuiltinDefaults = Defaults{CPUs: 2, Memory: 8 << 30, Disk: 20 << 30, Pids: 1024}

// Normalize validates the spec and fills defaults. It returns every problem
// at once so a caller can fix a spec in one round trip.
func (s *RunSpec) Normalize(d Defaults) error {
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if (s.Image.Ref == "") == (s.Image.Build == nil) {
		fail("image: exactly one of ref or build is required")
	}
	if b := s.Image.Build; b != nil {
		if strings.TrimSpace(b.Containerfile) == "" {
			fail("image.build.containerfile is required")
		}
		if b.Context != "" {
			fail("image.build.context is not supported yet: COPY and ADD have no build context")
		}
		if b.Cache != "" && !validCacheRepo(b.Cache) {
			fail("image.build.cache: need a repository with its registry and no tag, e.g. registry.example.com/team/lux-cache")
		}
	}

	w := &s.Workload
	if w.Adapter == "" {
		w.Adapter = "generic"
	}
	info, ok := Adapters[w.Adapter]
	if !ok {
		fail("workload.adapter: unknown adapter %q", w.Adapter)
	}
	if len(w.Command) == 0 {
		w.Command = info.DefaultCommand
	}
	if len(w.Command) == 0 {
		fail("workload.command is required for adapter %q", w.Adapter)
	}
	if w.Workdir != "" && !path.IsAbs(w.Workdir) {
		fail("workload.workdir must be absolute")
	}
	if w.TTY && w.Adapter != "generic" {
		fail("workload.tty is only for generic workloads: agent adapters own their stdio")
	}
	if w.Grace.Duration == 0 {
		w.Grace.Duration = DefaultGrace
	}

	for k := range s.Env {
		if !envRe.MatchString(k) {
			fail("env: invalid name %q", k)
		}
		if strings.HasPrefix(k, "LUX_") {
			fail("env: %q: the LUX_ prefix is reserved", k)
		}
	}

	seen := map[string]bool{}
	for i := range s.Secrets {
		sec := &s.Secrets[i]
		if !nameRe.MatchString(sec.Name) {
			fail("secrets[%d]: invalid name %q", i, sec.Name)
		}
		if seen[sec.Name] {
			fail("secrets: duplicate %q", sec.Name)
		}
		if strings.HasPrefix(sec.Name, ReservedSecretPrefix) {
			fail("secrets[%d]: %q: the %s prefix is reserved", i, sec.Name, ReservedSecretPrefix)
		}
		seen[sec.Name] = true
		if sec.As == "" {
			// Used only as git credentials or MCP headers: nowhere else.
			sec.As = "env"
			if s.onlyCredential(sec.Name) {
				sec.As = "none"
			}
		}
		switch sec.As {
		case "none":
		case "env":
			if !envRe.MatchString(sec.Name) {
				fail("secrets[%d]: %q is not a valid environment variable name", i, sec.Name)
			}
		case "file":
			if !path.IsAbs(sec.Path) {
				fail("secrets[%d]: file secrets need an absolute path", i)
			}
		default:
			fail("secrets[%d].as must be env, file or none", i)
		}
	}

	regs := map[string]bool{}
	for i, a := range s.Image.RegistryAuth {
		if !validRegistry(a.Registry) {
			fail("image.registryAuth[%d].registry: need a host with an optional port (ghcr.io, 10.0.0.5:5000), no scheme or path", i)
		}
		if regs[strings.ToLower(a.Registry)] {
			fail("image.registryAuth: duplicate registry %q", a.Registry)
		}
		regs[strings.ToLower(a.Registry)] = true
		if !seen[a.Secret] {
			fail("image.registryAuth[%d].secret: no secret named %q", i, a.Secret)
		}
	}

	vols := map[string]bool{}
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if !volumeRe.MatchString(v.Name) {
			fail("volumes[%d]: invalid name %q (lowercase, digits, - and _)", i, v.Name)
		}
		if vols[v.Name] {
			fail("volumes: duplicate %q", v.Name)
		}
		vols[v.Name] = true
		v.Path = path.Clean(v.Path)
		if !path.IsAbs(v.Path) || v.Path == "/" {
			fail("volumes[%d]: path must be absolute and not /", i)
		}
		if strings.HasPrefix(v.Path, "/.lux") {
			fail("volumes[%d]: /.lux is reserved", i)
		}
		if v.Kind == "" {
			v.Kind = "state"
		}
		if v.Kind != "state" && v.Kind != "ephemeral" {
			fail("volumes[%d].kind must be state or ephemeral", i)
		}
	}

	if s.Git != nil {
		names := map[string]bool{}
		for i := range s.Git.Repositories {
			r := &s.Git.Repositories[i]
			if !volumeRe.MatchString(r.Name) || names[r.Name] {
				fail("git.repositories[%d]: invalid or duplicate name %q", i, r.Name)
			}
			names[r.Name] = true
			if r.URL == "" {
				fail("git.repositories[%d].url is required", i)
			} else if u, err := url.Parse(r.URL); err == nil && u.User != nil {
				// It would end up in the checkout's config, the snapshot and
				// logs: credentials go through `credential`, never the URL.
				fail("git.repositories[%d].url must not carry credentials: use credential: <secret name>", i)
			}
			if r.Ref == "" {
				r.Ref = "HEAD"
			}
			if r.Path == "" {
				r.Path = "/workspace/repos/" + r.Name
			}
			r.Path = path.Clean(r.Path)
			if v := s.volumeFor(r.Path); v == nil || v.Kind != "state" {
				fail("git.repositories[%d]: %s is not on a state volume, so the checkout would not survive a stop", i, r.Path)
			}
			if r.Credential != "" && !seen[r.Credential] {
				fail("git.repositories[%d].credential: no secret named %q", i, r.Credential)
			}
		}
		if s.Git.Push != nil && s.Git.Push.Branch == "" {
			fail("git.push.branch is required")
		}
	}
	s.normalizeMCP(seen, fail)
	// Git and registry credentials are the runner's, never the
	// container's. (The shim still gets every value, to redact, and to
	// value MCP headers from; no header may name one.)
	for i := range s.Secrets {
		if s.isGitCredential(s.Secrets[i].Name) || s.isRegistryCredential(s.Secrets[i].Name) {
			s.Secrets[i].RunnerOnly = true
		}
	}

	for _, p := range info.StatePaths {
		home := "/home/agent"
		if w.User == "root" {
			home = "/root"
		}
		want := strings.ReplaceAll(p, "$HOME", home)
		if s.stateVolumeFor(want) == nil {
			fail("volumes: adapter %q keeps its session in %s, which must be on a state volume", w.Adapter, want)
		}
	}

	r := &s.Resources
	if r.CPUs == 0 {
		r.CPUs = d.CPUs
	}
	if r.Memory == 0 {
		r.Memory = d.Memory
	}
	if r.Pids == 0 {
		r.Pids = int64(d.Pids)
	}
	if r.Disk == 0 {
		r.Disk = d.Disk
	}
	if r.CPUs < 0 || r.Memory < 0 || r.Disk < 0 || r.Pids < 0 {
		fail("resources must not be negative")
	}
	if s.Timeout.Duration < 0 {
		fail("timeout must not be negative")
	}
	if s.Placement.Pool == "" {
		s.Placement.Pool = "default"
	}

	for i, e := range s.Network.Egress {
		if (e.Host == "") == (e.CIDR == "") {
			fail("network.egress[%d]: exactly one of host or cidr", i)
		}
		if strings.Contains(e.Host, "*") {
			fail("network.egress[%d]: wildcards cannot be resolved; list concrete hostnames", i)
		}
	}
	ports := map[string]bool{}
	for i, p := range s.Network.Ports {
		if p.Port < 1 || p.Port > 65535 || p.Name == "" || ports[p.Name] {
			fail("network.ports[%d]: need a port 1-65535 and a unique name", i)
		}
		ports[p.Name] = true
		if j := p.Port - ServiceBasePort; j >= 0 && j < len(s.Workload.Services) && s.Workload.Services[j].Loopback {
			fail("network.ports[%d]: %d is service %q's loopback port", i, p.Port, s.Workload.Services[j].Name)
		}
	}
	for i, p := range s.Artifacts.Paths {
		if !path.IsAbs(p) {
			fail("artifacts.paths[%d]: must be absolute", i)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// AddRepositories adds repositories to a stored (normalized) spec on
// resume, each marked as added by requestID, and declares any credential
// not yet among its secrets (without a value: it comes with the resume, and
// Normalize makes it runner-only, as: none). Returns the secrets it
// declared; the spec is normalized again, so a new name must be unique.
func (s *RunSpec) AddRepositories(repos []Repository, requestID string, d Defaults) ([]string, error) {
	if s.Git == nil {
		s.Git = &Git{}
	}
	// What the Run had before this resume: a secret it already exposes to
	// the workload can't become a credential (it would leave the container
	// mid-Run). One declared here, for several added repositories, can.
	existing := map[string]bool{}
	for _, sec := range s.Secrets {
		existing[sec.Name] = true
	}
	declared := map[string]bool{}
	var added, errs []string
	for i, r := range repos {
		r.AddedBy = requestID
		s.Git.Repositories = append(s.Git.Repositories, r)
		c := r.Credential
		switch {
		case c == "" || declared[c]:
		case !existing[c]:
			declared[c] = true
			s.Secrets = append(s.Secrets, Secret{Name: c})
			added = append(added, c)
		case !s.runnerOnly(c):
			// The container has it already: as a credential it would leave
			// the container mid-Run, and a credential is never in it.
			errs = append(errs, fmt.Sprintf("git.repositories[%d].credential: %q is a secret the workload sees: use a secret of its own", i, c))
		}
	}
	err := s.Normalize(d)
	if len(errs) > 0 {
		ve := &ValidationError{Problems: errs}
		if e, ok := err.(*ValidationError); ok {
			ve.Problems = append(ve.Problems, e.Problems...)
		}
		return nil, ve
	}
	return added, err
}

func (s *RunSpec) runnerOnly(name string) bool {
	i := slices.IndexFunc(s.Secrets, func(sec Secret) bool { return sec.Name == name })
	return i >= 0 && s.Secrets[i].RunnerOnly
}

// DropRepository removes a repository added by requestID (one whose clone
// failed); reports whether it was there. Its credential stays declared and
// runner-only: a value handed over as a git credential never enters the
// container later.
func (s *RunSpec) DropRepository(name, requestID string) bool {
	if s.Git == nil || requestID == "" {
		return false
	}
	n := len(s.Git.Repositories)
	s.Git.Repositories = slices.DeleteFunc(s.Git.Repositories, func(r Repository) bool { return r.Name == name && r.AddedBy == requestID })
	return len(s.Git.Repositories) < n
}

// headerRe is an HTTP header name: RFC 9110 token characters.
var headerRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (s *RunSpec) normalizeMCP(secrets map[string]bool, fail func(string, ...any)) {
	names := map[string]bool{}
	for i := range s.Workload.MCPServers {
		m := &s.Workload.MCPServers[i]
		at := fmt.Sprintf("workload.mcpServers[%d]", i)
		if m.Service != "" {
			// Its service's URL, egress and headers are checked as the
			// service's; the agent gets only the loopback address.
			if !volumeRe.MatchString(m.Name) {
				fail("%s: invalid name %q (lowercase, digits, - and _)", at, m.Name)
			}
			if m.URL != "" || len(m.Headers) > 0 {
				fail("%s: service replaces url and headers: give one or the other", at)
			}
			if s.ServicePort(m.Service) == 0 {
				fail("%s.service: no service %q with loopback: true", at, m.Service)
			}
			// No path: the service's URL as it is.
			if m.Path != "" && (!strings.HasPrefix(m.Path, "/") || strings.ContainsAny(m.Path, "?# ") || slices.Contains(strings.Split(m.Path, "/"), "..")) {
				fail("%s.path: need an absolute path, no query and no ..", at)
			}
		} else {
			if m.Path != "" {
				fail("%s.path: only with service", at)
			}
			s.normalizeEndpoint(at, "MCP server", m.Name, m.URL, m.Headers, secrets, fail)
		}
		if names[m.Name] {
			fail("workload.mcpServers: duplicate name %q", m.Name)
		}
		names[m.Name] = true
	}
	names = map[string]bool{}
	for i, v := range s.Workload.Services {
		s.normalizeEndpoint(fmt.Sprintf("workload.services[%d]", i), "service", v.Name, v.URL, v.Headers, secrets, fail)
		// Unique as their environment variable (LUX_SERVICE_<NAME>, - as _)
		// names them too: my-svc and my_svc would be the same.
		key := strings.ReplaceAll(v.Name, "-", "_")
		if names[key] {
			fail("workload.services: duplicate name %q (- and _ are the same in LUX_SERVICE_*)", v.Name)
		}
		names[key] = true
	}
}

// normalizeEndpoint checks one MCP server or service: an http(s) URL with
// no credentials, headers valued from secrets (never a git credential), and
// egress that allows its host.
func (s *RunSpec) normalizeEndpoint(at, what, name, rawURL string, headers []MCPHeader, secrets map[string]bool, fail func(string, ...any)) {
	if !volumeRe.MatchString(name) {
		fail("%s: invalid name %q (lowercase, digits, - and _)", at, name)
	}
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		fail("%s.url: need an http or https URL with a host", at)
		return
	case u.User != nil:
		// It would be stored with the spec: header values come from secrets.
		fail("%s.url must not carry credentials: use headers with a secret", at)
	}
	hdrs := map[string]bool{}
	for j, h := range headers {
		if !headerRe.MatchString(h.Name) {
			fail("%s.headers[%d]: invalid header name %q", at, j, h.Name)
		}
		if hdrs[strings.ToLower(h.Name)] {
			fail("%s.headers[%d]: duplicate header %q", at, j, h.Name)
		}
		hdrs[strings.ToLower(h.Name)] = true
		if !secrets[h.Secret] {
			fail("%s.headers[%d].secret: no secret named %q", at, j, h.Secret)
		} else if s.isGitCredential(h.Secret) {
			// The header would put it in the container, where a git
			// credential never is.
			fail("%s.headers[%d].secret: %q is a git credential, which never enters the container: use another secret", at, j, h.Secret)
		} else if s.isRegistryCredential(h.Secret) {
			fail("%s.headers[%d].secret: %q is a registry credential, which never enters the container: use another secret", at, j, h.Secret)
		}
	}
	if !s.Network.Unrestricted && !s.Network.allows(u.Hostname()) {
		fail("%s: network.egress does not allow %s %q at %s: add an egress rule for it (host: for a name, cidr: for an address)", at, what, name, u.Hostname())
	}
}

// ReservedSecretPrefix names files lux itself writes on the secrets tmpfs
// (adapters' credential and config files): no secret may use it.
const ReservedSecretPrefix = "lux-"

// onlyCredential reports whether a secret is used as a git or registry
// credential or an MCP server's or service's header and is referenced
// nowhere else a spec can name it.
func (s *RunSpec) onlyCredential(name string) bool {
	if s.isGitCredential(name) || s.isRegistryCredential(name) {
		return true
	}
	var headers []MCPHeader
	for _, m := range s.Workload.MCPServers {
		headers = append(headers, m.Headers...)
	}
	for _, v := range s.Workload.Services {
		headers = append(headers, v.Headers...)
	}
	return slices.ContainsFunc(headers, func(h MCPHeader) bool { return h.Secret == name })
}

func (s *RunSpec) isGitCredential(name string) bool {
	return s.Git != nil && slices.ContainsFunc(s.Git.Repositories, func(r Repository) bool { return r.Credential == name })
}

func (s *RunSpec) isRegistryCredential(name string) bool {
	return slices.ContainsFunc(s.Image.RegistryAuth, func(a RegistryAuth) bool { return a.Secret == name })
}

// validRegistry is a registry as auth.json keys it: a lowercase host name
// or IPv4 address, and an optional port. Not the runner's own host: its
// pulls and pushes go out from the host, outside the Run's egress rules.
func validRegistry(r string) bool {
	host, port, hasPort := strings.Cut(r, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
			return false
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Is4() && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast()
	}
	return hostLabelsRe.MatchString(host)
}

// validCacheRepo is a repository with its registry (see validRegistry)
// and a path, and no tag or digest.
func validCacheRepo(repo string) bool {
	reg, path, ok := strings.Cut(repo, "/")
	// The registry must be named as one (a dot or a port), or it would
	// read as a Docker Hub path.
	named := strings.ContainsAny(reg, ".:")
	return ok && named && validRegistry(reg) && repoPathRe.MatchString(path)
}

var (
	hostLabelsRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	repoPathRe   = regexp.MustCompile(`^[a-z0-9]+([._-]+[a-z0-9]+)*(/[a-z0-9]+([._-]+[a-z0-9]+)*)*$`)
)

// allows reports whether an egress rule covers host: a hostname equal to a
// host rule, or an address in a cidr rule. The runner's firewall enforces
// the rest (what a name resolves to).
func (n Network) allows(host string) bool {
	ip, ipErr := netip.ParseAddr(host)
	for _, e := range n.Egress {
		if e.Host != "" && strings.EqualFold(strings.TrimSuffix(e.Host, "."), strings.TrimSuffix(host, ".")) {
			return true
		}
		if p, err := netip.ParsePrefix(e.CIDR); err == nil && ipErr == nil && p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// volumeFor is the volume a path is on: the most specific one, as mounts
// nest (a volume at /workspace/repos hides /workspace's files there).
func (s *RunSpec) volumeFor(p string) *Volume {
	var best *Volume
	for i := range s.Volumes {
		v := &s.Volumes[i]
		if (p == v.Path || strings.HasPrefix(p, v.Path+"/")) && (best == nil || len(v.Path) > len(best.Path)) {
			best = v
		}
	}
	return best
}

func (s *RunSpec) stateVolumeFor(p string) *Volume {
	if v := s.volumeFor(p); v != nil && v.Kind == "state" {
		return v
	}
	return nil
}

// ValidationError lists every problem found in a spec.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return "invalid spec: " + strings.Join(e.Problems, "; ") }

// SecretRef is what is stored about a secret: never its value.
type SecretRef struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Fingerprint identifies a value without revealing it. Salted with the name
// so equal values under different names do not look alike.
func Fingerprint(name, value string) string {
	h := sha256.Sum256([]byte("lux-secret\x00" + name + "\x00" + value))
	return hex.EncodeToString(h[:8])
}

// SplitSecrets returns the spec without secret values, the stored refs, and
// the values by name.
func (s RunSpec) SplitSecrets() (RunSpec, []SecretRef, map[string]string) {
	refs := make([]SecretRef, 0, len(s.Secrets))
	values := map[string]string{}
	stripped := make([]Secret, len(s.Secrets))
	for i, sec := range s.Secrets {
		refs = append(refs, SecretRef{Name: sec.Name, Fingerprint: Fingerprint(sec.Name, sec.Value)})
		values[sec.Name] = sec.Value
		sec.Value = ""
		stripped[i] = sec
	}
	s.Secrets = stripped
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return s, refs, values
}

// From is a Containerfile's `FROM [--flag=v ...] image [AS name]` line.
type From struct {
	Flags []string
	Image string
	Name  string
}

// ParseFrom reads a FROM line; ok is false for any other line.
func ParseFrom(line string) (From, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
		return From{}, false
	}
	var f From
	rest := fields[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
		f.Flags = append(f.Flags, rest[0])
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return From{}, false
	}
	f.Image = rest[0]
	if len(rest) == 3 && strings.EqualFold(rest[1], "AS") {
		f.Name = rest[2]
	}
	return f, true
}

// With is the line with another image.
func (f From) With(image string) string {
	parts := append([]string{"FROM"}, f.Flags...)
	parts = append(parts, image)
	if f.Name != "" {
		parts = append(parts, "AS", f.Name)
	}
	return strings.Join(parts, " ")
}
