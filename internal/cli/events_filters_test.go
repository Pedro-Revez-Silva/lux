package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lux events --all sends --label (each as given, an empty value too) and
// --type (comma-separated or repeated, joined) as GET /v1/events' query.
func TestEventsAllFilters(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer srv.Close()
	for _, args := range [][]string{
		{"events", "--all", "--follow=false", "-l", "k=v", "--type", "state"},
		{"events", "--all", "--follow=false", "--label", "k=", "-l", "a=b=c", "--type", "state,exited", "--type", "input.consumed"},
		{"events", "--all", "--follow=false", "--after", "7"},
	} {
		if code, _, errOut := lux(srv.URL, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
	}
	want := []string{
		"/v1/events?follow=false&label=k%3Dv&type=state",
		"/v1/events?follow=false&label=k%3D&label=a%3Db%3Dc&type=state%2Cexited%2Cinput.consumed",
		"/v1/events?after=7&follow=false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Refused when given at all, an empty value too.
	got = nil
	for _, args := range [][]string{{"--type", "state"}, {"--type="}, {"-l", ""}} {
		args = append([]string{"events", "run_1"}, args...)
		if code, _, errOut := lux(srv.URL, args...); code == 0 || !strings.Contains(errOut, "--label and --type go with --all") {
			t.Fatalf("%v without --all: exit %d, stderr %q", args, code, errOut)
		}
	}
	if len(got) != 0 {
		t.Fatalf("requests without --all: %v", got)
	}
}
