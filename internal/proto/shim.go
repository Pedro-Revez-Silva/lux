package proto

import (
	"fmt"
	"strconv"

	"github.com/marcioapm/lux/internal/spec"
)

// Runner ↔ shim.
//
// The shim is PID 1 in every container. It reads ShimConfig from
// /.lux/run/config.json (written by the runner before the container
// starts; no secrets), listens on /.lux/run/shim.sock, and waits for the
// runner to connect and send "start" with the secret values. Nothing secret
// is ever written to the host's disk or to Podman's container config.
//
// Output does not go over the socket: the shim appends records to
// /.lux/run/output-<epoch>.jsonl (after redacting secrets), so a runner
// restart loses nothing. The runner tails that file to relay live output
// and to pick up adapter events (session id, idle/busy, input acks).
//
// When the workload is done the shim writes /.lux/run/exit-<epoch>.json
// and exits with the workload's code.

const (
	ShimDir        = "/.lux"
	ShimRunDir     = "/.lux/run"
	ShimBinary     = "/.lux/bin/lux-shim"
	ShimSocket     = "/.lux/run/shim.sock"
	ShimConfigFile = "/.lux/run/config.json"
	ShimSecretsDir = "/.lux/secrets"
)

func OutputFile(epoch int) string { return "output-" + strconv.Itoa(epoch) + ".jsonl" }
func ExitFile(epoch int) string   { return "exit-" + strconv.Itoa(epoch) + ".json" }

type ShimConfig struct {
	RunID    string            `json:"runId"`
	Epoch    int               `json:"epoch"`
	Adapter  string            `json:"adapter"`
	Command  []string          `json:"command"`
	Prompt   string            `json:"prompt,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	User     string            `json:"user,omitempty"` // name or uid[:gid]; empty: root
	TTY      bool              `json:"tty,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Init     string            `json:"init,omitempty"`
	GraceSec float64           `json:"graceSec"`
	// BeforeStop, when set, runs in the container on every stop before the
	// workload is signalled (spec.Workload.BeforeStop).
	BeforeStop           []string `json:"beforeStop,omitempty"`
	BeforeStopTimeoutSec float64  `json:"beforeStopTimeoutSec,omitempty"`
	// Resume is set when this is not the Run's first placement.
	Resume        bool     `json:"resume,omitempty"`
	ResumeCommand []string `json:"resumeCommand,omitempty"`
	SessionID     string   `json:"sessionId,omitempty"`
	// Paths of mounted volumes: their roots are handed to the workload user
	// when empty (a fresh volume is owned by root).
	VolumePaths []string `json:"volumePaths,omitempty"`
	// Secrets by name: how each is exposed (values arrive with "start").
	Secrets []spec.Secret `json:"secrets,omitempty"`
	// ArtifactsDir is watched for on-demand artifacts ($LUX_ARTIFACTS).
	ArtifactsDir string `json:"artifactsDir,omitempty"`
	// MCPServers as the spec has them: header values are secret names.
	MCPServers []spec.MCPServer `json:"mcpServers,omitempty"`
	// MCP is MCPServers with header values resolved from the secrets in
	// "start", by the shim, for its adapter. Never serialized: the config
	// file holds no secrets.
	MCP []MCPServer `json:"-"`
	// Services the shim proxies (header values are secret names); their
	// values stay in the shim's memory.
	Services []spec.Service `json:"services,omitempty"`
}

// ShimServicesDir holds each service's socket (a tmpfs of its own).
const ShimServicesDir = "/.lux/services"

// MCPServer is an MCP server as an adapter hands it to its agent.
type MCPServer struct {
	Name    string
	URL     string
	Headers []MCPHeader
}

type MCPHeader struct{ Name, Value string }

// ResolveMCP fills cfg.MCP from the spec's servers and the secret values.
func (cfg *ShimConfig) ResolveMCP(secrets map[string]string) {
	cfg.MCP = nil
	for _, m := range cfg.MCPServers {
		r := MCPServer{Name: m.Name, URL: cfg.MCPURL(m)}
		for _, h := range m.Headers {
			r.Headers = append(r.Headers, MCPHeader{Name: h.Name, Value: secrets[h.Secret]})
		}
		cfg.MCP = append(cfg.MCP, r)
	}
}

// MCPURL is where the agent reaches an MCP server: its url, or for one
// backed by a service, that service's loopback address (which adds the
// headers, so the agent is given none).
func (cfg *ShimConfig) MCPURL(m spec.MCPServer) string {
	if m.Service == "" {
		return m.URL
	}
	for i, v := range cfg.Services {
		if v.Name == m.Service {
			return fmt.Sprintf("http://127.0.0.1:%d%s", spec.ServiceBasePort+i, m.Path)
		}
	}
	return ""
}

// ShimMsg is one line on the shim socket, either direction.
type ShimMsg struct {
	Type string `json:"type"`
	// start
	Secrets map[string]string `json:"secrets,omitempty"`
	Input   *Input            `json:"input,omitempty"` // start (first input), input
	// stop
	Reason string `json:"reason,omitempty"`
	// GraceSec, when set, shortens the stop's grace (the host is going
	// away sooner than the workload's own grace allows).
	GraceSec float64 `json:"graceSec,omitempty"`
	// A stream's handshake: the connection then carries StreamData lines.
	Stream *StreamOpen `json:"stream,omitempty"`
	// replies
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok,omitempty"`
}

const (
	ShimStart     = "start"
	ShimInput     = "input"
	ShimInterrupt = "interrupt"
	ShimStop      = "stop"
	ShimPing      = "ping"
	// A connection that starts with stream (exec, attach) carries only that
	// stream from then on, as StreamData lines both ways; the shim's last
	// one has ExitCode or Error.
	ShimStream = "stream"
)

// Event types the shim writes as ch=event records. The runner forwards the
// lux.* ones to luxd as adapter events; all are visible in the output.
const (
	EvSession    = "lux.session"    // {"sessionId"}
	EvActivity   = "lux.activity"   // {"activity": "idle" | "busy"}
	EvInputAck   = "lux.input"      // {"requestId", "text"?, "truncated"?, "error"?}; the first prompt's id is "prompt"
	EvInit       = "lux.init"       // {"phase": "start" | "done", "exitCode"?}
	EvWorkload   = "lux.workload"   // {"phase": "start", "pid"}
	EvStop       = "lux.stop"       // {"reason"}
	EvBeforeStop = "lux.beforeStop" // {"phase": "start"|"done", "exitCode", "timedOut"}
	EvWarning    = "lux.warning"    // {"message"}
	EvArtifact   = "lux.artifact"   // {"path"}
)

// ExitInfo is the shim's account of how the workload ended.
type ExitInfo struct {
	ExitCode int    `json:"exitCode"`
	Signal   string `json:"signal,omitempty"`
	// exited | stopped | init-failed | start-failed
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	LastSeq int64  `json:"lastSeq"`
}
