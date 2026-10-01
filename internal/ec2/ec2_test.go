package ec2

import (
	"context"
	"encoding/base64"
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

// The runner env a pool's instances boot with: LUX_NESTED=true only when the
// template opts in, and a caller's own LUX_NESTED never leaks through a
// template that does not.
func TestLaunchUserDataCarriesNestedOnlyForAnOptedInTemplate(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	var userData string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		raw, _ := base64.StdEncoding.DecodeString(r.PostForm.Get("UserData"))
		userData = string(raw)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationId>r-1</reservationId>`+
			`<instancesSet><item><instanceId>i-0abc</instanceId><instanceType>m8g.2xlarge</instanceType>`+
			`<placement><availabilityZone>eu-north-1a</availabilityZone></placement>`+
			`<instanceState><code>0</code><name>pending</name></instanceState></item></instancesSet></RunInstancesResponse>`)
	}))
	defer fake.Close()
	launch := func(template string, env map[string]string) string {
		t.Helper()
		if _, err := New(fake.URL).Launch(context.Background(), json.RawMessage(template), nil, env); err != nil {
			t.Fatal(err)
		}
		return userData
	}
	base := map[string]string{"LUX_URL": "http://10.0.1.10:7070", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h"}
	for _, format := range []string{"env", "script", "ignition"} {
		on := decodedUserData(t, launch(`{"launchTemplate":"lt-1","userData":"`+format+`","nestedContainers":true}`, base))
		if !strings.Contains(on, "LUX_NESTED") || !strings.Contains(on, "true") {
			t.Errorf("%s: an opted-in template's user data lacks LUX_NESTED=true", format)
		}
		if format == "env" && !strings.Contains(on, "LUX_NESTED=true\n") {
			t.Errorf("env: %q", on)
		}
		off := decodedUserData(t, launch(`{"launchTemplate":"lt-1","userData":"`+format+`"}`, map[string]string{"LUX_URL": "u", "LUX_HOST_TOKEN": "t", "LUX_NESTED": "true"}))
		if strings.Contains(off, "LUX_NESTED") {
			t.Errorf("%s: a template without nestedContainers boots nested hosts", format)
		}
	}
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
