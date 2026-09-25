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
	"maps"
	"strings"

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
}

type Provider struct {
	endpoint string
	clients  map[string]*awsec2.Client
	// next subnet per pool: launches spread across the pool's subnets.
	next map[string]int
}

// New builds the provider. endpoint overrides the EC2 endpoint (tests).
func New(endpoint string) *Provider {
	return &Provider{endpoint: endpoint, clients: map[string]*awsec2.Client{}, next: map[string]int{}}
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

// Launch starts one instance and returns its id. tags are set on it
// (with the template's own); env is the runner's environment, passed as
// user data (KEY=value lines).
func (p *Provider) Launch(ctx context.Context, template json.RawMessage, tags, env map[string]string) (string, error) {
	t, err := parse(template)
	if err != nil {
		return "", err
	}
	if t.LaunchTemplate == "" {
		return "", errors.New("ec2: the pool template needs a launchTemplate")
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return "", err
	}
	// Every instance's runner watches for spot interruptions (on-demand
	// instances simply never get one).
	env = maps.Clone(env)
	if env["LUX_EC2_IMDS"] == "" {
		env["LUX_EC2_IMDS"] = "http://169.254.169.254"
	}
	ud, err := renderUserData(t.UserData, env)
	if err != nil {
		return "", err
	}
	lt := &types.LaunchTemplateSpecification{Version: aws.String("$Default")}
	if strings.HasPrefix(t.LaunchTemplate, "lt-") {
		lt.LaunchTemplateId = aws.String(t.LaunchTemplate)
	} else {
		lt.LaunchTemplateName = aws.String(t.LaunchTemplate)
	}
	var instTags []types.Tag
	for _, m := range []map[string]string{t.Tags, tags} {
		for k, v := range m {
			instTags = append(instTags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
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
		return "", fmt.Errorf("ec2 RunInstances: %w", err)
	}
	if len(out.Instances) != 1 || out.Instances[0].InstanceId == nil {
		return "", errors.New("ec2 RunInstances: no instance in the reply")
	}
	return *out.Instances[0].InstanceId, nil
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

// renderUserData builds the instance's user data in the pool's chosen
// format (hostboot.ValidUserData is checked when the pool is set).
func renderUserData(format string, env map[string]string) ([]byte, error) {
	he := hostboot.Env{URL: env["LUX_URL"], HostToken: env["LUX_HOST_TOKEN"], HostName: env["LUX_HOST_NAME"], EC2IMDS: env["LUX_EC2_IMDS"]}
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
