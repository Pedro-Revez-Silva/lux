package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/server"
)

// parseTransferPrices reads --price-transfer: comma-separated
// class=price per GB, class one of internet, zone, region-public or
// provider:<region>. zone and region-public set both directions (the
// provider bills each side), internet and provider:<region> outbound only
// (inbound is free); class.rx= or class.tx= sets one direction. publicIP
// is --price-public-ip: the host's public IPv4 charge per hour.
func parseTransferPrices(spec, publicIP, currency string) (*server.DataTransferPrices, error) {
	if spec == "" && publicIP == "" {
		return nil, nil
	}
	if currency == "" {
		return nil, fmt.Errorf("--price-transfer and --price-public-ip need --currency")
	}
	p := &server.DataTransferPrices{Currency: currency, PublicIPv4PerHour: publicIP}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, price, ok := strings.Cut(part, "=")
		if !ok || price == "" {
			return nil, fmt.Errorf("--price-transfer %q: want class=price", part)
		}
		class, dir, _ := strings.Cut(key, ".")
		if dir != "" && dir != "rx" && dir != "tx" {
			return nil, fmt.Errorf("--price-transfer %q: direction is rx or tx", part)
		}
		set := func(g server.PerGB, outboundOnly bool) server.PerGB {
			switch {
			case dir == "rx":
				g.Rx = price
			case dir == "tx" || outboundOnly:
				g.Tx = price
			default:
				g.Rx, g.Tx = price, price
			}
			return g
		}
		switch region, isProvider := strings.CutPrefix(class, "provider:"); {
		case class == "internet":
			p.Internet = set(p.Internet, true)
		case class == "zone":
			p.Zone = set(p.Zone, false)
		case class == "region-public":
			p.RegionPublic = set(p.RegionPublic, false)
		case isProvider && region != "":
			if p.Provider == nil {
				p.Provider = map[string]server.PerGB{}
			}
			p.Provider[region] = set(p.Provider[region], true)
		default:
			return nil, fmt.Errorf("--price-transfer %q: class is internet, zone, region-public or provider:<region>", part)
		}
	}
	return p, nil
}

// splitCIDRs reads a comma-separated --network-local or --network-zone.
func splitCIDRs(v string) []string {
	return slices.DeleteFunc(strings.Split(v, ","), func(s string) bool { return strings.TrimSpace(s) == "" })
}

// transferPriceLines are p as "class direction price" lines, for hosts get.
func transferPriceLines(p *server.DataTransferPrices) []string {
	var out []string
	add := func(class string, g server.PerGB) {
		for _, d := range []struct{ dir, v string }{{"rx", g.Rx}, {"tx", g.Tx}} {
			if d.v != "" {
				out = append(out, fmt.Sprintf("%-22s %s  %s %s/GB", class, d.dir, d.v, p.Currency))
			}
		}
	}
	add("internet", p.Internet)
	add("zone", p.Zone)
	add("region-public", p.RegionPublic)
	regions := make([]string, 0, len(p.Provider))
	for r := range p.Provider {
		regions = append(regions, r)
	}
	slices.Sort(regions)
	for _, r := range regions {
		add("provider:"+r, p.Provider[r])
	}
	if p.PublicIPv4PerHour != "" {
		out = append(out, fmt.Sprintf("%-22s     %s %s/h", "public IPv4", p.PublicIPv4PerHour, p.Currency))
	}
	return out
}
