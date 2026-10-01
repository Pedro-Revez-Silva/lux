package ec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
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
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
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

// fakeEC2 answers DescribeInstanceTypes (memMiB, an error when 0, no answer
// until the client gives up when negative) and RunInstances, counting calls
// and keeping each launch's user data.
func fakeEC2(t *testing.T, memMiB int) (url string, calls map[string]int, userData *[]string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
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
