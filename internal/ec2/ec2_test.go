package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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
	if _, err := New(fake.URL).Launch(context.Background(), json.RawMessage(template), lux, map[string]string{}); err != nil {
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
	p := New(fake.URL)
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

// Retag sends one CreateTags for every instance, with the one tag, and
// passes an IAM refusal back as an error.
func TestRetagSendsCreateTags(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var got []map[string]string
	deny := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		got = append(got, form)
		w.Header().Set("Content-Type", "text/xml")
		if deny {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>no</Message></Error></Errors><RequestID>x</RequestID></Response>`)
			return
		}
		fmt.Fprint(w, `<CreateTagsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><return>true</return></CreateTagsResponse>`)
	}))
	defer fake.Close()
	p := New(fake.URL)
	tmpl := json.RawMessage(`{"region":"eu-west-1"}`)
	if err := p.Retag(context.Background(), tmpl, []string{"i-1", "i-2"}, "lux:pool", "t1/new"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Action": "CreateTags", "ResourceId.1": "i-1", "ResourceId.2": "i-2", "Tag.1.Key": "lux:pool", "Tag.1.Value": "t1/new"}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %q, want %q (%v)", k, got[0][k], v, got[0])
		}
	}
	if _, extra := got[0]["Tag.2.Key"]; extra {
		t.Errorf("more than one tag sent: %v", got[0])
	}
	deny = true
	if err := p.Retag(context.Background(), tmpl, []string{"i-1"}, "lux:pool", "t1/new"); err == nil || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Errorf("a refused CreateTags: err = %v", err)
	}
}

// describeFake is an EC2 answering DescribeInstances by id for the known
// ones, and refusing a whole call that names an unknown one, as EC2 does.
// It records the ids of each call.
func describeFake(t *testing.T, known map[string]string) (*Provider, *[][]string) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var calls [][]string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var ids []string
		for i := 1; r.PostForm.Get(fmt.Sprintf("InstanceId.%d", i)) != ""; i++ {
			ids = append(ids, r.PostForm.Get(fmt.Sprintf("InstanceId.%d", i)))
		}
		calls = append(calls, ids)
		w.Header().Set("Content-Type", "text/xml")
		items := ""
		for _, id := range ids {
			state, ok := known[id]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>%s</Message></Error></Errors><RequestID>x</RequestID></Response>`, id)
				return
			}
			items += `<item><instanceId>` + id + `</instanceId><instanceState><code>16</code><name>` + state + `</name></instanceState>` +
				`<tagSet><item><key>lux:pool</key><value>t1/a</value></item></tagSet></item>`
		}
		fmt.Fprint(w, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationSet><item><reservationId>r-0</reservationId>`+
			`<instancesSet>`+items+`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
	}))
	t.Cleanup(fake.Close)
	return New(fake.URL), &calls
}

// Describe asks for the instances by id, and when EC2 refuses the call
// for an id it does not know, splits the call until that id is alone and
// leaves it out.
func TestDescribeByID(t *testing.T) {
	p, calls := describeFake(t, map[string]string{"i-1": "running", "i-2": "terminated"})
	got, err := p.Describe(context.Background(), json.RawMessage(`{"region":"eu-west-1"}`), []string{"i-1", "i-2", "i-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["i-1"].State != "running" || got["i-2"].State != "terminated" || got["i-1"].Tags["lux:pool"] != "t1/a" {
		t.Errorf("got %+v", got)
	}
	if _, ok := got["i-gone"]; ok {
		t.Error("an id EC2 does not know was answered")
	}
	if fmt.Sprint(*calls) != "[[i-1 i-2 i-gone] [i-1] [i-2 i-gone] [i-2] [i-gone]]" {
		t.Errorf("calls %v", *calls)
	}
}

// Many ids: at most describeBatch per call, and one unknown id costs a
// bisection of its batch, not a call per id.
func TestDescribeManyIDsIsBounded(t *testing.T) {
	known := map[string]string{}
	var ids []string
	for i := range 250 {
		id := fmt.Sprintf("i-%03d", i)
		ids = append(ids, id)
		if i != 137 {
			known[id] = "running"
		}
	}
	p, calls := describeFake(t, known)
	got, err := p.Describe(context.Background(), json.RawMessage(`{"region":"eu-west-1"}`), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 249 {
		t.Errorf("%d answered, want 249", len(got))
	}
	for _, c := range *calls {
		if len(c) > 100 {
			t.Errorf("a call with %d ids, want at most 100", len(c))
		}
	}
	// Three batches, and one bisection of the second: 2·log2(100) + 1.
	if n := len(*calls); n > 3+2*7 {
		t.Errorf("%d calls for 250 ids with one unknown", n)
	}
}
