package ec2

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/server"
)

type subnetEntry struct {
	subnets map[string][]string
	all     []server.NetworkSubnet
	at      time.Time
}

// CacheNetworkFor sets the VPC subnet refresh interval; zero disables caching.
func (p *Provider) CacheNetworkFor(d time.Duration) {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	p.subnetMaxAge = d
}

func (p *Provider) Network(ctx context.Context, template json.RawMessage, providerIDs []string) (map[string]server.HostNetwork, error) {
	t, err := parse(template)
	if err != nil {
		return nil, err
	}
	out := map[string]server.HostNetwork{}
	if len(providerIDs) == 0 {
		return out, nil
	}
	c, err := p.client(ctx, t.Region)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range providerIDs {
		want[id] = true
	}
	pages := awsec2.NewDescribeInstancesPaginator(c, &awsec2.DescribeInstancesInput{
		Filters: []types.Filter{{Name: aws.String("instance-id"), Values: providerIDs}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeInstances network: %w", err)
		}
		for _, reservation := range page.Reservations {
			for _, inst := range reservation.Instances {
				id := aws.ToString(inst.InstanceId)
				if !want[id] {
					continue
				}
				pairs := attachedSubnets(inst)
				n := server.HostNetwork{SubnetsUnavailable: len(pairs) == 0}
				refreshed := map[string]bool{}
				for pair := range pairs {
					vpc, subnet := pair[0], pair[1]
					entry, err := p.vpcSubnets(ctx, c, t.Region, vpc)
					// A subnet the cached VPC entry lacks may be newer than it: refetch once.
					if err == nil && len(entry.subnets[subnet]) == 0 && !refreshed[vpc] {
						refreshed[vpc] = true
						p.networkMu.Lock()
						delete(p.subnetCache, [2]string{t.Region, vpc})
						p.networkMu.Unlock()
						entry, err = p.vpcSubnets(ctx, c, t.Region, vpc)
					}
					if err != nil {
						n.SubnetsUnavailable = true
						continue
					}
					if len(entry.subnets[subnet]) == 0 {
						n.SubnetsUnavailable = true
					}
					n.SubnetCIDRs = append(n.SubnetCIDRs, entry.subnets[subnet]...)
					n.VPCSubnets = append(n.VPCSubnets, entry.all...)
				}
				n.SubnetCIDRs = uniquePrefixes(n.SubnetCIDRs)
				slices.SortFunc(n.VPCSubnets, compareSubnets)
				n.VPCSubnets = slices.Compact(n.VPCSubnets)
				if inst.Placement != nil {
					n.Zone = aws.ToString(inst.Placement.AvailabilityZone)
				}
				n.PublicIPv4 = publicIPv4(inst)
				out[id] = n
			}
		}
	}
	return out, nil
}

// attachedSubnets are the instance's distinct (VPC, subnet) pairs over its
// network interfaces, or its primary pair when no interface names one.
func attachedSubnets(inst types.Instance) map[[2]string]bool {
	pairs := map[[2]string]bool{}
	add := func(vpc, subnet *string) {
		if v, s := aws.ToString(vpc), aws.ToString(subnet); v != "" && s != "" {
			pairs[[2]string{v, s}] = true
		}
	}
	for _, eni := range inst.NetworkInterfaces {
		add(eni.VpcId, eni.SubnetId)
	}
	if len(pairs) == 0 {
		add(inst.VpcId, inst.SubnetId)
	}
	return pairs
}

// publicIPv4 counts distinct public addresses across all ENIs, including
// secondary IPs.
func publicIPv4(inst types.Instance) int {
	public := map[string]bool{}
	add := func(ip *string) {
		if s := aws.ToString(ip); s != "" {
			public[s] = true
		}
	}
	add(inst.PublicIpAddress)
	for _, eni := range inst.NetworkInterfaces {
		if eni.Association != nil {
			add(eni.Association.PublicIp)
		}
		for _, addr := range eni.PrivateIpAddresses {
			if addr.Association != nil {
				add(addr.Association.PublicIp)
			}
		}
	}
	return len(public)
}

func (p *Provider) vpcSubnets(ctx context.Context, c *awsec2.Client, region, vpc string) (subnetEntry, error) {
	p.networkMu.Lock()
	defer p.networkMu.Unlock()
	key := [2]string{region, vpc}
	if e, ok := p.subnetCache[key]; ok && p.now().Sub(e.at) < p.subnetMaxAge {
		return e, nil
	}
	e := subnetEntry{subnets: map[string][]string{}}
	pages := awsec2.NewDescribeSubnetsPaginator(c, &awsec2.DescribeSubnetsInput{
		Filters: []types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpc}}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return subnetEntry{}, fmt.Errorf("ec2 DescribeSubnets: %w", err)
		}
		for _, sn := range page.Subnets {
			cidrs := []string{}
			if cidr := aws.ToString(sn.CidrBlock); cidr != "" {
				cidrs = append(cidrs, cidr)
			}
			for _, a := range sn.Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState != nil && a.Ipv6CidrBlockState.State == types.SubnetCidrBlockStateCodeAssociated {
					if cidr := aws.ToString(a.Ipv6CidrBlock); cidr != "" {
						cidrs = append(cidrs, cidr)
					}
				}
			}
			slices.Sort(cidrs)
			e.subnets[aws.ToString(sn.SubnetId)] = cidrs
			for _, cidr := range cidrs {
				e.all = append(e.all, server.NetworkSubnet{CIDR: cidr, Zone: aws.ToString(sn.AvailabilityZone)})
			}
		}
	}
	slices.SortFunc(e.all, compareSubnets)
	e.at = p.now()
	if p.subnetCache == nil {
		p.subnetCache = map[[2]string]subnetEntry{}
	}
	p.subnetCache[key] = e
	return e, nil
}

func compareSubnets(a, b server.NetworkSubnet) int {
	return cmp.Or(cmp.Compare(a.CIDR, b.CIDR), cmp.Compare(a.Zone, b.Zone))
}

type ipRange struct {
	IPv4    string `json:"ip_prefix"`
	IPv6    string `json:"ipv6_prefix"`
	Region  string `json:"region"`
	Service string `json:"service"`
}

func (r ipRange) cidr() string { return cmp.Or(r.IPv4, r.IPv6) }

type ipRanges struct {
	Prefixes     []ipRange `json:"prefixes"`
	IPv6Prefixes []ipRange `json:"ipv6_prefixes"`
}

// Only same-region S3 and DynamoDB traffic is classified as free service traffic.
var freeRegionalServices = map[string]bool{"S3": true, "DYNAMODB": true}

// freeGlobalServices are the GLOBAL services EC2 sends to at no charge:
// CloudFront (origin fetches from AWS origins are free). Every other GLOBAL
// prefix (Route 53, Global Accelerator, AMAZON) bills as internet out.
var freeGlobalServices = map[string]bool{"CLOUDFRONT": true}

func (p *Provider) Prefixes(ctx context.Context, template json.RawMessage) (server.ProviderPrefixes, error) {
	t, err := parse(template)
	if err != nil {
		return server.ProviderPrefixes{}, err
	}
	p.prefixMu.Lock()
	defer p.prefixMu.Unlock()
	if p.prefixChecked.IsZero() || p.now().Sub(p.prefixChecked) >= 24*time.Hour {
		err = p.fetchPrefixes(ctx)
		if err != nil && !p.prefixGood {
			return server.ProviderPrefixes{}, err
		}
	}
	if !p.prefixGood {
		return server.ProviderPrefixes{}, fmt.Errorf("AWS IP ranges: no validated catalog available")
	}
	out := server.ProviderPrefixes{Source: "ec2-ip-ranges", FetchedAt: p.prefixFetched, Provider: map[string][]string{}}
	for _, r := range append(slices.Clone(p.prefixCatalog.Prefixes), p.prefixCatalog.IPv6Prefixes...) {
		cidr := r.cidr()
		switch {
		case r.Region == "GLOBAL" && freeGlobalServices[r.Service]:
			out.ProviderGlobalFree = append(out.ProviderGlobalFree, cidr)
		case r.Region == "GLOBAL":
			out.ProviderGlobal = append(out.ProviderGlobal, cidr)
		case r.Region != t.Region:
			out.Provider[r.Region] = append(out.Provider[r.Region], cidr)
		case freeRegionalServices[r.Service]:
			out.RegionService = append(out.RegionService, cidr)
		default:
			out.RegionPublic = append(out.RegionPublic, cidr)
		}
	}
	for r, list := range out.Provider {
		out.Provider[r] = uniquePrefixes(list)
	}
	out.ProviderGlobalFree = uniquePrefixes(out.ProviderGlobalFree)
	// CloudFront's prefixes are repeated under GLOBAL AMAZON too; free wins
	// (the runner's precedence would pick it anyway; this keeps the lists
	// apart).
	out.ProviderGlobal = slices.DeleteFunc(uniquePrefixes(out.ProviderGlobal), func(cidr string) bool {
		_, free := slices.BinarySearch(out.ProviderGlobalFree, cidr)
		return free
	})
	out.RegionService = uniquePrefixes(out.RegionService)
	// AWS repeats service prefixes under AMAZON; free classification wins.
	out.RegionPublic = slices.DeleteFunc(uniquePrefixes(out.RegionPublic), func(cidr string) bool {
		_, free := slices.BinarySearch(out.RegionService, cidr)
		return free
	})
	return out, nil
}

func uniquePrefixes(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}

func (p *Provider) fetchPrefixes(ctx context.Context) error {
	url := os.Getenv("LUX_IPRANGES_URL")
	if url == "" {
		url = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if p.prefixETag != "" {
		req.Header.Set("If-None-Match", p.prefixETag)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AWS IP ranges: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified && p.prefixGood {
		p.prefixChecked = p.now()
		return nil
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("AWS IP ranges: HTTP %d", res.StatusCode)
	}
	const maxBytes = 16 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("AWS IP ranges exceeds %d bytes", maxBytes)
	}
	var catalog ipRanges
	if err := json.Unmarshal(data, &catalog); err != nil {
		return err
	}
	if len(catalog.Prefixes)+len(catalog.IPv6Prefixes) == 0 {
		return fmt.Errorf("AWS IP ranges: empty catalog")
	}
	for _, r := range append(slices.Clone(catalog.Prefixes), catalog.IPv6Prefixes...) {
		cidr := r.cidr()
		if _, err := netip.ParsePrefix(cidr); err != nil || (r.Region != "GLOBAL" && !proto.ValidNetworkRegion(r.Region)) || r.Service == "" {
			return fmt.Errorf("AWS IP ranges: invalid entry %q", cidr)
		}
	}
	p.prefixCatalog, p.prefixETag, p.prefixGood = catalog, res.Header.Get("ETag"), true
	p.prefixFetched = p.now()
	p.prefixChecked = p.now()
	return nil
}
