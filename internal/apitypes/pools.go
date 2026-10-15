package apitypes

import (
	"github.com/marcioapm/lux/internal/spec"
)

type Pool struct {
	ID        string         `json:"id" readOnly:"true" doc:"The pool's immutable identity; unchanged by rename."`
	Name      string         `json:"name"`
	Tenant    string         `json:"tenant,omitempty" readOnly:"true" doc:"The owning tenant's name; empty for a platform pool."`
	Provider  string         `json:"provider"`
	Template  map[string]any `json:"template,omitempty"`
	MinHosts  int            `json:"minHosts"`
	MaxHosts  int            `json:"maxHosts"`
	WarmHosts int            `json:"warmHosts"`
	// ScaleDownAfter: how long a provisioned host stays idle before it is
	// released; empty for luxd's default.
	ScaleDownAfter  spec.Duration `json:"scaleDownAfter,omitempty" doc:"How long a provisioned host stays idle before it is released, e.g. 600s; empty: luxd's scale_down_after (default 10m)."`
	WarmWhileActive bool          `json:"warmWhileActive,omitempty" doc:"Keep warmHosts only while the pool is in use (a Run placed or ended within scaleDownAfter, or one waiting); an idle pool scales down to minHosts."`
	Shared          bool          `json:"shared"`
	Platform        bool          `json:"platform" readOnly:"true" doc:"A platform pool (no tenant). In a request, true is refused unless the caller acts on the platform's pools (an operator key without a tenant)."`
	// HourlyPrice and Currency: a static pool's default price, copied to
	// each host when it first registers. Changing it does not reprice the
	// pool's existing hosts (PUT /v1/hosts/{id}/price does, one host).
	HourlyPrice string `json:"hourlyPrice,omitempty" doc:"Static pools: the default hourly price of hosts registering into the pool, a decimal string with up to 9 fractional digits. Copied to each host when it first registers; changing it does not reprice existing hosts. Refused for ec2 pools, which the provider prices." example:"0.40"`
	Currency    string `json:"currency,omitempty" doc:"The currency of hourlyPrice (ISO 4217); both or neither." example:"USD"`
	// IsDefault: nil in a request leaves the mark as it is.
	IsDefault *bool `json:"isDefault,omitempty" doc:"Runs whose spec names no pool go to this pool (the tenant's; a platform default serves tenants without one). At most one per tenant: marking one clears the tenant's previous default. On create or update, omitted leaves the mark as it is. A body without provider changes only the mark of an existing pool: besides name and isDefault its fields must be absent or zero (\"\", 0, false, null, {}); any other value needs provider."`
	// HostSize, HostSizeFrom and InstanceType are read-only: a request's
	// are ignored (poolReadOnly).
	HostSize     *HostSize `json:"hostSize,omitempty" readOnly:"true" doc:"The capacity the pool's hosts register with, as the scheduler plans new hosts: the minimum of each resource over the latest 8 registrations (for an ec2 pool, of its current template), so a pool scaled to zero still has one. Runs ask in these terms: a Run's resources.memory is a share of hostSize.memory. Absent when no host ever registered in the pool."`
	HostSizeFrom string    `json:"hostSizeFrom,omitempty" readOnly:"true" enum:"running,history" doc:"running: the pool has at least one ready or draining host now; history: hostSize comes only from hosts that have gone. Absent with hostSize."`
	InstanceType string    `json:"instanceType,omitempty" readOnly:"true" doc:"The provider's instance type of the latest host hostSize is taken from; absent for static hosts and with hostSize." example:"c7a.8xlarge"`
	// Platforms and Capabilities: what a resume's placement.poolId checks
	// against (poolFacts), as the listing tenant would be checked.
	Platforms    []string          `json:"platforms,omitempty" readOnly:"true" doc:"The platforms (os/arch) the pool's hosts run: those of its live and latest registered hosts, and for an ec2 pool its template's instance types as the instance-type catalog describes them. Absent when lux knows none; a resume into it is then not refused for its platform." example:"[\"linux/arm64\"]"`
	Capabilities *PoolCapabilities `json:"capabilities,omitempty" readOnly:"true" doc:"What the pool's hosts offer a Run, as a resume's placement.poolId judges it."`
	Shape        *PoolShape        `json:"shape,omitempty" doc:"Makes the pool shaped: the nominal whole host its sizes halve down from. Runs ask for a size (resources.size) or have cpus, memory and disk rounded up to one. Checked at create and update (422 invalid_pool): family a lowercase slug, arch arm64 or amd64, cpus and minCpus powers of two (minCpus at least 0.5), memory and disk dividing exactly down to the smallest size, defaultCpus one of the sizes; for an ec2 pool, the template's instanceType and fallbackInstanceTypes the catalog knows must have its arch and at least its cpus and memory. Absent: unshaped, as every pool before. Like every field, a set without it removes it; Runs already submitted keep their resources."`
	Sizes        []PoolSize        `json:"sizes,omitempty" readOnly:"true" doc:"A shaped pool's sizes, smallest first: each one's name, cpus, memory, disk and fraction of the whole host. Absent for an unshaped pool."`
	Placement    string            `json:"placement,omitempty" enum:"spread,pack" doc:"How the scheduler chooses among the hosts a Run fits. spread: the host with the fewest Runs. pack: the most allocated host (the largest of its reserved cpus, memory and disk over its capacity), so whole hosts stay free for large sizes; among equally allocated hosts, the spread affinity (the snapshot's host, the image and git mirrors cached, preferred labels), then the lowest host id. Either way a Run whose snapshot only its host holds (not yet uploaded) waits for that host, and a host the Run must avoid comes last. Absent: the pool's default, pack for a shaped pool and spread otherwise. Like every field, a set without it returns it to the default. Other values: 422 invalid_pool."`
}

// PoolList is GET /v1/pools's answer.
type PoolList struct {
	Pools []Pool `json:"pools"`
}

// PoolCapabilities: null is unknown.
type PoolCapabilities struct {
	NestedContainers *bool `json:"nestedContainers" doc:"Whether a Run with sandbox.nestedContainers can run in the pool: an ec2 pool's template nestedContainers, or a host that registered with lux-runner --nested. null for a static pool no host registered in." nullable:"true"`
}

// HostSize is a host's capacity as it registers: CPUs, and bytes of
// memory and disk (disk 0: the host reserves none).
type HostSize struct {
	CPUs   float64 `json:"cpus" doc:"CPUs."`
	Memory int64   `json:"memory" doc:"Bytes of memory: the machine's gross memory, in which Runs ask."`
	Disk   int64   `json:"disk" doc:"Bytes of disk reserved for Runs; 0 when the host reserves none."`
}

// PoolShape is an operator-declared nominal host shape; pools.shape.
type PoolShape struct {
	Family      string     `json:"family" doc:"The size family, a lowercase slug (e.g. m): the first part of every size name. Pool metadata, never inferred from the provider."`
	Arch        string     `json:"arch" enum:"arm64,amd64" doc:"The hosts' architecture, the last part of every size name."`
	CPUs        float64    `json:"cpus" doc:"Nominal CPUs of a whole host: a power of two."`
	Memory      spec.Bytes `json:"memory" doc:"Nominal memory of a whole host, in the terms Runs ask in."`
	Disk        spec.Bytes `json:"disk" doc:"Nominal disk of a whole host (from its launch template, not its instance type)."`
	MinCPUs     float64    `json:"minCpus" doc:"The smallest size's CPUs: a power of two, at least 0.5. The ladder halves the whole host down to it."`
	DefaultCPUs float64    `json:"defaultCpus" doc:"The CPUs of the size a Run that asks for no resources gets: one of the ladder's."`
	// DiskHeadroom: nil reads as DefaultDiskHeadroom (a shape stored
	// before the field, or a request without it).
	DiskHeadroom *spec.Bytes `json:"diskHeadroom,omitempty" doc:"Bytes of each host's disk kept from Runs, for the OS, tools and image storage (lux-runner --disk-headroom): a Run's disk limit is its size's disk × min(1, (filesystem capacity − diskHeadroom) / disk). At least 0 and less than disk. Absent: 10 GiB."`
}

// PoolSize is one entry of a shaped pool's ladder.
type PoolSize struct {
	Name     string     `json:"name" example:"m-2-arm64"`
	CPUs     float64    `json:"cpus"`
	Memory   spec.Bytes `json:"memory" doc:"Bytes."`
	Disk     spec.Bytes `json:"disk" doc:"Bytes."`
	Fraction float64    `json:"fraction" doc:"Of a whole host: 1, 0.5, 0.25, ..."`
	Default  bool       `json:"default,omitempty" doc:"The size a Run that asks for no resources gets."`
}

// RunSizing is a Run's request and resolution on a shaped pool.
type RunSizing struct {
	Requested  *SizeRequest  `json:"requested,omitempty" doc:"What its submit asked for in resources.size, cpus, memory and disk, only those given (empty: none, the pool's default size). Absent for a Run submitted to an unshaped pool. A resume's request is in its answer's resize and its resume.requested event."`
	Resolved   *SizeResolved `json:"resolved,omitempty" doc:"The size the Run has now, and its cpus, memory and disk (as spec.resources). Absent while it is in an unshaped pool, or while its pool's current sizes have no size of that name with the Run's cpus, memory and disk (the shape changed since, even if the name stayed): its next resume resolves it again."`
	RaisedFrom string        `json:"raisedFrom,omitempty" doc:"With raisedFor: the size the resume that set its size asked for (its request resolved to)."`
	RaisedFor  string        `json:"raisedFor,omitempty" enum:"saved state" doc:"With resolved: why its size is larger than the resume that set it asked for. saved state: raisedFrom could not hold the Run's saved state, so it got the smallest larger size that could. Absent when it got what was asked."`
}

// SizeRequest is what a Run asked for in resources, as the sizing model
// reads it: only the fields given (a zero is not given).
type SizeRequest struct {
	Size   string      `json:"size,omitempty"`
	CPUs   *float64    `json:"cpus,omitempty"`
	Memory *spec.Bytes `json:"memory,omitempty" doc:"Bytes."`
	Disk   *spec.Bytes `json:"disk,omitempty" doc:"Bytes."`
}

// SizeResolved is the size a Run has, and its resources.
type SizeResolved struct {
	Size   string     `json:"size"`
	CPUs   float64    `json:"cpus"`
	Memory spec.Bytes `json:"memory" doc:"Bytes."`
	Disk   spec.Bytes `json:"disk" doc:"Bytes."`
}
