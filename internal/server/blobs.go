package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/blob"
	"github.com/marcioapm/lux/internal/store"
)

// presignTTL bounds how long a download URL works: long enough to fetch a
// large snapshot, short enough that a leaked URL is soon useless.
const presignTTL = 15 * time.Minute

// serveBlobUpload is PUT /runner/v1/blobs/{id}: a runner uploading a blob it
// reported in snapshot.done. luxd streams it to S3, verifying its sha256 on
// the way; runners never hold S3 credentials.
func (s *Server) serveBlobUpload(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	hostName := r.URL.Query().Get("host")
	var tenantID, runID, want, location, hostID string
	var epoch int
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT b.tenant_id, b.run_id, b.epoch, b.sha256, b.location, coalesce(b.host_id, '')
			FROM blobs b JOIN hosts h ON h.id = b.host_id
			WHERE b.id = $1 AND h.token_id = $2 AND h.name = $3`, id, tok.ID, hostName).
			Scan(&tenantID, &runID, &epoch, &want, &location, &hostID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errf(http.StatusNotFound, "not_found", "no blob %s from this host", id)
	}
	if err != nil {
		return err
	}
	if location == "s3" {
		writeJSON(w, http.StatusOK, map[string]any{"uploaded": true})
		return nil
	}
	if location != "host" {
		return errf(http.StatusGone, "gone", "blob %s was deleted", id)
	}
	key := blob.Key(tenantID, runID, id)
	h := sha256.New()
	body := io.TeeReader(r.Body, h)
	if err := s.blobs.Put(r.Context(), key, body, r.ContentLength, want); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = s.blobs.Delete(context.WithoutCancel(r.Context()), key)
		return errf(http.StatusBadRequest, "checksum_mismatch", "sha256 %s, expected %s", got, want)
	}
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `UPDATE blobs SET location = 's3', s3_key = $2, uploaded_at = now()
			WHERE id = $1 AND location = 'host'`, id, key); err != nil {
			return err
		}
		// A snapshot is uploaded once all its volumes are.
		if _, err := tx.Exec(r.Context(), `UPDATE snapshots s SET uploaded = true
			WHERE s.run_id = $1 AND s.epoch = $2 AND NOT s.uploaded AND NOT EXISTS (
				SELECT 1 FROM jsonb_array_elements(coalesce(nullif(s.manifest->'volumes', 'null'), '[]')) v
				JOIN blobs b ON b.id = v->>'blobId' WHERE b.location <> 's3')`, runID, epoch); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `UPDATE placements p SET uploaded_at = now()
			WHERE p.run_id = $1 AND p.epoch = $2 AND p.uploaded_at IS NULL AND NOT EXISTS (
				SELECT 1 FROM blobs b WHERE b.run_id = $1 AND b.epoch = $2 AND b.location = 'host')`, runID, epoch)
		return err
	})
	if err != nil {
		return err
	}
	s.Kick() // a resume elsewhere may have been waiting for this upload
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": true})
	return nil
}

// serveRunnerBlobDownload is GET /runner/v1/blobs/{id}: a runner fetching a
// snapshot volume for a Run assigned to it. Redirects to a presigned URL.
func (s *Server) serveRunnerBlobDownload(w http.ResponseWriter, r *http.Request) error {
	tok, err := s.authHostToken(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var key, location string
	err = s.db.Tx(r.Context(), store.System(), func(tx pgx.Tx) error {
		// Only for a Run whose current placement is on the asking host.
		return tx.QueryRow(r.Context(), `SELECT coalesce(b.s3_key, ''), b.location FROM blobs b
			JOIN runs rn ON rn.id = b.run_id
			JOIN placements p ON p.run_id = rn.id AND p.epoch = rn.current_epoch
			JOIN hosts h ON h.id = p.host_id
			WHERE b.id = $1 AND h.token_id = $2 AND h.name = $3`, id, tok.ID, r.URL.Query().Get("host")).Scan(&key, &location)
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
	Path        string    `json:"path"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Available   bool      `json:"available"`
	CreatedAt   time.Time `json:"createdAt"`
}

type listArtifactsOutput struct {
	Body struct {
		Artifacts []Artifact `json:"artifacts"`
	} `nameHint:"ArtifactList"`
}

func (s *Server) listArtifacts(ctx context.Context, in *RunPath) (*listArtifactsOutput, error) {
	p := principal(ctx)
	out := []Artifact{}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := requireRun(ctx, tx, in.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.id, a.epoch, a.path, a.content_type, a.size, a.sha256, b.location = 's3', a.created_at
			FROM artifacts a JOIN blobs b ON b.id = a.blob_id WHERE a.run_id = $1 ORDER BY a.epoch, a.path`, in.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Artifact
			if err := rows.Scan(&a.ID, &a.Epoch, &a.Path, &a.ContentType, &a.Size, &a.SHA256, &a.Available, &a.CreatedAt); err != nil {
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
		return errf(http.StatusGone, "gone", "artifact was deleted (retention)")
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
