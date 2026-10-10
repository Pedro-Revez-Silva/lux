// Package ec2 provisions lux hosts as EC2 instances.
//
// A pool's template names what to launch:
//
//	{"region": "eu-west-1", "launchTemplate": "lt-0abc…" (id or name),
//	 "instanceType": "m7i.2xlarge", "fallbackInstanceTypes": ["m6i.2xlarge"],
//	 "subnets": ["subnet-…", …], "tags": {"team": "platform"}, "spot": true,
//	 "userData": "ignition"}
//
// A launch EC2 has no capacity for (InsufficientInstanceCapacity,
// InsufficientCapacity, or Unsupported: the type is not offered in that
// zone) tries instanceType in each of the subnets, from the pool's next one
// round, then each fallbackInstanceTypes entry the same way. Any other
// error ends the launch. A candidate without capacity is skipped for
// SkipNoCapacityFor (default 30s; 0 never skips) by later launches with the
// same region, instance type, launch template, subnet and market, so a
// shortage costs one sweep of RunInstances calls, not one per launch; when
// every candidate is skipped, the launch fails without calling EC2.
//
// With "spot", instances are one-time spot instances, terminated on
// interruption. Every instance's user data sets LUX_EC2_IMDS, so its
// runner watches for the interruption notice and its Runs move to other
// hosts before the instance goes.
//
// userData picks how the runner's environment reaches the instance
// (hostboot.Render implements all three):
//
//   - "ignition" (default): an Ignition v3 config for Fedora CoreOS. No
//     package installation happens at boot: FCOS ships everything lux-runner
//     needs, so a host registers in well under a minute.
//   - "script": a #!/bin/bash script, which cloud-init runs on a stock
//     Fedora Cloud, Ubuntu, Debian or AL2023 image. It checks the host
//     requirements and installs only what is missing (dnf, else apt-get).
//   - "env": KEY=value lines, for a custom AMI whose own boot script reads
//     them (the pre-self-update behaviour).
//
// Whichever format, the instance downloads lux-runner and lux-shim from
// luxd (GET /runner/v1/bin/...) rather than carrying them in the AMI.
//
// Credentials and region come from the standard AWS configuration of the
// luxd process (environment, instance role). LUX_EC2_ENDPOINT points the
// client elsewhere: the test suite's fake EC2.
package ec2

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/marcioapm/lux/internal/hostboot"
	"github.com/marcioapm/lux/internal/server"
)

// Template is a pool's EC2 settings.
type Template struct {
	Region         string `json:"region"`
	LaunchTemplate string `json:"launchTemplate"`
	InstanceType   string `json:"instanceType"`
	// FallbackInstanceTypes are tried, in order, after InstanceType when
	// EC2 has no capacity for it in any of Subnets (see Launch).
	FallbackInstanceTypes []string          `json:"fallbackInstanceTypes"`
	Subnets               []string          `json:"subnets"`
	Tags                  map[string]string `json:"tags"`
	Spot                  bool              `json:"spot"`
	// UserData: "ignition" (default), "script", or "env". See the package
	// doc. Validated when the pool is set (internal/server), so an unknown
	// value is refused there, not here at launch time.
	UserData string `json:"userData"`
	// NestedContainers starts every host's runner with --nested, so the pool
	// offers sandbox.nestedContainers. The launch template's AMI must provide
	// /dev/fuse and /dev/net/tun (Fedora CoreOS does); a runner without them
	// exits rather than register. Validated when the pool is set.
	NestedContainers bool `json:"nestedContainers"`
}

type Provider struct {
	endpoint string
	// clientsMu guards clients: Check runs from request handlers while the
	// provisioner launches, lists and terminates.
	clientsMu sync.Mutex
	clients   map[string]*awsec2.Client
	// next subnet per pool: launches spread across the pool's subnets.
	next map[string]int
	// memory is each instance type's memory in bytes, per region, as
	// DescribeInstanceTypes gave it: fixed for a type, so asked once.
	memory map[[2]string]int64
	// memoryFailed is when each type's lookup last failed: within
	// memoryRetryAfter of it, launches go ahead without asking again.
	memoryFailed map[[2]string]time.Time
	// memoryTimeout bounds one lookup, which runs before RunInstances.
	memoryTimeout time.Duration
	// noCapacity is the last capacity error per capacityKey: within
	// noCapacityFor of it, launches skip the candidate.
	noCapacity    map[capacityKey]noCapacityMark
	noCapacityFor time.Duration
	now           func() time.Time
	log           *slog.Logger
}

// capacityKey is the EC2 capacity a candidate asks for: what lux sets on
// RunInstances plus the launch template, whose type, zone, placement group,
// tenancy or capacity reservation lux does not see.
type capacityKey struct {
	region, instanceType, launchTemplate, subnet string
	spot                                         bool
}

func (t Template) capacityKey(c candidate) capacityKey {
	return capacityKey{t.Region, c.instanceType, t.LaunchTemplate, c.subnet, t.Spot}
}

type noCapacityMark struct {
	at   time.Time
	code string
	err  error
}

const (
	memoryLookupTimeout = 5 * time.Second
	memoryRetryAfter    = 10 * time.Minute
)

// DefaultNoCapacityRetryAfter is how long a candidate EC2 had no capacity
// for is skipped: long enough that a shortage costs one sweep of
// RunInstances per pool, not one per launch, short enough to see capacity
// come back within a minute.
const DefaultNoCapacityRetryAfter = 30 * time.Second

// New builds the provider. endpoint overrides the EC2 endpoint (tests).
func New(endpoint string, log *slog.Logger) *Provider {
	return &Provider{endpoint: endpoint, clients: map[string]*awsec2.Client{}, next: map[string]int{},
		memory: map[[2]string]int64{}, memoryFailed: map[[2]string]time.Time{}, memoryTimeout: memoryLookupTimeout,
		noCapacity: map[capacityKey]noCapacityMark{}, noCapacityFor: DefaultNoCapacityRetryAfter, now: time.Now, log: log}
}

// SkipNoCapacityFor sets how long a candidate without capacity is skipped
// (default DefaultNoCapacityRetryAfter); 0 never skips.
func (p *Provider) SkipNoCapacityFor(d time.Duration) {
	p.noCapacityFor = d
}

// client is region's cached client, built on first use. The build runs
// without clientsMu (LoadDefaultConfig may reach IMDS or STS); when two
// callers build the same region, the first one published wins.
func (p *Provider) client(ctx context.Context, region string) (*awsec2.Client, error) {
	p.clientsMu.Lock()
	c := p.clients[region]
	p.clientsMu.Unlock()
	if c != nil {
		return c, nil
	}
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	c = awsec2.NewFromConfig(cfg, func(o *awsec2.Options) {
		if p.endpoint != "" {
			o.BaseEndpoint = aws.String(p.endpoint)
		}
	})
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	if won := p.clients[region]; won != nil {
		return won, nil
	}
	p.clients[region] = c
	return c, nil
}

// Launch starts one instance and returns its id, instance type and
// availability zone as RunInstances reports them, and its market (from the
// template's spot). tags are set on it (with the template's own); env is
// the runner's environment, rendered into user data. Without capacity, it
// falls back across subnets and fallbackInstanceTypes (see the package doc).
func (p *Provider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (server.Launched, error) {
	var none server.Launched
	t, err := parse(template)
	if err != nil {
		return none, err
	}
	if t.LaunchTemplate == "" {
		return none, errNoLaunchTemplate
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return none, err
	}
	env = runnerEnv(t, env)
	start := 0
	if len(t.Subnets) > 0 {
		start = p.next[t.LaunchTemplate] % len(t.Subnets)
		p.next[t.LaunchTemplate]++
	}
	return p.tryCandidates(ctx, c, t, runInput(t, tags), env, start)
}

var errNoLaunchTemplate = errors.New("ec2: the pool template needs a launchTemplate")

// runInput is the RunInstancesInput every candidate of t's launches shares:
// the launch template, its tags (t's and tags), and the spot market.
// withCandidate sets the rest; Launch then adds the user data.
func runInput(t Template, tags map[string]string) awsec2.RunInstancesInput {
	lt := &types.LaunchTemplateSpecification{Version: aws.String("$Default")}
	if strings.HasPrefix(t.LaunchTemplate, "lt-") {
		lt.LaunchTemplateId = aws.String(t.LaunchTemplate)
	} else {
		lt.LaunchTemplateName = aws.String(t.LaunchTemplate)
	}
	// EC2 refuses a key given twice, and lux lists a pool's instances by
	// its own tags, so those win over a template's same key.
	merged := make(map[string]string, len(t.Tags)+len(tags))
	maps.Copy(merged, t.Tags)
	maps.Copy(merged, tags)
	instTags := make([]types.Tag, 0, len(merged))
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		instTags = append(instTags, types.Tag{Key: aws.String(k), Value: aws.String(merged[k])})
	}
	base := awsec2.RunInstancesInput{
		MinCount:       aws.Int32(1),
		MaxCount:       aws.Int32(1),
		LaunchTemplate: lt,
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: instTags},
		},
	}
	if t.Spot {
		base.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
			},
		}
	}
	return base
}

// withCandidate is base asking for cand's instance type and subnet; an
// empty one leaves the launch template's.
func withCandidate(base awsec2.RunInstancesInput, cand candidate) awsec2.RunInstancesInput {
	in := base
	if cand.instanceType != "" {
		in.InstanceType = types.InstanceType(cand.instanceType)
	}
	if cand.subnet != "" {
		in.SubnetId = aws.String(cand.subnet)
	}
	return in
}

// CheckTimeout bounds a whole Check.
const CheckTimeout = 10 * time.Second

// Check asks EC2 whether template would launch with tags, launching
// nothing: RunInstances with DryRun, built by Launch's code (launch
// template, instance type, subnet, market, tags merged with the
// template's). The caller passes a launch's tags (server.LaunchTags); the
// user data is the template's format rendered without a host's URL, token
// or instance-type memory. One call per subnet with instanceType, then one
// per fallbackInstanceTypes entry in the first subnet. EC2 answers
// DryRunOperation where the real call would have been allowed; DryRun does
// not test capacity. The first other answer is the error, EC2's code
// first: "<code>: <message> (<candidate>)".
func (p *Provider) Check(ctx context.Context, template json.RawMessage, tags map[string]string) error {
	t, err := parse(template)
	if err != nil {
		return err
	}
	if t.LaunchTemplate == "" {
		return errNoLaunchTemplate
	}
	ud, err := renderUserData(t.UserData, runnerEnv(t, map[string]string{}))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return err
	}
	base := runInput(t, tags)
	base.DryRun = aws.Bool(true)
	base.UserData = aws.String(base64.StdEncoding.EncodeToString(ud))
	for _, cand := range checkCandidates(t) {
		in := withCandidate(base, cand)
		_, err := c.RunInstances(ctx, &in)
		var ae smithy.APIError
		switch {
		case err == nil:
			// EC2 answers a dry run it would allow with DryRunOperation;
			// an endpoint that answers success has not confirmed it.
			return fmt.Errorf("unexpected successful RunInstances response to DryRun; authorization was not confirmed (%s)", cand)
		case !errors.As(err, &ae):
			return fmt.Errorf("ec2 RunInstances (%s): %w", cand, err)
		case ae.ErrorCode() != "DryRunOperation":
			return fmt.Errorf("%s: %s (%s)", ae.ErrorCode(), ae.ErrorMessage(), cand)
		}
	}
	return nil
}

// checkCandidates are the RunInstances Check asks about: instanceType in
// every subnet, then each fallback type in the first.
func checkCandidates(t Template) []candidate {
	subnets := subnetsFrom(t, 0)
	out := make([]candidate, 0, len(subnets)+len(t.FallbackInstanceTypes))
	for _, sn := range subnets {
		out = append(out, candidate{t.InstanceType, sn})
	}
	for _, it := range t.FallbackInstanceTypes {
		out = append(out, candidate{it, subnets[0]})
	}
	return out
}

// tryCandidates sends base as each of t's candidates from subnet start,
// skipping those EC2 had no capacity for within noCapacityFor.
func (p *Provider) tryCandidates(ctx context.Context, c *awsec2.Client, t Template, base awsec2.RunInstancesInput, env map[string]string, start int) (server.Launched, error) {
	var none server.Launched
	userData := map[string]string{} // instance type → base64 user data
	cands := candidates(t, start)
	skipped, failed := 0, 0
	// One reading for the whole sweep: its marks expire together, so a
	// later launch sweeps all of them or none, whatever each call took.
	now := p.now()
	for n, cand := range cands {
		key := t.capacityKey(cand)
		if m, ok := p.noCapacity[key]; ok && now.Sub(m.at) < p.noCapacityFor {
			skipped++
			continue
		}
		ud, ok := userData[cand.instanceType]
		if !ok {
			raw, err := renderUserData(t.UserData, p.withMemory(ctx, c, t.Region, env, cand.instanceType))
			if err != nil {
				return none, err
			}
			ud = base64.StdEncoding.EncodeToString(raw)
			userData[cand.instanceType] = ud
		}
		in := withCandidate(base, cand)
		in.UserData = aws.String(ud)
		out, err := c.RunInstances(ctx, &in, noCapacityRetries)
		if err == nil {
			delete(p.noCapacity, key)
			if n > 0 {
				p.log.Warn("ec2: launched a later candidate: earlier ones had no capacity", "instanceType", cand.instanceType,
					"subnet", cand.subnet, "failed", failed, "skipped", skipped)
			}
			return launched(t, out)
		}
		code, capacity := capacityError(err)
		if !capacity {
			if n > 0 {
				return none, fmt.Errorf("ec2 RunInstances (%s, after %d without capacity): %w", cand, n, err)
			}
			return none, fmt.Errorf("ec2 RunInstances: %w", err)
		}
		failed++
		p.noCapacity[key] = noCapacityMark{at: now, code: code, err: err}
		p.log.Info("ec2: no capacity", "instanceType", cand.instanceType, "subnet", cand.subnet, "code", code)
	}
	// Every candidate failed now or has a recent mark, so the template's
	// first candidate has one; its error is wrapped because EC2's message
	// names the zone, and text that varied with the rotation would split
	// pool.launch_failed events. The code leads: the provisioner keeps 200
	// characters of a launch error.
	m := p.noCapacity[t.capacityKey(t.head())]
	return none, fmt.Errorf("ec2 RunInstances: %s for every candidate: %s: %w", m.code, candidateList(t), m.err)
}

// candidateList is every candidate of t: "m8g.2xlarge, m7g.2xlarge in
// subnet-a, subnet-b" (every type is tried in every subnet).
func candidateList(t Template) string {
	return candidate{strings.Join(t.instanceTypes(), ", "), strings.Join(t.Subnets, ", ")}.String()
}

// instanceTypes is InstanceType, then FallbackInstanceTypes: the order
// Launch tries them in.
func (t Template) instanceTypes() []string {
	return append([]string{t.InstanceType}, t.FallbackInstanceTypes...)
}

// A candidate is one RunInstances attempt: an instance type ("" for the
// launch template's) in a subnet ("" for the launch template's or the
// default VPC's).
type candidate struct{ instanceType, subnet string }

func (c candidate) String() string {
	s := cmp.Or(c.instanceType, "template type")
	if c.subnet != "" {
		s += " in " + c.subnet
	}
	return s
}

// head is t's first candidate whatever the rotation: InstanceType in the
// first subnet.
func (t Template) head() candidate {
	return candidate{t.InstanceType, subnetsFrom(t, 0)[0]}
}

// subnetsFrom is t's subnets from start, wrapping round; [""] (the launch
// template's or the default VPC's) when it names none.
func subnetsFrom(t Template, start int) []string {
	if len(t.Subnets) == 0 {
		return []string{""}
	}
	return append(slices.Clone(t.Subnets[start:]), t.Subnets[:start]...)
}

// candidates is the order Launch tries: each instance type (InstanceType,
// then FallbackInstanceTypes) in each subnet, from start and wrapping round.
func candidates(t Template, start int) []candidate {
	itypes := t.instanceTypes()
	subnets := subnetsFrom(t, start)
	out := make([]candidate, 0, len(itypes)*len(subnets))
	for _, it := range itypes {
		for _, sn := range subnets {
			out = append(out, candidate{it, sn})
		}
	}
	return out
}

// capacityCodes are the RunInstances errors that say this type is not
// to be had in this zone now (InsufficientInstanceCapacity,
// InsufficientCapacity) or at all (Unsupported): another subnet or type may
// still launch. Every other error (quotas, permissions, parameters) would
// fail the same way for every candidate.
var capacityCodes = map[string]bool{
	"InsufficientInstanceCapacity": true,
	"InsufficientCapacity":         true,
	"Unsupported":                  true,
}

func capacityError(err error) (string, bool) {
	var ae smithy.APIError
	if errors.As(err, &ae) && capacityCodes[ae.ErrorCode()] {
		return ae.ErrorCode(), true
	}
	return "", false
}

// noCapacityRetries keeps the SDK from retrying a capacity error (EC2
// answers InsufficientInstanceCapacity with a 500, which the default
// retryer repeats against the same zone): Launch moves to the next
// candidate instead. Other errors keep the client's retry behaviour.
func noCapacityRetries(o *awsec2.Options) {
	o.Retryer = &capacityNotRetryable{RetryerV2: asRetryerV2(o.Retryer)}
}

type capacityNotRetryable struct{ aws.RetryerV2 }

func (r *capacityNotRetryable) IsErrorRetryable(err error) bool {
	if _, capacity := capacityError(err); capacity {
		return false
	}
	return r.RetryerV2.IsErrorRetryable(err)
}

// asRetryerV2 is r as a RetryerV2. The SDK's own retryers are; any other
// gets AddWithMaxAttempts' adapter, keeping its attempt count.
func asRetryerV2(r aws.Retryer) aws.RetryerV2 {
	if v, ok := r.(aws.RetryerV2); ok {
		return v
	}
	return retry.AddWithMaxAttempts(r, r.MaxAttempts()).(aws.RetryerV2)
}

// withMemory is env with instanceType's memory as LUX_RUNNER_MEMORY: the
// runner offers the machine's gross memory, which Runs ask in. Only a
// named type says it before the launch; without one (or when its lookup
// fails) the runner offers its MemTotal. A caller's LUX_RUNNER_MEMORY wins.
func (p *Provider) withMemory(ctx context.Context, c *awsec2.Client, region string, env map[string]string, instanceType string) map[string]string {
	if instanceType == "" || env["LUX_RUNNER_MEMORY"] != "" {
		return env
	}
	mem, err := p.instanceMemory(ctx, c, region, instanceType)
	if err != nil {
		p.log.Warn("ec2: instance type memory unknown; the host offers its MemTotal", "instanceType", instanceType, "err", err)
		return env
	}
	env = maps.Clone(env)
	env["LUX_RUNNER_MEMORY"] = strconv.FormatInt(mem, 10)
	return env
}

func launched(t Template, out *awsec2.RunInstancesOutput) (server.Launched, error) {
	var none server.Launched
	if len(out.Instances) != 1 || out.Instances[0].InstanceId == nil {
		return none, errors.New("ec2 RunInstances: no instance in the reply")
	}
	i := out.Instances[0]
	l := server.Launched{ProviderID: *i.InstanceId, InstanceType: string(i.InstanceType), Market: server.MarketOnDemand}
	if i.Placement != nil {
		l.Zone = aws.ToString(i.Placement.AvailabilityZone)
	}
	if t.Spot {
		l.Market = server.MarketSpot
	}
	return l, nil
}

// Terminate ends an instance. One that no longer exists is done.
func (p *Provider) Terminate(ctx context.Context, template json.RawMessage, id string) error {
	t, err := parse(template)
	if err != nil {
		return err
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return err
	}
	_, err = c.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if isNotFound(err) {
		return nil
	}
	return err
}

// Instances lists the instances carrying all the given tags, with their
// state (pending, running, shutting-down, terminated, …) and tags. A
// filtered call: ids EC2 does not know do not fail it.
func (p *Provider) Instances(ctx context.Context, template json.RawMessage, tags map[string]string) (map[string]server.Instance, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	var filters []types.Filter
	for k, v := range tags {
		filters = append(filters, types.Filter{Name: aws.String("tag:" + k), Values: []string{v}})
	}
	out := map[string]server.Instance{}
	pages := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{Filters: filters})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if i.InstanceId == nil || i.State == nil {
					continue
				}
				inst := server.Instance{State: string(i.State.Name), Tags: map[string]string{}}
				for _, tag := range i.Tags {
					inst.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
				}
				out[*i.InstanceId] = inst
			}
		}
	}
	return out, nil
}

// Volumes is one DescribeVolumes call (paged) filtered by
// attachment.instance-id for every id given, all in the template's region.
// Only attachments with DeleteOnTermination count: a volume that outlives
// the instance is not its cost. An instance with attachments but none
// deleted with it maps to an empty slice; one with no attachment at all is
// absent (its volumes are not attached yet).
func (p *Provider) Volumes(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string][]server.HostVolume, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	if len(providerIDs) == 0 {
		return map[string][]server.HostVolume{}, nil
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range providerIDs {
		want[id] = true
	}
	out := map[string][]server.HostVolume{}
	pages := awsec2.NewDescribeVolumesPaginator(c, &awsec2.DescribeVolumesInput{
		Filters: []types.Filter{{Name: aws.String("attachment.instance-id"), Values: providerIDs}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeVolumes: %w", err)
		}
		for _, v := range page.Volumes {
			for _, a := range v.Attachments {
				id := aws.ToString(a.InstanceId)
				if !want[id] {
					continue
				}
				if _, seen := out[id]; !seen {
					out[id] = []server.HostVolume{}
				}
				if !aws.ToBool(a.DeleteOnTermination) {
					continue
				}
				out[id] = append(out[id], server.HostVolume{Type: string(v.VolumeType), SizeGiB: int64(aws.ToInt32(v.Size)),
					IOPS: int64(aws.ToInt32(v.Iops)), ThroughputMiBps: int64(aws.ToInt32(v.Throughput))})
			}
		}
	}
	return out, nil
}

func isNotFound(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "InvalidInstanceID.NotFound"
}

// instanceMemory is instanceType's memory in bytes (DescribeInstanceTypes'
// MemoryInfo.SizeInMiB), asked once per region and type for the process,
// within memoryTimeout, and not again for memoryRetryAfter after a failure.
func (p *Provider) instanceMemory(ctx context.Context, c *awsec2.Client, region, instanceType string) (int64, error) {
	key := [2]string{region, instanceType}
	if mem, ok := p.memory[key]; ok {
		return mem, nil
	}
	if at, ok := p.memoryFailed[key]; ok && time.Since(at) < memoryRetryAfter {
		return 0, fmt.Errorf("ec2 DescribeInstanceTypes failed %s ago; not asked again before %s", time.Since(at).Round(time.Second), memoryRetryAfter)
	}
	ctx, cancel := context.WithTimeout(ctx, p.memoryTimeout)
	defer cancel()
	out, err := c.DescribeInstanceTypes(ctx, &awsec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
	})
	if err == nil && (len(out.InstanceTypes) != 1 || out.InstanceTypes[0].MemoryInfo == nil || aws.ToInt64(out.InstanceTypes[0].MemoryInfo.SizeInMiB) <= 0) {
		err = errors.New("no memory in the reply")
	}
	if err != nil {
		p.memoryFailed[key] = time.Now()
		return 0, fmt.Errorf("ec2 DescribeInstanceTypes: %w", err)
	}
	delete(p.memoryFailed, key)
	mem := aws.ToInt64(out.InstanceTypes[0].MemoryInfo.SizeInMiB) << 20
	p.memory[key] = mem
	return mem, nil
}

// runnerEnv is env plus what the template decides about every instance's
// runner: it watches for spot interruptions (on-demand instances simply
// never get one), and it offers nested containers only when the template
// opts in.
func runnerEnv(t Template, env map[string]string) map[string]string {
	env = maps.Clone(env)
	if env["LUX_EC2_IMDS"] == "" {
		env["LUX_EC2_IMDS"] = "http://169.254.169.254"
	}
	delete(env, "LUX_NESTED")
	if t.NestedContainers {
		env["LUX_NESTED"] = "true"
	}
	return env
}

// renderUserData builds the instance's user data in the pool's chosen
// format (hostboot.ValidUserData is checked when the pool is set).
func renderUserData(format string, env map[string]string) ([]byte, error) {
	he := hostboot.Env{
		URL: env["LUX_URL"], HostToken: env["LUX_HOST_TOKEN"], HostName: env["LUX_HOST_NAME"],
		EC2IMDS: env["LUX_EC2_IMDS"], Memory: env["LUX_RUNNER_MEMORY"], Nested: env["LUX_NESTED"] == "true",
	}
	return hostboot.Render(format, he)
}

func parse(raw json.RawMessage) (Template, error) {
	var t Template
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &t); err != nil {
			return t, fmt.Errorf("ec2 template: %w", err)
		}
	}
	return t, nil
}
