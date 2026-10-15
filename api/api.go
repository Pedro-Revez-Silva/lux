// Package api is lux's Go API for clients outside this module: the HTTP
// client, the configuration the lux CLI signs in with, the RunSpec a Run
// is submitted with, and the types luxd's answers decode into.
//
// Stability: the api package follows lux releases; types are aliases of
// lux's own and may grow fields. Decode answers into them knowing a newer
// luxd may send fields an older client ignores, and an older luxd may
// leave newer fields zero.
//
// A tty workload's terminal is reached with
//
//	ws, err := c.Dial(ctx, "/v1/runs/"+id+"/attach?restore=true&rows=24&cols=80")
//
// then StreamData messages both ways (wsjson). With restore, the output
// starts with a snapshot that redraws the terminal; a host from before
// restore sends none, and output is then live only.
package api

import (
	"github.com/marcioapm/lux/internal/apitypes"
	"github.com/marcioapm/lux/internal/client"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// The client.
type (
	Client   = client.Client
	APIError = client.APIError
	SSEEvent = client.SSEEvent
	// Config is where the lux CLI finds luxd: url, api_key and tenant.
	Config = client.Config
)

// New is a client of the luxd at base (e.g. https://lux.example.com),
// authenticating with an API key.
func New(base, key string) *Client { return client.New(base, key) }

// LoadConfig is the lux CLI's configuration: ~/.config/lux/config.toml
// (the user config dir), overridden by LUX_URL, LUX_API_KEY and
// LUX_TENANT. Flags a caller takes go on top.
func LoadConfig() Config { return client.LoadConfig() }

// FromConfig is a client of cfg's luxd, narrowed to its tenant if any.
func FromConfig(cfg Config) *Client {
	c := client.New(cfg.URL, cfg.APIKey)
	c.Tenant = cfg.Tenant
	return c
}

// Interactive streams: exec, attach and port forwarding.
type (
	StreamOpen = proto.StreamOpen
	StreamData = proto.StreamData
)

// The RunSpec, and what it is built from.
type (
	RunSpec          = spec.RunSpec
	Image            = spec.Image
	RegistryAuth     = spec.RegistryAuth
	Build            = spec.Build
	Workload         = spec.Workload
	Server           = spec.Server
	BeforeStop       = spec.BeforeStop
	Service          = spec.Service
	MCPServer        = spec.MCPServer
	MCPHeader        = spec.MCPHeader
	Resume           = spec.Resume
	Init             = spec.Init
	Secret           = spec.Secret
	SecretRef        = spec.SecretRef
	Git              = spec.Git
	Repository       = spec.Repository
	Push             = spec.Push
	Volume           = spec.Volume
	Resources        = spec.Resources
	Placement        = spec.Placement
	Network          = spec.Network
	EgressRule       = spec.EgressRule
	Port             = spec.Port
	Sandbox          = spec.Sandbox
	Artifacts        = spec.Artifacts
	Steer            = spec.Steer
	Attachment       = spec.Attachment
	Duration         = spec.Duration
	OptionalDuration = spec.OptionalDuration
	Bytes            = spec.Bytes
)

// ParseBytes reads a size such as 512Mi, 2G or 1073741824.
func ParseBytes(s string) (Bytes, error) { return spec.ParseBytes(s) }

// Answers: a Run (GET /v1/runs/{id}; GET /v1/runs lists them in RunList),
// the submit answer (POST /v1/runs), events (GET /v1/runs/{id}/events in
// EventList; GET /v1/events streams FeedEvent), and pools and their sizes
// (GET /v1/pools in PoolList).
type (
	Run              = apitypes.Run
	RunPlacement     = apitypes.Placement
	RunUsage         = apitypes.RunUsage
	RunSubmitter     = apitypes.RunSubmitter
	Resumability     = apitypes.Resumability
	RunServer        = apitypes.RunServer
	RunCostBrief     = apitypes.RunCostBrief
	CostTotal        = apitypes.CostTotal
	RunSizing        = apitypes.RunSizing
	SizeRequest      = apitypes.SizeRequest
	SizeResolved     = apitypes.SizeResolved
	SubmitAnswer     = apitypes.SubmitAnswer
	AdmissionBlocker = apitypes.AdmissionBlocker
	Event            = apitypes.Event
	FeedEvent        = apitypes.FeedEvent
	Pool             = apitypes.Pool
	PoolShape        = apitypes.PoolShape
	PoolSize         = apitypes.PoolSize
	PoolCapabilities = apitypes.PoolCapabilities
	HostSize         = apitypes.HostSize
	ImageResolution  = proto.ImageResolution
	RunList          = apitypes.ListRunsBody
	EventList        = apitypes.EventList
	PoolList         = apitypes.PoolList
)
