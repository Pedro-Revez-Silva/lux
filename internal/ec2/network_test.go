package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestNetworkDiscoveryPaginationAndCache(t *testing.T) {
	awsTestEnv(t)
	subnetCalls, instanceCalls := 0, 0
	failSubnets := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		switch r.Form.Get("Action") {
		case "DescribeInstances":
			instanceCalls++
			if r.Form.Get("Filter.1.Name") != "instance-id" || r.Form.Get("Filter.1.Value.1") != "i-a" {
				t.Errorf("unexpected instance filter: %v", r.Form)
			}
			if r.Form.Get("NextToken") == "" {
				fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/><nextToken>instances-2</nextToken></DescribeInstancesResponse>`)
				return
			}
			fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-a</instanceId><subnetId>subnet-a</subnetId><vpcId>vpc-a</vpcId><placement><availabilityZone>eu-north-1a</availabilityZone></placement><ipAddress>192.0.2.1</ipAddress><networkInterfaceSet><item><association><publicIp>192.0.2.1</publicIp></association><privateIpAddressesSet><item><association><publicIp>192.0.2.2</publicIp></association></item></privateIpAddressesSet></item><item><association><publicIp>192.0.2.3</publicIp></association></item></networkInterfaceSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		case "DescribeSubnets":
			subnetCalls++
			if failSubnets {
				ec2Error(w, 403, "UnauthorizedOperation", "denied")
				return
			}
			if r.Form.Get("Filter.1.Name") != "vpc-id" || r.Form.Get("Filter.1.Value.1") != "vpc-a" {
				t.Errorf("unexpected VPC filter: %v", r.Form)
			}
			if r.Form.Get("NextToken") == "" {
				fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-a</subnetId><cidrBlock>10.0.0.0/24</cidrBlock><availabilityZone>eu-north-1a</availabilityZone><ipv6CidrBlockAssociationSet><item><ipv6CidrBlock>2001:db8::/64</ipv6CidrBlock><ipv6CidrBlockState><state>associated</state></ipv6CidrBlockState></item><item><ipv6CidrBlock>2001:db8:bad::/64</ipv6CidrBlock><ipv6CidrBlockState><state>disassociated</state></ipv6CidrBlockState></item></ipv6CidrBlockAssociationSet></item></subnetSet><nextToken>subnets-2</nextToken></DescribeSubnetsResponse>`)
			} else {
				fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-b</subnetId><cidrBlock>10.0.1.0/24</cidrBlock><availabilityZone>eu-north-1b</availabilityZone></item></subnetSet></DescribeSubnetsResponse>`)
			}
		default:
			t.Errorf("unexpected action %v", r.Form)
		}
	}))
	defer fake.Close()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	p := New(fake.URL, discard)
	now := time.Now()
	p.now = func() time.Time { return now }
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	want := map[string]server.HostNetwork{"i-a": {SubnetCIDRs: []string{"10.0.0.0/24", "2001:db8::/64"}, Zone: "eu-north-1a", PublicIPv4: 3,
		VPCSubnets: []server.NetworkSubnet{{CIDR: "10.0.0.0/24", Zone: "eu-north-1a"}, {CIDR: "10.0.1.0/24", Zone: "eu-north-1b"}, {CIDR: "2001:db8::/64", Zone: "eu-north-1a"}}}}
	for range 2 {
		got, err := p.Network(context.Background(), template, []string{"i-a", "i-missing"})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("network = %+v, %v; want %+v", got, err, want)
		}
	}
	if subnetCalls != 2 || instanceCalls != 4 {
		t.Fatalf("calls instances=%d subnets=%d", instanceCalls, subnetCalls)
	}
	now = now.Add(server.DefaultProviderCheckEvery)
	failSubnets = true
	got, err := p.Network(context.Background(), template, []string{"i-a"})
	if err != nil || !got["i-a"].SubnetsUnavailable || got["i-a"].PublicIPv4 != 3 {
		t.Fatalf("failed refresh = %v, %v", got, err)
	}
	failSubnets = false
	if _, err := p.Network(context.Background(), template, []string{"i-a"}); err != nil {
		t.Fatal(err)
	}
	if subnetCalls != 5 {
		t.Fatalf("subnet refresh calls = %d", subnetCalls)
	}
	p.CacheNetworkFor(0)
	if _, err := p.Network(context.Background(), template, []string{"i-a"}); err != nil {
		t.Fatal(err)
	}
	if subnetCalls != 7 {
		t.Fatalf("disabled cache calls = %d", subnetCalls)
	}
}

func TestNetworkAttachedENIUnion(t *testing.T) {
	awsTestEnv(t)
	attached, fail, missing, empty := false, false, false, false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		if r.Form.Get("Action") == "DescribeInstances" {
			if empty {
				fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-a</instanceId></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				return
			}
			extra := ""
			if attached {
				extra = `<item><vpcId>vpc-a</vpcId><subnetId>sub-a2</subnetId></item><item><vpcId>vpc-b</vpcId><subnetId>sub-b</subnetId></item><item><vpcId>vpc-b</vpcId><subnetId>sub-b</subnetId></item>`
			}
			fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-a</instanceId><vpcId>vpc-a</vpcId><subnetId>sub-a</subnetId><placement><availabilityZone>zone-a</availabilityZone></placement><networkInterfaceSet><item><vpcId>vpc-a</vpcId><subnetId>sub-a</subnetId></item>%s</networkInterfaceSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`, extra)
			return
		}
		if r.Form.Get("Filter.1.Value.1") == "vpc-b" {
			if fail {
				ec2Error(w, 403, "UnauthorizedOperation", "denied")
				return
			}
			fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>sub-b</subnetId><cidrBlock>10.2.0.0/24</cidrBlock><availabilityZone>zone-a</availabilityZone></item><item><subnetId>sub-c</subnetId><cidrBlock>10.2.1.0/24</cidrBlock><availabilityZone>zone-b</availabilityZone></item></subnetSet></DescribeSubnetsResponse>`)
		} else {
			extra := ""
			if attached && !missing {
				extra = `<item><subnetId>sub-a2</subnetId><cidrBlock>10.1.1.0/24</cidrBlock><availabilityZone>zone-b</availabilityZone></item>`
			}
			fmt.Fprintf(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>sub-a</subnetId><cidrBlock>10.1.0.0/24</cidrBlock><availabilityZone>zone-a</availabilityZone></item>%s</subnetSet></DescribeSubnetsResponse>`, extra)
		}
	}))
	defer fake.Close()
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	p := New(fake.URL, discard)
	p.CacheNetworkFor(time.Hour)
	check := func(want server.HostNetwork) {
		t.Helper()
		got, err := p.Network(context.Background(), json.RawMessage(`{"region":"eu-west-1"}`), []string{"i-a"})
		if err != nil || !reflect.DeepEqual(got["i-a"], want) {
			t.Fatalf("network = %+v, %v; want %+v", got["i-a"], err, want)
		}
	}
	a := server.NetworkSubnet{CIDR: "10.1.0.0/24", Zone: "zone-a"}
	a2 := server.NetworkSubnet{CIDR: "10.1.1.0/24", Zone: "zone-b"}
	b := server.NetworkSubnet{CIDR: "10.2.0.0/24", Zone: "zone-a"}
	c := server.NetworkSubnet{CIDR: "10.2.1.0/24", Zone: "zone-b"}
	check(server.HostNetwork{SubnetCIDRs: []string{a.CIDR}, VPCSubnets: []server.NetworkSubnet{a}, Zone: "zone-a"})
	attached = true
	check(server.HostNetwork{SubnetCIDRs: []string{a.CIDR, a2.CIDR, b.CIDR}, VPCSubnets: []server.NetworkSubnet{a, a2, b, c}, Zone: "zone-a"})
	p.CacheNetworkFor(0)
	fail = true
	check(server.HostNetwork{SubnetCIDRs: []string{a.CIDR, a2.CIDR}, VPCSubnets: []server.NetworkSubnet{a, a2}, Zone: "zone-a", SubnetsUnavailable: true})
	fail, missing = false, true
	check(server.HostNetwork{SubnetCIDRs: []string{a.CIDR, b.CIDR}, VPCSubnets: []server.NetworkSubnet{a, b, c}, Zone: "zone-a", SubnetsUnavailable: true})
	empty = true
	check(server.HostNetwork{SubnetsUnavailable: true})
}

func TestNetworkEmptyAndInstanceFailure(t *testing.T) {
	awsTestEnv(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ec2Error(w, 403, "UnauthorizedOperation", "denied") }))
	defer fake.Close()
	p := New(fake.URL, discard)
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	if got, err := p.Network(context.Background(), template, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	if got, err := p.Network(context.Background(), template, []string{"i-a"}); err == nil || got != nil {
		t.Fatalf("failure = %v, %v", got, err)
	}
}

func TestPrefixesClassificationCacheConditionalAndFallback(t *testing.T) {
	calls := 0
	mode := "ok"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 && r.Header.Get("If-None-Match") != `"catalog-1"` {
			t.Errorf("conditional header = %q", r.Header.Get("If-None-Match"))
		}
		switch mode {
		case "not-modified":
			w.WriteHeader(304)
		case "error":
			w.WriteHeader(503)
		case "invalid":
			fmt.Fprint(w, `{"prefixes":[{"ip_prefix":"invalid","region":"eu-north-1","service":"S3"}]}`)
		default:
			w.Header().Set("ETag", `"catalog-1"`)
			fmt.Fprint(w, `{"prefixes":[{"ip_prefix":"192.0.2.0/24","region":"eu-north-1","service":"AMAZON"},{"ip_prefix":"192.0.2.0/24","region":"eu-north-1","service":"EC2"},{"ip_prefix":"198.51.100.0/24","region":"eu-north-1","service":"AMAZON"},{"ip_prefix":"198.51.100.0/24","region":"eu-north-1","service":"S3"},{"ip_prefix":"13.32.0.0/15","region":"GLOBAL","service":"CLOUDFRONT"},{"ip_prefix":"13.32.0.0/15","region":"GLOBAL","service":"AMAZON"},{"ip_prefix":"15.177.0.0/18","region":"GLOBAL","service":"ROUTE53_HEALTHCHECKS"},{"ip_prefix":"203.0.113.0/24","region":"us-east-1","service":"S3"}],"ipv6_prefixes":[{"ipv6_prefix":"2001:db8::/48","region":"eu-north-1","service":"DYNAMODB"}]}`)
		}
	}))
	defer fake.Close()
	t.Setenv("LUX_IPRANGES_URL", fake.URL)
	p := New("", discard)
	now := time.Now()
	p.now = func() time.Time { return now }
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	want := server.ProviderPrefixes{Source: "ec2-ip-ranges", FetchedAt: now, RegionPublic: []string{"192.0.2.0/24"}, RegionService: []string{"198.51.100.0/24", "2001:db8::/48"}, ProviderGlobal: []string{"15.177.0.0/18"}, ProviderGlobalFree: []string{"13.32.0.0/15"}, Provider: map[string][]string{"us-east-1": {"203.0.113.0/24"}}}
	check := func() {
		t.Helper()
		got, err := p.Prefixes(context.Background(), template)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("prefixes = %+v, %v; want %+v", got, err, want)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(check)
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("initial fetches = %d", calls)
	}
	for _, next := range []string{"not-modified", "error", "invalid"} {
		mode = next
		now = now.Add(24 * time.Hour)
		check()
		now = now.Add(prefixRetryAfter)
		check()
	}
	if calls != 6 {
		t.Fatalf("refresh fetches = %d", calls)
	}
	other, err := p.Prefixes(context.Background(), json.RawMessage(`{"region":"us-east-1"}`))
	if err != nil || !reflect.DeepEqual(other.RegionService, []string{"203.0.113.0/24"}) || len(other.Provider["eu-north-1"]) != 3 {
		t.Fatalf("other region = %+v, %v", other, err)
	}
}

func TestPrefixesInvalidRegionRetainsCatalogAndRetries(t *testing.T) {
	region := "eu-west-1"
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"prefixes":[{"ip_prefix":"192.0.2.0/24","region":%q,"service":"EC2"}]}`, region)
	}))
	defer fake.Close()
	t.Setenv("LUX_IPRANGES_URL", fake.URL)
	p := New("", discard)
	now := time.Now()
	p.now = func() time.Time { return now }
	template := json.RawMessage(`{"region":"us-east-1"}`)
	good, err := p.Prefixes(context.Background(), template)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	for _, invalid := range []string{"eu-west-1 ", strings.Repeat("a", 41)} {
		region = invalid
		for range 2 {
			now = now.Add(prefixRetryAfter)
			got, err := p.Prefixes(context.Background(), template)
			if err != nil || !reflect.DeepEqual(got, good) {
				t.Fatalf("%+v %v", got, err)
			}
		}
		cold := New("", discard)
		if _, err := cold.Prefixes(context.Background(), template); err == nil {
			t.Fatal("cold malformed region accepted")
		}
	}
	if calls != 7 {
		t.Fatal(calls)
	}
}

func TestPrefixesInitialFailure(t *testing.T) {
	for _, body := range []string{`{}`, `not json`, `{"prefixes":[{"ip_prefix":"bogus","region":"us-east-1","service":"S3"}]}`} {
		t.Run(body, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer fake.Close()
			t.Setenv("LUX_IPRANGES_URL", fake.URL)
			p := New("", discard)
			if _, err := p.Prefixes(context.Background(), json.RawMessage(`{}`)); err == nil {
				t.Fatal("invalid catalog accepted")
			}
			if _, err := p.Prefixes(context.Background(), json.RawMessage(`{}`)); err == nil {
				t.Fatal("failed catalog cached as success")
			}
		})
	}
}

// A failed refresh is not retried for prefixRetryAfter: calls in between
// return the last good catalog, or with none the same error, without a
// request.
func TestPrefixesFailedRefreshBacksOff(t *testing.T) {
	var calls int
	var mu sync.Mutex
	failing := true
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"prefixes":[{"ip_prefix":"192.0.2.0/24","region":"eu-north-1","service":"EC2"}]}`)
	}))
	defer fake.Close()
	t.Setenv("LUX_IPRANGES_URL", fake.URL)
	p := New("", discard)
	now := time.Now()
	p.now = func() time.Time { return now }
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	setFailing := func(f bool) { mu.Lock(); defer mu.Unlock(); failing = f }

	_, err1 := p.Prefixes(context.Background(), template)
	now = now.Add(time.Minute - time.Second)
	_, err2 := p.Prefixes(context.Background(), template)
	if err1 == nil || err2 == nil || err2.Error() != err1.Error() || !strings.Contains(err2.Error(), "503") || count() != 1 {
		t.Fatalf("cold: errors %v / %v, %d requests; want the same error and one request", err1, err2, count())
	}
	now = now.Add(time.Second)
	if _, err := p.Prefixes(context.Background(), template); err == nil || count() != 2 {
		t.Fatalf("after a minute: %v, %d requests; want a retry", err, count())
	}

	// Warm: a failed refresh serves the last good catalog until its retry.
	setFailing(false)
	now = now.Add(time.Minute)
	good, err := p.Prefixes(context.Background(), template)
	if err != nil || count() != 3 {
		t.Fatalf("recovery: %v, %d requests", err, count())
	}
	setFailing(true)
	now = now.Add(24 * time.Hour)
	for range 2 {
		if got, err := p.Prefixes(context.Background(), template); err != nil || !reflect.DeepEqual(got, good) {
			t.Fatalf("warm failure: %+v %v, want the last good catalog", got, err)
		}
	}
	if count() != 4 {
		t.Fatalf("warm failure: %d requests, want 4", count())
	}
	now = now.Add(time.Minute)
	if _, err := p.Prefixes(context.Background(), template); err != nil || count() != 5 {
		t.Fatalf("warm retry: %v, %d requests, want 5", err, count())
	}
}

// A caller that gives up mid-fetch is not the source failing: the next
// caller fetches at once.
func TestPrefixesCancelledCallIsNotABackoff(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"prefixes":[{"ip_prefix":"192.0.2.0/24","region":"eu-north-1","service":"EC2"}]}`)
	}))
	defer fake.Close()
	t.Setenv("LUX_IPRANGES_URL", fake.URL)
	p := New("", discard)
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Prefixes(ctx, template); err == nil {
		t.Fatal("cancelled call succeeded")
	}
	if _, err := p.Prefixes(context.Background(), template); err != nil {
		t.Fatalf("after a cancelled call: %v", err)
	}
}

// The retry spacing runs from the end of the failed fetch, so a fetch that
// hangs for its whole timeout still waits a full minute before the next.
func TestPrefixesBackoffCountsFromFailure(t *testing.T) {
	var calls int
	var mu sync.Mutex
	p := New("", discard)
	now := time.Now()
	p.now = func() time.Time { return now }
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		now = now.Add(30 * time.Second) // the fetch hangs half a minute, then fails
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer fake.Close()
	t.Setenv("LUX_IPRANGES_URL", fake.URL)
	template := json.RawMessage(`{"region":"eu-north-1"}`)
	_, _ = p.Prefixes(context.Background(), template)
	now = now.Add(45 * time.Second)
	_, _ = p.Prefixes(context.Background(), template)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d requests 45s after a failure that ended 75s after it began; want 1", calls)
	}
}
