package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

func TestDownloadPathStaysInside(t *testing.T) {
	for _, bad := range []string{"/../../etc/passwd", "../x", "/workspace/../../x", "/", ""} {
		if p, err := downloadPath("/tmp/d", 1, bad); err == nil {
			t.Errorf("%q → %q: want refused", bad, p)
		}
	}
	p, err := downloadPath("/tmp/d", 2, "/workspace/out/a.txt")
	if err != nil || p != filepath.FromSlash("/tmp/d/2/workspace/out/a.txt") {
		t.Errorf("got %q, %v", p, err)
	}
}

func TestParseAddRepo(t *testing.T) {
	f := false
	for in, want := range map[string]spec.Repository{
		"two=http://10.0.0.1:8080/two.git":                  {Name: "two", URL: "http://10.0.0.1:8080/two.git"},
		"two=https://h/o/two.git@main,credential=GIT_TOKEN": {Name: "two", URL: "https://h/o/two.git", Ref: "main", Credential: "GIT_TOKEN"},
		"g=git@github.com:o/r.git":                          {Name: "g", URL: "git@github.com:o/r.git"},
		"g=git@github.com:r.git":                            {Name: "g", URL: "git@github.com:r.git"},
		"g=git@github.com:o/r.git@v1":                       {Name: "g", URL: "git@github.com:o/r.git", Ref: "v1"},
		"s=ssh://git@h/o/r.git":                             {Name: "s", URL: "ssh://git@h/o/r.git"},
		"b=https://h/r.git,ref=feature/x,path=/workspace/b": {Name: "b", URL: "https://h/r.git", Ref: "feature/x", Path: "/workspace/b"},
		"c=https://h/r.git,push=false,credential=T":         {Name: "c", URL: "https://h/r.git", Push: &f, Credential: "T"},
		"q=https://h/r.git?a=1,b=2":                         {Name: "q", URL: "https://h/r.git?a=1,b=2"},
	} {
		got, err := parseAddRepo(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "two", "two=", "=u", "t=,credential=X", "t=https://h/r.git@a,ref=b", "t=https://h/r.git,push=maybe"} {
		if r, err := parseAddRepo(bad); err == nil {
			t.Errorf("%q: want an error, got %+v", bad, r)
		}
	}
}

// pools set --default alone sends a marker-only body, exactly name and
// isDefault, which luxd refuses to read as anything else; with a setting,
// the whole pool.
func TestPoolsSetDefaultSendsOnlyTheMark(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		got = append(got, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, args := range [][]string{{"pools", "set", "a", "--default"}, {"pools", "set", "a", "--default=false"}, {"pools", "set", "a", "--provider", "static", "--default"}} {
		a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
		root := a.root()
		root.SetArgs(append([]string{"--url", srv.URL, "--api-key", "k"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if want := (map[string]any{"name": "a", "isDefault": true}); !reflect.DeepEqual(got[0], want) {
		t.Errorf("--default sent %v, want %v", got[0], want)
	}
	if want := (map[string]any{"name": "a", "isDefault": false}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("--default=false sent %v, want %v", got[1], want)
	}
	if got[2]["provider"] != "static" || got[2]["isDefault"] != true {
		t.Errorf("a full set sent %v", got[2])
	}
}
