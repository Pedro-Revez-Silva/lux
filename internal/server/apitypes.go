package server

import "github.com/marcioapm/lux/internal/apitypes"

// The API's response types that clients decode into are declared in
// internal/apitypes, which depends on none of luxd's dependencies.
type (
	Run              = apitypes.Run
	Resumability     = apitypes.Resumability
	Placement        = apitypes.Placement
	RunUsage         = apitypes.RunUsage
	RunSubmitter     = apitypes.RunSubmitter
	SubmitAnswer     = apitypes.SubmitAnswer
	AdmissionBlocker = apitypes.AdmissionBlocker
	Event            = apitypes.Event
	FeedEvent        = apitypes.FeedEvent
	RunServer        = apitypes.RunServer
	RunCostBrief     = apitypes.RunCostBrief
	CostTotal        = apitypes.CostTotal
	Pool             = apitypes.Pool
	PoolCapabilities = apitypes.PoolCapabilities
	HostSize         = apitypes.HostSize
	PoolShape        = apitypes.PoolShape
	PoolSize         = apitypes.PoolSize
	RunSizing        = apitypes.RunSizing
	SizeRequest      = apitypes.SizeRequest
	SizeResolved     = apitypes.SizeResolved
	listRunsBody     = apitypes.ListRunsBody
	EventList        = apitypes.EventList
	PoolList         = apitypes.PoolList
)
