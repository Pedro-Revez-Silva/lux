package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/marcioapm/lux/internal/server"
)

// DataTransfer returns region's transfer list prices from the Pricing API:
// service AWSDataTransfer, filtered by fromRegionCode and transferType, and
// the public IPv4 address charge from AmazonVPC.
//
//   - "AWS Outbound" (to External): EC2 to the internet. Tiered by monthly
//     volume; the first non-zero tier is taken (list price: the free tier is
//     account-wide). Inbound from the internet is free.
//   - "IntraRegion": in-region transfer between zones or through public
//     addresses (DataTransfer-Regional-Bytes), charged on each side, so in
//     and out each; also what region-public costs.
//   - "InterRegion Outbound" to an AWS Region (toRegionCode): per
//     destination; inbound is free.
//   - AmazonVPC group VPCPublicIPv4Address, usagetype *PublicIPv4:InUseAddress:
//     per in-use public IPv4 address per hour.
func (p *Prices) DataTransfer(ctx context.Context, region string) (server.DataTransferPrices, error) {
	out := server.DataTransferPrices{Currency: "USD", Provider: map[string]server.PerGB{}}
	transfer := func(transferType string, visit func(attrs map[string]string, perGB string) error) error {
		filters := []struct{ Field, Type, Value string }{
			{"fromRegionCode", "TERM_MATCH", region},
			{"transferType", "TERM_MATCH", transferType},
		}
		return p.eachProduct(ctx, "AWSDataTransfer", filters, func(doc productDoc) error {
			v, err := doc.firstPaidTier("GB")
			if err != nil {
				return fmt.Errorf("%s %v: %w", transferType, doc.Product.Attributes["toRegionCode"], err)
			}
			return visit(doc.Product.Attributes, v)
		})
	}
	err := transfer("AWS Outbound", func(a map[string]string, v string) error {
		if a["toLocation"] != "External" {
			return nil
		}
		if out.Internet.Tx != "" {
			return errors.New("AWS Outbound: ambiguous price")
		}
		out.Internet.Tx = v
		return nil
	})
	if err == nil {
		err = transfer("IntraRegion", func(a map[string]string, v string) error {
			if out.Zone.Tx != "" {
				return errors.New("IntraRegion: ambiguous price")
			}
			out.Zone = server.PerGB{Rx: v, Tx: v}
			out.RegionPublic = out.Zone
			return nil
		})
	}
	if err == nil {
		err = transfer("InterRegion Outbound", func(a map[string]string, v string) error {
			to := a["toRegionCode"]
			if a["toLocationType"] != "AWS Region" || to == "" || to == region {
				return nil
			}
			if _, dup := out.Provider[to]; dup {
				return fmt.Errorf("InterRegion Outbound to %s: ambiguous price", to)
			}
			out.Provider[to] = server.PerGB{Tx: v}
			return nil
		})
	}
	if err == nil {
		filters := []struct{ Field, Type, Value string }{
			{"regionCode", "TERM_MATCH", region},
			{"group", "TERM_MATCH", "VPCPublicIPv4Address"},
		}
		err = p.eachProduct(ctx, "AmazonVPC", filters, func(doc productDoc) error {
			if !strings.HasSuffix(doc.Product.Attributes["usagetype"], "PublicIPv4:InUseAddress") {
				return nil
			}
			v, err := doc.firstPaidTier("Hrs")
			if err != nil {
				return fmt.Errorf("public IPv4: %w", err)
			}
			if out.PublicIPv4PerHour != "" {
				return errors.New("public IPv4: ambiguous price")
			}
			out.PublicIPv4PerHour = v
			return nil
		})
	}
	if err != nil {
		return server.DataTransferPrices{}, fmt.Errorf("ec2 GetProducts data transfer %s: %w", region, err)
	}
	if out.Internet.Tx == "" {
		return server.DataTransferPrices{}, fmt.Errorf("ec2 GetProducts data transfer %s: no internet price", region)
	}
	return out, nil
}

// productDoc is one Pricing API PriceList entry.
type productDoc struct {
	Product struct {
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				BeginRange   string            `json:"beginRange"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// firstPaidTier is the USD price of the lowest tier (by beginRange) whose
// price is not zero, or "0" when every tier is free (a free transfer type,
// such as EC2 to CloudFront).
func (d productDoc) firstPaidTier(unit string) (string, error) {
	type tier struct {
		begin *big.Rat
		price string
	}
	var tiers []tier
	for _, term := range d.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if dim.Unit != unit {
				continue
			}
			v, ok := dim.PricePerUnit["USD"]
			if !ok {
				continue
			}
			v, err := decimal(v)
			if err != nil {
				return "", fmt.Errorf("invalid USD price %q: %w", dim.PricePerUnit["USD"], err)
			}
			begin, ok := new(big.Rat).SetString(dim.BeginRange)
			if dim.BeginRange == "" {
				begin, ok = new(big.Rat), true
			}
			if !ok {
				return "", fmt.Errorf("invalid beginRange %q", dim.BeginRange)
			}
			tiers = append(tiers, tier{begin, v})
		}
	}
	if len(tiers) == 0 {
		return "", fmt.Errorf("missing USD per-%s price", unit)
	}
	var best *tier
	for i := range tiers {
		t := &tiers[i]
		if mustRat(t.price).Sign() == 0 {
			continue
		}
		if best != nil && best.begin.Cmp(t.begin) == 0 {
			return "", fmt.Errorf("two USD per-%s prices from %s", unit, t.begin.RatString())
		}
		if best == nil || t.begin.Cmp(best.begin) < 0 {
			best = t
		}
	}
	if best == nil {
		return "0", nil
	}
	return best.price, nil
}

func mustRat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

// eachProduct pages GetProducts on service with filters, decoding each
// product.
func (p *Prices) eachProduct(ctx context.Context, service string, filters any, visit func(productDoc) error) error {
	var token string
	seen := map[string]bool{}
	for {
		in := struct {
			ServiceCode string `json:"ServiceCode"`
			Filters     any    `json:"Filters"`
			NextToken   string `json:"NextToken,omitempty"`
		}{service, filters, token}
		var out struct {
			PriceList []string `json:"PriceList"`
			NextToken string   `json:"NextToken"`
		}
		if err := p.getProducts(ctx, in, &out); err != nil {
			return err
		}
		for _, product := range out.PriceList {
			var doc productDoc
			if err := json.Unmarshal([]byte(product), &doc); err != nil {
				return fmt.Errorf("invalid price data: %w", err)
			}
			if err := visit(doc); err != nil {
				return err
			}
		}
		if out.NextToken == "" {
			return nil
		}
		if seen[out.NextToken] {
			return errors.New("repeated pagination token")
		}
		seen[out.NextToken] = true
		token = out.NextToken
	}
}
