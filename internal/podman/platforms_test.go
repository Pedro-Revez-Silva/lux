package podman

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// bigData writes containers/storage's big data item key for image id, as
// the file it keeps it in: "=" + base64(key).
func bigData(t *testing.T, imagesDir, id, key, body string) {
	t.Helper()
	dir := filepath.Join(imagesDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "="+base64.StdEncoding.EncodeToString([]byte(key))), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	imgID    = "ab3fe4defd29ba6231229a4d41440ac8bde8218e85870e53876277faa24b35c4"
	listDig  = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	armDig   = "sha256:45e09956dc667c5eff3583c9d94830261fb1ca0be10a0a7db36266edf5de9e1d"
	ociIndex = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"digest":"sha256:1","platform":{"architecture":"amd64","os":"linux"}},` +
		`{"digest":"sha256:2","platform":{"architecture":"unknown","os":"unknown"}},` +
		`{"digest":"sha256:3","platform":{"architecture":"arm","os":"linux","variant":"v7"}},` +
		`{"digest":"` + armDig + `","platform":{"architecture":"arm64","os":"linux","variant":"v8"}}]}`
	armManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + imgID + `"},"layers":[]}`
	armConfig   = `{"architecture":"arm64","os":"linux","variant":"v8","config":{}}`
)

// A pulled multi-arch image: the index Podman stored beside it lists every
// platform the tag offers, attestation entries (unknown/unknown) left out.
// The same image pulled by its platform manifest's digest offers only that.
func TestImagePlatformsFromStoredIndex(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, ociIndex)
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "manifest", armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	info := ImageInfo{ID: imgID, Names: []string{"docker.io/library/alpine:3.20"}, OS: "linux", Architecture: "arm64"}

	for ref, want := range map[string]string{
		"docker.io/library/alpine:3.20":           "[linux/amd64 linux/arm/v7 linux/arm64/v8]",
		"docker.io/library/alpine@" + listDig:     "[linux/amd64 linux/arm/v7 linux/arm64/v8]",
		"docker.io/library/alpine@" + armDig:      "[linux/arm64/v8]",
		"docker.io/library/alpine:3.20@" + armDig: "[linux/arm64/v8]",
	} {
		got, err := ImagePlatforms(dir, ref, info)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v %v, want %s", ref, got, err, want)
		}
	}
}

// A single-arch image (no list stored) offers its own platform, variant
// included from its config; without a config, the inspect's platform. An
// image Podman has no directory for is an error, never a guess.
func TestImagePlatformsSingleArch(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	if got, err := ImagePlatforms(dir, "quay.io/x/y:1", ImageInfo{ID: "sha256:" + imgID}); err != nil || fmt.Sprint(got) != "[linux/arm64/v8]" {
		t.Fatalf("single-arch: %v %v", got, err)
	}

	other := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	bigData(t, dir, other, "manifest", armManifest)
	if got, err := ImagePlatforms(dir, "quay.io/x/z:1", ImageInfo{ID: other, OS: "linux", Architecture: "amd64"}); err != nil || fmt.Sprint(got) != "[linux/amd64]" {
		t.Fatalf("from inspect: %v %v", got, err)
	}

	if got, err := ImagePlatforms(dir, "quay.io/x/gone:1", ImageInfo{ID: "feed", OS: "linux", Architecture: "amd64"}); err == nil {
		t.Fatalf("no image directory: %v, want an error", got)
	}
}

// One image reached through two indexes: a multi-arch one, then an
// arm64-only one listing the same arm64 image. Storage keeps both lists and
// both names on the image (as Podman 5.8 does) without recording which tag
// came by which list, so either tag's platforms are unknown; a ref pinned
// to one list's digest still offers exactly that list's.
func TestImagePlatformsTwoIndexesOneImage(t *testing.T) {
	const armOnlyDig = "sha256:4ca140bcbc88fa038acf33daab9e205a075cafea017d8ad079ba3bd124692e47"
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, ociIndex)
	bigData(t, dir, imgID, "manifest-"+armOnlyDig, `{"schemaVersion":2,"manifests":[{"digest":"`+armDig+`","platform":{"architecture":"arm64","os":"linux","variant":"v8"}}]}`)
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "manifest", armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	info := ImageInfo{ID: imgID, Names: []string{"reg/x/armonly:1", "reg/x/multi:1"}, OS: "linux", Architecture: "arm64"}

	for _, ref := range []string{"reg/x/armonly:1", "reg/x/multi:1"} {
		if got, err := ImagePlatforms(dir, ref, info); got != nil || err == nil {
			t.Errorf("%s: %v %v, want unknown", ref, got, err)
		}
	}
	for ref, want := range map[string]string{
		"reg/x/armonly@" + armOnlyDig: "[linux/arm64/v8]",
		"reg/x/multi@" + listDig:      "[linux/amd64 linux/arm/v7 linux/arm64/v8]",
	} {
		if got, err := ImagePlatforms(dir, ref, info); err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v %v, want %s", ref, got, err, want)
		}
	}

	// One name, but its tag moved to a second list holding the same image:
	// which list it now names is not recorded either.
	info.Names = []string{"reg/x/multi:1"}
	if got, err := ImagePlatforms(dir, "reg/x/multi:1", info); got != nil || err == nil {
		t.Errorf("one name, two lists: %v %v, want unknown", got, err)
	}
}

// One list but two names: the image is also reached by a single-arch tag,
// which stores no list. Which name came by the list is not recorded, so
// neither tag can claim its platforms.
func TestImagePlatformsOneListTwoNames(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, ociIndex)
	bigData(t, dir, imgID, "manifest-"+armDig, armManifest)
	bigData(t, dir, imgID, "sha256:"+imgID, armConfig)
	info := ImageInfo{ID: imgID, Names: []string{"reg/x/multi:1", "reg/x/single:1"}, OS: "linux", Architecture: "arm64"}
	if got, err := ImagePlatforms(dir, "reg/x/single:1", info); got != nil || err == nil {
		t.Fatalf("%v %v, want unknown", got, err)
	}
}

// A list naming a platform more than once (Windows indexes list one per
// os.version) offers it once; the platforms come sorted.
func TestImagePlatformsDedupedSorted(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, `{"manifests":[`+
		`{"platform":{"architecture":"amd64","os":"windows","os.version":"10.0.20348.1"}},`+
		`{"platform":{"architecture":"amd64","os":"linux"}},`+
		`{"platform":{"architecture":"amd64","os":"windows","os.version":"10.0.17763.1"}},`+
		`{"platform":{"architecture":"386","os":"linux"}}]}`)
	if got, err := ImagePlatforms(dir, "x/y:1", ImageInfo{ID: imgID}); err != nil || fmt.Sprint(got) != "[linux/386 linux/amd64 windows/amd64]" {
		t.Fatalf("%v %v", got, err)
	}
}

// An image's metadata directory is that of the store its layers are in:
// the graph root's by default, the image store's when storage.conf sets
// `imagestore` (which `podman info` does not show). The UpperDirs are what
// Podman 5.8 inspects in each case.
func TestImageInspectImagesDir(t *testing.T) {
	const layer = "1b349a3334531b575b01d409540dd8aca14ef29abddacd50f36643d7c949db4b"
	for upper, want := range map[string]string{
		"/var/lib/containers/storage/overlay/" + layer + "/diff": "/var/lib/containers/storage/overlay-images",
		"/srv/images/overlay/" + layer + "/diff":                 "/srv/images/overlay-images",
		"/srv/images/vfs/" + layer + "/diff":                     "", // not the driver it names
		"/srv/images/overlay/" + layer + "/merged":               "", // not a layer's diff
		"": "",
	} {
		bin := filepath.Join(t.TempDir(), "podman")
		inspect := `[{"Id":"` + imgID + `","Os":"linux","Architecture":"arm64","GraphDriver":{"Name":"overlay","Data":{"UpperDir":"` + upper + `"}}}]`
		if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat <<'EOF'\n"+inspect+"\nEOF\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		info, err := (&Podman{Bin: bin}).ImageInspect(context.Background(), "alpine:3.24.2")
		if err != nil || info.ImagesDir != want {
			t.Errorf("UpperDir %q: images dir %q %v, want %q", upper, info.ImagesDir, err, want)
		}
	}
}

// An index entry luxd could not store (a NUL in it) is left out; the
// others still count.
func TestImagePlatformsSkipsUnstorable(t *testing.T) {
	dir := t.TempDir()
	bigData(t, dir, imgID, "manifest-"+listDig, `{"manifests":[`+
		`{"platform":{"architecture":"amd64","os":"linux"}},`+
		`{"platform":{"architecture":"arm64","os":"linux","variant":"v8\u0000"}}]}`)
	if got, err := ImagePlatforms(dir, "docker.io/library/alpine@"+listDig, ImageInfo{ID: imgID}); err != nil || fmt.Sprint(got) != "[linux/amd64]" {
		t.Fatalf("%q %v, want [linux/amd64]", got, err)
	}
}
