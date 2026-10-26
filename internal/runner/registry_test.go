package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func TestAuthFile(t *testing.T) {
	dir := t.TempDir()
	auths := []spec.RegistryAuth{
		{Registry: "ghcr.io", Secret: "GH"},
		{Registry: "10.0.0.5:5000", Secret: "REG"},
		{Registry: "missing.example.com", Secret: "UNSET"},
	}
	a, err := writeAuthFile(dir, auths, map[string]string{"GH": "ghp_tok", "REG": "alice:s3cret:x"}, nil)
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
	none, err := writeAuthFile(dir, auths[:1], map[string]string{}, nil)
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

const mintedHost = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

func readAuths(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range got.Auths {
		dec, _ := base64.StdEncoding.DecodeString(v.Auth)
		out[k] = string(dec)
	}
	return out
}

// requireNoReportHolds fails if any report f sent contains one of secrets.
func requireNoReportHolds(t *testing.T, f *finishFixture, secrets ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reports {
		for _, s := range secrets {
			if strings.Contains(string(r.Data), s) {
				t.Fatalf("token in a report: %s", r.Data)
			}
		}
	}
}

// luxd's minted pull credentials are written under <host>/<owner> keys
// beside the spec's, redacted like them; a spec entry for the registry's
// host wins over every minted entry for it.
func TestAuthFileMinted(t *testing.T) {
	dir := t.TempDir()
	minted := []proto.RegistryCredential{
		{Registry: mintedHost + "/acme", User: "AWS", Password: "ecr-acme-token"},
		{Registry: mintedHost + "/team", User: "AWS", Password: "ecr-team-token"},
	}
	a, err := writeAuthFile(dir, nil, nil, minted)
	if err != nil || a == nil {
		t.Fatalf("minted only: %v %v", a, err)
	}
	want := map[string]string{mintedHost + "/acme": "AWS:ecr-acme-token", mintedHost + "/team": "AWS:ecr-team-token"}
	if got := readAuths(t, a.path()); !reflect.DeepEqual(got, want) {
		t.Fatalf("auths %v, want %v", got, want)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("AWS:ecr-acme-token"))
	e := a.redact(errors.New("pull: denied with ecr-acme-token (" + enc + ") and ecr-team-token"))
	if strings.Contains(e.Error(), "ecr-acme-token") || strings.Contains(e.Error(), "ecr-team-token") || strings.Contains(e.Error(), enc) {
		t.Fatalf("minted token not redacted: %v", e)
	}
	a.close()

	// Beside the spec's entries for other registries.
	b, err := writeAuthFile(dir, []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "GH"}}, map[string]string{"GH": "ghp_tok"}, minted[:1])
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]string{"ghcr.io": "lux:ghp_tok", mintedHost + "/acme": "AWS:ecr-acme-token"}
	if got := readAuths(t, b.path()); !reflect.DeepEqual(got, want) {
		t.Fatalf("auths %v, want %v", got, want)
	}
	b.close()

	// An explicit registryAuth for the host wins: the minted entries, more
	// specific, would otherwise replace it on the owners' repositories.
	c, err := writeAuthFile(dir, []spec.RegistryAuth{{Registry: mintedHost, Secret: "MINE"}}, map[string]string{"MINE": "AWS:explicit-token"}, minted)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]string{mintedHost: "AWS:explicit-token"}
	if got := readAuths(t, c.path()); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit entry did not win: %v, want %v", got, want)
	}
	c.close()

	// An explicit entry whose secret has no value is no entry: minted ones stand.
	d, err := writeAuthFile(dir, []spec.RegistryAuth{{Registry: mintedHost, Secret: "UNSET"}}, nil, minted[:1])
	if err != nil {
		t.Fatal(err)
	}
	if got := readAuths(t, d.path()); !reflect.DeepEqual(got, map[string]string{mintedHost + "/acme": "AWS:ecr-acme-token"}) {
		t.Fatalf("auths %v", got)
	}
	d.close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

// A placement whose assignment carries minted credentials pulls with them
// from its 0600 auth file, which is gone afterwards; a failed pull's error
// and events never hold the token, nor does the container's config or
// command line.
func TestMintedCredentialsStayInTheAuthFile(t *testing.T) {
	f := newFinishFixture(t)
	if err := os.MkdirAll(filepath.Join(f.r.cfg.DataDir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	const token = "ecr-minted-secret-token"
	enc := base64.StdEncoding.EncodeToString([]byte("AWS:" + token))
	// pull copies its auth file aside, then fails echoing the file, as a
	// careless registry error might.
	writeFile(t, f.bin, fakePodman(`"pull -q")
  [ "$3" = --authfile ] || exit 3
  stat -c %a "$4" > "$0.authmode" 2>/dev/null || stat -f %Lp "$4" > "$0.authmode"
  cp "$4" "$0.auth"
  echo "unauthorized: $(cat "$4") `+token+`" >&2
  exit 1 ;;`))
	f.p.assign = &proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1,
		RegistryCredentials: []proto.RegistryCredential{{Registry: mintedHost + "/acme", User: "AWS", Password: token}}}
	sp := spec.RunSpec{Image: spec.Image{Ref: mintedHost + "/acme/app:1"},
		Secrets: []spec.Secret{{Name: "VISIBLE", Value: "v", As: "env"}}}
	_, err := f.p.ensureImage(context.Background(), sp, podman.Network{})
	if err == nil {
		t.Fatal("the pull did not fail")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), enc) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error not redacted: %v", err)
	}
	if got := readAuths(t, f.bin+".auth"); !reflect.DeepEqual(got, map[string]string{mintedHost + "/acme": "AWS:" + token}) {
		t.Fatalf("auth file %v", got)
	}
	if mode, _ := os.ReadFile(f.bin + ".authmode"); strings.TrimSpace(string(mode)) != "600" {
		t.Fatalf("auth file mode %q", mode)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.r.cfg.DataDir, "tmp")); len(entries) != 0 {
		t.Fatalf("auth file left behind: %v", entries)
	}
	waitFor(t, "image.pull event", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, r := range f.reports {
			if strings.Contains(string(r.Data), "image.pull") {
				return true
			}
		}
		return false
	})
	requireNoReportHolds(t, f, token, enc)

	// The container: neither its shim config nor its command line.
	rt := t.TempDir()
	f.r.mounts.Store(runtimeVolume("run1"), rt)
	if err := f.p.writeShimConfig(context.Background(), sp, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(rt, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(f.p.createArgs(sp, "img", podman.Network{}), " ")
	for where, text := range map[string]string{"shim config": string(cfg), "container args": args} {
		if strings.Contains(text, token) || strings.Contains(text, enc) {
			t.Fatalf("token in the %s: %s", where, text)
		}
	}
}

// A build uses the minted credentials for its FROM base and for reading
// its build cache; a push to the cache, which the pull-only token cannot
// do, is an image.cache warning without the token, and each auth file is
// gone afterwards.
func TestMintedCredentialsInABuild(t *testing.T) {
	f := newFinishFixture(t)
	tmp := filepath.Join(f.r.cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	const token = "ecr-minted-build-token"
	enc := base64.StdEncoding.EncodeToString([]byte("AWS:" + token))
	// Each pull or push keeps a copy of its auth file under its ref; the
	// push is refused, echoing the file.
	writeFile(t, f.bin, fakePodman(`"image exists") exit 1 ;;
"image inspect") echo sha256:0123 ;;
"tag "*) ;;
"pull -q") cp "$4" "$0.auth.$(echo "$5" | tr '/:@' '___')" ;;
"push -q")
  cp "$4" "$0.auth.push"
  echo "denied: not authorized to push with $(cat "$4") `+token+`" >&2
  exit 1 ;;`))
	f.p.assign = &proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1,
		RegistryCredentials: []proto.RegistryCredential{{Registry: mintedHost + "/acme", User: "AWS", Password: token}}}
	base, cache := mintedHost+"/acme/base:1", mintedHost+"/acme/cache"
	sp := spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM " + base + "\nRUN true", Cache: cache}}}
	auth, err := f.p.registryLogin(sp)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := f.p.pinFroms(context.Background(), sp.Image.Build.Containerfile, auth)
	if err != nil || !strings.HasPrefix(pinned, "FROM "+base+"@sha256:0123") {
		t.Fatalf("pinned %q: %v", pinned, err)
	}
	ref := cacheRef(cache, "k1")
	if _, ok := f.p.pullCached(context.Background(), ref, "localhost/lux-build:k1", auth); !ok {
		t.Fatal("the cache pull did not hit")
	}
	auth.close()
	want := map[string]string{mintedHost + "/acme": "AWS:" + token}
	for _, pulled := range []string{base, ref} {
		if got := readAuths(t, f.bin+".auth."+strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(pulled)); !reflect.DeepEqual(got, want) {
			t.Fatalf("auth file for %s: %v", pulled, got)
		}
	}

	f.p.pushCached(context.Background(), sp, "localhost/lux-build:k1", ref)
	if got := readAuths(t, f.bin+".auth.push"); !reflect.DeepEqual(got, want) {
		t.Fatalf("push auth file: %v", got)
	}
	var warning string
	waitFor(t, "the push's image.cache warning", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, r := range f.reports {
			var ev proto.RunEvent
			if json.Unmarshal(r.Data, &ev) == nil && ev.Type == "image.cache" && ev.Data["pushed"] == false {
				warning, _ = ev.Data["warning"].(string)
				return true
			}
		}
		return false
	})
	if !strings.Contains(warning, "not authorized to push") || strings.Contains(warning, token) || strings.Contains(warning, enc) {
		t.Fatalf("push warning %q", warning)
	}
	requireNoReportHolds(t, f, token, enc)
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("auth files left behind: %v", entries)
	}
}
