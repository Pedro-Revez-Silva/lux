package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// Artifacts are files a Run produces, collected on every exit:
//
//   - files matching the spec's artifacts.paths (globs, ** for any depth),
//     on the Run's volumes;
//   - whatever the workload put in $LUX_ARTIFACTS (the runtime volume's
//     artifacts directory), published under /.lux/artifacts/<name>.
//
// The workload controls every one of those files and directories, so the
// runner (root, on the host) reads them only through an os.Root per volume:
// nothing it opens, renames or removes can resolve outside the volume,
// whatever symlinks the workload made. Symlinks are never collected.
//
// Each artifact becomes a blob, uploaded like the snapshot. Its FileSize
// and FileSHA256 are the file's (what a download returns), not the blob's.

// Limits: an artifact set is for results, not a second snapshot.
const (
	maxArtifacts     = 1000
	maxArtifactBytes = 1 << 30 // per file
)

var errTooMany = fmt.Errorf("more than %d artifacts: the rest were skipped", maxArtifacts)

type collector struct {
	p    *placement
	out  []proto.Artifact
	errs []error
	full bool
}

func (c *collector) add(root *os.Root, rel, name string) {
	if len(c.out) >= maxArtifacts {
		if !c.full {
			c.full = true
			c.errs = append(c.errs, errTooMany)
		}
		return
	}
	a, err := c.p.artifactBlob(root, rel, name)
	if err != nil {
		c.errs = append(c.errs, fmt.Errorf("%s: %w", name, err))
		return
	}
	c.out = append(c.out, a)
}

func (p *placement) collectArtifacts(ctx context.Context) ([]proto.Artifact, error) {
	c := &collector{p: p}
	var patterns [][]string
	if p.assign != nil {
		for _, pat := range p.assign.Spec.Artifacts.Paths {
			patterns = append(patterns, splitPath(pat))
		}
	}
	if len(patterns) > 0 {
		// Each volume at its own mount path: a file is taken from the
		// volume the container sees it on (volumes may nest).
		vols := slices.Clone(p.state.Volumes)
		sort.Slice(vols, func(i, j int) bool { return vols[i].Path < vols[j].Path })
		for _, v := range vols {
			mp, err := p.r.mountpoint(ctx, v.Volume)
			if err != nil {
				c.errs = append(c.errs, err)
				continue
			}
			var inner []string // volumes mounted inside this one
			for _, o := range vols {
				if strings.HasPrefix(o.Path, v.Path+"/") {
					inner = append(inner, o.Path)
				}
			}
			c.walkVolume(mp, v.Path, inner, patterns)
		}
	}
	// Published on demand, in $LUX_ARTIFACTS.
	if rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID)); err == nil {
		c.collectPublished(rt)
	}
	return c.out, errors.Join(c.errs...)
}

// walkVolume collects a volume's files that match a pattern, skipping
// directories no pattern can reach and paths another volume covers.
func (c *collector) walkVolume(mountpoint, mountPath string, inner []string, patterns [][]string) {
	root, err := os.OpenRoot(mountpoint)
	if err != nil {
		c.errs = append(c.errs, err)
		return
	}
	defer root.Close()
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		switch {
		case c.full:
			return fs.SkipAll
		case err != nil:
			return nil // unreadable: skipped
		}
		name := path.Join(mountPath, rel)
		if d.IsDir() {
			if rel != "." && (slices.Contains(inner, name) || !anyReaches(patterns, splitPath(name))) {
				return fs.SkipDir
			}
			return nil
		}
		ns := splitPath(name)
		if d.Type().IsRegular() && slices.ContainsFunc(patterns, func(ps []string) bool { return globMatch(ps, ns) }) {
			c.add(root, rel, name)
		}
		return nil
	})
}

// publishedAside is where collected $LUX_ARTIFACTS wait, in the runtime
// volume, until luxd has the report that lists them.
const publishedAside = "artifacts.collected"

// collectPublished takes what the workload put in $LUX_ARTIFACTS. The
// runner first moves that directory aside (within the runtime volume,
// through the Root), then collects from there. If the runner restarts
// before luxd has the report, the moved-aside directory is collected
// again; it is removed only once the report went through (clearPublished).
func (c *collector) collectPublished(rt string) {
	root, err := os.OpenRoot(rt)
	if err != nil {
		c.errs = append(c.errs, err)
		return
	}
	defer root.Close()
	if _, err := root.Lstat(publishedAside); errors.Is(err, fs.ErrNotExist) {
		if fi, err := root.Lstat("artifacts"); err == nil && fi.IsDir() {
			if err := root.Rename("artifacts", publishedAside); err != nil {
				c.errs = append(c.errs, err)
				return
			}
		}
	}
	aside, err := root.OpenRoot(publishedAside)
	if err != nil {
		return // nothing published
	}
	defer aside.Close()
	_ = fs.WalkDir(aside.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if c.full {
			return fs.SkipAll
		}
		if err == nil && d.Type().IsRegular() {
			c.add(aside, rel, path.Join("/.lux/artifacts", rel))
		}
		return nil
	})
}

// clearPublished removes collected $LUX_ARTIFACTS, once reported.
func (p *placement) clearPublished(ctx context.Context) {
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return
	}
	if root, err := os.OpenRoot(rt); err == nil {
		_ = root.RemoveAll(publishedAside)
		root.Close()
	}
}

// artifactBlob stores one file as a blob (zstd, like every blob).
func (p *placement) artifactBlob(root *os.Root, rel, name string) (proto.Artifact, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return proto.Artifact{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return proto.Artifact{}, err
	}
	if !fi.Mode().IsRegular() {
		return proto.Artifact{}, errors.New("not a regular file")
	}
	if fi.Size() > maxArtifactBytes {
		return proto.Artifact{}, fmt.Errorf("%d bytes: over the %d-byte limit", fi.Size(), maxArtifactBytes)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = http.DetectContentType(head[:n])
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return proto.Artifact{}, err
	}
	// The file's own size and hash, as a download returns it.
	h := sha256.New()
	var size int64
	blobID := ids.New(ids.Blob)
	blobSize, blobSum, err := p.r.writeBlobLevel(blobID, compressionFor(ctype), func(w io.Writer) error {
		var err error
		size, err = io.Copy(io.MultiWriter(w, h), io.LimitReader(f, maxArtifactBytes))
		return err
	})
	if err != nil {
		return proto.Artifact{}, err
	}
	return proto.Artifact{
		BlobInfo: proto.BlobInfo{BlobID: blobID, Size: blobSize, SHA256: blobSum},
		Path:     name, ContentType: ctype,
		FileSize: size, FileSHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func splitPath(p string) []string { return strings.Split(strings.Trim(p, "/"), "/") }

// anyReaches: whether some pattern could match something under dir.
func anyReaches(patterns [][]string, dir []string) bool {
	return slices.ContainsFunc(patterns, func(ps []string) bool { return globPrefix(ps, dir) })
}

// globPrefix: whether the pattern could match paths under dir: dir's
// segments match the pattern's leading ones, or a ** is reached first.
func globPrefix(ps, dir []string) bool {
	for i, d := range dir {
		if i >= len(ps) {
			return false
		}
		if ps[i] == "**" {
			return true
		}
		if ok, _ := path.Match(ps[i], d); !ok {
			return false
		}
	}
	return true
}

// globMatch matches a path's segments against a pattern's, where *
// matches within a segment and ** any number of segments.
func globMatch(ps, ns []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			for i := 0; i <= len(ns); i++ {
				if globMatch(ps[1:], ns[i:]) {
					return true
				}
			}
			return false
		}
		if len(ns) == 0 {
			return false
		}
		if ok, _ := path.Match(ps[0], ns[0]); !ok {
			return false
		}
		ps, ns = ps[1:], ns[1:]
	}
	return len(ns) == 0
}

// compressionFor: already-compressed content is stored at the fastest
// level (zstd framing is still needed: every blob is zstd).
func compressionFor(ctype string) zstd.EncoderLevel {
	for _, c := range []string{"image/", "video/", "audio/", "application/zip", "application/gzip",
		"application/x-gzip", "application/zstd", "application/x-xz", "application/x-bzip2"} {
		if strings.HasPrefix(ctype, c) && ctype != "image/svg+xml" {
			return zstd.SpeedFastest
		}
	}
	return zstd.SpeedDefault
}
