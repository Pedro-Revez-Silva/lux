package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// The default context is written as given, 0644 files in 0755
// directories whatever the umask, and nothing lands outside it.
func TestWriteContextFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeContextFiles(dir, map[string]string{"setup.sh": "echo hi\n", "conf/deep/app.ini": "x=1\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "conf", "deep", "app.ini")); string(b) != "x=1\n" {
		t.Fatalf("app.ini %q", b)
	}
	for name, want := range map[string]os.FileMode{".": 0o755, "conf": 0o755, "conf/deep": 0o755, "setup.sh": 0o644, "conf/deep/app.ini": 0o644} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", name, fi.Mode(), err, want)
		}
	}
	outside := t.TempDir()
	for _, name := range []string{"../escape", "/abs", "a/../../escape", "a//b"} {
		d := t.TempDir()
		if err := writeContextFiles(d, map[string]string{name: "x"}); err == nil {
			t.Errorf("%q written", name)
		}
	}
	// Through a symlink already there (not one luxd could send, but the
	// write is confined regardless).
	d := t.TempDir()
	os.Symlink(outside, filepath.Join(d, "link"))
	if err := writeContextFiles(d, map[string]string{"link/escape": "x"}); err == nil {
		t.Error("wrote through a symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("outside: %v", entries)
	}
}

// The build key covers every input: a different context file, named
// context commit or URL, or platform is a different build; the same
// inputs, in any map order, the same.
func TestBuildKeyCoversInputs(t *testing.T) {
	net := spec.Network{Egress: []spec.EgressRule{{Host: "github.com"}}}
	ctx := func(url, ref string) map[string]spec.BuildContext {
		return map[string]spec.BuildContext{"repo": {Git: &spec.GitContext{URL: url, Ref: ref}}}
	}
	base := buildInputs{Files: map[string]string{"a": "1", "b/c": "2"}, Contexts: ctx("https://github.com/a/b", strings.Repeat("a", 40)), Platform: "linux/arm64"}
	key := buildKey("t1", net, "FROM x", map[string]string{"K": "V"}, base)
	same := buildInputs{Files: map[string]string{"b/c": "2", "a": "1"}, Contexts: ctx("https://github.com/a/b", strings.Repeat("a", 40)), Platform: "linux/arm64"}
	if buildKey("t1", net, "FROM x", map[string]string{"K": "V"}, same) != key {
		t.Fatal("same inputs, different key")
	}
	for name, in := range map[string]buildInputs{
		"file content":   {Files: map[string]string{"a": "1", "b/c": "3"}, Contexts: base.Contexts, Platform: base.Platform},
		"file name":      {Files: map[string]string{"a": "1", "b/d": "2"}, Contexts: base.Contexts, Platform: base.Platform},
		"file moved":     {Files: map[string]string{"a": "1b/c", "": "2"}, Contexts: base.Contexts, Platform: base.Platform},
		"no files":       {Contexts: base.Contexts, Platform: base.Platform},
		"commit":         {Files: base.Files, Contexts: ctx("https://github.com/a/b", strings.Repeat("b", 40)), Platform: base.Platform},
		"context url":    {Files: base.Files, Contexts: ctx("https://github.com/a/c", strings.Repeat("a", 40)), Platform: base.Platform},
		"no context":     {Files: base.Files, Platform: base.Platform},
		"other platform": {Files: base.Files, Contexts: base.Contexts, Platform: "linux/amd64"},
	} {
		if buildKey("t1", net, "FROM x", map[string]string{"K": "V"}, in) == key {
			t.Errorf("%s: same key", name)
		}
	}
	if buildKey("t2", net, "FROM x", map[string]string{"K": "V"}, base) == key {
		t.Error("another tenant, same key")
	}
}

func managedAuth(t *testing.T) *imageAuth {
	t.Helper()
	dir := t.TempDir()
	m := &proto.ManagedImages{Root: "ghcr.io/acme/lux", Repository: "ghcr.io/acme/lux/ten_a",
		Shared: []string{"ghcr.io/acme/base"}, Credential: "bot:s3cret"}
	r := &Runner{cfg: Config{DataDir: dir}}
	os.Mkdir(filepath.Join(dir, "tmp"), 0o700)
	p := &placement{r: r, tenantID: "ten_a", assign: &proto.Assign{ManagedImages: m}}
	managed, err := p.managedLogin()
	if err != nil {
		t.Fatal(err)
	}
	user, err := writeAuthFile(filepath.Join(dir, "tmp"), []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "GH"}}, map[string]string{"GH": "mine"})
	if err != nil {
		t.Fatal(err)
	}
	return &imageAuth{user: user, managed: managed, m: m, tenant: "ten_a"}
}

// The managed credential is in a file of its own, keyed by the tenant's
// repository and the shared ones only, so Podman offers it nowhere else;
// it is used for those, refused for another tenant's repository, and the
// spec's own credentials serve everything else.
func TestManagedAuthIsScoped(t *testing.T) {
	a := managedAuth(t)
	defer a.close()
	b, _ := os.ReadFile(a.managed.path())
	var got struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Auths) != 2 {
		t.Fatalf("keys %v", got.Auths)
	}
	for _, k := range []string{"ghcr.io/acme/lux/ten_a", "ghcr.io/acme/base"} {
		v, _ := base64.StdEncoding.DecodeString(got.Auths[k].Auth)
		if string(v) != "bot:s3cret" {
			t.Fatalf("%s: %q", k, v)
		}
	}
	for ref, want := range map[string]*registryAuth{
		"ghcr.io/acme/lux/ten_a@sha256:" + strings.Repeat("a", 64): a.managed,
		"ghcr.io/acme/base:1":   a.managed,
		"ghcr.io/acme/other:1":  a.user,
		"alpine":                a.user,
		"ghcr.io/acme/luxe/x:1": a.user,
		"ghcr.io/acme/base/x:1": a.user,
	} {
		if got, err := a.forPull(ref); err != nil || got != want {
			t.Errorf("%s: %v %v", ref, got, err)
		}
	}
	for _, ref := range []string{"ghcr.io/acme/lux/ten_b@sha256:" + strings.Repeat("a", 64), "ghcr.io/acme/lux/ten_a/x:1", "ghcr.io/acme/lux:1"} {
		if _, err := a.forPull(ref); err == nil || !strings.Contains(err.Error(), "another tenant's") {
			t.Errorf("%s: %v", ref, err)
		}
	}
	e := a.redact(errors.New("denied bot:s3cret " + got.Auths["ghcr.io/acme/base"].Auth + " mine"))
	if strings.Contains(e.Error(), "s3cret") || strings.Contains(e.Error(), "mine") || strings.Contains(e.Error(), got.Auths["ghcr.io/acme/base"].Auth) {
		t.Fatalf("not redacted: %v", e)
	}
	// Without a credential from luxd, managed images fail, saying so.
	a.managed.close()
	a.managed = nil
	if _, err := a.forPull("ghcr.io/acme/base:1"); !errors.Is(err, errNoManagedCredential) {
		t.Fatalf("no credential: %v", err)
	}
	if got, err := a.forPull("alpine"); err != nil || got != a.user {
		t.Fatalf("alpine without a managed credential: %v %v", got, err)
	}
}

// COPY --from may name a declared context as well as an earlier stage;
// an image, or an undeclared name, is still refused before any pull.
func TestPinFromsKnowsContexts(t *testing.T) {
	p := &placement{r: &Runner{}, tenantID: "t"}
	cf := "FROM scratch\nCOPY --from=repo go.mod /seed/\nRUN --mount=type=bind,from=repo,target=/r true\n"
	if _, err := p.pinFroms(t.Context(), cf, map[string]bool{"repo": true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pinFroms(t.Context(), cf, nil, nil); err == nil || !strings.Contains(err.Error(), "only earlier stages") {
		t.Fatalf("undeclared: %v", err)
	}
	if _, err := p.pinFroms(t.Context(), "FROM scratch AS repo\n", map[string]bool{"repo": true}, nil); err == nil {
		t.Fatal("a stage named as a context")
	}
}

// A credential luxd says has expired is refused before a managed pull or
// push, naming the expiry; one without an expiry (static) never is.
func TestManagedCredentialExpiry(t *testing.T) {
	a := managedAuth(t)
	defer a.close()
	ref := "ghcr.io/acme/lux/ten_a@sha256:" + strings.Repeat("a", 64)
	if _, err := a.forPull(ref); err != nil {
		t.Fatal(err)
	}
	a.m.CredentialExpiresAt = time.Now().Add(3 * time.Hour).UnixMilli()
	if _, err := a.forPull(ref); err != nil {
		t.Fatal(err)
	}
	a.m.CredentialExpiresAt = time.Now().Add(30 * time.Second).UnixMilli()
	if _, err := a.forPull(ref); err == nil || !strings.Contains(err.Error(), "ecr_min_validity") {
		t.Fatalf("expired: %v", err)
	}
	// Other registries' pulls are unaffected.
	if got, err := a.forPull("alpine"); err != nil || got != a.user {
		t.Fatalf("alpine: %v", err)
	}
}
