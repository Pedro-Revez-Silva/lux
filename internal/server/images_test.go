package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// A Run whose image build has a context or publishes is placed only on a
// runner that has managed images: an older one would build without the
// context, or not publish, and say nothing. The wait reason and spot
// admission name it.
func TestHostFitManagedImages(t *testing.T) {
	builds := map[string]spec.Build{
		"publish":      {Containerfile: "FROM x", Publish: true},
		"contextFiles": {Containerfile: "FROM x", ContextFiles: map[string]string{"a": "b"}},
		"contexts":     {Containerfile: "FROM x", Contexts: map[string]spec.BuildContext{"repo": {Git: &spec.GitContext{}}}},
	}
	for name, b := range builds {
		r, h := fitFixture()
		r.Spec.Image.Build = &b
		want := []fitBlocker{{Kind: kindManagedImages, Reason: "host's lux-runner predates managed images (build contexts, publish, managed registry credential)"}}
		if got := hostFit(r, h); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s on an older runner: %+v", name, got)
		}
		w := &waitCapacity{}
		w.add(hostFit(r, h))
		if got := w.reason(r); !strings.Contains(got, "1 host in its pool runs a lux-runner without managed images") {
			t.Fatalf("%s: wait reason %q", name, got)
		}
		set := blockerSet{}
		if set.classify(hostFit(r, h)) || set.list()[0].Code != blockCapability {
			t.Fatalf("%s: spot classification %+v", name, set.list())
		}
		h.Caps = []string{proto.CapManagedImages}
		if got := hostFit(r, h); got != nil {
			t.Fatalf("%s on a runner with managed images: %+v", name, got)
		}
	}
	// A plain build or ref needs nothing new.
	r, h := fitFixture()
	r.Spec.Image.Build = &spec.Build{Containerfile: "FROM x", Cache: "registry.example.com/c"}
	if got := hostFit(r, h); got != nil {
		t.Fatalf("plain build: %+v", got)
	}
}

// A Run that needs the managed registry's credential, as luxd's own
// configuration decides (its published image as image.ref, a shared base
// as FROM), waits for a runner that takes it; one with only its own
// registryAuth, or any Run on a luxd without [images], does not.
func TestManagedAuthRunsNeedACapableRunner(t *testing.T) {
	s := imagesServer(t, ImagesConfig{Repository: "ghcr.io/acme/lux", CredentialEnv: "X", Shared: []string{"ghcr.io/acme/base"}})
	off := imagesServer(t, ImagesConfig{})
	digest := "@sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		sp   spec.RunSpec
		res  *proto.ImageResolution
		want bool
	}{
		"own published ref": {sp: spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/lux/t1" + digest}}, want: true},
		"shared ref":        {sp: spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/base:1"}}, want: true},
		"shared FROM":       {sp: spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM ghcr.io/acme/base:1"}}}, want: true},
		"pinned shared FROM": {sp: spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine"}}},
			res: &proto.ImageResolution{Containerfile: "FROM ghcr.io/acme/base" + digest}, want: true},
		"external registry with its own auth": {sp: spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/other" + digest,
			RegistryAuth: []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "GH"}}}}},
		"public ref": {sp: spec.RunSpec{Image: spec.Image{Ref: "alpine"}}},
	} {
		r, h := fitFixture()
		r.Spec.Image, r.ImageResolved = tc.sp.Image, tc.res
		s.markManagedAuth(&r)
		if r.ManagedAuth != tc.want {
			t.Fatalf("%s: ManagedAuth %v", name, r.ManagedAuth)
		}
		got := hostFit(r, h)
		if tc.want != (len(got) == 1 && got[0].Kind == kindManagedImages) || !tc.want && got != nil {
			t.Fatalf("%s on an older runner: %+v", name, got)
		}
		h.Caps = []string{proto.CapManagedImages}
		if got := hostFit(r, h); got != nil {
			t.Fatalf("%s on a capable runner: %+v", name, got)
		}
		off.markManagedAuth(&r)
		if r.ManagedAuth {
			t.Fatalf("%s: managed without [images]", name)
		}
	}
}

func imagesServer(t *testing.T, cfg ImagesConfig) *Server {
	t.Helper()
	return &Server{cfg: Config{Images: cfg}, secrets: newSecretCache(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The managed registry's credential is attached to an assignment as it
// is sent, only for a Run that needs it, read from the file each time
// (a rotated file takes effect), scoped to the Run's tenant's repository
// and the shared ones; the stored message never has it.
func TestAttachManagedImages(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(file, []byte("bot:first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := imagesServer(t, ImagesConfig{Repository: "ghcr.io/acme/lux", CredentialFile: file, Shared: []string{"ghcr.io/acme/base"}})
	assign := func(sp spec.RunSpec, res *proto.ImageResolution) proto.Assign {
		t.Helper()
		stored := proto.Marshal(proto.Assign{RunID: "r1", TenantID: "ten_a", Spec: sp, ImageResolved: res})
		if strings.Contains(string(stored), "first") {
			t.Fatal("the stored assignment has the credential")
		}
		var a proto.Assign
		if err := json.Unmarshal(s.attachSecrets("r1", stored), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	publish := spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine", Publish: true}}}
	a := assign(publish, nil)
	want := &proto.ManagedImages{Root: "ghcr.io/acme/lux", Repository: "ghcr.io/acme/lux/ten_a", Shared: []string{"ghcr.io/acme/base"}, Credential: "bot:first"}
	if !reflect.DeepEqual(a.ManagedImages, want) {
		t.Fatalf("publish: %+v", a.ManagedImages)
	}
	if err := os.WriteFile(file, []byte("bot:rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := assign(publish, nil); a.ManagedImages.Credential != "bot:rotated" {
		t.Fatalf("rotated: %+v", a.ManagedImages)
	}
	for name, sp := range map[string]spec.RunSpec{
		"own published ref": {Image: spec.Image{Ref: "ghcr.io/acme/lux/ten_a@sha256:" + strings.Repeat("a", 64)}},
		"shared base FROM":  {Image: spec.Image{Build: &spec.Build{Containerfile: "FROM ghcr.io/acme/base@sha256:" + strings.Repeat("b", 64)}}},
	} {
		if a := assign(sp, nil); a.ManagedImages == nil {
			t.Fatalf("%s: no managed registry", name)
		}
	}
	// The pinned Containerfile counts too: a FROM pinned to a managed image.
	plain := spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine"}}}
	if a := assign(plain, &proto.ImageResolution{Containerfile: "FROM ghcr.io/acme/lux/ten_a@sha256:" + strings.Repeat("c", 64)}); a.ManagedImages == nil {
		t.Fatal("pinned managed FROM: no managed registry")
	}
	for name, sp := range map[string]spec.RunSpec{
		"public ref":   {Image: spec.Image{Ref: "alpine"}},
		"plain build":  plain,
		"other ghcr":   {Image: spec.Image{Ref: "ghcr.io/acme/other@sha256:" + strings.Repeat("a", 64)}},
		"root sibling": {Image: spec.Image{Ref: "ghcr.io/acme/luxury/x:1"}},
	} {
		if a := assign(sp, nil); a.ManagedImages != nil {
			t.Fatalf("%s: got the managed registry %+v", name, a.ManagedImages)
		}
	}
	// No [images]: nothing attached, ever.
	off := imagesServer(t, ImagesConfig{})
	var a2 proto.Assign
	_ = json.Unmarshal(off.attachSecrets("r1", proto.Marshal(proto.Assign{RunID: "r1", TenantID: "ten_a", Spec: publish})), &a2)
	if a2.ManagedImages != nil {
		t.Fatalf("unconfigured: %+v", a2.ManagedImages)
	}
	// From the environment, when so configured.
	t.Setenv("LUX_TEST_IMAGES_CRED", "envtoken")
	env := imagesServer(t, ImagesConfig{Repository: "ghcr.io/acme/lux", CredentialEnv: "LUX_TEST_IMAGES_CRED"})
	var a3 proto.Assign
	_ = json.Unmarshal(env.attachSecrets("r1", proto.Marshal(proto.Assign{RunID: "r1", TenantID: "ten_a", Spec: publish})), &a3)
	if a3.ManagedImages == nil || a3.ManagedImages.Credential != "envtoken" {
		t.Fatalf("env credential: %+v", a3.ManagedImages)
	}
}

// Submission refuses publishing to a luxd with no managed registry, and a
// spec naming another tenant's managed repository (its image, a FROM) or
// using the managed registry as a build cache.
func TestCheckManagedImages(t *testing.T) {
	off := imagesServer(t, ImagesConfig{})
	pub := spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine", Publish: true}}}
	if err := off.checkManagedImages("ten_a", pub); err == nil || !strings.Contains(err.Error(), "image.build.publish: this luxd has no managed image registry") {
		t.Fatalf("unconfigured publish: %v", err)
	}
	on := imagesServer(t, ImagesConfig{Repository: "ghcr.io/acme/lux", CredentialEnv: "X", Shared: []string{"ghcr.io/acme/base"}})
	if err := on.checkManagedImages("ten_a", pub); err != nil {
		t.Fatal(err)
	}
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		sp   spec.RunSpec
		want string
	}{
		{"own ref", spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/lux/ten_a" + digest}}, ""},
		{"shared ref", spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/base:1"}}, ""},
		{"other tenant's ref", spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/lux/ten_b" + digest}}, "image.ref: ghcr.io/acme/lux/ten_b is another tenant's"},
		{"under own repo", spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/lux/ten_a/x" + digest}}, "another tenant's"},
		{"root itself", spec.RunSpec{Image: spec.Image{Ref: "ghcr.io/acme/lux:latest"}}, "another tenant's"},
		{"other tenant's FROM", spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM ghcr.io/acme/lux/ten_b" + digest + " AS b\nFROM b"}}}, "FROM ghcr.io/acme/lux/ten_b is another tenant's"},
		{"managed cache", spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine", Cache: "ghcr.io/acme/lux/ten_a"}}}, "image.build.cache: the managed image registry is not a build cache"},
	} {
		err := on.checkManagedImages("ten_a", tc.sp)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The Run's image resolution is written once, in the report's fenced
// transaction: the first build wins, with its published image. A later
// report changes nothing, except to add a published image to a
// resolution of the same pinned Containerfile that has none.
func TestRecordImageResolvedKeepsThePublishedImage(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	cf := "FROM alpine@sha256:" + strings.Repeat("a", 64)
	ref := "ghcr.io/acme/lux/t1@sha256:" + strings.Repeat("d", 64)
	get := func() (res proto.ImageResolution) {
		t.Helper()
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT image_resolved FROM runs WHERE id = 'r1'`).Scan(&res)
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	record := func(data map[string]any) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error { return recordImageResolved(ctx, tx, "r1", data) }); err != nil {
			t.Fatal(err)
		}
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO runs (id, tenant_id, state, spec) VALUES ('r1', 't1', 'running', '{}')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	record(map[string]any{"tag": "t", "imageId": "i1", "containerfile": cf, "cached": false})
	record(map[string]any{"imageId": "i2", "containerfile": cf + "\n", "publishedRef": ref, "platform": "linux/arm64"})
	if got := get(); got != (proto.ImageResolution{Containerfile: cf, ImageID: "i1"}) {
		t.Fatalf("another Containerfile's publication was taken: %+v", got)
	}
	record(map[string]any{"imageId": "i2", "containerfile": cf, "publishedRef": ref, "platform": "linux/arm64"})
	if got := get(); got != (proto.ImageResolution{Containerfile: cf, ImageID: "i1", PublishedRef: ref, Platform: "linux/arm64"}) {
		t.Fatalf("published: %+v", got)
	}
	record(map[string]any{"imageId": "i3", "containerfile": cf, "publishedRef": "ghcr.io/acme/lux/t1@sha256:" + strings.Repeat("e", 64), "platform": "linux/amd64"})
	if got := get(); got.PublishedRef != ref || got.Platform != "linux/arm64" || got.ImageID != "i1" {
		t.Fatalf("the first publication was replaced: %+v", got)
	}
}

type fakeSource struct {
	value   string
	expires time.Time
	err     error
	calls   int
}

func (f *fakeSource) Credential(context.Context) (string, time.Time, error) {
	f.calls++
	return f.value, f.expires, f.err
}

// With a credential source ([images] auth = "ecr"), each assignment that
// needs the managed registry gets the source's credential and its expiry
// as it is sent; one that does not never asks the source; a source that
// fails gives the runner no credential (it fails, saying so), and luxd's
// log has nothing of a credential.
func TestAttachManagedImagesFromASource(t *testing.T) {
	expires := time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC)
	src := &fakeSource{value: "AWS:ecr-password", expires: expires}
	var logs strings.Builder
	s := &Server{cfg: Config{Images: ImagesConfig{Repository: "123456789012.dkr.ecr.eu-north-1.amazonaws.com/lux", Source: src}},
		secrets: newSecretCache(), log: slog.New(slog.NewTextHandler(&logs, nil))}
	if !s.cfg.Images.configured() {
		t.Fatal("a source is a configured managed registry")
	}
	send := func(sp spec.RunSpec) proto.Assign {
		var a proto.Assign
		stored := proto.Marshal(proto.Assign{RunID: "r1", TenantID: "ten_a", Spec: sp})
		if strings.Contains(string(stored), "ecr-password") {
			t.Fatal("stored with the credential")
		}
		if err := json.Unmarshal(s.attachSecrets("r1", stored), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	publish := spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM alpine", Publish: true}}}
	a := send(publish)
	if m := a.ManagedImages; m == nil || m.Credential != "AWS:ecr-password" || m.CredentialExpiresAt != expires.UnixMilli() ||
		m.Repository != "123456789012.dkr.ecr.eu-north-1.amazonaws.com/lux/ten_a" {
		t.Fatalf("%+v", a.ManagedImages)
	}
	if send(spec.RunSpec{Image: spec.Image{Ref: "alpine"}}).ManagedImages != nil || src.calls != 1 {
		t.Fatalf("a Run without managed images asked the source (%d calls)", src.calls)
	}
	src.err, src.value = errors.New("ecr: GetAuthorizationToken failed: AccessDeniedException"), "AWS:should-not-leak"
	a = send(publish)
	if a.ManagedImages == nil || a.ManagedImages.Credential != "" || a.ManagedImages.CredentialExpiresAt != 0 {
		t.Fatalf("failed source: %+v", a.ManagedImages)
	}
	if strings.Contains(logs.String(), "ecr-password") || strings.Contains(logs.String(), "should-not-leak") || !strings.Contains(logs.String(), "AccessDeniedException") {
		t.Fatalf("log: %s", logs.String())
	}
}

// Through the API: a tenant can name neither another tenant's managed
// repository nor lux's local name for a published image (full or short),
// as its image or a base.
func TestPublishedNamesRefusedAtSubmit(t *testing.T) {
	s, ctx, _, key2 := wakeFixture(t)
	s.cfg.Defaults = spec.BuiltinDefaults
	s.cfg.Images = ImagesConfig{Repository: "registry.test/managed", CredentialEnv: "X"}
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider, is_default) VALUES ('pp', 'plat', 'static', true)`)
	digest := strings.Repeat("a", 64)
	for _, sp := range []spec.RunSpec{
		{Image: spec.Image{Ref: "registry.test/managed/t1@sha256:" + digest}},
		{Image: spec.Image{Ref: "localhost/lux-published:t1-" + digest}},
		{Image: spec.Image{Ref: "lux-published:t1-" + digest}},
		{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM lux-published:t1-" + digest + "\n"}}},
	} {
		sp.Workload.Command = []string{"true"}
		if w := apiCall(t, s, key2, http.MethodPost, "/v1/runs", sp); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%+v: HTTP %d %s", sp.Image, w.Code, w.Body)
		}
	}
	// With [images] off too: the name is lux's regardless.
	s.cfg.Images = ImagesConfig{}
	if w := apiCall(t, s, key2, http.MethodPost, "/v1/runs", spec.RunSpec{Image: spec.Image{Ref: "lux-published:t1-" + digest},
		Workload: spec.Workload{Command: []string{"true"}}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("[images] off: HTTP %d", w.Code)
	}
}
