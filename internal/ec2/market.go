package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/marcioapm/lux/internal/server"
)

var _ server.MarketProvider = (*Provider)(nil)

// MarketScope is a template's region and the instance types it launches:
// instanceType (when set), then fallbackInstanceTypes. No EC2 call.
func (p *Provider) MarketScope(template json.RawMessage) (string, []string, error) {
	t, err := parse(template)
	if err != nil {
		return "", nil, err
	}
	var out []string
	for _, it := range append([]string{t.InstanceType}, t.FallbackInstanceTypes...) {
		if it != "" && !slices.Contains(out, it) {
			out = append(out, it)
		}
	}
	return t.Region, out, nil
}

// Zones are the availability zones a template launches into, by name and
// id, from DescribeSubnets: its subnets' zones, or, with no subnets, the
// zones of the region's default subnets (where RunInstances puts an
// instance without one).
func (p *Provider) Zones(ctx context.Context, template json.RawMessage) ([]server.Zone, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	in := &awsec2.DescribeSubnetsInput{SubnetIds: t.Subnets}
	if len(t.Subnets) == 0 {
		in.Filters = []types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}}
	}
	var out []server.Zone
	pages := awsec2.NewDescribeSubnetsPaginator(c, in, func(o *awsec2.DescribeSubnetsPaginatorOptions) { o.StopOnDuplicateToken = true })
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeSubnets: %w", err)
		}
		for _, sn := range page.Subnets {
			z := server.Zone{Name: aws.ToString(sn.AvailabilityZone), ID: aws.ToString(sn.AvailabilityZoneId)}
			if z.Name != "" && !slices.Contains(out, z) {
				out = append(out, z)
			}
		}
	}
	slices.SortFunc(out, func(a, b server.Zone) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// PlacementScores are EC2's spot placement scores (1-10) for one instance
// of instanceType in each availability zone of region, by zone id: one
// GetSpotPlacementScores with TargetCapacity 1 in units (instances),
// SingleAvailabilityZone and RegionNames [region]. EC2 answers its top 10
// zones; one it leaves out has no score. EC2 documents that a request
// naming fewer than three instance types always scores low, so a score
// here compares one type's zones and hours, not types with a fleet's.
func (p *Provider) PlacementScores(ctx context.Context, region, instanceType string) (map[string]int, error) {
	c, err := p.client(ctx, region)
	if err != nil {
		return nil, err
	}
	pages := awsec2.NewGetSpotPlacementScoresPaginator(c, &awsec2.GetSpotPlacementScoresInput{
		TargetCapacity:         aws.Int32(1),
		TargetCapacityUnitType: types.TargetCapacityUnitTypeUnits,
		InstanceTypes:          []string{instanceType},
		SingleAvailabilityZone: aws.Bool(true),
		RegionNames:            []string{region},
	}, func(o *awsec2.GetSpotPlacementScoresPaginatorOptions) { o.StopOnDuplicateToken = true })
	out := map[string]int{}
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 GetSpotPlacementScores: %w", err)
		}
		for _, s := range page.SpotPlacementScores {
			if id := aws.ToString(s.AvailabilityZoneId); id != "" && s.Score != nil && aws.ToString(s.Region) == region {
				out[id] = int(*s.Score)
			}
		}
	}
	return out, nil
}
