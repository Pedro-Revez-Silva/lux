package spec

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bounds of a build's context (image.build.contextFiles, contexts).
const (
	MaxContextFiles      = 64
	MaxContextBytes      = 1 << 20
	MaxContextPath       = 255
	MaxContextPathDepth  = 16
	MaxBuildContexts     = 4
	maxContextNameLength = 63
)

var (
	contextPartRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)
	contextNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	commitRe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// checkBuildContext validates image.build's contextFiles and contexts;
// secrets are the names the spec declares. It reads only the spec.
func (s *RunSpec) checkBuildContext(secrets map[string]bool) []string {
	b := s.Image.Build
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if len(b.ContextFiles) > MaxContextFiles {
		fail("image.build.contextFiles: %d files, at most %d", len(b.ContextFiles), MaxContextFiles)
	}
	total := 0
	for _, name := range sortedKeys(b.ContextFiles) {
		text := b.ContextFiles[name]
		if err := CheckContextPath(name); err != nil {
			fail("image.build.contextFiles: %v", err)
			continue
		}
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			fail("image.build.contextFiles: %q is not UTF-8 text (or holds NUL)", name)
		}
		total += len(name) + len(text)
		// A file can't also be a directory of another.
		for other := range b.ContextFiles {
			if strings.HasPrefix(other, name+"/") {
				fail("image.build.contextFiles: %q is a file and a directory of %q", name, other)
				break
			}
		}
	}
	if total > MaxContextBytes {
		fail("image.build.contextFiles: %d bytes in all, at most %d", total, MaxContextBytes)
	}

	if len(b.Contexts) > MaxBuildContexts {
		fail("image.build.contexts: %d, at most %d", len(b.Contexts), MaxBuildContexts)
	}
	stages := StageNames(b.Containerfile)
	for _, name := range sortedKeys(b.Contexts) {
		c := b.Contexts[name]
		at := "image.build.contexts." + name
		if !contextNameRe.MatchString(name) || name == "scratch" || name == "context" {
			fail("image.build.contexts: invalid name %q: 1-%d of a-z 0-9 _ -, starting with a letter, not scratch or context", name, maxContextNameLength)
			continue
		}
		if stages[name] {
			fail("%s: a stage of the Containerfile has that name too", at)
		}
		g := c.Git
		if g == nil {
			fail("%s: needs a source (git)", at)
			continue
		}
		u, err := url.Parse(g.URL)
		switch {
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "":
			// The commit pins the content (git checks every object's
			// hash), so plain http is allowed; no other transport is.
			fail("%s.git.url: need an https (or http) URL", at)
		case u.User != nil:
			fail("%s.git.url must not carry credentials: use credential: <secret name>", at)
		case u.RawQuery != "" || u.Fragment != "" || strings.Contains(g.URL, "#"):
			fail("%s.git.url: no query or fragment", at)
		case !publicHost(u.Hostname()):
			fail("%s.git.url: %q is not a host the runner fetches from (localhost, loopback, link-local and IPv6 literals are refused)", at, u.Hostname())
		case !s.Network.Unrestricted && !s.Network.allows(u.Hostname()):
			fail("%s.git.url: network.egress does not allow %s: add an egress rule for it", at, u.Hostname())
		}
		if !commitRe.MatchString(g.Ref) {
			fail("%s.git.ref: need a commit's full 40-character lowercase hex sha, not a branch, tag or short sha", at)
		}
		if g.Credential != "" && !secrets[g.Credential] {
			fail("%s.git.credential: no secret named %q", at, g.Credential)
		}
	}
	return errs
}

// CheckContextPath is what a contextFiles path must be: relative, clean,
// of at most MaxContextPathDepth components of contextPartRe (not . or
// ..), at most MaxContextPath bytes.
func CheckContextPath(name string) error {
	if name == "" || len(name) > MaxContextPath {
		return fmt.Errorf("path %q: 1 to %d bytes", truncate(name, 80), MaxContextPath)
	}
	parts := strings.Split(name, "/")
	if len(parts) > MaxContextPathDepth {
		return fmt.Errorf("path %q: at most %d components", truncate(name, 80), MaxContextPathDepth)
	}
	for _, p := range parts {
		if p == "." || p == ".." || !contextPartRe.MatchString(p) {
			return fmt.Errorf("path %q: need a relative path of A-Z a-z 0-9 . _ @ + - components, without . or ..", truncate(name, 80))
		}
	}
	return nil
}

// StageNames are the Containerfile's stage names (FROM ... AS name),
// lowercased.
func StageNames(cf string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(cf, "\n") {
		if f, ok := ParseFrom(line); ok && f.Name != "" {
			out[strings.ToLower(f.Name)] = true
		}
	}
	return out
}

// FromImages are the images the Containerfile's FROM lines name (stages,
// scratch and variables included: callers judge).
func FromImages(cf string) []string {
	var out []string
	for _, line := range strings.Split(cf, "\n") {
		if f, ok := ParseFrom(line); ok {
			out = append(out, f.Image)
		}
	}
	return out
}

// publicHost: a host name or IPv4 address the runner may fetch from, as
// validRegistry has it: not localhost, a loopback, link-local or
// unspecified address, and no IPv6 literal.
func publicHost(h string) bool {
	if h == "" || strings.Contains(h, ":") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return validRegistry(h)
	}
	return validRegistry(strings.ToLower(h))
}

// PublishedRepo is the local repository a runner names its tenants'
// published images in (localhost/lux-published:<tenant>-<digest>). It is
// lux's: no spec may name it, however spelled (Podman resolves the short
// name lux-published:... to it too), since a local name skips the
// registry's own check of who may pull.
const PublishedRepo = "lux-published"

// ReservedImage reports whether ref names PublishedRepo: its repository's
// last path component, under any registry or none.
func ReservedImage(ref string) bool {
	name := strings.ToLower(strings.TrimSpace(ref))
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	return name[strings.LastIndexByte(name, '/')+1:] == PublishedRepo
}

// ManagedRepoRoot reports whether root is a valid [images] repository: a
// repository with its registry and no tag (as image.build.cache).
func ManagedRepoRoot(root string) bool { return validCacheRepo(root) }

// TenantRepository is the managed repository a tenant publishes to.
func TenantRepository(root, tenant string) string {
	return root + "/" + strings.ToLower(tenant)
}

// ImageRepository is ref's repository: fully qualified as Podman names it
// (docker.io/library/alpine for alpine), without tag or digest.
func ImageRepository(ref string) string {
	name := ref
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	first, _, ok := strings.Cut(name, "/")
	switch {
	case !ok:
		name = "docker.io/library/" + name
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		name = "docker.io/" + name
	}
	return name
}

// ManagedImages is luxd's managed image registry as one tenant sees it:
// its own repository under Root, and the shared repositories every
// tenant may pull. Root "" is none.
type ManagedImages struct {
	Root   string
	Shared []string
}

// Under reports whether ref is in the managed registry: under Root, or
// one of the shared repositories.
func (m ManagedImages) Under(ref string) bool {
	if m.Root == "" {
		return false
	}
	repo := ImageRepository(ref)
	return repo == m.Root || strings.HasPrefix(repo, m.Root+"/") || slices.Contains(m.Shared, repo)
}

// Pullable reports whether tenant may pull ref with the managed
// credential: its own repository's, or a shared repository's.
func (m ManagedImages) Pullable(ref, tenant string) bool {
	if m.Root == "" {
		return false
	}
	repo := ImageRepository(ref)
	return repo == TenantRepository(m.Root, tenant) || slices.Contains(m.Shared, repo)
}

// Uses reports whether the spec needs the managed registry: it publishes,
// or its image or a FROM is in it. resolved is the Run's pinned
// Containerfile, if any.
func (m ManagedImages) Uses(s RunSpec, resolved string) bool {
	if s.Image.Build != nil && s.Image.Build.Publish {
		return true
	}
	if s.Image.Ref != "" {
		return m.Under(s.Image.Ref)
	}
	if s.Image.Build == nil {
		return false
	}
	for _, cf := range []string{s.Image.Build.Containerfile, resolved} {
		if slices.ContainsFunc(FromImages(cf), m.Under) {
			return true
		}
	}
	return false
}

// CheckManaged is what a tenant's spec may do with the managed registry:
// publish only when there is one, and name only its own repository's and
// the shared repositories' images; a build cache is never in it.
func (m ManagedImages) CheckManaged(s RunSpec, tenant string) []string {
	var errs []string
	b := s.Image.Build
	if b != nil && b.Publish && m.Root == "" {
		errs = append(errs, "image.build.publish: this luxd has no managed image registry ([images] repository and its credential)")
	}
	if m.Root == "" {
		return errs
	}
	if s.Image.Ref != "" && m.Under(s.Image.Ref) && !m.Pullable(s.Image.Ref, tenant) {
		errs = append(errs, fmt.Sprintf("image.ref: %s is another tenant's managed repository", ImageRepository(s.Image.Ref)))
	}
	if b != nil {
		for _, img := range FromImages(b.Containerfile) {
			if m.Under(img) && !m.Pullable(img, tenant) {
				errs = append(errs, fmt.Sprintf("image.build.containerfile: FROM %s is another tenant's managed repository", ImageRepository(img)))
			}
		}
		if b.Cache != "" && m.Under(b.Cache) {
			errs = append(errs, "image.build.cache: the managed image registry is not a build cache: use image.build.publish")
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
