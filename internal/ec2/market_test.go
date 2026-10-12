package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// marketEC2 answers DescribeSubnets and GetSpotPlacementScores as EC2
// does, recording each request's form.
func marketEC2(t *testing.T, handle func(action string, form url.Values, w http.ResponseWriter)) (string, *[]url.Values) {
	t.Helper()
	awsTestEnv(t)
	var forms []url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms = append(forms, r.PostForm)
		w.Header().Set("Content-Type", "text/xml")
		handle(r.PostForm.Get("Action"), r.PostForm, w)
	}))
	t.Cleanup(fake.Close)
	return fake.URL, &forms
}

const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`

// A template's zones are its subnets' (each once, by name and id); without
// subnets, the region's default subnets'.
func TestZones(t *testing.T) {
	endpoint, forms := marketEC2(t, func(action string, form url.Values, w http.ResponseWriter) {
		if action != "DescribeSubnets" {
			ec2Error(w, http.StatusBadRequest, "InvalidAction", action)
			return
		}
		fmt.Fprintf(w, `<DescribeSubnetsResponse %s><subnetSet>`+
			`<item><subnetId>subnet-b</subnetId><availabilityZone>us-east-1b</availabilityZone><availabilityZoneId>use1-az2</availabilityZoneId></item>`+
			`<item><subnetId>subnet-a</subnetId><availabilityZone>us-east-1a</availabilityZone><availabilityZoneId>use1-az1</availabilityZoneId></item>`+
			`<item><subnetId>subnet-c</subnetId><availabilityZone>us-east-1a</availabilityZone><availabilityZoneId>use1-az1</availabilityZoneId></item>`+
			`</subnetSet></DescribeSubnetsResponse>`, ns)
	})
	p := New(endpoint, discard)
	zones, err := p.Zones(context.Background(), json.RawMessage(`{"region":"us-east-1","subnets":["subnet-a","subnet-b","subnet-c"]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []server.Zone{{Name: "us-east-1a", ID: "use1-az1"}, {Name: "us-east-1b", ID: "use1-az2"}}
	if !slices.Equal(zones, want) {
		t.Fatalf("zones %v, want %v", zones, want)
	}
	if f := (*forms)[0]; f.Get("SubnetId.1") != "subnet-a" || f.Get("SubnetId.3") != "subnet-c" || f.Get("Filter.1.Name") != "" {
		t.Fatalf("with subnets: %v", f)
	}
	if _, err := p.Zones(context.Background(), json.RawMessage(`{"region":"us-east-1"}`)); err != nil {
		t.Fatal(err)
	}
	if f := (*forms)[1]; f.Get("SubnetId.1") != "" || f.Get("Filter.1.Name") != "default-for-az" || f.Get("Filter.1.Value.1") != "true" {
		t.Fatalf("without subnets: %v", f)
	}
}

// One request per type: one instance (TargetCapacity 1, units), scored per
// zone of the template's region only; scores come back by zone id. A
// refusal is an error naming the call and EC2's code.
func TestPlacementScores(t *testing.T) {
	refuse := false
	endpoint, forms := marketEC2(t, func(action string, form url.Values, w http.ResponseWriter) {
		if refuse {
			ec2Error(w, http.StatusForbidden, "UnauthorizedOperation", "You are not authorized to perform this operation.")
			return
		}
		fmt.Fprintf(w, `<GetSpotPlacementScoresResponse %s><spotPlacementScoreSet>`+
			`<item><region>us-east-1</region><availabilityZoneId>use1-az1</availabilityZoneId><score>7</score></item>`+
			`<item><region>us-east-1</region><availabilityZoneId>use1-az4</availabilityZoneId><score>3</score></item>`+
			`</spotPlacementScoreSet></GetSpotPlacementScoresResponse>`, ns)
	})
	p := New(endpoint, discard)
	scores, err := p.PlacementScores(context.Background(), "us-east-1", "m7i.large")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(scores) != "map[use1-az1:7 use1-az4:3]" {
		t.Fatalf("scores %v", scores)
	}
	f := (*forms)[0]
	for k, want := range map[string]string{
		"Action": "GetSpotPlacementScores", "TargetCapacity": "1", "TargetCapacityUnitType": "units",
		"InstanceType.1": "m7i.large", "InstanceType.2": "", "SingleAvailabilityZone": "true",
		"RegionName.1": "us-east-1", "RegionName.2": "",
	} {
		if got := f.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	refuse = true
	if _, err := p.PlacementScores(context.Background(), "us-east-1", "m7i.large"); err == nil ||
		!strings.Contains(err.Error(), "GetSpotPlacementScores") || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Fatalf("refused: %v", err)
	}
}

// A template's market scope: its region, instanceType then fallbacks, each
// once; no EC2 call.
func TestMarketScope(t *testing.T) {
	region, types, err := New("http://127.0.0.1:9", discard).MarketScope(json.RawMessage(
		`{"region":"eu-west-1","instanceType":"m7i.large","fallbackInstanceTypes":["m7g.large","m7i.large"]}`))
	if err != nil || region != "eu-west-1" || !slices.Equal(types, []string{"m7i.large", "m7g.large"}) {
		t.Fatalf("%q %v %v", region, types, err)
	}
	if _, types, _ := New("", discard).MarketScope(json.RawMessage(`{"region":"eu-west-1"}`)); len(types) != 0 {
		t.Fatalf("no instanceType: %v", types)
	}
}
