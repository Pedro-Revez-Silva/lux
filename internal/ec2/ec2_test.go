package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/server"
)

func TestRenderUserData(t *testing.T) {
	env := map[string]string{"LUX_URL": "http://10.0.1.10:7070", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_EC2_IMDS": "http://169.254.169.254"}

	ign, err := renderUserData("", env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(ign, &m); err != nil {
		t.Fatalf("default format is not valid JSON (want Ignition): %v", err)
	}
	if m["ignition"].(map[string]any)["version"] == nil {
		t.Error("default format has no ignition.version")
	}

	ign2, err := renderUserData("ignition", env)
	if err != nil {
		t.Fatal(err)
	}
	if string(ign) != string(ign2) {
		t.Error(`"" and "ignition" rendered differently`)
	}

	script, err := renderUserData("script", env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(script), "#!/bin/bash\n") || !strings.Contains(string(script), "LUX_URL='http://10.0.1.10:7070'") {
		t.Errorf("script format: %s", script[:200])
	}

	lines, err := renderUserData("env", env)
	if err != nil {
		t.Fatal(err)
	}
	if string(lines) != "LUX_URL=http://10.0.1.10:7070\nLUX_HOST_TOKEN=luxh_x\nLUX_HOST_NAME=h\nLUX_EC2_IMDS=http://169.254.169.254\n" {
		t.Errorf("env format: %q", lines)
	}

	if _, err := renderUserData("cloud-init-yaml", env); err == nil {
		t.Error("an unknown format was not rejected")
	}
}

// A template tag with a key lux also sets (lux:pool, as the Terraform
// module once generated) is sent once, lux's value: EC2 refuses a request
// naming a key twice ("Duplicate tag key"), which failed every launch.
func TestLaunchSendsEachTagKeyOnceLuxWins(t *testing.T) {
	awsTestEnv(t)
	var sent [][2]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		for i := 1; ; i++ {
			k := r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i))
			if k == "" {
				break
			}
			sent = append(sent, [2]string{k, r.PostForm.Get(fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i))})
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
			`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>m8g.2xlarge</instanceType>`+
			`<placement><availabilityZone>eu-north-1a</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
	}))
	defer fake.Close()
	template := `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env", "tags": {"lux:pool": "arm64", "team": "platform"}}`
	lux := map[string]string{"lux:managed": "true", "lux:pool": "ten_1/default"}
	if _, err := New(fake.URL, discard).Launch(context.Background(), json.RawMessage(template), lux, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"lux:managed", "true"}, {"lux:pool", "ten_1/default"}, {"team", "platform"}}
	if !slices.Equal(sent, want) {
		t.Errorf("tags sent %v, want %v", sent, want)
	}
}

// Launch returns what RunInstances reports: the instance type (not the
// template's) and the zone; the market follows the template's spot.
func TestLaunchReturnsInstanceFacts(t *testing.T) {
	awsTestEnv(t)
	var markets []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		markets = append(markets, r.PostForm.Get("InstanceMarketOptions.MarketType"))
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
			`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>m7i.2xlarge</instanceType>`+
			`<placement><availabilityZone>eu-west-1b</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
	}))
	defer fake.Close()
	p := New(fake.URL, discard)
	for _, c := range []struct {
		template, market string
	}{
		{`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env"}`, server.MarketOnDemand},
		{`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "spot": true}`, server.MarketSpot},
	} {
		l, err := p.Launch(context.Background(), json.RawMessage(c.template), nil, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		want := server.Launched{ProviderID: "i-0abc", InstanceType: "m7i.2xlarge", Zone: "eu-west-1b", Market: c.market}
		if l != want {
			t.Errorf("%s: got %+v, want %+v", c.template, l, want)
		}
	}
	if fmt.Sprint(markets) != "[ spot]" {
		t.Errorf("requested markets %q, want on-demand (none) then spot", markets)
	}
}

// The runner env a pool's instances boot with: LUX_NESTED=true only when the
// template opts in, and a caller's own LUX_NESTED never leaks through a
// template that does not.
func TestLaunchUserDataCarriesNestedOnlyForAnOptedInTemplate(t *testing.T) {
	url, _, userData := fakeEC2(t, 0)
	launch := func(template string, env map[string]string) string {
		t.Helper()
		if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(template), nil, env); err != nil {
			t.Fatal(err)
		}
		return (*userData)[len(*userData)-1]
	}
	base := map[string]string{"LUX_URL": "http://10.0.1.10:7070", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_RUNNER_MEMORY": "68719476736"}
	with := func(k, v string) map[string]string {
		m := maps.Clone(base)
		m[k] = v
		return m
	}
	for _, format := range []string{"env", "script", "ignition"} {
		for _, c := range []struct {
			name, nested string // nested: the template's nestedContainers, "" for absent
			caller       map[string]string
			want         string // LUX_NESTED in the runner env, "" for absent
		}{
			{"opted in", "true", base, "true"},
			{"opted in, caller says false", "true", with("LUX_NESTED", "false"), "true"},
			{"absent, caller says true", "", with("LUX_NESTED", "true"), ""},
			{"false, caller says true", "false", with("LUX_NESTED", "true"), ""},
		} {
			template := `{"launchTemplate":"lt-1","userData":"` + format + `"`
			if c.nested != "" {
				template += `,"nestedContainers":` + c.nested
			}
			got := runnerEnvOf(t, format, launch(template+"}", c.caller))
			if v, ok := got["LUX_NESTED"]; v != c.want || ok != (c.want != "") {
				t.Errorf("%s, %s: LUX_NESTED=%q (set %v), want %q", format, c.name, v, ok, c.want)
			}
			if got["LUX_RUNNER_MEMORY"] != "68719476736" || got["LUX_URL"] != base["LUX_URL"] {
				t.Errorf("%s, %s: runner env lost the memory or URL: %v", format, c.name, got)
			}
		}
	}
}

// attempt is one RunInstances request the capacity fake received.
type attempt struct{ instanceType, subnet, memory string }

// capacityEC2 is a fake EC2 whose RunInstances fails with fail[{type,
// subnet}] (an EC2 error code) and otherwise launches the requested type;
// "*" as the subnet fails the type everywhere. Capacity errors and
// InternalError come with a 500, as EC2 sends them, others with a 400.
// DescribeInstanceTypes answers memMiB[type] (an error when absent).
func capacityEC2(t *testing.T, fail map[[2]string]string, memMiB map[string]int) (url string, attempts *[]attempt, calls map[string]int) {
	t.Helper()
	awsTestEnv(t)
	attempts, calls = &[]attempt{}, map[string]int{}
	var mu sync.Mutex
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		action := r.PostForm.Get("Action")
		calls[action]++
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "DescribeInstanceTypes":
			it := r.PostForm.Get("InstanceType.1")
			mem, ok := memMiB[it]
			if !ok {
				ec2Error(w, http.StatusBadRequest, "InvalidInstanceType", "no "+it+" (fake)")
				return
			}
			fmt.Fprintf(w, `<DescribeInstanceTypesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><instanceTypeSet><item>`+
				`<instanceType>%s</instanceType><memoryInfo><sizeInMiB>%d</sizeInMiB></memoryInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`, it, mem)
		case "RunInstances":
			it, sn := r.PostForm.Get("InstanceType"), r.PostForm.Get("SubnetId")
			ud, _ := base64.StdEncoding.DecodeString(r.PostForm.Get("UserData"))
			*attempts = append(*attempts, attempt{it, sn, runnerEnvOf(t, "env", string(ud))["LUX_RUNNER_MEMORY"]})
			code := fail[[2]string{it, sn}]
			if code == "" {
				code = fail[[2]string{it, "*"}]
			}
			switch code {
			case "":
			case "InsufficientInstanceCapacity", "InsufficientCapacity", "Unsupported":
				ec2Error(w, http.StatusInternalServerError, code, noCapacityMessage(it, sn))
				return
			case "InternalError":
				ec2Error(w, http.StatusInternalServerError, code, "internal error (fake)")
				return
			default:
				ec2Error(w, http.StatusBadRequest, code, "no "+code+" (fake)")
				return
			}
			fmt.Fprintf(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
				`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>%s</instanceType>`+
				`<placement><availabilityZone>%s-az</availabilityZone></placement>`+
				`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`, it, sn)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(fake.Close)
	return fake.URL, attempts, calls
}

const fallbackTemplate = `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env",
	"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge"], "subnets": ["subnet-a", "subnet-b", "subnet-c"]}`

// With no capacity anywhere for the first type, Launch tries it in every
// subnet from the pool's next one round, then the fallback the same way,
// once each (the SDK does not retry a capacity error), and reports the
// instance type and zone of the one that launched.
func TestLaunchFallsBackTypeMajorFromTheRoundRobinSubnet(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}:        "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "subnet-b"}: "Unsupported",
		{"m7g.2xlarge", "subnet-c"}: "InsufficientCapacity",
	}, map[string]int{"m8g.2xlarge": 32768, "m7g.2xlarge": 32768})
	p := New(url, discard)
	// A launch sharing the launch template and subnets moves the round
	// robin to subnet-b.
	prime := `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c8g.2xlarge",
		"subnets": ["subnet-a", "subnet-b", "subnet-c"]}`
	if _, err := p.Launch(context.Background(), json.RawMessage(prime), nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	*attempts = nil
	l, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{"LUX_URL": "http://luxd"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-c", "m8g.2xlarge@subnet-a",
		"m7g.2xlarge@subnet-b", "m7g.2xlarge@subnet-c", "m7g.2xlarge@subnet-a"}
	if got := placements(*attempts); !slices.Equal(got, want) {
		t.Errorf("attempts %v, want %v", got, want)
	}
	if l.InstanceType != "m7g.2xlarge" || l.Zone != "subnet-a-az" || l.ProviderID != "i-0abc" {
		t.Errorf("launched %+v, want m7g.2xlarge in subnet-a-az", l)
	}
}

// With the first fallback out of capacity too, the launch goes on to the
// second, in the same order.
func TestLaunchFallsBackToTheSecondFallback(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity",
	}, nil)
	template := `{"region": "eu-north-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge",
		"fallbackInstanceTypes": ["m7g.2xlarge", "c8g.2xlarge"], "subnets": ["subnet-a", "subnet-b"]}`
	l, err := New(url, discard).Launch(context.Background(), json.RawMessage(template), nil, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b", "m7g.2xlarge@subnet-a", "m7g.2xlarge@subnet-b", "c8g.2xlarge@subnet-a"}
	if got := placements(*attempts); !slices.Equal(got, want) {
		t.Errorf("attempts %v, want %v", got, want)
	}
	if l.InstanceType != "c8g.2xlarge" || l.Zone != "subnet-a-az" {
		t.Errorf("launched %+v, want c8g.2xlarge in subnet-a-az", l)
	}
}

// Only capacity errors move to the next candidate: a quota, a permission or
// a bad parameter would fail every candidate the same way, so Launch
// returns it after its one request.
func TestLaunchDoesNotFallBackOnOtherErrors(t *testing.T) {
	for _, code := range []string{"InstanceLimitExceeded", "VcpuLimitExceeded", "MaxSpotInstanceCountExceeded",
		"UnauthorizedOperation", "InvalidParameterValue"} {
		t.Run(code, func(t *testing.T) {
			url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: code}, nil)
			_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Errorf("err %v, want %s", err, code)
			}
			if len(*attempts) != 1 {
				t.Errorf("%d RunInstances requests, want 1: %v", len(*attempts), *attempts)
			}
		})
	}
}

// A non-capacity error after capacity failures ends the launch there and
// says which candidate it came from.
func TestLaunchStopsAtANonCapacityErrorMidway(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}:        "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "subnet-a"}: "VcpuLimitExceeded",
	}, nil)
	_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "VcpuLimitExceeded") || !strings.Contains(err.Error(), "m7g.2xlarge in subnet-a") {
		t.Errorf("err %v, want VcpuLimitExceeded naming m7g.2xlarge in subnet-a", err)
	}
	if len(*attempts) != 4 {
		t.Errorf("%d RunInstances requests, want 4: %v", len(*attempts), *attempts)
	}
}

// Every candidate without capacity: the error leads with the code (the
// provisioner keeps 200 characters), names the candidates, and ends with
// EC2's own error for the template's first candidate. EC2's message names
// the zone, so wrapping whichever candidate came last would differ by the
// subnet the launch began in; this reads the same for every rotation, and
// repeated failures fold into one pool event. One request per candidate:
// none is SDK-retried.
func TestLaunchWithNoCapacityAnywhere(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity",
	}, nil)
	p := New(url, discard)
	p.SkipNoCapacityFor(0) // a full sweep every launch, each from another subnet
	want := "ec2 RunInstances: InsufficientInstanceCapacity for every candidate: " +
		"m8g.2xlarge, m7g.2xlarge in subnet-a, subnet-b, subnet-c: " +
		"operation error EC2: RunInstances, https response error StatusCode: 500, api error InsufficientInstanceCapacity: " +
		"We currently do not have sufficient m8g.2xlarge capacity in the Availability Zone you requested (subnet-a-az)."
	for i := range 3 {
		_, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
		if err == nil {
			t.Fatal("launched without capacity")
		}
		if got := strings.ReplaceAll(err.Error(), "RequestID: 1, ", ""); got != want {
			t.Errorf("launch %d (from subnet %d):\n%q\nwant\n%q", i, i, got, want)
		}
	}
	if len(*attempts) != 18 {
		t.Errorf("%d RunInstances requests for 3×6 candidates: %v", len(*attempts), *attempts)
	}
}

// The all-fail error names the head candidate's mark and lists the
// candidates for a spot pool, a pool on its launch template's type and a
// pool without subnets. The second launch starts at the other subnet and
// skips everything, so it reads the head's mark from the first sweep.
func TestLaunchWithNoCapacityAnywhereNamesTheHeadPerPoolShape(t *testing.T) {
	const tail = "operation error EC2: RunInstances, https response error StatusCode: 500, RequestID: 1, api error InsufficientInstanceCapacity: "
	for _, c := range []struct{ name, tmpl, want string }{
		{"spot", `{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge", "spot": true, "subnets": ["subnet-a", "subnet-b"]}`,
			"ec2 RunInstances: InsufficientInstanceCapacity for every candidate: m8g.2xlarge in subnet-a, subnet-b: " + tail + noCapacityMessage("m8g.2xlarge", "subnet-a")},
		{"template type", `{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "subnets": ["subnet-a", "subnet-b"]}`,
			"ec2 RunInstances: InsufficientInstanceCapacity for every candidate: template type in subnet-a, subnet-b: " + tail + noCapacityMessage("", "subnet-a")},
		{"no subnets", `{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge"}`,
			"ec2 RunInstances: InsufficientInstanceCapacity for every candidate: m8g.2xlarge: " + tail + noCapacityMessage("m8g.2xlarge", "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, _, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity", {"", "*"}: "InsufficientInstanceCapacity"}, nil)
			p := New(url, discard)
			for i := range 2 {
				if _, err := p.Launch(context.Background(), json.RawMessage(c.tmpl), nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err == nil || err.Error() != c.want {
					t.Errorf("launch %d: %v, want %s", i, err, c.want)
				}
			}
		})
	}
}

// Zones failing with different codes: the headline code is the template's
// first candidate's, whichever subnet the launch began in.
func TestLaunchWithNoCapacityAnywhereKeepsOneCode(t *testing.T) {
	url, _, _ := capacityEC2(t, map[[2]string]string{
		{"m8g.2xlarge", "subnet-c"}: "Unsupported",
		{"m8g.2xlarge", "*"}:        "InsufficientInstanceCapacity",
		{"m7g.2xlarge", "subnet-a"}: "InsufficientCapacity",
		{"m7g.2xlarge", "*"}:        "Unsupported",
	}, nil)
	p := New(url, discard)
	p.SkipNoCapacityFor(0)
	for i := range 3 {
		_, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
		if err == nil {
			t.Fatal("launched without capacity")
		}
		if head, _, _ := strings.Cut(err.Error(), " for every candidate"); head != "ec2 RunInstances: InsufficientInstanceCapacity" {
			t.Errorf("launch %d: headline %q, want the code of m8g.2xlarge in subnet-a", i, head)
		}
	}
}

// A template without subnets tries its types only, with no SubnetId; one
// without fallbacks makes one request, as before.
func TestLaunchWithoutSubnetsTriesEachType(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", ""}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	template := `{"launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge", "c8g.2xlarge"]}`
	l, err := p.Launch(context.Background(), json.RawMessage(template), nil, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []attempt{{"m8g.2xlarge", "", ""}, {"m7g.2xlarge", "", ""}}; !slices.Equal(*attempts, want) {
		t.Errorf("attempts %v, want %v", *attempts, want)
	}
	if l.InstanceType != "m7g.2xlarge" {
		t.Errorf("launched %+v", l)
	}
	*attempts = nil
	if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(`{"launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge"}`), nil, map[string]string{}); err == nil {
		t.Error("launched without capacity")
	}
	if len(*attempts) != 1 {
		t.Errorf("%d requests without fallbacks or subnets, want 1", len(*attempts))
	}
}

// Errors the SDK retries for any call (a 500 InternalError here) are still
// retried for RunInstances: only capacity errors are not.
func TestLaunchKeepsTheSDKRetryingOtherErrors(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "subnet-a"}: "InternalError"}, nil)
	_, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "InternalError") || !strings.Contains(err.Error(), "exceeded maximum number of attempts, 3") {
		t.Errorf("err %v, want InternalError after the SDK's 3 attempts", err)
	}
	if len(*attempts) != 3 {
		t.Errorf("%d RunInstances requests, want the SDK's 3 for one candidate", len(*attempts))
	}
}

// Each attempt's user data offers the memory of the type it asks for; a
// caller's LUX_RUNNER_MEMORY stays for every attempt.
func TestLaunchUserDataCarriesEachTypesMemory(t *testing.T) {
	fail := map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity", {"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity"}
	mem := map[string]int{"m8g.2xlarge": 32768, "m7g.2xlarge": 16384}

	url, attempts, calls := capacityEC2(t, fail, mem)
	if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err == nil {
		t.Fatal("launched without capacity")
	}
	for _, a := range *attempts {
		if want := map[string]string{"m8g.2xlarge": gib(32), "m7g.2xlarge": gib(16)}[a.instanceType]; a.memory != want {
			t.Errorf("%s in %s offered %q, want %s", a.instanceType, a.subnet, a.memory, want)
		}
	}
	if calls["DescribeInstanceTypes"] != 2 {
		t.Errorf("%d DescribeInstanceTypes for two types", calls["DescribeInstanceTypes"])
	}

	url, attempts, _ = capacityEC2(t, fail, mem)
	if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{"LUX_RUNNER_MEMORY": "1234"}); err == nil {
		t.Fatal("launched without capacity")
	}
	for _, a := range *attempts {
		if a.memory != "1234" {
			t.Errorf("%s in %s offered %q, want the caller's 1234", a.instanceType, a.subnet, a.memory)
		}
	}
}

// One type's memory known and the other's lookup failing, either way
// round: each attempt offers its own type's memory or none, never the
// other type's.
func TestLaunchUserDataMemoryWithOneLookupFailing(t *testing.T) {
	fail := map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity", {"m7g.2xlarge", "*"}: "InsufficientInstanceCapacity"}
	for _, c := range []struct {
		name string
		mem  map[string]int
		want map[string]string // instance type → LUX_RUNNER_MEMORY, "" for none
	}{
		{"fallback lookup fails", map[string]int{"m8g.2xlarge": 32768}, map[string]string{"m8g.2xlarge": gib(32), "m7g.2xlarge": ""}},
		{"primary lookup fails", map[string]int{"m7g.2xlarge": 16384}, map[string]string{"m8g.2xlarge": "", "m7g.2xlarge": gib(16)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, attempts, _ := capacityEC2(t, fail, c.mem)
			if _, err := New(url, discard).Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err == nil {
				t.Fatal("launched without capacity")
			}
			if len(*attempts) != 6 {
				t.Fatalf("%d attempts, want 6", len(*attempts))
			}
			for _, a := range *attempts {
				if a.memory != c.want[a.instanceType] {
					t.Errorf("%s in %s offered %q, want %q", a.instanceType, a.subnet, a.memory, c.want[a.instanceType])
				}
			}
		})
	}
}

// The round robin moves one subnet per Launch, however many attempts the
// launch took.
func TestLaunchAdvancesTheRoundRobinOncePerLaunch(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "subnet-a"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	p.SkipNoCapacityFor(0)
	var first []string
	for range 4 {
		*attempts = nil
		if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err != nil {
			t.Fatal(err)
		}
		first = append(first, (*attempts)[0].subnet)
	}
	if want := []string{"subnet-a", "subnet-b", "subnet-c", "subnet-a"}; !slices.Equal(first, want) {
		t.Errorf("launches started at %v, want %v", first, want)
	}
}

// throttledEC2 is a fake EC2 with EC2's documented default RunInstances
// request bucket (5 burst, 2 per second refill): beyond it, RunInstances
// answers RequestLimitExceeded. The noCapacity types have no capacity
// anywhere and every other type launches. stats are the RunInstances
// requests received (throttled ones included), the throttled count, and the
// (type, subnet) of each request served.
func throttledEC2(t *testing.T, noCapacity ...string) (url string, stats func() (total, throttled int, served []string)) {
	t.Helper()
	awsTestEnv(t)
	var mu sync.Mutex
	tokens, last := 5.0, time.Now()
	var served []string
	total, throttled := 0, 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		if r.PostForm.Get("Action") != "RunInstances" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		tokens = min(5, tokens+2*now.Sub(last).Seconds())
		last = now
		total++
		if tokens < 1 {
			throttled++
			ec2Error(w, http.StatusServiceUnavailable, "RequestLimitExceeded", "Request limit exceeded.")
			return
		}
		tokens--
		it, sn := r.PostForm.Get("InstanceType"), r.PostForm.Get("SubnetId")
		served = append(served, it+"@"+sn)
		if slices.Contains(noCapacity, it) {
			ec2Error(w, http.StatusInternalServerError, "InsufficientInstanceCapacity", noCapacityMessage(it, sn))
			return
		}
		fmt.Fprintf(w, `<RunInstancesResponse><instancesSet><item><instanceId>i-1</instanceId><instanceType>%s</instanceType><placement><availabilityZone>%s-az</availabilityZone></placement></item></instancesSet></RunInstancesResponse>`, it, sn)
	}))
	t.Cleanup(fake.Close)
	return fake.URL, func() (int, int, []string) {
		mu.Lock()
		defer mu.Unlock()
		return total, throttled, slices.Clone(served)
	}
}

// A scale-up of 3 hosts while the primary type has no capacity in either
// subnet: the first launch learns that, and the others go straight to the
// fallback, so k hosts cost len(subnets)+k RunInstances (5), within EC2's
// request burst. Without the marks every launch repeats the primary's
// sweep (3k = 9 calls), which the bucket throttles.
func TestLaunchSkipsCandidatesRecentlyWithoutCapacity(t *testing.T) {
	url, stats := throttledEC2(t, "m8g.2xlarge")
	p := New(url, discard)
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env",
		"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge"], "subnets": ["subnet-a", "subnet-b"]}`)
	for i := range 3 {
		l, err := p.Launch(context.Background(), template, nil, map[string]string{"LUX_RUNNER_MEMORY": "1"})
		if err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
		if l.InstanceType != "m7g.2xlarge" {
			t.Errorf("launch %d got %+v, want the fallback", i, l)
		}
	}
	total, throttled, served := stats()
	want := []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b", "m7g.2xlarge@subnet-a",
		"m7g.2xlarge@subnet-b", "m7g.2xlarge@subnet-a"}
	if total != 5 || throttled != 0 || !slices.Equal(served, want) {
		t.Errorf("%d RunInstances (%d throttled), served %v; want 5 (none throttled): %v", total, throttled, served, want)
	}
}

// A pool with no capacity for any of its 5 types in its 3 subnets, launched
// on every 1s provisioner pass for 65s: it sweeps its 15 candidates once per
// DefaultNoCapacityRetryAfter (at 0s, 30s and 60s), not once per pass, and every
// pass fails with the same error, whether it swept or skipped them all.
func TestLaunchStuckPoolSweepsOncePerRetryAfter(t *testing.T) {
	fail := map[[2]string]string{}
	for _, it := range []string{"m8g.2xlarge", "m7g.2xlarge", "c8g.2xlarge", "c7g.2xlarge", "r8g.2xlarge"} {
		fail[[2]string{it, "*"}] = "InsufficientInstanceCapacity"
	}
	url, attempts, _ := capacityEC2(t, fail, nil)
	p := New(url, discard)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env",
		"instanceType": "m8g.2xlarge", "fallbackInstanceTypes": ["m7g.2xlarge", "c8g.2xlarge", "c7g.2xlarge", "r8g.2xlarge"],
		"subnets": ["subnet-a", "subnet-b", "subnet-c"]}`)
	var first string
	var sweeps []int // seconds at which a pass called EC2
	for sec := range 66 {
		before := len(*attempts)
		_, err := p.Launch(context.Background(), template, nil, map[string]string{})
		if err == nil {
			t.Fatal("launched without capacity")
		}
		if sec == 0 {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("pass at %ds failed with\n%q\nwant\n%q", sec, err, first)
		}
		if n := len(*attempts) - before; n > 0 {
			if n != 15 {
				t.Errorf("pass at %ds made %d RunInstances, want a sweep of 15", sec, n)
			}
			sweeps = append(sweeps, sec)
		}
		now = now.Add(time.Second)
	}
	if want := []int{0, 30, 60}; !slices.Equal(sweeps, want) {
		t.Errorf("sweeps at %vs, want %vs", sweeps, want)
	}
}

// Marks are per region, type and subnet (market and launch template below):
// a type without capacity in one subnet is still tried in the others, and the
// same type in another region is not skipped.
func TestLaunchSkipsOnlyTheMarkedCandidates(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "subnet-a"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	got := func() []string {
		defer func() { *attempts = nil }()
		return placements(*attempts)
	}
	for range 3 {
		if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := got(), []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-c"}; !slices.Equal(got, want) {
		t.Errorf("attempts %v, want %v", got, want)
	}
	// The fourth launch begins at subnet-a again, in a region with no mark.
	other := strings.Replace(fallbackTemplate, "eu-north-1", "eu-west-1", 1)
	if _, err := p.Launch(context.Background(), json.RawMessage(other), nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if got, want := got(), []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b"}; !slices.Equal(got, want) {
		t.Errorf("another region: attempts %v, want %v", got, want)
	}
}

// Spot and on-demand capacity are separate in EC2: a mark from one market
// does not skip the same type and subnet in the other, either way round.
func TestLaunchMarksArePerMarket(t *testing.T) {
	const spot = `{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge", "spot": true, "subnets": ["subnet-a"]}`
	const onDemand = `{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m8g.2xlarge", "subnets": ["subnet-a"]}`
	for _, c := range []struct{ name, first, second string }{
		{"spot then on-demand", spot, onDemand},
		{"on-demand then spot", onDemand, spot},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity"}, nil)
			p := New(url, discard)
			for i, tmpl := range []string{c.first, c.second} {
				*attempts = nil
				if _, err := p.Launch(context.Background(), json.RawMessage(tmpl), nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err == nil {
					t.Fatal("launched without capacity")
				}
				if len(*attempts) != 1 {
					t.Errorf("pool %d made %d RunInstances, want 1", i, len(*attempts))
				}
			}
		})
	}
}

// Template-type candidates (no instanceType) on different launch templates
// keep separate marks, like every candidate.
func TestLaunchTemplateTypeMarksArePerLaunchTemplate(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"", "subnet-a"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	// lt-a's second launch is within its own mark.
	var calls []int
	for _, lt := range []string{"lt-a", "lt-b", "lt-a"} {
		before := len(*attempts)
		tmpl := `{"region": "eu-west-1", "launchTemplate": "` + lt + `", "userData": "env", "subnets": ["subnet-a"]}`
		if _, err := p.Launch(context.Background(), json.RawMessage(tmpl), nil, map[string]string{}); err == nil {
			t.Fatal("launched without capacity")
		}
		calls = append(calls, len(*attempts)-before)
	}
	if want := []int{1, 1, 0}; !slices.Equal(calls, want) {
		t.Errorf("RunInstances per launch (lt-a, lt-b, lt-a) %v, want %v", calls, want)
	}
}

// A launch template can carry a zone, placement group, tenancy or capacity
// reservation lux does not see, so pools on different launch templates do
// not share marks even for the same type, market and subnet.
func TestLaunchMarksArePerLaunchTemplate(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err != nil {
		t.Fatal(err)
	}
	*attempts = nil
	other := strings.Replace(fallbackTemplate, `"lt-1"`, `"lt-2"`, 1)
	if _, err := p.Launch(context.Background(), json.RawMessage(other), nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"m8g.2xlarge@subnet-a", "m8g.2xlarge@subnet-b", "m8g.2xlarge@subnet-c", "m7g.2xlarge@subnet-a"}
	if got := placements(*attempts); !slices.Equal(got, want) {
		t.Errorf("lt-2 attempts %v, want %v: lt-1's marks do not skip m8g.2xlarge", got, want)
	}
}

// A sweep's marks share one clock reading, so they expire together and a
// later pass sweeps every candidate or none, however long each
// RunInstances takes and wherever in its tick a pass starts. The clock
// moves 5ms per reading (a round trip); pass 3 starts 3ms early, between
// the times the two subnets would have been marked a round trip apart.
func TestLaunchSweepDoesNotSplit(t *testing.T) {
	url, attempts, _ := capacityEC2(t, map[[2]string]string{{"m7i.large", "*"}: "InsufficientInstanceCapacity"}, nil)
	p := New(url, discard)
	p.SkipNoCapacityFor(3 * time.Second)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var off time.Duration
	p.now = func() time.Time { off += 5 * time.Millisecond; return base.Add(off) }
	tmpl := json.RawMessage(`{"region": "us-east-1", "launchTemplate": "lt-1", "userData": "env",
		"instanceType": "m7i.large", "subnets": ["subnet-a", "subnet-b"]}`)
	var calls []int
	for pass := range 9 {
		off = time.Duration(pass) * time.Second
		if pass == 3 {
			off -= 3 * time.Millisecond
		}
		before := len(*attempts)
		if _, err := p.Launch(context.Background(), tmpl, nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err == nil {
			t.Fatal("launched without capacity")
		}
		calls = append(calls, len(*attempts)-before)
	}
	if want := []int{2, 0, 0, 0, 2, 0, 0, 2, 0}; !slices.Equal(calls, want) {
		t.Errorf("RunInstances per pass %v, want %v", calls, want)
	}
}

// Within noCapacityFor of the primary's mark a launch goes straight to the
// fallback; once it expires, the primary is asked first again, and gets
// the host if its capacity came back.
func TestLaunchRetriesThePrimaryOnceItsMarkExpires(t *testing.T) {
	fail := map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity"}
	url, attempts, _ := capacityEC2(t, fail, nil)
	p := New(url, discard)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	env := map[string]string{"LUX_RUNNER_MEMORY": "1"}
	launch := func(want string) {
		t.Helper()
		*attempts = nil
		l, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, env)
		if err != nil {
			t.Fatal(err)
		}
		if l.InstanceType != want {
			t.Errorf("at %s launched %s (attempts %v), want %s", now.Format(time.TimeOnly), l.InstanceType, placements(*attempts), want)
		}
	}
	launch("m7g.2xlarge")
	// The fake reads fail only while serving a request, none of which is
	// in flight here.
	delete(fail, [2]string{"m8g.2xlarge", "*"})
	now = now.Add(DefaultNoCapacityRetryAfter - time.Second)
	launch("m7g.2xlarge")
	if got, want := placements(*attempts), []string{"m7g.2xlarge@subnet-b"}; !slices.Equal(got, want) {
		t.Errorf("before expiry: attempts %v, want %v", got, want)
	}
	now = now.Add(time.Second)
	launch("m8g.2xlarge")
	if got, want := placements(*attempts), []string{"m8g.2xlarge@subnet-c"}; !slices.Equal(got, want) {
		t.Errorf("after expiry: attempts %v, want %v", got, want)
	}
}

// Each capacity failure is logged at info; a launch that gets a later
// candidate warns once, naming what it got and how many it failed or
// skipped. A skipped candidate logs nothing.
func TestLaunchLogsFallbacks(t *testing.T) {
	url, _, _ := capacityEC2(t, map[[2]string]string{{"m8g.2xlarge", "*"}: "InsufficientInstanceCapacity"}, nil)
	var buf strings.Builder
	p := New(url, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})))
	lines := func() []string {
		defer buf.Reset()
		return strings.Split(strings.TrimSpace(buf.String()), "\n")
	}
	env := map[string]string{"LUX_RUNNER_MEMORY": "1"}
	if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, env); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`level=INFO msg="ec2: no capacity" instanceType=m8g.2xlarge subnet=subnet-a code=InsufficientInstanceCapacity`,
		`level=INFO msg="ec2: no capacity" instanceType=m8g.2xlarge subnet=subnet-b code=InsufficientInstanceCapacity`,
		`level=INFO msg="ec2: no capacity" instanceType=m8g.2xlarge subnet=subnet-c code=InsufficientInstanceCapacity`,
		`level=WARN msg="ec2: launched a later candidate: earlier ones had no capacity" instanceType=m7g.2xlarge subnet=subnet-a failed=3 skipped=0`,
	}
	if got := lines(); !slices.Equal(got, want) {
		t.Errorf("first launch logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := p.Launch(context.Background(), json.RawMessage(fallbackTemplate), nil, env); err != nil {
		t.Fatal(err)
	}
	want = []string{`level=WARN msg="ec2: launched a later candidate: earlier ones had no capacity" instanceType=m7g.2xlarge subnet=subnet-b failed=0 skipped=3`}
	if got := lines(); !slices.Equal(got, want) {
		t.Errorf("second launch logged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// awsTestEnv points the AWS SDK's configuration away from the machine's:
// static credentials, no shared files, no IMDS, the default retryer.
func awsTestEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_RETRY_MODE", "")
	t.Setenv("AWS_MAX_ATTEMPTS", "")
}

// ec2Error writes an EC2 query-API error reply.
func ec2Error(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>1</RequestID></Response>`, code, msg)
}

// noCapacityMessage is EC2's capacity error wording, which names the zone
// asked for (here subnet+"-az").
func noCapacityMessage(instanceType, subnet string) string {
	return fmt.Sprintf("We currently do not have sufficient %s capacity in the Availability Zone you requested (%s-az).", instanceType, subnet)
}

// placements is each attempt as "type@subnet".
func placements(attempts []attempt) []string {
	var out []string
	for _, a := range attempts {
		out = append(out, a.instanceType+"@"+a.subnet)
	}
	return out
}

func gib(n int64) string { return strconv.FormatInt(n<<30, 10) }

// runnerEnvOf is the runner env user data in format carries, key to value:
// env's lines, script's exports, or Ignition's decoded runner.env.
func runnerEnvOf(t *testing.T, format, ud string) map[string]string {
	t.Helper()
	text := ud
	if format == "ignition" {
		text = decodedUserData(t, ud)
	}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if format == "script" {
			var ok bool
			if line, ok = strings.CutPrefix(line, "export "); !ok {
				continue
			}
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "LUX_") {
			continue
		}
		if format == "script" {
			v = strings.TrimSuffix(strings.TrimPrefix(v, "'"), "'")
		}
		out[k] = v
	}
	return out
}

// decodedUserData is user data with Ignition's embedded runner.env decoded,
// so every format can be searched as text.
func decodedUserData(t *testing.T, ud string) string {
	t.Helper()
	var cfg struct {
		Storage struct {
			Files []struct {
				Path     string `json:"path"`
				Contents struct {
					Source string `json:"source"`
				} `json:"contents"`
			} `json:"files"`
		} `json:"storage"`
	}
	if json.Unmarshal([]byte(ud), &cfg) != nil {
		return ud
	}
	for _, f := range cfg.Storage.Files {
		if f.Path == "/etc/lux/runner.env" {
			data, ok := strings.CutPrefix(f.Contents.Source, "data:;base64,")
			if !ok {
				t.Fatalf("runner.env source is not a base64 data URL: %.40s", f.Contents.Source)
			}
			raw, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatalf("runner.env source: %v", err)
			}
			return string(raw)
		}
	}
	t.Fatal("ignition config has no /etc/lux/runner.env")
	return ""
}

// fakeEC2 answers DescribeInstanceTypes (memMiB, an error when 0, no answer
// until the client gives up when negative) and RunInstances, counting calls
// and keeping each launch's user data.
func fakeEC2(t *testing.T, memMiB int) (url string, calls map[string]int, userData *[]string) {
	t.Helper()
	awsTestEnv(t)
	calls, userData = map[string]int{}, &[]string{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		action := r.PostForm.Get("Action")
		calls[action]++
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "DescribeInstanceTypes":
			if memMiB < 0 {
				<-r.Context().Done()
				return
			}
			if memMiB == 0 {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>no</Message></Error></Errors><RequestID>1</RequestID></Response>`)
				return
			}
			fmt.Fprintf(w, `<DescribeInstanceTypesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><instanceTypeSet><item>`+
				`<instanceType>%s</instanceType><memoryInfo><sizeInMiB>%d</sizeInMiB></memoryInfo></item></instanceTypeSet></DescribeInstanceTypesResponse>`,
				r.PostForm.Get("InstanceType.1"), memMiB)
		case "RunInstances":
			ud, _ := base64.StdEncoding.DecodeString(r.PostForm.Get("UserData"))
			*userData = append(*userData, string(ud))
			fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
				`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>c7a.8xlarge</instanceType>`+
				`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(fake.Close)
	return fake.URL, calls, userData
}

// A template naming its instance type launches a runner offering that
// type's gross memory; the type is looked up once for the process.
func TestLaunchOffersTheInstanceTypesMemory(t *testing.T) {
	url, calls, userData := fakeEC2(t, 65536)
	p := New(url, discard)
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	for range 2 {
		if _, err := p.Launch(context.Background(), template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls["DescribeInstanceTypes"] != 1 || calls["RunInstances"] != 2 {
		t.Errorf("calls %v, want one DescribeInstanceTypes for two RunInstances", calls)
	}
	for _, ud := range *userData {
		if !slices.Contains(strings.Split(ud, "\n"), fmt.Sprintf("LUX_RUNNER_MEMORY=%d", int64(65536)<<20)) {
			t.Errorf("user data without the type's memory:\n%s", ud)
		}
	}
}

// A failed lookup never fails the launch: the runner offers its MemTotal.
func TestLaunchWithoutTheInstanceTypesMemory(t *testing.T) {
	url, calls, userData := fakeEC2(t, 0)
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	if _, err := New(url, discard).Launch(context.Background(), template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
		t.Fatalf("launch failed with the lookup: %v", err)
	}
	if calls["RunInstances"] != 1 || len(*userData) != 1 || strings.Contains((*userData)[0], "LUX_RUNNER_MEMORY") {
		t.Errorf("calls %v, user data %q: want one launch without LUX_RUNNER_MEMORY", calls, *userData)
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// A lookup that never answers costs its own timeout, not the launch; the
// type is not asked again while its failure is recent.
func TestLaunchDoesNotWaitOnAHangingLookup(t *testing.T) {
	url, calls, userData := fakeEC2(t, -1)
	p := New(url, discard)
	p.memoryTimeout = 100 * time.Millisecond
	template := json.RawMessage(`{"region": "eu-west-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "c7a.8xlarge"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		start := time.Now()
		if _, err := p.Launch(ctx, template, nil, map[string]string{"LUX_URL": "http://luxd"}); err != nil {
			t.Fatalf("launch failed with the lookup hanging: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("launch took %s with a %s lookup timeout", d, p.memoryTimeout)
		}
	}
	if calls["DescribeInstanceTypes"] != 1 || calls["RunInstances"] != 2 {
		t.Errorf("calls %v, want one DescribeInstanceTypes for two RunInstances", calls)
	}
	for _, ud := range *userData {
		if strings.Contains(ud, "LUX_RUNNER_MEMORY") {
			t.Errorf("user data with a memory nobody gave:\n%s", ud)
		}
	}
}
