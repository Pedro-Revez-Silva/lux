package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

func TestAuthFile(t *testing.T) {
	dir := t.TempDir()
	auths := []spec.RegistryAuth{
		{Registry: "ghcr.io", Secret: "GH"},
		{Registry: "10.0.0.5:5000", Secret: "REG"},
		{Registry: "missing.example.com", Secret: "UNSET"},
	}
	a, err := writeAuthFile(dir, auths, map[string]string{"GH": "ghp_tok", "REG": "alice:s3cret:x"})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(a.path())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file %v %v", fi, err)
	}
	b, _ := os.ReadFile(a.path())
	var got struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	dec := func(r string) string {
		v, _ := base64.StdEncoding.DecodeString(got.Auths[r].Auth)
		return string(v)
	}
	// A bare token is the password of the user lux; the first colon splits
	// user from password.
	if dec("ghcr.io") != "lux:ghp_tok" || dec("10.0.0.5:5000") != "alice:s3cret:x" || len(got.Auths) != 2 {
		t.Fatalf("auths %s", b)
	}
	e := a.redact(errors.New("podman pull: auth s3cret:x failed for " + got.Auths["ghcr.io"].Auth + " ghp_tok"))
	if strings.Contains(e.Error(), "s3cret") || strings.Contains(e.Error(), "ghp_tok") || strings.Contains(e.Error(), got.Auths["ghcr.io"].Auth) {
		t.Fatalf("not redacted: %v", e)
	}
	a.close()
	if _, err := os.Stat(a.path()); !os.IsNotExist(err) {
		t.Fatalf("auth file left behind: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}

	// No credentials: no file, and a nil auth is usable.
	none, err := writeAuthFile(dir, auths[:1], map[string]string{})
	if err != nil || none != nil || none.path() != "" {
		t.Fatalf("%v %v", none, err)
	}
	none.close()
	if e := none.redact(errors.New("x")); e.Error() != "x" {
		t.Fatal(e)
	}
}

func TestCacheRef(t *testing.T) {
	if got := cacheRef("registry.example.com/team/lux-cache", "0123456789abcdef01234567"); got != "registry.example.com/team/lux-cache:0123456789abcdef01234567" {
		t.Fatal(got)
	}
}

func TestFullRef(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                       "docker.io/library/alpine:latest",
		"alpine:3":                     "docker.io/library/alpine:3",
		"acme/tool:1":                  "docker.io/acme/tool:1",
		"10.0.0.5:5000/x/y":            "10.0.0.5:5000/x/y:latest",
		"localhost/lux-build:k":        "localhost/lux-build:k",
		"ghcr.io/a/b@sha256:abc":       "ghcr.io/a/b@sha256:abc",
		"docker.io/library/alpine:3.2": "docker.io/library/alpine:3.2",
	} {
		if got := fullRef(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestPickEvictions(t *testing.T) {
	const recent = 1000
	c := func(ref, id string, size, last int64, names ...string) imageCandidate {
		if names == nil {
			names = []string{ref}
		}
		return imageCandidate{Ref: ref, ID: id, Names: names, Size: size, LastUsed: last}
	}
	cands := []imageCandidate{
		c("localhost/lux-build:new", "n", 100, 900),
		c("localhost/lux-build:old", "o", 100, 100),
		c("localhost/lux-build:mid", "m", 300, 500),
		// Two lux names on one image: removed together, as used as the
		// latest of them.
		c("localhost/lux-build:k", "c", 50, 200, "localhost/lux-build:k", "reg.example.com/cache:k"),
		c("reg.example.com/cache:k", "c", 50, 600, "localhost/lux-build:k", "reg.example.com/cache:k"),
		// In use by a container.
		c("docker.io/library/busy:1", "b", 1000, 1),
		// Also named by an operator: removing lux's name frees nothing.
		c("localhost/lux-base:d", "shared", 1000, 2, "localhost/lux-base:d", "docker.io/library/theirs:1"),
		// Used just now.
		c("localhost/lux-build:hot", "h", 1000, 5000),
	}
	inUse := map[string]bool{"b": true}

	if got := pickEvictions(cands, inUse, nil, 0, recent); len(got) != 0 {
		t.Fatalf("nothing needed: %v", got)
	}
	if got := pickEvictions(cands, inUse, nil, 50, recent); !reflect.DeepEqual(got, []string{"localhost/lux-build:old"}) {
		t.Fatal(got)
	}
	if got := pickEvictions(cands, inUse, nil, 350, recent); !reflect.DeepEqual(got, []string{"localhost/lux-build:old", "localhost/lux-build:mid"}) {
		t.Fatal(got)
	}
	want := []string{"localhost/lux-build:old", "localhost/lux-build:mid", "localhost/lux-build:k", "reg.example.com/cache:k", "localhost/lux-build:new"}
	if got := pickEvictions(cands, inUse, nil, 1<<40, recent); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// Disk pressure leaves the image of a stopped Run whose volumes wait here
// for a resume (its container, which pinned the image, is gone), and takes
// one no local copy needs.
func TestPickEvictionsLeavesALocalCopysImage(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{cfg: Config{DataDir: dir}}
	for run, st := range map[string]*runState{
		"held":       {RunID: "held", Phase: "reported", Image: "alpine:3", VolumesSnapshot: "snap1"},
		"diverged":   {RunID: "diverged", Phase: "started", Image: "ghcr.io/a/other:1"},
		"no-volumes": {RunID: "no-volumes", Phase: "reported", Image: "alpine"},
	} {
		if err := writeRunState(r.runDir(run), st); err != nil {
			t.Fatal(err)
		}
	}
	held := r.localCopyImages()
	if !reflect.DeepEqual(held, map[string]bool{"docker.io/library/alpine:3": true}) {
		t.Fatalf("local copies' images %v, want docker.io/library/alpine:3 only", held)
	}
	cands := []imageCandidate{
		{Ref: "alpine:3", ID: "h", Names: []string{"alpine:3"}, Size: 100, LastUsed: 1},
		{Ref: "ghcr.io/a/free:1", ID: "f", Names: []string{"ghcr.io/a/free:1"}, Size: 100, LastUsed: 2},
	}
	if got := pickEvictions(cands, nil, held, 1<<40, 1000); !reflect.DeepEqual(got, []string{"ghcr.io/a/free:1"}) {
		t.Fatalf("evicted %v, want only ghcr.io/a/free:1", got)
	}
}

func TestImageUseTouchOnlyWhatLuxHas(t *testing.T) {
	dir := t.TempDir()
	u := newImageUse(dir)
	u.touch("docker.io/library/alpine:3", "t1") // the host had it: not lux's
	u.pulledBy("ghcr.io/a/b:1", "t1")
	if got := u.snapshot(); len(got) != 1 || got["ghcr.io/a/b:1"] == 0 {
		t.Fatal(got)
	}
	// Persisted across restarts.
	if got := newImageUse(dir).snapshot(); len(got) != 1 {
		t.Fatal(got)
	}
	u.forget("ghcr.io/a/b:1")
	if got := newImageUse(dir).snapshot(); len(got) != 0 {
		t.Fatal(got)
	}
}

// Images are host-wide, but pulling one is proof of access: one lux pulled
// for a tenant is reused without a pull by that tenant only.
func TestImageUseAccessIsPerTenant(t *testing.T) {
	dir := t.TempDir()
	u := newImageUse(dir)
	if !u.mayUse("docker.io/library/alpine:3", "t2") {
		t.Fatal("an image lux never pulled (the host's own) is anyone's")
	}
	u.pulledBy("ghcr.io/a/private:1", "t1")
	if !u.mayUse("ghcr.io/a/private:1", "t1") || u.mayUse("ghcr.io/a/private:1", "t2") {
		t.Fatal("a pulled image is its pullers' only")
	}
	u.pulledBy("ghcr.io/a/private:1", "t2")
	if !u.mayUse("ghcr.io/a/private:1", "t2") {
		t.Fatal("a second tenant that pulled it may use it")
	}
	if !newImageUse(dir).mayUse("ghcr.io/a/private:1", "t2") || newImageUse(dir).mayUse("ghcr.io/a/private:1", "t3") {
		t.Fatal("not persisted")
	}
}
