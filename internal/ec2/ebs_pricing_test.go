package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// ebsFixturePages is a real Pricing API answer for gp3 in eu-north-1 (three
// products, one page each), as luxd's GetProducts decodes it.
func ebsFixturePages(t *testing.T) []json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/ebs-pricing-eu-north-1.json")
	if err != nil {
		t.Fatal(err)
	}
	var pages []json.RawMessage
	if err := json.Unmarshal(raw, &pages); err != nil {
		t.Fatal(err)
	}
	return pages
}

// The three products of the real answer, paged, give gp3's storage, IOPS and
// throughput prices; the throughput one stays per GiB/s-month.
func TestPricesBlockStorage(t *testing.T) {
	awsTestEnv(t)
	pages := ebsFixturePages(t)
	var filters []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ServiceCode string
			Filters     []struct{ Field, Type, Value string }
			NextToken   string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.ServiceCode != "AmazonEC2" {
			t.Errorf("service %q", in.ServiceCode)
		}
		if in.NextToken == "" {
			filters = nil
			for _, f := range in.Filters {
				filters = append(filters, f.Field+"="+f.Value)
			}
		}
		i := 0
		fmt.Sscanf(in.NextToken, "p%d", &i)
		var page map[string]any
		_ = json.Unmarshal(pages[i], &page)
		if i+1 < len(pages) {
			page["NextToken"] = fmt.Sprintf("p%d", i+1)
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer fake.Close()
	p := NewPrices("us-east-1", fake.URL, "")
	got, err := p.BlockStorage(context.Background(), "eu-north-1", "gp3")
	if err != nil {
		t.Fatal(err)
	}
	want := server.BlockStoragePrice{Currency: "USD", PerGBMonth: "0.0836000000", PerIOPSMonth: "0.0052000000", PerGiBpsMonth: "42.8032000000"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if strings.Join(filters, " ") != "regionCode=eu-north-1 volumeApiName=gp3" {
		t.Errorf("filters %v", filters)
	}
}

// A storage product priced twice is ambiguous; an answer without one has no
// storage price: both errors, never a partial price.
func TestPricesBlockStorageRefusesAmbiguousOrMissing(t *testing.T) {
	awsTestEnv(t)
	pages := ebsFixturePages(t)
	var first struct{ PriceList []string }
	_ = json.Unmarshal(pages[0], &first)
	var third struct{ PriceList []string }
	_ = json.Unmarshal(pages[2], &third)
	for name, list := range map[string][]string{
		"ambiguous": {first.PriceList[0], first.PriceList[0]},
		"missing":   {third.PriceList[0]},
	} {
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"PriceList": list})
		}))
		_, err := NewPrices("us-east-1", fake.URL, "").BlockStorage(context.Background(), "eu-north-1", "gp3")
		fake.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
