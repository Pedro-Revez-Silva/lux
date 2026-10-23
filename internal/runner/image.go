package runner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func (p *placement) ensureImage(ctx context.Context, sp spec.RunSpec, net podman.Network) (string, error) {
	user, err := p.registryLogin(sp)
	if err != nil {
		return "", err
	}
	managed, err := p.managedLogin()
	if err != nil {
		user.close()
		return "", err
	}
	auth := &imageAuth{user: user, managed: managed, m: p.assign.ManagedImages, tenant: p.tenantID}
	defer auth.close()
	if sp.Image.Build != nil {
		tag, err := p.buildImage(ctx, sp, net, auth)
		return tag, auth.redact(err)
	}
	return sp.Image.Ref, auth.redact(p.pullIfMissing(ctx, sp.Image.Ref, auth))
}

// pullIfMissing makes sure the host has ref, for this Run's tenant. Images
// are host-wide, but pulling one is proof of access: an image lux pulled
// for one tenant (perhaps with its registry credentials) is reused without
// a pull only by a tenant that has pulled it too; any other pulls it again
// with its own credentials, which fails without access (and costs little
// with the layers already here). An image the host had before lux (the
// operator's) is anyone's, and never lux's to remove.
func (p *placement) pullIfMissing(ctx context.Context, ref string, auth *imageAuth) error {
	// Another tenant's managed image is refused even when it is here.
	if _, err := auth.forPull(ref); err != nil {
		return err
	}
	p.r.images.touch(ref, p.tenantID)
	if p.r.pm.ImageExists(ctx, ref) && p.r.images.mayUse(ref, p.tenantID) {
		return nil
	}
	return p.pull(ctx, ref, auth)
}

func (p *placement) pull(ctx context.Context, ref string, auth *imageAuth) error {
	a, err := auth.forPull(ref)
	if err != nil {
		return err
	}
	p.event(ctx, "image.pull", map[string]any{"ref": ref})
	if err := p.r.pm.Pull(ctx, ref, a.path()); err != nil {
		return err
	}
	p.r.images.pulledBy(ref, p.tenantID)
	return nil
}

// buildImage builds the spec's Containerfile on this host. The first build
// pins every FROM to the digest it resolves to here and reports the pinned
// Containerfile; luxd keeps it, and later placements build from it, so a
// moved tag does not change what a Run runs on. Built images are tagged by
// a hash of what they are built from, so a host builds each once.
//
// The build runs tenant code, so it is contained like the workload: its own
// user namespace, the workload's capabilities and limits, and the Run's
// network, with its egress rules and DNS stub. What a build produces depends
// on what it could reach, so a built image is reused only by the same tenant
// with the same network rules, and there is no layer cache (it would share
// RUN results across tenants and rules). A shared build cache (a registry
// repository) holds whole images under the same key, so it shares them
// only as a host would.
//
// With publish, the image is pushed to the tenant's managed repository and
// read back by digest before the build is reported: the Run's image
// resolution then names it (publishedRef), and a later placement on a host
// of the same platform runs exactly that image rather than rebuilding.
// The managed repository also serves as the tenant's own build cache.
func (p *placement) buildImage(ctx context.Context, sp spec.RunSpec, net podman.Network, auth *imageAuth) (string, error) {
	b := sp.Image.Build
	if b.Publish && p.assign.ManagedImages == nil {
		return "", errors.New("image.build.publish: luxd sent no managed image registry ([images] repository is not configured)")
	}
	if b.Publish {
		if err := auth.managedUsable(); err != nil {
			return "", err
		}
	}
	var prev proto.ImageResolution
	if p.assign.ImageResolved != nil {
		prev = *p.assign.ImageResolved
	}
	cf := prev.Containerfile
	var err error
	if cf == "" {
		if cf, err = p.pinFroms(ctx, b.Containerfile, contextNames(b), auth); err != nil {
			return "", err
		}
	}
	platform := hostPlatform()
	key := buildKey(p.tenantID, sp.Network, cf, b.Args, buildInputs{Files: b.ContextFiles, Contexts: b.Contexts, Platform: platform})
	tag := "localhost/lux-build:" + key
	// The resolution is what later placements build from: the first one
	// (and the first published one) must reach luxd before the Run goes on.
	must := prev.Containerfile == "" || (b.Publish && prev.PublishedRef == "")

	if b.Publish && prev.PublishedRef != "" && prev.Platform == platform {
		name, id, err := p.usePublished(ctx, prev, tag, auth)
		if err != nil {
			return "", err
		}
		p.r.images.used(name)
		p.event(ctx, "image.built", map[string]any{"tag": tag, "image": name, "imageId": id, "cached": true, "containerfile": cf,
			"fromPublished": true, "publishedRef": prev.PublishedRef, "platform": prev.Platform})
		return name, nil
	}

	id, err := p.r.pm.ImageID(ctx, tag)
	cached := err == nil
	fromCache, fromManaged := false, false
	var published string
	if b.Publish {
		// The tenant's repository is authoritative for the key: what it
		// has there is the build, even over a local one (which may be an
		// inconsistent rebuild), and a key it has is never pushed over.
		ref := managedCacheRef(p.assign.ManagedImages.Repository, key)
		if _, fromManaged = p.pullCached(ctx, ref, tag, auth.managed, true); fromManaged {
			// The image is the digest pulled, not whatever the key's
			// names say now (another placement may move them).
			if published, err = p.publishedAs(ctx, ref); err != nil {
				return "", err
			}
			if id, err = p.r.pm.ImageID(ctx, published); err != nil {
				return "", err
			}
		}
	}
	if !cached && !fromManaged && b.Cache != "" {
		id, fromCache = p.pullCached(ctx, cacheRef(b.Cache, key), tag, auth.user, false)
	}
	if !cached && !fromCache && !fromManaged {
		p.event(ctx, "image.build", map[string]any{"tag": tag})
		local, err := p.localBases(ctx, cf, auth)
		if err != nil {
			return "", err
		}
		if id, err = p.build(ctx, sp, local, tag, net); err != nil {
			return "", err
		}
		if b.Cache != "" {
			go p.pushCached(context.WithoutCancel(ctx), sp, tag, cacheRef(b.Cache, key))
		}
	}
	p.r.images.used(tag)
	// luxd keeps the first of these it sees as the Run's image resolution.
	built := map[string]any{"tag": tag, "imageId": id, "cached": cached, "containerfile": cf}
	if fromCache {
		built["fromCache"] = true
	}
	// What the placement runs: the build key's name, or, published, a
	// name of that digest's alone. The key's name is shared by every Run
	// built from the same inputs, and so may move to another Run's
	// published image (non-reproducible builds of one key on two hosts)
	// before this placement creates its container.
	image := tag
	if b.Publish {
		if published == "" {
			if published, err = p.publish(ctx, id, key, auth); err != nil {
				return "", err
			}
		}
		if image, err = p.namePublished(ctx, id, published); err != nil {
			return "", err
		}
		built["image"], built["publishedRef"], built["platform"] = image, published, platform
		p.event(ctx, "image.published", map[string]any{"ref": published, "platform": platform, "reused": fromManaged})
	}
	if must {
		if err := p.reportRetrying(ctx, proto.RunEvent{Type: "image.built", Data: built}); err != nil {
			return "", err
		}
	} else {
		p.event(ctx, "image.built", built)
	}
	if prev.ImageID != "" && prev.ImageID != id && (prev.Platform == "" || prev.Platform == platform) {
		// Not a failure: state volumes do not depend on the image, only the
		// writable layer would, and that does not travel.
		p.event(ctx, "image.rebuild-differs", map[string]any{"imageId": id, "firstImageId": prev.ImageID,
			"hint": "the build is not reproducible (RUN steps that fetch or date things); the Run continues on this image"})
	}
	return image, nil
}

// usePublished readies the Run's published image under a name of its
// own (publishedName), and returns it and the image's id: the host's own
// build if it is that image (by id), else the published digest, pulled
// with the managed credential. Never the build key's name, which another
// Run's placement may move.
func (p *placement) usePublished(ctx context.Context, prev proto.ImageResolution, tag string, auth *imageAuth) (name, id string, err error) {
	name = publishedName(p.tenantID, prev.PublishedRef)
	if id, err := p.r.pm.ImageID(ctx, name); err == nil {
		p.r.images.touch(name, p.tenantID)
		return name, id, nil
	}
	src := prev.PublishedRef
	if local, err := p.r.pm.ImageID(ctx, tag); err == nil && local == prev.ImageID {
		src = local
	} else if err := p.pullIfMissing(ctx, prev.PublishedRef, auth); err != nil {
		return "", "", fmt.Errorf("published image %s: %w", prev.PublishedRef, err)
	}
	if id, err = p.r.pm.ImageID(ctx, src); err != nil {
		return "", "", err
	}
	if _, err := p.r.pm.Run(ctx, "tag", id, name); err != nil {
		return "", "", err
	}
	return name, id, nil
}

// namePublished gives image id, published as ref, its own name
// (publishedName) and returns it.
func (p *placement) namePublished(ctx context.Context, id, ref string) (string, error) {
	name := publishedName(p.tenantID, ref)
	if _, err := p.r.pm.Run(ctx, "tag", id, name); err != nil {
		return "", err
	}
	p.r.images.used(name)
	return name, nil
}

// publishedName is the local name of a tenant's published image: its
// digest's, which no other image ever gets (unlike the build key's name,
// shared by every build of the same inputs).
func publishedName(tenant, ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return "localhost/lux-published:" + strings.ToLower(tenant) + "-" + strings.TrimPrefix(digest, "sha256:")
}

// adopt names ref's image tag, the local build's name (what later builds
// of the same key look for), and returns its id.
func (p *placement) adopt(ctx context.Context, ref, tag string) (string, error) {
	if _, err := p.r.pm.Run(ctx, "tag", ref, tag); err != nil {
		return "", err
	}
	return p.r.pm.ImageID(ctx, tag)
}

// publish pushes image id (by id: the build key's name may have moved to
// another build since) to the tenant's managed repository, as
// <repository>:<key>, and reads it back by the manifest digest the push
// reported: the registry has exactly this image (same id) under that
// digest. Returns repository@digest.
func (p *placement) publish(ctx context.Context, id, key string, auth *imageAuth) (string, error) {
	m := p.assign.ManagedImages
	if err := auth.managedUsable(); err != nil {
		return "", err
	}
	dest := managedCacheRef(m.Repository, key)
	pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	digest, err := p.r.pm.PushDigest(pctx, id, dest, auth.managed.path(), filepath.Join(p.r.cfg.DataDir, "tmp"))
	if err != nil {
		return "", fmt.Errorf("publishing to %s: %w", dest, err)
	}
	ref := m.Repository + "@" + digest
	if err := p.pull(pctx, ref, auth); err != nil {
		return "", fmt.Errorf("reading back %s: %w", ref, err)
	}
	got, err := p.r.pm.ImageID(ctx, ref)
	if err != nil {
		return "", err
	}
	if got != id {
		return "", fmt.Errorf("published %s is image %s, not the build's %s", ref, got, id)
	}
	return ref, nil
}

// publishedAs is ref (just pulled from the managed repository) as
// repository@digest.
func (p *placement) publishedAs(ctx context.Context, ref string) (string, error) {
	digest, err := p.r.pm.ImageDigest(ctx, ref)
	if err != nil {
		return "", err
	}
	return spec.ImageRepository(ref) + "@" + digest, nil
}

// pullCached pulls a build from a build cache (the spec's, or with
// managed the tenant's managed repository) and tags it as the local
// build; ok is false when the cache does not have it (or can't be read,
// which is a warning: the placement builds).
func (p *placement) pullCached(ctx context.Context, ref, tag string, auth *registryAuth, managed bool) (id string, ok bool) {
	err := p.r.pm.Pull(ctx, ref, auth.path())
	if err == nil {
		p.r.images.pulledBy(ref, p.tenantID)
		id, err = p.adopt(ctx, ref, tag)
	}
	data := map[string]any{"hit": err == nil, "ref": ref}
	if managed {
		data["managed"] = true
	}
	switch {
	case err == nil:
		p.event(ctx, "image.cache", data)
		return id, true
	case ctx.Err() != nil:
	case podman.IsManifestUnknown(err):
		p.event(ctx, "image.cache", data)
	default:
		data["warning"] = "pulling from the build cache failed; building: " + tail(auth.redactString(err.Error()), 1000)
		p.event(ctx, "image.cache", data)
	}
	return "", false
}

// pushCached pushes a new build to the build cache, in the background:
// the Run does not wait for it. Two hosts building the same key push the
// same tag; either is the build.
func (p *placement) pushCached(ctx context.Context, sp spec.RunSpec, tag, ref string) {
	// Its own auth file: the placement's is removed once its image is ready.
	auth, err := p.registryLogin(sp)
	if err == nil {
		defer auth.close()
		pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err = p.r.pm.Push(pctx, tag, ref, auth.path())
		cancel()
	}
	if err != nil {
		p.event(ctx, "image.cache", map[string]any{"pushed": false, "ref": ref,
			"warning": "pushing to the build cache failed: " + tail(auth.redactString(err.Error()), 1000)})
		return
	}
	p.event(ctx, "image.cache", map[string]any{"pushed": true, "ref": ref})
}

// build runs podman build and returns the image id.
func (p *placement) build(ctx context.Context, sp spec.RunSpec, cf, tag string, net podman.Network) (string, error) {
	dir, err := os.MkdirTemp(filepath.Join(p.r.cfg.DataDir, "tmp"), "build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	// The Containerfile is beside the context, not in it.
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(cf), 0o600); err != nil {
		return "", err
	}
	ctxDir := filepath.Join(dir, "context")
	if err := os.Mkdir(ctxDir, 0o755); err != nil {
		return "", err
	}
	if err := writeContextFiles(ctxDir, sp.Image.Build.ContextFiles); err != nil {
		return "", err
	}
	named, err := p.materializeContexts(ctx, sp, dir)
	if err != nil {
		return "", err
	}
	iid := filepath.Join(dir, "iid")
	cg, err := newBuildCgroup(p.runID, sp.Resources)
	if err != nil {
		return "", err
	}
	// Whatever the build started goes with it, however it ended: a
	// cancelled podman build can leave its RUN step running.
	defer cg.remove()
	mem := fmt.Sprintf("%d", p.r.mem.limit(int64(sp.Resources.Memory)))
	args := append(containment(true), "--quiet", "--pull=never", "--isolation=oci",
		"--layers=false", "--no-cache",
		"--timestamp=0", // no build-time dates in the image: rebuilds match when the steps do
		"--iidfile", iid,
		"--memory", mem, "--memory-swap", mem,
		"--cpu-period=100000", fmt.Sprintf("--cpu-quota=%d", int64(sp.Resources.CPUs*100000)),
		// The spec's process limit (podman build has no --pids-limit) is
		// on this cgroup, which the build's steps run under.
		"--cgroup-parent", cg.path,
		"--network", networkName(p.runID),
		"--label", LabelManaged+"=true",
		"-t", tag, "-f", file,
	)
	args = append(args, dnsArgs(sp, net)...)
	for _, k := range slices.Sorted(maps.Keys(sp.Image.Build.Args)) {
		args = append(args, "--build-arg", k+"="+sp.Image.Build.Args[k])
	}
	for _, name := range slices.Sorted(maps.Keys(named)) {
		args = append(args, "--build-context", name+"="+named[name])
	}
	if _, err := p.r.pm.Build(ctx, append(args, ctxDir)...); err != nil {
		return "", fmt.Errorf("build: %s", tail(err.Error(), 2000))
	}
	b, err := os.ReadFile(iid)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "sha256:"), nil
}

// pinFroms rewrites every FROM that names an image to that image at the
// digest it has on this host (pulling it if the host lacks it). contexts
// are the build's named contexts, which COPY --from may name too.
func (p *placement) pinFroms(ctx context.Context, cf string, contexts map[string]bool, auth *imageAuth) (string, error) {
	var out []string
	stages := map[string]bool{}
	n := 0
	for _, line := range strings.Split(cf, "\n") {
		f, ok := spec.ParseFrom(line)
		if !ok {
			// COPY --from and RUN --mount=from=... may only name earlier
			// stages: an image named there would be neither pinned nor pulled.
			for _, from := range fromRefs(line) {
				if !stages[strings.ToLower(from)] && !contexts[from] {
					return "", fmt.Errorf("%q names an image; only earlier stages can be used there. Add `FROM %s AS <name>` and use the name", strings.TrimSpace(line), from)
				}
			}
			out = append(out, line)
			continue
		}
		if f.Name != "" && contexts[strings.ToLower(f.Name)] {
			return "", fmt.Errorf("stage %s has the name of a build context", f.Name)
		}
		stages[fmt.Sprint(n)] = true
		n++
		pinned, err := p.pinFrom(ctx, f, stages, auth)
		if err != nil {
			return "", err
		}
		out = append(out, pinned)
		if f.Name != "" {
			stages[strings.ToLower(f.Name)] = true
		}
	}
	return strings.Join(out, "\n"), nil
}

// pinFrom pins one FROM line; earlier stages, scratch and images already
// pinned are left as they are.
func (p *placement) pinFrom(ctx context.Context, f spec.From, stages map[string]bool, auth *imageAuth) (string, error) {
	if f.Image == "scratch" || stages[strings.ToLower(f.Image)] {
		return f.With(f.Image), nil
	}
	if strings.Contains(f.Image, "@sha256:") {
		// Pinned already; still not another tenant's managed image.
		if _, err := auth.forPull(f.Image); err != nil {
			return "", err
		}
		return f.With(f.Image), nil
	}
	if strings.Contains(f.Image, "$") {
		return "", fmt.Errorf("FROM %s: a base image named by a variable cannot be pinned; name it literally", f.Image)
	}
	if err := p.pullIfMissing(ctx, f.Image, auth); err != nil {
		return "", err
	}
	digest, err := p.r.pm.ImageDigest(ctx, f.Image)
	if err != nil {
		return "", err
	}
	return f.With(f.Image + "@" + digest), nil
}

// localBases rewrites a pinned Containerfile's FROMs to a name that is the
// same on every host: localhost/lux-base:<digest>, tagged on the image with
// that digest (under whatever name the host has it, pulled if it has none).
// Builds record how FROM named their base, so naming it identically is what
// makes a rebuild on another host produce the same image.
func (p *placement) localBases(ctx context.Context, cf string, auth *imageAuth) (string, error) {
	byDigest, err := p.r.pm.ImagesByDigest(ctx)
	if err != nil {
		return "", err
	}
	lines := strings.Split(cf, "\n")
	for i, line := range lines {
		f, ok := spec.ParseFrom(line)
		if !ok || !strings.Contains(f.Image, "@sha256:") {
			continue
		}
		_, digest, _ := strings.Cut(f.Image, "@")
		// Under any name; or by this reference (a digest of a multi-arch
		// index resolves to the platform image it was pulled as).
		// A local copy counts only if this tenant may use it (see
		// pullIfMissing); otherwise pull it by digest, as itself.
		if _, err := auth.forPull(f.Image); err != nil {
			return "", err
		}
		id := ""
		if p.r.images.mayUse(f.Image, p.tenantID) {
			id = byDigest[digest]
			if id == "" {
				id, _ = p.r.pm.ImageID(ctx, f.Image)
			}
		}
		if id == "" {
			if err := p.pull(ctx, f.Image, auth); err != nil {
				return "", err
			}
			if id, err = p.r.pm.ImageID(ctx, f.Image); err != nil {
				return "", err
			}
		}
		local := "localhost/lux-base:" + strings.TrimPrefix(digest, "sha256:")
		if _, err := p.r.pm.Run(ctx, "tag", id, local); err != nil {
			return "", err
		}
		p.r.images.used(local)
		lines[i] = f.With(local)
	}
	return strings.Join(lines, "\n"), nil
}

// fromRefs are the images or stages a non-FROM line reads from:
// COPY --from=x and RUN --mount=...,from=x.
func fromRefs(line string) []string {
	var refs []string
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "--from="); ok {
			refs = append(refs, v)
		}
		if v, ok := strings.CutPrefix(f, "--mount="); ok {
			for _, kv := range strings.Split(v, ",") {
				if from, ok := strings.CutPrefix(kv, "from="); ok {
					refs = append(refs, from)
				}
			}
		}
	}
	return refs
}

// buildInputs are a build's inputs besides its Containerfile and args:
// its context and the platform it is built for.
type buildInputs struct {
	Files    map[string]string
	Contexts map[string]spec.BuildContext
	Platform string
}

// buildKey names a build by what it is built from, and by whom under what
// network rules: a build's result depends on what it could reach. The
// context (every file's path and content, every named context's source at
// its commit) and the platform are part of what it is built from.
func buildKey(tenant string, network spec.Network, cf string, args map[string]string, in buildInputs) string {
	h := sha256.New()
	nb, _ := json.Marshal(struct {
		Egress       []spec.EgressRule
		Unrestricted bool
	}{network.Egress, network.Unrestricted})
	fmt.Fprintf(h, "%s\x00%s\x00", tenant, nb)
	h.Write([]byte(cf))
	for _, k := range slices.Sorted(maps.Keys(args)) {
		fmt.Fprintf(h, "\x00%s=%s", k, args[k])
	}
	// Each length-prefixed: no two contexts hash alike.
	fmt.Fprintf(h, "\x00platform=%s", in.Platform)
	for _, k := range slices.Sorted(maps.Keys(in.Files)) {
		fmt.Fprintf(h, "\x00file %d:%s %d:", len(k), k, len(in.Files[k]))
		h.Write([]byte(in.Files[k]))
	}
	for _, k := range slices.Sorted(maps.Keys(in.Contexts)) {
		if g := in.Contexts[k].Git; g != nil {
			fmt.Fprintf(h, "\x00context %d:%s git %d:%s %s", len(k), k, len(g.URL), g.URL, g.Ref)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// hostPlatform is the platform this host builds for.
func hostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// managedCacheRef is the tag a build has in the tenant's managed
// repository: the local build's key, which covers the tenant, platform and
// every input.
func managedCacheRef(repo, key string) string { return repo + ":" + key }

// contextNames are the build's named contexts.
func contextNames(b *spec.Build) map[string]bool {
	out := map[string]bool{}
	for name := range b.Contexts {
		out[name] = true
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Whole lines from the end.
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "…" + s
}

// imageUse remembers the images this host's lux pulled, built or tagged
// (image.ref and FROM bases it pulled, the build cache's, lux-build and
// lux-base), and when each was last used, in a file, so the GC can remove
// the ones unused for longer than the host TTL, across runner restarts.
// Images the host had before lux needed them are not in it: lux never
// removes those.
type imageUse struct {
	mu   sync.Mutex
	path string
	last map[string]int64 // ref → unix ms
	// by: the tenants that pulled each ref lux pulled, so may reuse it.
	by    map[string][]string
	path2 string
	// pinned: images a starting placement is about to use, never GC'd.
	pinned map[string]int
	// gen counts changes to last and by: the disk guard's diskGen.
	gen atomic.Uint64
}

func newImageUse(dataDir string) *imageUse {
	u := &imageUse{path: filepath.Join(dataDir, "images.json"), path2: filepath.Join(dataDir, "images-by.json"),
		last: map[string]int64{}, by: map[string][]string{}, pinned: map[string]int{}}
	if b, err := os.ReadFile(u.path); err == nil {
		_ = json.Unmarshal(b, &u.last)
	}
	if b, err := os.ReadFile(u.path2); err == nil {
		_ = json.Unmarshal(b, &u.by)
	}
	return u
}

// pulledBy records that tenant pulled ref: it has access, and ref is lux's.
func (u *imageUse) pulledBy(ref, tenant string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.last[ref] = time.Now().UnixMilli()
	if !slices.Contains(u.by[ref], tenant) {
		u.by[ref] = append(u.by[ref], tenant)
	}
	u.saveLocked()
}

// mayUse: whether tenant may use the local ref without pulling it. One lux
// never pulled (the host's own) is anyone's; one lux pulled, only its
// pullers'.
func (u *imageUse) mayUse(ref, tenant string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	by, pulled := u.by[ref]
	return !pulled || slices.Contains(by, tenant)
}

// used records a use of an image lux made (or pulled): it is lux's.
func (u *imageUse) used(ref string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.last[ref] = time.Now().UnixMilli()
	u.saveLocked()
}

// touch records a use of ref only if lux has it already, and by a tenant
// that may use it.
func (u *imageUse) touch(ref, tenant string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if by, pulled := u.by[ref]; pulled && !slices.Contains(by, tenant) {
		return
	}
	if _, ok := u.last[ref]; ok {
		u.last[ref] = time.Now().UnixMilli()
		u.saveLocked()
	}
}

func (u *imageUse) forget(ref string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.last, ref)
	delete(u.by, ref)
	u.saveLocked()
}

// pin keeps ref from the GC until the returned func is called.
func (u *imageUse) pin(ref string) func() {
	u.mu.Lock()
	u.pinned[ref]++
	u.mu.Unlock()
	return sync.OnceFunc(func() {
		u.mu.Lock()
		if u.pinned[ref]--; u.pinned[ref] <= 0 {
			delete(u.pinned, ref)
		}
		u.mu.Unlock()
	})
}

// snapshot is the GC's view: lux's images, less those a starting placement
// has pinned.
func (u *imageUse) snapshot() map[string]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := maps.Clone(u.last)
	for ref := range u.pinned {
		delete(out, ref)
	}
	return out
}

func (u *imageUse) saveLocked() {
	u.gen.Add(1)
	b, _ := json.Marshal(u.last)
	_ = writeFileAtomic(u.path, b, 0o600)
	b, _ = json.Marshal(u.by)
	_ = writeFileAtomic(u.path2, b, 0o600)
}

// gcImages removes the images lux pulled, built or tagged that went unused
// for longer than ttl; then, while the disk holding Podman's storage is
// fuller than --image-disk-high, lux's images no container uses, least
// recently used first. An image a container still uses is left (podman
// refuses, and the next pass tries again once it is gone).
func (r *Runner) gcImages(ctx context.Context, ttl time.Duration) {
	var removed []string
	for ref, at := range r.images.snapshot() {
		if time.Since(time.UnixMilli(at)) > ttl && r.rmi(ctx, ref) {
			removed = append(removed, ref)
		}
	}
	removed = append(removed, r.relieveDisk(ctx)...)
	if len(removed) > 0 {
		// No "image prune": it would take the operator's unnamed images too.
		// Removing an image by name already frees its layers.
		slices.Sort(removed)
		r.log.Info("removed images", "images", removed)
	}
}

// rmi removes one of lux's image names; true once it is gone.
func (r *Runner) rmi(ctx context.Context, ref string) bool {
	if _, err := r.pm.Run(ctx, "rmi", ref); err != nil && !errors.Is(err, podman.ErrNotFound) {
		return false
	}
	r.images.forget(ref)
	return true
}

// relieveDisk removes lux's unused images, least recently used first, until
// the disk holding Podman's storage is under the high mark again.
func (r *Runner) relieveDisk(ctx context.Context) []string {
	high := r.cfg.ImageDiskHigh
	if high <= 0 {
		return nil
	}
	root := r.graphRoot(ctx)
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return nil
	}
	used := int64(st.Blocks-st.Bfree) * st.Bsize
	total := used + int64(st.Bavail)*st.Bsize
	need := used - int64(float64(total)*high/100)
	if need <= 0 {
		return nil
	}
	removed := r.reclaimImages(ctx, need)
	if len(removed) > 0 {
		r.log.Info("disk over --image-disk-high: removed unused images", "usedPct", 100*used/max(total, 1), "images", removed)
	}
	return removed
}

// reclaimImages removes lux's unused images, least recently used first,
// toward freeing need bytes (pickEvictions: never one a container uses, a
// held local copy needs, or one used within recentUse).
func (r *Runner) reclaimImages(ctx context.Context, need int64) []string {
	if need <= 0 {
		return nil
	}
	inUse, err := r.pm.ContainerImages(ctx)
	if err != nil {
		return nil
	}
	var cands []imageCandidate
	for ref, at := range r.images.snapshot() {
		info, err := r.pm.ImageInspect(ctx, ref)
		if err != nil {
			if errors.Is(err, podman.ErrNotFound) {
				r.images.forget(ref)
			}
			continue
		}
		cands = append(cands, imageCandidate{Ref: ref, ID: info.ID, Names: info.Names, Size: info.Size, LastUsed: at})
	}
	var removed []string
	for _, ref := range pickEvictions(cands, inUse, r.localCopyImages(), need, time.Now().Add(-recentUse).UnixMilli()) {
		if r.rmi(ctx, ref) {
			removed = append(removed, ref)
		}
	}
	return removed
}

// learnPlatforms records the platforms image's reference offers, from what
// Podman stored when it pulled it (podman.ImagePlatforms); never a registry
// call. An image built here offers the host's platform. Not knowing leaves
// them nil, logged at debug: never a reason to fail the start.
func (p *placement) learnPlatforms(ctx context.Context, image string, built bool, info podman.ImageInfo) {
	platforms := []string{runtime.GOOS + "/" + runtime.GOARCH}
	if !built {
		dir := info.ImagesDir
		var err error
		if dir == "" {
			dir, err = p.r.imagesDir(ctx)
		}
		if err == nil {
			platforms, err = podman.ImagePlatforms(dir, image, info)
		}
		if err != nil {
			p.r.log.Debug("image platforms unknown", "run", p.runID, "epoch", p.epoch, "image", image, "err", err)
			return
		}
	}
	p.mu.Lock()
	p.imagePlatforms = platforms
	p.mu.Unlock()
}

func (p *placement) platforms() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.imagePlatforms)
}

// imagesDir is containers/storage's image metadata directory, asked of
// Podman until it answers.
func (r *Runner) imagesDir(ctx context.Context) (string, error) {
	r.imagesDirMu.Lock()
	defer r.imagesDirMu.Unlock()
	if r.imagesDirPath == "" {
		d, err := r.pm.ImagesDir(ctx)
		if err != nil {
			return "", err
		}
		r.imagesDirPath = d
	}
	return r.imagesDirPath, nil
}

// graphRoot is where Podman keeps images: asked once, /var/lib/containers
// if Podman won't say.
func (r *Runner) graphRoot(ctx context.Context) string {
	r.graphOnce.Do(func() {
		r.graph = "/var/lib/containers"
		if g, err := r.pm.GraphRoot(ctx); err == nil && g != "" {
			r.graph = g
		}
	})
	return r.graph
}

// fullRef is a reference as podman names images: docker.io/library/alpine:3
// for alpine:3, and :latest when it has no tag or digest.
func fullRef(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	switch {
	case !ok:
		ref = "docker.io/library/" + ref
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		ref = "docker.io/" + ref
	}
	if last := ref[strings.LastIndex(ref, "/")+1:]; !strings.ContainsAny(last, ":@") {
		ref += ":latest"
	}
	return ref
}

// recentUse: an image used this recently may be about to be (a placement
// between pulling it and creating its container, or building on it), so
// disk pressure does not remove it.
const recentUse = 10 * time.Minute

// imageCandidate is one of lux's image names, for the disk-pressure GC.
type imageCandidate struct {
	Ref      string
	ID       string
	Names    []string // every name the image has, lux's or not
	Size     int64
	LastUsed int64
}

// localCopyImages protects images needed by local volumes awaiting a resume,
// keyed by full ref to match eviction candidates.
func (r *Runner) localCopyImages() map[string]bool {
	out := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
	for _, e := range entries {
		if st, err := readRunState(r.runDir(e.Name())); err == nil && st.VolumesSnapshot != "" && st.Image != "" {
			out[fullRef(st.Image)] = true
		}
	}
	return out
}

// pickEvictions chooses which of lux's image names to remove to free need
// bytes: whole images no container uses and no local copy awaits (held,
// by full ref), least recently used first (an image's last use is its
// latest name's), and none used after recent. An image that also has a name
// lux did not give it would not be freed, so it
// is left alone. Sizes are estimates (layers can be shared): the next pass
// measures again.
func pickEvictions(cands []imageCandidate, inUse, held map[string]bool, need, recent int64) []string {
	type image struct {
		refs []string
		size int64
		last int64
		all  []string
	}
	byID := map[string]*image{}
	for _, c := range cands {
		im := byID[c.ID]
		if im == nil {
			im = &image{size: c.Size, all: c.Names}
			byID[c.ID] = im
		}
		im.refs = append(im.refs, fullRef(c.Ref))
		im.last = max(im.last, c.LastUsed)
	}
	var ids []string
	for id, im := range byID {
		if inUse[id] || inUse[strings.TrimPrefix(id, "sha256:")] || im.last > recent {
			continue
		}
		if slices.ContainsFunc(im.refs, func(ref string) bool { return held[ref] }) {
			continue
		}
		if slices.ContainsFunc(im.all, func(n string) bool { return !slices.Contains(im.refs, n) }) {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := cmp.Compare(byID[a].last, byID[b].last); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	var out []string
	for _, id := range ids {
		if need <= 0 {
			break
		}
		im := byID[id]
		slices.Sort(im.refs)
		out = append(out, im.refs...)
		need -= im.size
	}
	return out
}
