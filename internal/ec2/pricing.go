package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/marcioapm/lux/internal/server"
)

// Prices obtains EC2 on-demand and spot hourly prices.
type Prices struct {
	pricingRegion   string
	pricingEndpoint string
	ec2Endpoint     string

	mu  sync.Mutex
	ec2 map[string]*awsec2.Client
}

// NewPrices constructs a price provider. Empty endpoints use AWS endpoints.
func NewPrices(pricingRegion, pricingEndpoint, ec2Endpoint string) *Prices {
	return &Prices{
		pricingRegion: pricingRegion, pricingEndpoint: pricingEndpoint,
		ec2Endpoint: ec2Endpoint, ec2: make(map[string]*awsec2.Client),
	}
}

func (p *Prices) ec2Client(ctx context.Context, region string) (*awsec2.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.ec2[region]; c != nil {
		return c, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("ec2 pricing configuration: %w", err)
	}
	c := awsec2.NewFromConfig(cfg, func(o *awsec2.Options) {
		if p.ec2Endpoint != "" {
			o.BaseEndpoint = aws.String(p.ec2Endpoint)
		}
	})
	p.ec2[region] = c
	return c, nil
}

// OnDemand returns the Linux shared-tenancy on-demand hourly rate in region.
func (p *Prices) OnDemand(ctx context.Context, region, instanceType string) (server.HourlyRate, error) {
	filters := []struct{ Field, Type, Value string }{
		{"regionCode", "TERM_MATCH", region},
		{"instanceType", "TERM_MATCH", instanceType},
		{"operatingSystem", "TERM_MATCH", "Linux"},
		{"tenancy", "TERM_MATCH", "Shared"},
		{"preInstalledSw", "TERM_MATCH", "NA"},
		{"capacitystatus", "TERM_MATCH", "Used"},
	}
	var price string
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
			return server.HourlyRate{}, err
		}
		for _, product := range out.PriceList {
			if price != "" {
				return server.HourlyRate{}, errors.New("ec2 GetProducts: ambiguous price data")
			}
			var err error
			price, err = onDemandRate(product)
			if err != nil {
				return server.HourlyRate{}, fmt.Errorf("ec2 GetProducts: %w", err)
			}
		}
		if out.NextToken == "" {
			break
		}
		if seen[out.NextToken] {
			return server.HourlyRate{}, errors.New("ec2 GetProducts: repeated pagination token")
		}
		seen[out.NextToken] = true
		token = out.NextToken
	}
	if price == "" {
		return server.HourlyRate{}, errors.New("ec2 GetProducts: no price data")
	}
	return server.HourlyRate{PerHour: price, Currency: "USD"}, nil
}

// getProducts uses the AWS SDK v2 configuration and SigV4 signer. The Pricing
// service is JSON 1.1, so it needs no extra service module in go.mod.
func (p *Prices) getProducts(ctx context.Context, in, out any) error {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.pricingRegion))
	if err != nil {
		return fmt.Errorf("ec2 GetProducts configuration: %w", err)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	endpoint := p.pricingEndpoint
	if endpoint == "" {
		endpoint = "https://api.pricing." + p.pricingRegion + ".amazonaws.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSPriceListService.GetProducts")
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("ec2 GetProducts credentials: %w", err)
	}
	sha := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sha[:]), "pricing", p.pricingRegion, time.Now()); err != nil {
		return fmt.Errorf("ec2 GetProducts signing: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ec2 GetProducts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ec2 GetProducts: HTTP %d: %s", resp.StatusCode, string(message))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("ec2 GetProducts response: %w", err)
	}
	return nil
}

// SpotHistory returns Linux/UNIX spot prices in chronological order.
func (p *Prices) SpotHistory(ctx context.Context, zone, instanceType string, from, to time.Time) ([]server.SpotRate, error) {
	region, err := zoneRegion(zone)
	if err != nil {
		return nil, err
	}
	c, err := p.ec2Client(ctx, region)
	if err != nil {
		return nil, err
	}
	pages := awsec2.NewDescribeSpotPriceHistoryPaginator(c, &awsec2.DescribeSpotPriceHistoryInput{
		AvailabilityZone: aws.String(zone), InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
		ProductDescriptions: []string{"Linux/UNIX"}, StartTime: aws.Time(from), EndTime: aws.Time(to),
	}, func(o *awsec2.DescribeSpotPriceHistoryPaginatorOptions) { o.StopOnDuplicateToken = true })
	var rates []server.SpotRate
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2 DescribeSpotPriceHistory: %w", err)
		}
		for _, price := range page.SpotPriceHistory {
			if price.Timestamp == nil || price.SpotPrice == nil {
				return nil, errors.New("ec2 DescribeSpotPriceHistory: incomplete price data")
			}
			rate, err := decimal(*price.SpotPrice)
			if err != nil {
				return nil, fmt.Errorf("ec2 DescribeSpotPriceHistory: invalid price %q: %w", *price.SpotPrice, err)
			}
			rates = append(rates, server.SpotRate{
				At: *price.Timestamp, HourlyRate: server.HourlyRate{PerHour: rate, Currency: "USD"},
			})
		}
	}
	sort.SliceStable(rates, func(i, j int) bool { return rates[i].At.Before(rates[j].At) })
	return rates, nil
}

func zoneRegion(zone string) (string, error) {
	if len(zone) < 3 || zone[len(zone)-1] < 'a' || zone[len(zone)-1] > 'z' {
		return "", fmt.Errorf("ec2 spot price: invalid availability zone %q", zone)
	}
	region := zone[:len(zone)-1]
	if !strings.Contains(region, "-") || region[len(region)-1] < '0' || region[len(region)-1] > '9' {
		return "", fmt.Errorf("ec2 spot price: invalid availability zone %q", zone)
	}
	return region, nil
}

func decimal(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty decimal")
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '.' && i > 0 && i < len(s)-1 && strings.Count(s, ".") == 1 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return "", errors.New("not a decimal number")
		}
	}
	if _, ok := new(big.Rat).SetString(s); !ok {
		return "", errors.New("not a decimal number")
	}
	return s, nil
}

func onDemandRate(product string) (string, error) {
	var doc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string            `json:"unit"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(product), &doc); err != nil {
		return "", fmt.Errorf("invalid price data: %w", err)
	}
	var usd []string
	for _, term := range doc.Terms.OnDemand {
		for _, dimension := range term.PriceDimensions {
			if dimension.Unit == "Hrs" {
				if value, ok := dimension.PricePerUnit["USD"]; ok {
					usd = append(usd, value)
				}
			}
		}
	}
	if len(usd) == 0 {
		return "", errors.New("missing USD hourly on-demand price")
	}
	if len(usd) != 1 {
		return "", errors.New("ambiguous USD hourly on-demand price")
	}
	if _, err := decimal(usd[0]); err != nil {
		return "", fmt.Errorf("invalid USD hourly on-demand price %q: %w", usd[0], err)
	}
	return usd[0], nil
}
