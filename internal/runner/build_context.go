package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/spec"
)

// writeContextFiles writes image.build.contextFiles into dir, the build's
// default context: each a regular file, 0644, through an os.Root so no
// path can land outside it. The paths were checked by luxd; they are
// checked again here, as the runner trusts no path it writes as root.
func writeContextFiles(dir string, files map[string]string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := spec.CheckContextPath(name); err != nil {
			return fmt.Errorf("image.build.contextFiles: %w", err)
		}
		if d := path.Dir(name); d != "." {
			if err := root.MkdirAll(d, 0o755); err != nil {
				return fmt.Errorf("image.build.contextFiles: %s: %w", name, err)
			}
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("image.build.contextFiles: %s: %w", name, err)
		}
		_, err = f.WriteString(files[name])
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			// Not the umask's: COPY keeps modes.
			err = root.Chmod(name, 0o644)
		}
		if err != nil {
			return err
		}
	}
	return gitws.FixDirModes(root)
}

// materializeContexts fetches the build's named contexts into dir, each
// in a directory of its own, before the build: a git context is its
// commit's tree, through the tenant's own mirror (never another tenant's),
// with the Run's credential. It returns each context's directory by name,
// for podman build --build-context.
func (p *placement) materializeContexts(ctx context.Context, sp spec.RunSpec, dir string) (map[string]string, error) {
	contexts := sp.Image.Build.Contexts
	out := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(contexts)) {
		g := contexts[name].Git
		if g == nil {
			return nil, fmt.Errorf("image.build.contexts.%s: no source", name)
		}
		d := filepath.Join(dir, "contexts", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
		repo := gitws.Repo{Tenant: p.tenantID, Name: "context " + name, URL: g.URL, Ref: g.Ref, Token: p.assign.Secrets[g.Credential]}
		if err := p.r.git.Export(ctx, repo, d, int64(sp.Resources.Disk)); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			if errors.Is(err, gitws.ErrContextTooLarge) {
				return nil, fmt.Errorf("image.build.contexts.%s: %w (%s)", name, err, spec.BytesText(int64(sp.Resources.Disk)))
			}
			return nil, fmt.Errorf("image.build.contexts.%s: %s", name, scrubURL(err, g.URL))
		}
		p.event(ctx, "image.context", map[string]any{"name": name, "url": gitws.Scrub(g.URL), "commit": g.Ref})
		out[name] = d
	}
	return out, nil
}
