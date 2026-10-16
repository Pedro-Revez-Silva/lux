package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// transferFixture serves the recorded eu-north-1 answers
// (testdata/datatransfer-pricing-eu-north-1.json: the AWSDataTransfer and
// AmazonVPC offer files' products, as GetProducts returns them, paged as
// AWS pages them at 100) by service and transferType.
func transferFixture(t *testing.T, edit func(key string, page map[string]any)) (*httptest.Server, *[]string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/datatransfer-pricing-eu-north-1.json")
	if err != nil {
		t.Fatal(err)
	}
	var pages map[string][]map[string]any
	if err := json.Unmarshal(raw, &pages); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ServiceCode string
			Filters     []struct{ Field, Type, Value string }
			NextToken   string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		f := map[string]string{}
		var parts []string
		for _, x := range in.Filters {
			f[x.Field] = x.Value
			parts = append(parts, x.Field+"="+x.Value)
		}
		key := f["transferType"]
		if in.ServiceCode == "AmazonVPC" {
			key = "AmazonVPC"
		}
		mu.Lock()
		if in.NextToken == "" {
			asked = append(asked, in.ServiceCode+" "+strings.Join(parts, " "))
		}
		mu.Unlock()
		i := 0
		fmt.Sscanf(in.NextToken, "p%d", &i)
		list := pages[key]
		page := map[string]any{"PriceList": []any{}, "FormatVersion": "aws_v1"}
		if (f["fromRegionCode"] == "eu-north-1" || f["regionCode"] == "eu-north-1") && i < len(list) {
			page = list[i]
			if i+1 < len(list) {
				page["NextToken"] = fmt.Sprintf("p%d", i+1)
			}
		}
		if edit != nil {
			edit(key, page)
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(fake.Close)
	return fake, &asked
}

// The recorded answer: internet out at its first tier (0.09; later tiers
// 0.085, 0.07, 0.05 are not used), IntraRegion both ways, InterRegion per
// destination region (Ireland 0.02, Mexico 0.05; Local Zones and edge
// locations skipped), the in-use public IPv4 hourly charge (not the idle
// or contiguous-block one).
func TestPricesDataTransfer(t *testing.T) {
	awsTestEnv(t)
	fake, asked := transferFixture(t, nil)
	p := NewPrices("us-east-1", fake.URL, "")
	got, err := p.DataTransfer(context.Background(), "eu-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != "USD" || got.Internet != (server.PerGB{Tx: "0.0900000000"}) {
		t.Errorf("internet %+v %s", got.Internet, got.Currency)
	}
	intra := server.PerGB{Rx: "0.0100000000", Tx: "0.0100000000"}
	if got.Zone != intra || got.RegionPublic != intra {
		t.Errorf("zone %+v region-public %+v", got.Zone, got.RegionPublic)
	}
	if got.Provider["eu-west-1"] != (server.PerGB{Tx: "0.0200000000"}) || got.Provider["mx-central-1"] != (server.PerGB{Tx: "0.0500000000"}) {
		t.Errorf("inter-region %+v", got.Provider)
	}
	if _, ok := got.Provider["us-west-2-lax-1"]; !ok {
		t.Error("an AWS Region-typed destination (Los Angeles) was skipped")
	}
	if _, ok := got.Provider["eu-north-1"]; ok {
		t.Error("the source region priced as inter-region")
	}
	for r := range got.Provider {
		if r == "" {
			t.Error("an edge location (no toRegionCode) priced as a region")
		}
	}
	if len(got.Provider) != 37 {
		t.Errorf("%d destination regions, want the fixture's 37", len(got.Provider))
	}
	if got.PublicIPv4PerHour != "0.0050000000" {
		t.Errorf("public IPv4 %q", got.PublicIPv4PerHour)
	}
	want := []string{
		"AWSDataTransfer fromRegionCode=eu-north-1 transferType=AWS Outbound",
		"AWSDataTransfer fromRegionCode=eu-north-1 transferType=IntraRegion",
		"AWSDataTransfer fromRegionCode=eu-north-1 transferType=InterRegion Outbound",
		"AmazonVPC regionCode=eu-north-1 group=VPCPublicIPv4Address",
	}
	if !reflect.DeepEqual(*asked, want) {
		t.Errorf("requests %q", *asked)
	}
}

// A zero first tier (a promotional free tier in the list) is skipped for
// the first non-zero one.
func TestFirstPaidTier(t *testing.T) {
	doc := productDoc{}
	doc.Terms.OnDemand = map[string]struct {
		PriceDimensions map[string]struct {
			Unit         string            `json:"unit"`
			BeginRange   string            `json:"beginRange"`
			PricePerUnit map[string]string `json:"pricePerUnit"`
		} `json:"priceDimensions"`
	}{"t": {PriceDimensions: map[string]struct {
		Unit         string            `json:"unit"`
		BeginRange   string            `json:"beginRange"`
		PricePerUnit map[string]string `json:"pricePerUnit"`
	}{
		"a": {"GB", "0", map[string]string{"USD": "0.0000000000"}},
		"b": {"GB", "100", map[string]string{"USD": "0.0900000000"}},
		"c": {"GB", "10240", map[string]string{"USD": "0.0850000000"}},
	}}}
	if v, err := doc.firstPaidTier("GB"); err != nil || v != "0.0900000000" {
		t.Errorf("got %q %v", v, err)
	}
}

// No internet price is an error, never a zero price.
func TestPricesDataTransferMissingInternet(t *testing.T) {
	awsTestEnv(t)
	fake, _ := transferFixture(t, func(key string, page map[string]any) {
		if key == "AWS Outbound" {
			page["PriceList"] = []any{}
		}
	})
	if _, err := NewPrices("us-east-1", fake.URL, "").DataTransfer(context.Background(), "eu-north-1"); err == nil {
		t.Error("no AWS Outbound product answered prices")
	}
}
