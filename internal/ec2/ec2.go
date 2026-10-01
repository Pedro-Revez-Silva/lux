// Package ec2 provisions lux hosts as EC2 instances.
//
// A pool's template names what to launch:
//
//	{"region": "eu-west-1", "launchTemplate": "lt-0abc…" (id or name),
//	 "instanceType": "m7i.2xlarge", "subnets": ["subnet-…", …],
//	 "tags": {"team": "platform"}, "spot": true, "userData": "ignition"}
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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/marcioapm/lux/internal/hostboot"
	"github.com/marcioapm/lux/internal/server"
)

// Template is a pool's EC2 settings.
type Template struct {
	Region         string            `json:"region"`
	LaunchTemplate string            `json:"launchTemplate"`
	InstanceType   string            `json:"instanceType"`
	Subnets        []string          `json:"subnets"`
	Tags           map[string]string `json:"tags"`
	Spot           bool              `json:"spot"`
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
	clients  map[string]*awsec2.Client
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
	log           *slog.Logger
}

const (
	memoryLookupTimeout = 5 * time.Second
	memoryRetryAfter    = 10 * time.Minute
)

// New builds the provider. endpoint overrides the EC2 endpoint (tests).
func New(endpoint string, log *slog.Logger) *Provider {
	return &Provider{endpoint: endpoint, clients: map[string]*awsec2.Client{}, next: map[string]int{},
		memory: map[[2]string]int64{}, memoryFailed: map[[2]string]time.Time{}, memoryTimeout: memoryLookupTimeout, log: log}
}

func (p *Provider) client(ctx context.Context, region string) (*awsec2.Client, error) {
	if c := p.clients[region]; c != nil {
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
	c := awsec2.NewFromConfig(cfg, func(o *awsec2.Options) {
		if p.endpoint != "" {
			o.BaseEndpoint = aws.String(p.endpoint)
		}
	})
	p.clients[region] = c
	return c, nil
}

// Launch starts one instance and returns its id, instance type and
// availability zone as RunInstances reports them, and its market (from the
// template's spot). tags are set on it (with the template's own); env is
// the runner's environment, rendered into user data.
func (p *Provider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (server.Launched, error) {
	var none server.Launched
	t, err := parse(template)
	if err != nil {
		return none, err
	}
	if t.LaunchTemplate == "" {
		return none, errors.New("ec2: the pool template needs a launchTemplate")
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return none, err
	}
	env = runnerEnv(t, env)
	// The runner offers the machine's gross memory, which Runs ask in. Only
	// a template naming its type says it before the launch; without it the
	// runner offers its MemTotal.
	if t.InstanceType != "" && env["LUX_RUNNER_MEMORY"] == "" {
		if mem, err := p.instanceMemory(ctx, c, t.Region, t.InstanceType); err != nil {
			p.log.Warn("ec2: instance type memory unknown; the host offers its MemTotal", "instanceType", t.InstanceType, "err", err)
		} else {
			env["LUX_RUNNER_MEMORY"] = strconv.FormatInt(mem, 10)
		}
	}
	ud, err := renderUserData(t.UserData, env)
	if err != nil {
		return none, err
	}
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
	in := &awsec2.RunInstancesInput{
		MinCount:       aws.Int32(1),
		MaxCount:       aws.Int32(1),
		LaunchTemplate: lt,
		UserData:       aws.String(base64.StdEncoding.EncodeToString(ud)),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: instTags},
		},
	}
	if t.Spot {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
			},
		}
	}
	if t.InstanceType != "" {
		in.InstanceType = types.InstanceType(t.InstanceType)
	}
	if len(t.Subnets) > 0 {
		i := p.next[t.LaunchTemplate] % len(t.Subnets)
		p.next[t.LaunchTemplate]++
		in.SubnetId = aws.String(t.Subnets[i])
	}
	out, err := c.RunInstances(ctx, in)
	if err != nil {
		return none, fmt.Errorf("ec2 RunInstances: %w", err)
	}
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
