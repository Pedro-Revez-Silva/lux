package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// The configured ec2.no_capacity_retry_after reaches the provider: by
// default a second launch within it is refused without calling EC2; with
// 0s every launch asks EC2 again.
func TestProvidersWiresNoCapacityRetryAfter(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_PROFILE", "")
	cfgPath := t.TempDir() + "/luxd.toml"
	if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUX_CONFIG", cfgPath)
	for _, c := range []struct {
		env  string
		want int64
	}{{"", 1}, {"0s", 2}} {
		t.Run("retry_after="+c.env, func(t *testing.T) {
			var runs atomic.Int64
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.PostForm.Get("Action") == "RunInstances" {
					runs.Add(1)
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `<Response><Errors><Error><Code>InsufficientInstanceCapacity</Code><Message>none</Message></Error></Errors><RequestID>1</RequestID></Response>`)
			}))
			defer fake.Close()
			t.Setenv("LUX_EC2_ENDPOINT", fake.URL)
			t.Setenv("LUX_EC2_NO_CAPACITY_RETRY_AFTER", c.env)
			cfg, err := loadConfig("")
			if err != nil {
				t.Fatal(err)
			}
			p := providers(cfg, slog.New(slog.DiscardHandler))["ec2"]
			tmpl := json.RawMessage(`{"region": "us-east-1", "launchTemplate": "lt-1", "userData": "env", "instanceType": "m7i.large", "subnets": ["subnet-a"]}`)
			for range 2 {
				if _, err := p.Launch(context.Background(), tmpl, nil, map[string]string{"LUX_RUNNER_MEMORY": "1"}); err == nil {
					t.Fatal("launched without capacity")
				}
			}
			if got := runs.Load(); got != c.want {
				t.Errorf("RunInstances %d, want %d", got, c.want)
			}
		})
	}
}
