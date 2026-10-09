package server

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
)

// Artifacts are versioned per (run, path): each new file under a path is
// the next version, numbered while the caller holds the Run's row lock
// (applyReport's FOR UPDATE), so two reports cannot take the same number.
// A published file is always a new version; one collected at exit
// (artifacts.paths) only when its content differs from the latest.

// latestArtifact is the file sha256 of path's latest version ("" if it
// has none) and that version (0 if none).
func latestArtifact(ctx context.Context, tx pgx.Tx, runID, path string) (sha string, version int, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce((array_agg(sha256 ORDER BY version DESC))[1], ''), coalesce(max(version), 0)
		FROM artifacts WHERE run_id = $1 AND md5(path) = md5($2) AND path = $2`, runID, path).Scan(&sha, &version)
	return sha, version, err
}

// insertArtifact records an artifact (its blob already recorded) as the
// next version of its path.
func insertArtifact(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, id, snapshotID, description string, a proto.Artifact) error {
	_, version, err := latestArtifact(ctx, tx, runID, a.Path)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO artifacts (id, tenant_id, run_id, epoch, path, blob_id, content_type, size, sha256, snapshot_id, version, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), $11, $12)`,
		id, tenantID, runID, epoch, a.Path, a.BlobID, a.ContentType, a.FileSize, a.FileSHA256, snapshotID, version+1, description)
	return err
}

// validPublished: a report luxd can record. Anything else would fail the
// insert on every redelivery (Postgres text and jsonb refuse NUL), so it
// is refused instead.
func validPublished(ap proto.ArtifactPublished) bool {
	name, ok := strings.CutPrefix(ap.Path, proto.PublishedPrefix)
	return ok && proto.ValidArtifactID(ap.ID) && proto.ValidPublishFields(name, ap.ContentType, ap.Description) == nil &&
		storableText(ap.BlobID, ap.SHA256, ap.FileSHA256)
}

// storableText: no NUL, which encoding/json decodes from \u0000 and
// Postgres text cannot hold, and short enough for a btree key (blobs.id).
// JSON decoding already replaces invalid UTF-8.
func storableText(ss ...string) bool {
	for _, s := range ss {
		if len(s) > 255 || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}

// applyArtifactPublished records a published artifact under the id the
// shim gave it. The same report again (a runner that restarted before it
// had the ack) is accepted as it is; any other report naming a recorded
// id or blob is refused.
func applyArtifactPublished(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, ap proto.ArtifactPublished) (refused bool, err error) {
	if !validPublished(ap) {
		return true, addEvent(ctx, tx, tenantID, runID, epoch, "artifacts.failed", map[string]any{"error": "a malformed published artifact was refused"})
	}
	var gotRun, gotBlob, gotSHA string
	err = tx.QueryRow(ctx, `SELECT run_id, blob_id, sha256 FROM artifacts WHERE id = $1`, ap.ID).Scan(&gotRun, &gotBlob, &gotSHA)
	switch {
	case err == nil && gotRun == runID && gotBlob == ap.BlobID && gotSHA == ap.FileSHA256:
		return false, nil
	case err == nil:
		return true, refusedReuse(ctx, tx, tenantID, runID, epoch, ap)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	// A savepoint: a blob id recorded as something else refuses the
	// report whole.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	err = insertBlob(ctx, sp, tenantID, runID, epoch, hostID, ap.BlobID, "artifact", ap.Path, ap.Size, ap.SHA256, "")
	var foreign *foreignBlobError
	if errors.As(err, &foreign) {
		if err := sp.Rollback(ctx); err != nil {
			return false, err
		}
		return true, refusedReuse(ctx, tx, tenantID, runID, epoch, ap)
	}
	if err != nil {
		return false, err
	}
	if err := insertArtifact(ctx, sp, tenantID, runID, epoch, ap.ID, "", ap.Description, ap.Artifact); err != nil {
		return false, err
	}
	return false, sp.Commit(ctx)
}

func refusedReuse(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, ap proto.ArtifactPublished) error {
	return addEvent(ctx, tx, tenantID, runID, epoch, "artifacts.failed", map[string]any{
		"error": artifactName(ap.Path) + ": refused, its artifact id or blob is already recorded as another"})
}

// artifactUploaded adds artifact.published for the artifact whose blob just
// reached S3: the moment it can be downloaded. Once per blob, as the
// blob's move to s3 is.
func artifactUploaded(ctx context.Context, tx pgx.Tx, blobID string) error {
	var tenantID, runID, id, path, ctype, sha, description string
	var epoch, version int
	var size int64
	err := tx.QueryRow(ctx, `SELECT tenant_id, run_id, epoch, id, path, version, description, size, sha256, content_type
		FROM artifacts WHERE blob_id = $1`, blobID).Scan(&tenantID, &runID, &epoch, &id, &path, &version, &description, &size, &sha, &ctype)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not an artifact's blob
	}
	if err != nil {
		return err
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, "artifact.published", map[string]any{
		"artifactId": id, "path": path, "name": artifactName(path), "version": version, "description": description,
		"size": size, "sha256": sha, "contentType": ctype})
}

// artifactName is a published artifact's name; any other's path.
func artifactName(path string) string {
	if n, ok := strings.CutPrefix(path, proto.PublishedPrefix); ok {
		return n
	}
	return path
}
