package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// Published artifacts: `lux-shim publish` stages a copy on the runtime
// volume and records lux.artifact; tailEvents hands each record here.
// The shim's artifact id is the key end to end: the record (named by it)
// says what was done, so a record seen again after a runner restart (the
// output file is re-read from its start) is reported at most once more
// and stored never again. A host lost before the report loses the file.

// maxPublished caps the publishes of one placement.
const maxPublished = 1000

// stagingDir is the shim's ArtifactsDir relative to the runtime volume.
const stagingDir = "artifacts"

var errTooManyPublished = fmt.Errorf("more than %d published artifacts: the rest were dropped", maxPublished)

// publishedBlobID: one blob id per artifact, so storing it again after a
// crash before its record was saved replaces the same file.
func publishedBlobID(artifactID string) string {
	return ids.Blob + "_" + strings.TrimPrefix(artifactID, ids.Artifact+"_")
}

func (p *placement) onPublished(ctx context.Context, a proto.StagedArtifact) {
	if !proto.ValidArtifactID(a.ID) || proto.ValidPublishFields(a.Name, a.ContentType, a.Description) != nil {
		p.logf("ignoring a malformed lux.artifact record", "id", a.ID)
		return
	}
	if p.published == nil {
		p.published = map[string]bool{}
	}
	if p.published[a.ID] {
		return
	}
	full := len(p.published) >= maxPublished
	if !full {
		p.published[a.ID] = true
	}
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		p.logf("published artifact: runtime volume", "id", a.ID, "err", err)
		return
	}
	root, err := os.OpenRoot(rt)
	if err != nil {
		p.logf("published artifact: runtime volume", "id", a.ID, "err", err)
		return
	}
	defer root.Close()
	staged := path.Join(stagingDir, a.ID)
	if full {
		// Reported once: a record seen again finds its file gone.
		if err := root.Remove(staged); err == nil {
			p.event(ctx, "artifacts.failed", map[string]any{"error": fmt.Sprintf("%s: %v", a.Name, errTooManyPublished)})
		}
		return
	}
	rec, err := p.r.readRecord(a.ID)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.logf("published artifact: record", "id", a.ID, "err", err)
		return
	}
	// Records are host-wide: one of another Run (an id forged by this
	// Run's workload) or a snapshot's is not this publish's.
	if rec != nil && (rec.RunID != p.runID || rec.Published == nil) {
		p.logf("ignoring a lux.artifact record naming another record", "id", a.ID)
		return
	}
	if rec == nil {
		if rec, err = p.storePublished(root, staged, a); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				p.logf("published artifact not stored", "id", a.ID, "err", err)
				p.event(ctx, "artifacts.failed", map[string]any{"error": fmt.Sprintf("%s: %v", a.Name, err)})
				_ = root.Remove(staged)
			}
			// Gone without a record: refused by luxd, or never stored.
			return
		}
	}
	// The blob is on the host now; the staged copy is not needed.
	if err := root.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.logf("published artifact: removing its staged copy", "id", a.ID, "err", err)
	}
	if rec.Reported || rec.Published == nil {
		return
	}
	for {
		ack, err := p.reportAck(ctx, proto.MsgArtifactPublished, rec.Published)
		if err == nil {
			p.r.publishedAcked(a.ID, ack)
			p.r.uploads.kick()
			return
		}
		if p.isStale() || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// storePublished writes the staged file as a blob and saves its record.
func (p *placement) storePublished(root *os.Root, staged string, a proto.StagedArtifact) (*snapshotRecord, error) {
	blobID := publishedBlobID(a.ID)
	art, err := p.artifactBlob(blobID, root, staged, proto.PublishedPrefix+a.Name, a.ContentType)
	if err != nil {
		return nil, err
	}
	if art.FileSize != a.Size || art.FileSHA256 != a.SHA256 {
		os.Remove(p.r.blobPath(blobID))
		return nil, fmt.Errorf("the staged copy is not what was published (%d bytes, sha256 %s)", art.FileSize, art.FileSHA256)
	}
	rec := &snapshotRecord{RunID: p.runID, Epoch: p.epoch, Created: time.Now().UnixMilli(),
		Uploads:   []pendingUpload{{BlobID: blobID, Path: p.r.blobPath(blobID), Size: art.Size}},
		Published: &proto.ArtifactPublished{ID: a.ID, Description: a.Description, Artifact: art}}
	if err := p.r.saveSnapshotRecord(a.ID, rec); err != nil {
		os.Remove(p.r.blobPath(blobID))
		return nil, err
	}
	return rec, nil
}

// publishedAcked follows luxd's ack of a published artifact: its blob is
// uploaded, or, refused, its files are deleted.
func (r *Runner) publishedAcked(id string, ack proto.Ack) {
	if !ack.Refused {
		r.updateRecord(id, func(rec *snapshotRecord) { rec.Reported = true })
		return
	}
	r.log.Warn("luxd refused the published artifact; deleting its files", "artifact", id)
	if rec, err := r.readRecord(id); err == nil {
		removeSnapshotFiles(r, id, rec)
	}
}
