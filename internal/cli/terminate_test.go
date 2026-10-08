package cli

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// lux terminate posts to /terminate; lux cancel, its hidden deprecated
// alias, posts there too and says on stderr that it is deprecated. Only
// terminate is listed in help.
func TestTerminateAndCancelAlias(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		wantStderr string
	}{{"terminate", ""}, {"cancel", "Command \"cancel\" is deprecated, use lux terminate\n"}} {
		t.Run(c.cmd, func(t *testing.T) {
			var posted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posted = append(posted, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"run_1","state":"stopping"}`))
			}))
			defer srv.Close()
			code, out, errOut := lux(srv.URL, c.cmd, "run_1")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if len(posted) != 1 || posted[0] != "POST /v1/runs/run_1/terminate" {
				t.Fatalf("requests: %v", posted)
			}
			if errOut != c.wantStderr {
				t.Fatalf("stderr %q, want %q", errOut, c.wantStderr)
			}
			if strings.TrimSpace(out) != "run_1 stopping" {
				t.Fatalf("stdout %q", out)
			}
		})
	}
	a := &app{stdin: strings.NewReader(""), stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	root := a.root()
	terminate, _, err := root.Find([]string{"terminate"})
	if err != nil || !terminate.IsAvailableCommand() {
		t.Fatalf("terminate not listed in help: %v", err)
	}
	cancel, _, err := root.Find([]string{"cancel"})
	if err != nil || cancel.Name() != "cancel" {
		t.Fatalf("cancel: %v", err)
	}
	if cancel.IsAvailableCommand() {
		t.Fatal("cancel is listed in help")
	}
}

// lux terminate --wait waits for terminated alone: a Run that reads
// succeeded on the way (its placement exited before the terminate took)
// is not where it stops.
func TestTerminateWaitsForTerminated(t *testing.T) {
	gets := []string{"stopping", "succeeded", "terminated"}
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		state := "stopping"
		if r.Method == http.MethodGet {
			state, gets = gets[0], gets[min(1, len(gets)-1):]
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		_, _ = w.Write([]byte(`{"id":"run_1","state":"` + state + `"}`))
	}))
	defer srv.Close()
	code, out, errOut := lux(srv.URL, "terminate", "--wait", "run_1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "run_1 terminated" {
		t.Fatalf("stdout %q, want run_1 terminated", got)
	}
	want := []string{"POST /v1/runs/run_1/terminate", "GET /v1/runs/run_1", "GET /v1/runs/run_1", "GET /v1/runs/run_1"}
	if !slices.Equal(requests, want) {
		t.Fatalf("requests %v, want %v", requests, want)
	}
}
