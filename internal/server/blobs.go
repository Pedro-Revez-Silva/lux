package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// presignTTL bounds how long a download URL works: long enough to fetch a
// large snapshot, short enough that a leaked URL is soon useless.
const presignTTL = 15 * time.Minute

// uploadPresignTTL bounds an upload URL: every part of a blob is presigned
// at once and PUT in turn, so it must outlast the whole upload.
const uploadPresignTTL = time.Hour

const maxUploadParts = 10000

// runnerBlob is a blob as its uploading host sees it.
type runnerBlob struct {
	id, tenantID, runID, sha256, location, kind string
	epoch                                       int
	size                                        int64
}

func (b *runnerBlob) key() string { return blob.Key(b.tenantID, b.runID, b.id) }

// hostBlob is the blob {id} reported by the asking host: 404 for any other.
func (s *Server) hostBlob(r *http.Request) (*runnerBlob, error) {
	tok, err := s.authHostToken(r)
	if err != nil {
		return nil, err
	}
	b := &runnerBlob{id: r.PathValue("id")}
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT b.tenant_id, b.run_id, b.epoch, b.sha256, b.size, b.location, b.kind
			FROM blobs b JOIN hosts h ON h.id = b.host_id
			WHERE b.id = $1 AND h.token_id = $2 AND h.name = $3`, b.id, tok.ID, r.URL.Query().Get("host")).
			Scan(&b.tenantID, &b.runID, &b.epoch, &b.sha256, &b.size, &b.location, &b.kind)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errf(http.StatusNotFound, "not_found", "no blob %s from this host", b.id)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// uploadable: done for a blob S3 already has, an error (410) for a deleted
// one, neither for a blob still on its host.
func (b *runnerBlob) uploadable() (done bool, err error) {
	switch b.location {
	case "host":
		return false, nil
	case "s3":
		return true, nil
	}
	return false, errf(http.StatusGone, "gone", "blob %s was deleted", b.id)
}

// uploaded is every upload route's answer once S3 has the blob:
// {"uploaded":true}.
var uploaded = proto.BlobUploadResponse{Uploaded: true}

// serveBlobUpload is PUT /runner/v1/blobs/{id}: a runner uploading a blob it
// reported in snapshot.done through luxd, which streams it to S3, verifying
// its sha256 on the way. Runners that upload straight to S3 use
// serveBlobUploadStart instead; this stays for those that predate it.
func (s *Server) serveBlobUpload(w http.ResponseWriter, r *http.Request) error {
	b, err := s.hostBlob(r)
	if err != nil {
		return err
	}
	if done, err := b.uploadable(); done || err != nil {
		if done {
			writeJSON(w, http.StatusOK, uploaded)
		}
		return err
	}
	key := b.key()
	h := sha256.New()
	body := io.TeeReader(r.Body, h)
	if err := s.blobs.Put(r.Context(), key, body, r.ContentLength, b.sha256); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != b.sha256 {
		_ = s.blobs.Delete(context.WithoutCancel(r.Context()), key)
		return errf(http.StatusBadRequest, "checksum_mismatch", "sha256 %s, expected %s", got, b.sha256)
	}
	if err := s.blobUploaded(r.Context(), b); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, uploaded)
	return nil
}

// serveBlobUploadStart is POST /runner/v1/blobs/{id}/upload: presigned URLs
// for the runner to upload the blob straight to S3, one PutObject or one
// UploadPart per proto.BlobPartSize part, each with its SHA-256 signed in so
// S3 refuses any other bytes. The runner holds no S3 credentials.
func (s *Server) serveBlobUploadStart(w http.ResponseWriter, r *http.Request) error {
	b, err := s.hostBlob(r)
	if err != nil {
		return err
	}
	var in proto.BlobUploadRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if done, err := b.uploadable(); done || err != nil {
		if done {
			writeJSON(w, http.StatusOK, uploaded)
			return nil
		}
		s.dropDeletedUpload(r.Context(), b)
		return err
	}
	whole, err := hex.DecodeString(in.SHA256)
	if err != nil || len(whole) != sha256.Size || in.Size < 0 {
		return errf(http.StatusBadRequest, "bad_request", "sha256 must be a hex SHA-256 and size non-negative")
	}
	// Unconditional, as the streaming PUT: a row without a sha256 never
	// reaches s3.
	if in.SHA256 != b.sha256 {
		return errf(http.StatusBadRequest, "checksum_mismatch", "sha256 %s, expected %s", in.SHA256, b.sha256)
	}
	// Size 0 is also an empty blob's; the signed checksum pins its bytes.
	if b.size > 0 && in.Size != b.size {
		return errf(http.StatusBadRequest, "size_mismatch", "size %d, expected %d", in.Size, b.size)
	}
	// S3's own limit; also bounds the presigning a size-0 row would allow.
	if len(in.Parts) > maxUploadParts {
		return errf(http.StatusBadRequest, "bad_request", "%d parts, at most %d", len(in.Parts), maxUploadParts)
	}
	if n := proto.BlobParts(in.Size); len(in.Parts) != n {
		return errf(http.StatusBadRequest, "bad_request", "%d parts for %d bytes, expected %d", len(in.Parts), in.Size, n)
	}
	for i, p := range in.Parts {
		if d, err := base64.StdEncoding.DecodeString(p); err != nil || len(d) != sha256.Size {
			return errf(http.StatusBadRequest, "bad_request", "part %d: not a base64 SHA-256", i+1)
		}
	}
	key := b.key()
	if len(in.Parts) == 1 {
		wholeB64 := base64.StdEncoding.EncodeToString(whole)
		if in.Parts[0] != wholeB64 {
			return errf(http.StatusBadRequest, "checksum_mismatch", "a single part's sha256 must be the file's")
		}
		p, err := s.blobs.PresignPut(r.Context(), key, in.Size, in.SHA256, wholeB64, uploadPresignTTL)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, proto.BlobUploadResponse{Single: &p})
		return nil
	}
	uploadID, err := s.blobs.CreateMultipart(r.Context(), key, in.SHA256)
	if err != nil {
		return err
	}
	out := proto.BlobUploadResponse{UploadID: uploadID}
	for i, sum := range in.Parts {
		size := min(int64(proto.BlobPartSize), in.Size-int64(i)*proto.BlobPartSize)
		p, err := s.blobs.PresignUploadPart(r.Context(), key, uploadID, int32(i+1), size, sum, uploadPresignTTL)
		if err != nil {
			s.abortUpload(r.Context(), key, uploadID)
			return err
		}
		out.Parts = append(out.Parts, proto.PresignedPart{Number: int32(i + 1), PresignedRequest: p})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// serveBlobUploadComplete is POST /runner/v1/blobs/{id}/complete: the
// runner's direct upload is done. luxd completes a multipart upload, checks
// the object's length, and records the blob uploaded as serveBlobUpload
// does.
func (s *Server) serveBlobUploadComplete(w http.ResponseWriter, r *http.Request) error {
	b, err := s.hostBlob(r)
	if err != nil {
		return err
	}
	var in proto.BlobCompleteRequest
	if err := readJSON(r, &in); err != nil {
		return err
	}
	key := b.key()
	// The runner gave up on this upload id: drop its parts, whatever the
	// blob's state.
	if in.Abort {
		if in.UploadID == "" {
			return errf(http.StatusBadRequest, "bad_request", "abort needs an uploadId")
		}
		s.abortUpload(r.Context(), key, in.UploadID)
		writeJSON(w, http.StatusOK, map[string]any{"aborted": true})
		return nil
	}
	if done, err := b.uploadable(); done || err != nil {
		if in.UploadID != "" {
			s.abortUpload(r.Context(), key, in.UploadID)
		}
		if done {
			writeJSON(w, http.StatusOK, uploaded)
			return nil
		}
		s.dropDeletedUpload(r.Context(), b)
		return err
	}
	if in.UploadID != "" {
		parts := make([]blob.Part, len(in.Parts))
		for i, p := range in.Parts {
			parts[i] = blob.Part{Number: p.Number, ETag: p.ETag, SHA256: p.SHA256}
		}
		if err := s.blobs.CompleteMultipart(r.Context(), key, in.UploadID, parts); err != nil {
			// S3 refused the parts: they can never complete. A 5xx or
			// network error may be transient, or even have completed: the
			// runner retries, then aborts.
			if permanentCompleteError(err) {
				s.abortUpload(r.Context(), key, in.UploadID)
				return err
			}
			return errf(http.StatusBadGateway, "s3_unavailable", "complete multipart upload: %v", err)
		}
	}
	size, err := s.blobs.Size(r.Context(), key)
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return errf(http.StatusConflict, "not_uploaded", "blob %s is not in S3", b.id)
	}
	if err != nil {
		return err
	}
	// A size-0 row has no length to check: /upload's signed checksum
	// already pinned the bytes.
	if b.size > 0 && size != b.size {
		_ = s.blobs.Delete(context.WithoutCancel(r.Context()), key)
		return errf(http.StatusBadRequest, "size_mismatch", "%d bytes in S3, expected %d", size, b.size)
	}
	if err := s.blobUploaded(r.Context(), b); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, uploaded)
	return nil
}

// permanentCompleteError: S3 refused a CompleteMultipartUpload for good.
// By error code first: S3 can send such a refusal inside a 200 answer,
// which the SDK reports as a 500. Then any 4xx.
func permanentCompleteError(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "InvalidPart", "InvalidPartOrder", "EntityTooSmall", "BadDigest", "NoSuchUpload", "InvalidRequest":
			return true
		}
	}
	var se interface{ HTTPStatusCode() int }
	return errors.As(err, &se) && se.HTTPStatusCode()/100 == 4
}

// dropDeletedUpload deletes a deleted blob's object: a direct upload's PUT
// can reach S3 after deleteArtifacts claimed the blob on its host, so the
// claim had no key to delete.
func (s *Server) dropDeletedUpload(ctx context.Context, b *runnerBlob) {
	if b.location == "deleted" {
		s.deleteObjects(context.WithoutCancel(ctx), "direct upload of a deleted blob", []string{b.key()})
	}
}

// abortUpload drops a multipart upload's parts. Best effort: the bucket's
// lifecycle rule aborts incomplete uploads after 7 days.
func (s *Server) abortUpload(ctx context.Context, key, uploadID string) {
	if err := s.blobs.AbortMultipart(context.WithoutCancel(ctx), key, uploadID); err != nil {
		s.log.Warn("abort multipart upload", "key", key, "err", err)
	}
}

// blobUploaded records that b's object is in S3, after either upload
// route. A blob deleted while it uploaded is 410, its object deleted.
func (s *Server) blobUploaded(ctx context.Context, b *runnerBlob) error {
	key := b.key()
	var gone bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE blobs SET location = 's3', s3_key = $2, uploaded_at = now()
			WHERE id = $1 AND location = 'host'`, b.id, key)
		if err != nil {
			return err
		}
		// Deleted while it uploaded (deleteArtifacts claims blobs still on
		// their host): the object just written is nobody's.
		if tag.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT location = 'deleted' FROM blobs WHERE id = $1`, b.id).Scan(&gone); err != nil || gone {
				return err
			}
		}
		if err := markUploaded(ctx, tx, b.runID, b.epoch); err != nil {
			return err
		}
		// Only the upload that moved the blob to s3 adds the event, and
		// last: event streams come after every row lock.
		if tag.RowsAffected() == 0 || b.kind != "artifact" {
			return nil
		}
		return artifactUploaded(ctx, tx, b.id)
	})
	if err != nil {
		return err
	}
	if gone {
		s.deleteObjects(context.WithoutCancel(ctx), "upload of a deleted blob", []string{key})
		return errf(http.StatusGone, "gone", "blob %s was deleted", b.id)
	}
	s.Kick() // a resume elsewhere may have been waiting for this upload
	return nil
}

// markUploaded records what a placement has in S3 after one of its blobs
// arrived: its snapshot once every volume is a volume blob of the same Run
// and placement (epoch) in S3, the placement once it has reported its
// snapshot and none of its blobs is left on the host (a published
// artifact's upload while it runs is not the placement's).
func markUploaded(ctx context.Context, tx pgx.Tx, runID string, epoch int) error {
	if _, err := tx.Exec(ctx, `UPDATE snapshots s SET uploaded = true
		WHERE s.run_id = $1 AND s.epoch = $2 AND NOT s.uploaded AND NOT EXISTS (
			SELECT 1 FROM jsonb_array_elements(coalesce(nullif(s.manifest->'volumes', 'null'), '[]')) v
			WHERE NOT EXISTS (SELECT 1 FROM blobs b
				WHERE b.id = v->>'blobId' AND b.run_id = s.run_id AND b.epoch = s.epoch
				  AND b.kind = 'volume' AND b.location = 's3'))`, runID, epoch); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE placements p SET uploaded_at = now()
		WHERE p.run_id = $1 AND p.epoch = $2 AND p.uploaded_at IS NULL AND p.snapshot_done_at IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM blobs b WHERE b.run_id = $1 AND b.epoch = $2 AND b.location = 'host')`, runID, epoch)
	return err
}

// serveRunnerBlobDownload is GET /runner/v1/blobs/{id}: a runner fetching a
// snapshot volume for a Run assigned to it. Redirects to a presigned URL.
// Only a volume of the snapshot that Run restores, for its current
// placement on the asking host.
func (s *Server) serveRunnerBlobDownload(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var key, location string
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT coalesce(b.s3_key, ''), b.location FROM blobs b
			JOIN runs rn ON rn.id = b.run_id
			JOIN snapshots sn ON sn.id = rn.snapshot_id AND sn.run_id = rn.id
			JOIN placements p ON p.run_id = rn.id AND p.epoch = rn.current_epoch
			JOIN hosts h ON h.id = p.host_id
			WHERE b.id = $1 AND b.kind = 'volume' AND b.epoch = sn.epoch AND h.token_id = $2 AND h.name = $3
			  AND EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(nullif(sn.manifest->'volumes', 'null'), '[]')) v
				WHERE v->>'blobId' = b.id)`, id, tok.ID, r.URL.Query().Get("host")).Scan(&key, &location)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errf(http.StatusNotFound, "not_found", "no blob %s for a run assigned to this host", id)
	}
	if err != nil {
		return err
	}
	if location != "s3" {
		return errf(http.StatusConflict, "not_uploaded", "blob %s is not uploaded yet", id)
	}
	url, err := s.blobs.PresignGet(r.Context(), key, presignTTL)
	if err != nil {
		return err
	}
	http.Redirect(w, r, url, http.StatusFound)
	return nil
}

type Artifact struct {
	ID          string    `json:"id"`
	Epoch       int       `json:"epoch"`
	Path        string    `json:"path" doc:"Where the file was: its path on a volume, or /.lux/artifacts/<name> for one the Run published."`
	Version     int       `json:"version" doc:"1 for the first file at this path in the Run, then one more for each new one: none is replaced."`
	Description string    `json:"description" doc:"What the Run said the file is, when it published it."`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Available   bool      `json:"available"`
	CreatedAt   time.Time `json:"createdAt"`
}

type listArtifactsInput struct {
	RunPath
	Versions string `query:"versions" enum:"latest,all," doc:"latest (the default): each path's latest version; all: every version."`
}

type listArtifactsOutput struct {
	Body struct {
		Artifacts []Artifact `json:"artifacts"`
	} `nameHint:"ArtifactList"`
}

func (s *Server) listArtifacts(ctx context.Context, in *listArtifactsInput) (*listArtifactsOutput, error) {
	p := principal(ctx)
	out := []Artifact{}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.id, a.epoch, a.path, a.version, a.description, a.content_type, a.size, a.sha256, b.location = 's3', a.created_at
			FROM artifacts a JOIN blobs b ON b.id = a.blob_id
			WHERE a.run_id = $1 AND ($2 OR a.version = (SELECT max(l.version) FROM artifacts l WHERE l.run_id = a.run_id AND md5(l.path) = md5(a.path) AND l.path = a.path))
			ORDER BY a.path, a.version`, in.ID, in.Versions == "all")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Artifact
			if err := rows.Scan(&a.ID, &a.Epoch, &a.Path, &a.Version, &a.Description, &a.ContentType, &a.Size, &a.SHA256, &a.Available, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	res := &listArtifactsOutput{}
	res.Body.Artifacts = out
	return res, nil
}

type artifactPath struct {
	AID string `path:"aid" doc:"The artifact's id."`
}

type deleteArtifactsOutput struct {
	Body struct {
		Deleted int `json:"deleted" doc:"How many artifacts this request deleted: 0 when none were left."`
	} `nameHint:"ArtifactsDeleted"`
}

// deleteArtifacts deletes every artifact of a terminated Run:
// retention never does. The claim is reapRetention's: the Run locked and
// its state checked, the blobs marked deleted in one transaction, the S3
// objects deleted after commit. Blobs still on their host are claimed too,
// and their keys deleted (a direct PUT may already be in S3);
// serveBlobUploadStart and serveBlobUploadComplete drop an object that
// arrives later.
func (s *Server) deleteArtifacts(ctx context.Context, in *RunPath) (*deleteArtifactsOutput, error) {
	p := principal(ctx)
	var keys []string
	var deleted int
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1 FOR UPDATE`, in.ID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		if !terminal(state) {
			return errf(http.StatusConflict, "not_terminal", "run is %s: only a terminated Run's artifacts can be deleted", state)
		}
		// A blob still on its host may have its object in S3 already (a
		// direct PUT not yet /complete'd, its runner gone): its key is
		// deterministic, and deleting an absent key is harmless.
		// o is the row before this UPDATE: its location.
		rows, err := tx.Query(ctx, `UPDATE blobs b SET location = 'deleted', deleted_at = now()
			FROM artifacts a, blobs o WHERE a.run_id = $1 AND b.id = a.blob_id AND o.id = b.id AND b.location <> 'deleted'
			RETURNING b.id, b.tenant_id, b.run_id, coalesce(b.s3_key, ''), o.location`, in.ID)
		if err != nil {
			return err
		}
		type claim struct{ id, tenantID, runID, key, location string }
		claimed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claim, error) {
			var c claim
			return c, r.Scan(&c.id, &c.tenantID, &c.runID, &c.key, &c.location)
		})
		if err != nil {
			return err
		}
		deleted = len(claimed)
		for _, c := range claimed {
			switch {
			case c.key != "":
				keys = append(keys, c.key)
			case c.location == "host":
				keys = append(keys, blob.Key(c.tenantID, c.runID, c.id))
			}
		}
		if deleted == 0 {
			return nil
		}
		return addEvent(ctx, tx, p.TenantID, in.ID, 0, "artifacts.deleted", map[string]any{"by": p.Actor(), "count": deleted})
	})
	if err != nil {
		return nil, err
	}
	s.deleteObjects(context.WithoutCancel(ctx), "artifact delete", keys)
	out := &deleteArtifactsOutput{}
	out.Body.Deleted = deleted
	return out, nil
}

// downloadArtifact streams an artifact through luxd, as the file the Run
// wrote. (Blobs are stored zstd-compressed, so a presigned URL would hand
// out the compressed bytes; luxd decompresses on the way.)
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request, in *artifactPath) error {
	p := principal(r.Context())
	var key, location, ctype, name, sum string
	var size int64
	err := s.db.Tx(r.Context(), store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT coalesce(b.s3_key, ''), b.location, a.content_type, a.path, a.size, a.sha256
			FROM artifacts a JOIN blobs b ON b.id = a.blob_id WHERE a.id = $1`, in.AID).Scan(&key, &location, &ctype, &name, &size, &sum)
	})
	if err != nil {
		return err
	}
	switch location {
	case "s3":
	case "host":
		w.Header().Set("Retry-After", "2")
		return errf(http.StatusConflict, "not_uploaded", "artifact is still being uploaded from its host")
	default:
		return errf(http.StatusGone, "gone", "artifact was deleted")
	}
	body, _, err := s.blobs.Get(r.Context(), key)
	if err != nil {
		return err
	}
	defer body.Close()
	zr, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	// The file's length and hash: a download cut short (an S3 or decode
	// error once the body started) is detectable by the client.
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Lux-SHA256", sum)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	if _, err := io.Copy(w, zr); err != nil {
		s.log.Warn("artifact download cut short", "artifact", in.AID, "err", err)
		panic(http.ErrAbortHandler) // abort the response: never a clean end
	}
	return nil
}
