package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/marcioapm/lux/internal/server"
)

// BlockStorage returns an EBS volume type's monthly list prices in region,
// from the Pricing API (AmazonEC2, regionCode, volumeApiName): the Storage
// product's GB-Mo, the System Operation product of group "EBS IOPS"
// (IOPS-Mo), and the Provisioned Throughput product (GiBps-mo: per GiB/s,
// not MiB/s). A dimension the type does not have is left empty. A tiered
// dimension (io2's IOPS) is priced at its first tier.
func (p *Prices) BlockStorage(ctx context.Context, region, volumeType string) (server.BlockStoragePrice, error) {
	filters := []struct{ Field, Type, Value string }{
		{"regionCode", "TERM_MATCH", region},
		{"volumeApiName", "TERM_MATCH", volumeType},
	}
	var price server.BlockStoragePrice
	var token string
	seen := map[string]bool{}
	for {
		in := struct {
			ServiceCode string `json:"ServiceCode"`
			Filters     any    `json:"Filters"`
			NextToken   string `json:"NextToken,omitempty"`
		}{"AmazonEC2", filters, token}
		var out struct {
			PriceList []string `json:"PriceList"`
			NextToken string   `json:"NextToken"`
		}
		if err := p.getProducts(ctx, in, &out); err != nil {
			return server.BlockStoragePrice{}, err
		}
		for _, product := range out.PriceList {
			if err := ebsProduct(product, &price); err != nil {
				return server.BlockStoragePrice{}, fmt.Errorf("ec2 GetProducts %s %s: %w", region, volumeType, err)
			}
		}
		if out.NextToken == "" {
			break
		}
		if seen[out.NextToken] {
			return server.BlockStoragePrice{}, errors.New("ec2 GetProducts: repeated pagination token")
		}
		seen[out.NextToken] = true
		token = out.NextToken
	}
	if price.PerGBMonth == "" {
		return server.BlockStoragePrice{}, fmt.Errorf("ec2 GetProducts %s %s: no storage price", region, volumeType)
	}
	price.Currency = "USD"
	return price, nil
}

// ebsProduct sets the dimension one Pricing API product prices, if it is one
// of the three; any other product of the volume type (I/O requests,
// snapshots) is ignored. A dimension priced twice is ambiguous.
func ebsProduct(product string, price *server.BlockStoragePrice) error {
	var doc struct {
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
	if err := json.Unmarshal([]byte(product), &doc); err != nil {
		return fmt.Errorf("invalid price data: %w", err)
	}
	var unit string
	var dest *string
	switch fam := doc.Product.ProductFamily; {
	case fam == "Storage":
		unit, dest = "GB-Mo", &price.PerGBMonth
	case fam == "System Operation" && doc.Product.Attributes["group"] == "EBS IOPS":
		unit, dest = "IOPS-Mo", &price.PerIOPSMonth
	case fam == "Provisioned Throughput":
		unit, dest = "GiBps-mo", &price.PerGiBpsMonth
	default:
		return nil
	}
	var usd []string
	for _, term := range doc.Terms.OnDemand {
		for _, d := range term.PriceDimensions {
			if !strings.EqualFold(d.Unit, unit) || (d.BeginRange != "" && d.BeginRange != "0") {
				continue
			}
			if v, ok := d.PricePerUnit["USD"]; ok {
				usd = append(usd, v)
			}
		}
	}
	switch {
	case len(usd) == 0:
		return fmt.Errorf("missing USD %s price", unit)
	case len(usd) > 1 || *dest != "":
		return fmt.Errorf("ambiguous USD %s price", unit)
	}
	v, err := decimal(usd[0])
	if err != nil {
		return fmt.Errorf("invalid USD %s price %q: %w", unit, usd[0], err)
	}
	*dest = v
	return nil
}
