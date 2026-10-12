package ec2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/marcioapm/lux/internal/server"
)

var _ server.CatalogProvider = (*Provider)(nil)

// catalogMaxAge is how old a cached description the catalog accepts.
const catalogMaxAge = 24 * time.Hour

// typeEntry is one instance type's description and when EC2 gave it.
type typeEntry struct {
	info server.InstanceTypeInfo
	at   time.Time
}

// InstanceTypes describes instanceTypes in region: from what this process
// learned within catalogMaxAge (a launch's memory lookup included), the
// rest from DescribeInstanceTypes, up to 100 types per call. A type EC2
// does not offer there is absent from the answer.
func (p *Provider) InstanceTypes(ctx context.Context, region string, instanceTypes []string) ([]server.InstanceTypeInfo, error) {
	var out []server.InstanceTypeInfo
	var ask []string
	p.typesMu.Lock()
	for _, it := range instanceTypes {
		if e, ok := p.types[[2]string{region, it}]; ok && time.Since(e.at) < catalogMaxAge {
			out = append(out, e.info)
		} else {
			ask = append(ask, it)
		}
	}
	p.typesMu.Unlock()
	if len(ask) == 0 {
		return out, nil
	}
	c, err := p.client(ctx, region)
	if err != nil {
		return nil, err
	}
	for chunk := range slices.Chunk(ask, 100) {
		infos, err := describeInstanceTypes(ctx, c, chunk)
		if err != nil {
			return nil, err
		}
		p.typesMu.Lock()
		for _, info := range infos {
			p.types[[2]string{region, info.Name}] = typeEntry{info, time.Now()}
		}
		p.typesMu.Unlock()
		out = append(out, infos...)
	}
	return out, nil
}

// describeInstanceTypes is one DescribeInstanceTypes (with its pages) for
// names, as lux records them.
func describeInstanceTypes(ctx context.Context, c *awsec2.Client, names []string) ([]server.InstanceTypeInfo, error) {
	in := &awsec2.DescribeInstanceTypesInput{}
	for _, n := range names {
		in.InstanceTypes = append(in.InstanceTypes, types.InstanceType(n))
	}
	var out []server.InstanceTypeInfo
	pages := awsec2.NewDescribeInstanceTypesPaginator(c, in, func(o *awsec2.DescribeInstanceTypesPaginatorOptions) { o.StopOnDuplicateToken = true })
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeInstanceTypes: %w", err)
		}
		for _, it := range page.InstanceTypes {
			out = append(out, instanceTypeInfo(it))
		}
	}
	return out, nil
}

func instanceTypeInfo(it types.InstanceTypeInfo) server.InstanceTypeInfo {
	info := server.InstanceTypeInfo{Name: string(it.InstanceType), Burstable: it.BurstablePerformanceSupported}
	if it.VCpuInfo != nil && it.VCpuInfo.DefaultVCpus != nil {
		v := int(*it.VCpuInfo.DefaultVCpus)
		info.VCPUs = &v
	}
	if it.MemoryInfo != nil {
		info.MemoryBytes = aws.ToInt64(it.MemoryInfo.SizeInMiB) << 20
	}
	if pi := it.ProcessorInfo; pi != nil {
		for _, a := range pi.SupportedArchitectures {
			info.Architectures = append(info.Architectures, string(a))
		}
		nested := slices.Contains(pi.SupportedFeatures, types.SupportedAdditionalProcessorFeatureNestedVirtualization)
		info.NestedVirtualization = &nested
	}
	if ni := it.NetworkInfo; ni != nil {
		// The sum over the network cards: what the instance can sustain.
		var gbps float64
		for _, card := range ni.NetworkCards {
			gbps += aws.ToFloat64(card.BaselineBandwidthInGbps)
		}
		if gbps > 0 {
			info.NetworkBaselineGbps = &gbps
		}
		if ni.NetworkPerformance != nil {
			info.NetworkPerformance = *ni.NetworkPerformance
		}
	}
	return info
}
