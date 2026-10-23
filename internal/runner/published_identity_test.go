package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
)

// fakeImagePodman is a podman with images, names and a registry in files, for
// what image naming does: image inspect/exists, tag, pull and push.
// A name resolves to an image id; tag moves a name; a push records the
// pushed image under a digest of its id.
const fakeImagePodman = `#!/bin/bash
S=$FAKE_PODMAN_STATE
enc() { printf %s "$1" | tr '/:@' '___'; }
resolve() {
	if [ -f "$S/names/$(enc "$1")" ]; then cat "$S/names/$(enc "$1")"
	elif [ -f "$S/ids/$1" ]; then echo "$1"
	else return 1; fi
}
case "$1" in
image)
	case "$2" in
	inspect) id=$(resolve "$5") || { echo "Error: $5: image not known" >&2; exit 125; }
		if [ "$4" = "{{.Id}}" ]; then echo "$id"; else cat "$S/digest/$id"; fi ;;
	exists) resolve "$3" >/dev/null || exit 1 ;;
	esac ;;
tag) id=$(resolve "$2") || { echo "Error: $2: image not known" >&2; exit 125; }
	echo "$id" > "$S/names/$(enc "$3")" ;;
pull) ref="${@: -1}"
	[ -f "$S/registry/$(enc "$ref")" ] || { echo "manifest unknown" >&2; exit 125; }
	id=$(cat "$S/registry/$(enc "$ref")"); touch "$S/ids/$id"; echo "$id" > "$S/names/$(enc "$ref")" ;;
push) shift; df=""; args=()
	while [ $# -gt 0 ]; do case "$1" in
		-q) ;; --digestfile) df=$2; shift ;; --authfile) shift ;; *) args+=("$1") ;;
	esac; shift; done
	id=$(resolve "${args[0]}") || { echo "Error: ${args[0]}: image not known" >&2; exit 125; }
	echo "$id" >> "$S/pushed"
	digest="sha256:$(printf %s "$id" | sha256sum | cut -c1-64)"
	echo "$digest" > "$df"; echo "$digest" > "$S/digest/$id"
	echo "$id" > "$S/registry/$(enc "${args[1]%:*}@$digest")" ;;
images) ;;
*) echo "fake podman: $*" >&2; exit 2 ;;
esac
`

type fakeImages struct {
	t     *testing.T
	state string
	pm    *podman.Podman
}

func newFakeImages(t *testing.T) *fakeImages {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	for _, d := range []string{"names", "ids", "registry", "digest"} {
		if err := os.MkdirAll(filepath.Join(state, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(fakeImagePodman), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PODMAN_STATE", state)
	return &fakeImages{t: t, state: state, pm: &podman.Podman{Bin: bin}}
}

func enc(s string) string { return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(s) }

// image makes a local image named name; returns its id.
func (f *fakeImages) image(seed, name string) string {
	sum := sha256.Sum256([]byte(seed))
	id := hex.EncodeToString(sum[:])
	os.WriteFile(filepath.Join(f.state, "ids", id), nil, 0o644)
	f.tag(id, name)
	return id
}

func (f *fakeImages) tag(id, name string) {
	os.WriteFile(filepath.Join(f.state, "names", enc(name)), []byte(id+"\n"), 0o644)
}

func (f *fakeImages) resolve(name string) string {
	id, err := f.pm.ImageID(context.Background(), name)
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return id
}

func (f *fakeImages) placement(dir string) *placement {
	r := &Runner{cfg: Config{DataDir: dir}, pm: f.pm, images: newImageUse(dir)}
	m := &proto.ManagedImages{Root: "registry.test/managed", Repository: "registry.test/managed/ten_a"}
	// Stale: its events go nowhere (there is no luxd).
	return &placement{r: r, tenantID: "ten_a", stale: true, assign: &proto.Assign{ManagedImages: m}}
}

// Two Runs of one build key published different images (non-reproducible
// first builds on two hosts). On a third host, each placement readies its
// own published image under a name of that digest's, so another
// placement moving the build key's name in between (readying its own, or
// a cache pull) never changes the image the first creates its container
// from.
func TestPublishedImagesKeepTheirIdentity(t *testing.T) {
	f := newFakeImages(t)
	ctx := context.Background()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "tmp"), 0o700)
	tag := "localhost/lux-build:samekey"
	refA := "registry.test/managed/ten_a@sha256:" + strings.Repeat("a", 64)
	refB := "registry.test/managed/ten_a@sha256:" + strings.Repeat("b", 64)
	idA, idB := f.image("A", refA), f.image("B", refB)
	auth := &imageAuth{m: &proto.ManagedImages{Root: "registry.test/managed", Repository: "registry.test/managed/ten_a"}, managed: &registryAuth{}}

	pa, pb := f.placement(dir), f.placement(dir)
	nameA, gotA, err := pa.usePublished(ctx, proto.ImageResolution{ImageID: idA, PublishedRef: refA}, tag, auth)
	if err != nil || gotA != idA {
		t.Fatalf("A: %s %s %v", nameA, gotA, err)
	}
	unpin := pa.r.images.pin(nameA)
	defer unpin()
	// Meanwhile: B readies its image, and the key's name moves to it.
	f.tag(idB, tag)
	nameB, gotB, err := pb.usePublished(ctx, proto.ImageResolution{ImageID: idB, PublishedRef: refB}, tag, auth)
	if err != nil || gotB != idB {
		t.Fatalf("B: %s %s %v", nameB, gotB, err)
	}
	if nameA == tag || nameB == tag || nameA == nameB {
		t.Fatalf("names %s %s (key %s)", nameA, nameB, tag)
	}
	// What each creates its container from.
	if got := f.resolve(nameA); got != idA {
		t.Fatalf("A's image is now %s, not %s", got, idA)
	}
	if got := f.resolve(nameB); got != idB {
		t.Fatalf("B's image is %s, not %s", got, idB)
	}
	// A again (a resume): its name, still its image, whatever the key says.
	if name, got, err := pa.usePublished(ctx, proto.ImageResolution{ImageID: idA, PublishedRef: refA}, tag, auth); err != nil || name != nameA || got != idA {
		t.Fatalf("A again: %s %s %v", name, got, err)
	}
	// The host's own build of A's image, under the key's name: used by id.
	f.tag(idA, tag)
	os.Remove(filepath.Join(f.state, "names", enc(nameA)))
	if name, got, err := pa.usePublished(ctx, proto.ImageResolution{ImageID: idA, PublishedRef: refA}, tag, auth); err != nil || got != idA || f.resolve(name) != idA {
		t.Fatalf("from the local build: %s %s %v", name, got, err)
	}
}

// A new build is published by its image id: the build key's name moving
// to another image before the push does not publish that image.
func TestPublishPushesTheBuildNotTheKeysName(t *testing.T) {
	f := newFakeImages(t)
	ctx := context.Background()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "tmp"), 0o700)
	tag := "localhost/lux-build:samekey"
	built := f.image("mine", tag)
	other := f.image("other", "localhost/elsewhere:x")
	f.tag(other, tag) // another placement's build of the same key
	p := f.placement(dir)
	auth := &imageAuth{m: p.assign.ManagedImages, managed: &registryAuth{}}
	ref, err := p.publish(ctx, built, "samekey", auth)
	if err != nil {
		t.Fatal(err)
	}
	pushed, _ := os.ReadFile(filepath.Join(f.state, "pushed"))
	if strings.TrimSpace(string(pushed)) != built {
		t.Fatalf("pushed %q, not the build %s", pushed, built)
	}
	name, err := p.namePublished(ctx, built, ref)
	if err != nil || f.resolve(name) != built || name != publishedName("ten_a", ref) {
		t.Fatalf("named %s: %v", name, err)
	}
}

// A published image's local name is its tenant's: recorded as such when
// it is made or reused (and across a runner restart), never usable by
// another tenant, and never accepted as a spec's image or base, however
// spelled, whether or not the assignment has a managed registry. Images
// the host had before lux are still anyone's.
func TestPublishedNamesAreTheTenantsOwn(t *testing.T) {
	f := newFakeImages(t)
	ctx := context.Background()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "tmp"), 0o700)
	ref := "registry.test/managed/ten_a@sha256:" + strings.Repeat("a", 64)
	id := f.image("private", "localhost/source:x")
	pa := f.placement(dir)
	name, err := pa.namePublished(ctx, id, ref)
	if err != nil {
		t.Fatal(err)
	}
	r := pa.r
	if !r.images.mayUse(name, "ten_a") || r.images.mayUse(name, "ten_b") {
		t.Fatal("the published name is not ten_a's alone")
	}
	// Reused by a resume (and so recorded even if an older runner made it).
	r.images.forget(name)
	if n, got, err := pa.usePublished(ctx, proto.ImageResolution{ImageID: id, PublishedRef: ref}, "localhost/lux-build:k", &imageAuth{}); err != nil || n != name || got != id {
		t.Fatalf("reuse: %s %s %v", n, got, err)
	}
	if r.images.mayUse(name, "ten_b") {
		t.Fatal("reuse left the name anyone's")
	}
	// A runner restart reads the owners back.
	if newImageUse(dir).mayUse(name, "ten_b") {
		t.Fatal("ownership lost on restart")
	}
	// Tenant B, with no managed registry in its assignment (as for a plain
	// image.ref), and with one.
	pb := &placement{r: r, tenantID: "ten_b", stale: true, assign: &proto.Assign{}}
	short := strings.TrimPrefix(name, "localhost/")
	for _, auth := range []*imageAuth{{}, nil, {m: &proto.ManagedImages{Root: "registry.test/managed", Repository: "registry.test/managed/ten_b"}, managed: &registryAuth{}}} {
		for _, n := range []string{name, short, "docker.io/library/" + short, "lux-published@sha256:" + strings.Repeat("c", 64)} {
			if err := pb.pullIfMissing(ctx, n, auth); err == nil || !strings.Contains(err.Error(), "lux's own name") {
				t.Fatalf("%s: %v", n, err)
			}
		}
		pinned := "FROM " + name
		if _, err := pb.pinFroms(ctx, pinned, nil, auth); err == nil || !strings.Contains(err.Error(), "lux's own name") {
			t.Fatalf("FROM: %v", err)
		}
		if _, err := pb.localBases(ctx, "FROM "+short+"@sha256:"+strings.Repeat("d", 64), auth); err == nil || !strings.Contains(err.Error(), "lux's own name") {
			t.Fatalf("pinned FROM: %v", err)
		}
	}
	// Not even its own tenant names it in a spec: the publishedRef is the name.
	if err := pa.pullIfMissing(ctx, name, &imageAuth{}); err == nil {
		t.Fatal("ten_a named its published name")
	}
	// An image the host had before lux needed it: still anyone's.
	f.image("operator", "localhost/operator-base:1")
	if err := pb.pullIfMissing(ctx, "localhost/operator-base:1", &imageAuth{}); err != nil {
		t.Fatalf("operator's image: %v", err)
	}
}
