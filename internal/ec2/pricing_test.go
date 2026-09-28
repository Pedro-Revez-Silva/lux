package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestPricesOnDemand(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); !strings.HasSuffix(got, ".GetProducts") {
			t.Errorf("X-Amz-Target = %q, want GetProducts", got)
		}
		var in struct {
			ServiceCode string
			Filters     []struct {
				Field string
				Type  string
				Value string
			}
			NextToken string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.ServiceCode != "AmazonEC2" {
			t.Errorf("serviceCode = %q", in.ServiceCode)
		}
		want := map[string]string{
			"regionCode":      "us-east-1",
			"instanceType":    "m7i.large",
			"operatingSystem": "Linux",
			"tenancy":         "Shared",
			"preInstalledSw":  "NA",
			"capacitystatus":  "Used",
		}
		if len(in.Filters) != len(want) {
			t.Errorf("filters = %#v", in.Filters)
		}
		for _, f := range in.Filters {
			if f.Type != "TERM_MATCH" || want[f.Field] != f.Value {
				t.Errorf("filter = %#v, want exact filters %#v", f, want)
			}
			delete(want, f.Field)
		}
		if len(want) != 0 {
			t.Errorf("missing filters: %#v", want)
		}

		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		calls++
		if calls == 1 {
			if in.NextToken != "" {
				t.Errorf("first token = %q", in.NextToken)
			}
			fmt.Fprint(w, `{"NextToken":"more"}`)
			return
		}
		if in.NextToken != "more" {
			t.Errorf("second token = %q", in.NextToken)
		}
		fmt.Fprintf(w, `{"PriceList":[%q]}`, onDemandProduct("0.0960000000"))
	}))
	defer endpoint.Close()

	rate, err := NewPrices("us-east-1", endpoint.URL, "").OnDemand(context.Background(), "us-east-1", "m7i.large")
	if err != nil {
		t.Fatal(err)
	}
	if want := (server.HourlyRate{PerHour: "0.0960000000", Currency: "USD"}); rate != want {
		t.Errorf("rate = %#v, want %#v", rate, want)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestOnDemandRateRejectsIncompleteOrAmbiguousData(t *testing.T) {
	for _, product := range []string{
		`{}`,
		onDemandProduct("not-a-number"),
		`{"terms":{"OnDemand":{"a":{"priceDimensions":{"one":{"unit":"Hrs","pricePerUnit":{"USD":"0.1"}},"two":{"unit":"Hrs","pricePerUnit":{"USD":"0.2"}}}}}}}`,
	} {
		if _, err := onDemandRate(product); err == nil {
			t.Errorf("onDemandRate(%s) succeeded", product)
		}
	}
}

func TestPricesSpotHistory(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	from := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form := r.Form
		for key, want := range map[string]string{
			"Action":               "DescribeSpotPriceHistory",
			"AvailabilityZone":     "us-east-1a",
			"InstanceType.1":       "m7i.large",
			"ProductDescription.1": "Linux/UNIX",
			"StartTime":            from.Format(time.RFC3339),
			"EndTime":              to.Format(time.RFC3339),
		} {
			if got := form.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		calls++
		w.Header().Set("Content-Type", "text/xml")
		if calls == 1 {
			if got := form.Get("NextToken"); got != "" {
				t.Errorf("first token = %q", got)
			}
			fmt.Fprint(w, spotResponse("next", "0.0700000000", from.Add(time.Hour)))
			return
		}
		if got := form.Get("NextToken"); got != "next" {
			t.Errorf("second token = %q", got)
		}
		fmt.Fprint(w, spotResponse("", "0.0500000000", from))
	}))
	defer endpoint.Close()

	rates, err := NewPrices("us-east-1", "", endpoint.URL).SpotHistory(context.Background(), "us-east-1a", "m7i.large", from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []server.SpotRate{
		{At: from, HourlyRate: server.HourlyRate{PerHour: "0.0500000000", Currency: "USD"}},
		{At: from.Add(time.Hour), HourlyRate: server.HourlyRate{PerHour: "0.0700000000", Currency: "USD"}},
	}
	if len(rates) != len(want) {
		t.Fatalf("rates = %#v, want %#v", rates, want)
	}
	for i := range want {
		if rates[i] != want[i] {
			t.Errorf("rates[%d] = %#v, want %#v", i, rates[i], want[i])
		}
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func onDemandProduct(usd string) string {
	return fmt.Sprintf(`{"terms":{"OnDemand":{"offer":{"priceDimensions":{"rate":{"unit":"Hrs","pricePerUnit":{"USD":%q}}}}}}}`, usd)
}

func spotResponse(nextToken, price string, at time.Time) string {
	next := ""
	if nextToken != "" {
		next = "<nextToken>" + nextToken + "</nextToken>"
	}
	return `<DescribeSpotPriceHistoryResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
		`<spotPriceHistorySet><item><availabilityZone>us-east-1a</availabilityZone>` +
		`<instanceType>m7i.large</instanceType><productDescription>Linux/UNIX</productDescription>` +
		`<spotPrice>` + price + `</spotPrice><timestamp>` + at.Format(time.RFC3339) + `</timestamp></item></spotPriceHistorySet>` +
		next + `<requestId>test</requestId></DescribeSpotPriceHistoryResponse>`
}

func TestZoneRegion(t *testing.T) {
	if got, err := zoneRegion("eu-west-1b"); err != nil || got != "eu-west-1" {
		t.Errorf("zoneRegion = %q, %v", got, err)
	}
	for _, zone := range []string{"", "us-east-1", "us-east-1-1", "1"} {
		if _, err := zoneRegion(zone); err == nil {
			t.Errorf("zoneRegion(%q) succeeded", zone)
		}
	}
}
