package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// A Run's diff: per repository, from its base (the commit it was cloned
// at, or its HEAD) to its working tree. Computed live in the Run's
// container while it runs; otherwise read from the latest snapshot that
// has one (every snapshot stores both kinds).

type diffInput struct {
	RunPath
	Repo   string `query:"repo" doc:"Only this repository."`
	Base   string `query:"base" enum:"clone,head" doc:"clone (default): from the commit each repository was cloned at. head: from its HEAD (uncommitted work only)."`
	Stat   string `query:"stat" doc:"true for only the stats, no patches." example:"true"`
	Accept string `header:"Accept" doc:"text/x-diff for the patches alone, concatenated."`
}

// RepoDiff is one repository's diff.
type RepoDiff struct {
	Repo       string           `json:"repo"`
	Push       bool             `json:"push" doc:"Whether lux push pushes it (false: push: false in the spec)."`
	Base       string           `json:"base" doc:"The commit diffed from."`
	Head       string           `json:"head" doc:"The checkout's HEAD commit."`
	Source     string           `json:"source" enum:"live,snapshot"`
	SnapshotID string           `json:"snapshotId,omitempty" doc:"With source snapshot: the snapshot it is from."`
	At         time.Time        `json:"at" doc:"When it was computed: now (live), or the snapshot's time."`
	Truncated  bool             `json:"truncated" doc:"The patch was cut at the limit (10 MiB); the stat is still the whole diff's."`
	Files      int              `json:"files"`
	Insertions int              `json:"insertions"`
	Deletions  int              `json:"deletions"`
	FileStats  []proto.DiffFile `json:"fileStats"`
	Patch      string           `json:"patch,omitempty" doc:"The patch (git diff format), when it is valid UTF-8."`
	// PatchBase64 carries a patch that is not valid UTF-8 (a text file in
	// another encoding), which a JSON string would change.
	PatchBase64    []byte   `json:"patchBase64,omitempty" doc:"The patch, base64, when it is not valid UTF-8 (patch is then absent)."`
	FiltersIgnored bool     `json:"filtersIgnored,omitempty" doc:"Some changed files have a clean/smudge filter attribute; filters are not run, so they are compared raw and may show as changed when they are not."`
	FilteredPaths  []string `json:"filteredPaths,omitempty" doc:"Those files (at most 100)."`
	Error          string   `json:"error,omitempty" doc:"Why this repository's diff could not be computed."`
	ErrorCode      string   `json:"errorCode,omitempty" enum:"base_unreachable" doc:"base_unreachable: the commit it was cloned at is no longer in its history (base=head still works)."`
}

// RunDiff is a Run's diff, per repository.
type RunDiff struct {
	Base  string     `json:"base" enum:"clone,head"`
	Repos []RepoDiff `json:"repos"`
}

// serveDiff is GET /v1/runs/{id}/diff.
func (s *Server) serveDiff(w http.ResponseWriter, r *http.Request, in *diffInput) error {
	ctx := r.Context()
	p := principal(ctx)
	kind := in.Base
	if kind == "" {
		kind = proto.DiffBaseClone
	}
	if kind != proto.DiffBaseClone && kind != proto.DiffBaseHead {
		return errf(http.StatusBadRequest, "bad_request", "base: want clone or head")
	}
	statOnly := in.Stat == "true"
	asPatch := acceptsDiff(in.Accept)
	if asPatch && statOnly {
		return errf(http.StatusBadRequest, "bad_request", "stat=true has no patch: ask for application/json")
	}
	t, err := s.diffTarget(ctx, p.TenantID, in.ID, in.Repo)
	if err != nil {
		return err
	}
	var repos []RepoDiff
	if t.live {
		repos, err = s.liveDiff(ctx, t, kind, statOnly)
		if errors.Is(err, errNotRunning) {
			t.live = false
		} else if err != nil {
			return err
		}
	}
	if !t.live {
		if repos, err = s.snapshotDiff(ctx, p.TenantID, t, kind, statOnly); err != nil {
			return err
		}
	}
	if asPatch {
		return writePatches(w, repos)
	}
	writeJSON(w, http.StatusOK, RunDiff{Base: kind, Repos: repos})
	return nil
}

func acceptsDiff(accept string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		if mt, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mt == "text/x-diff" {
			return true
		}
	}
	return false
}

// writePatches writes the patches alone, one after the other. Repositories
// whose diff failed are named in X-Lux-Diff-Errors.
func writePatches(w http.ResponseWriter, repos []RepoDiff) error {
	var failed []string
	for _, d := range repos {
		if d.Error != "" {
			failed = append(failed, d.Repo)
		}
	}
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	if len(failed) > 0 {
		w.Header().Set("X-Lux-Diff-Errors", strings.Join(failed, ","))
	}
	if len(repos) > 0 {
		w.Header().Set("X-Lux-Diff-Source", repos[0].Source)
		if repos[0].SnapshotID != "" {
			w.Header().Set("X-Lux-Snapshot-Id", repos[0].SnapshotID)
		}
	}
	w.WriteHeader(http.StatusOK)
	for _, d := range repos {
		if d.PatchBase64 != nil {
			_, _ = w.Write(d.PatchBase64)
		} else {
			_, _ = io.WriteString(w, d.Patch)
		}
	}
	return nil
}

type diffTarget struct {
	runID  string
	state  string
	hostID string
	epoch  int
	live   bool
	repos  []spec.Repository
	bases  map[string]string
}

var errNotRunning = errors.New("the Run's container is not running")

// diffTarget reads what a diff of the Run needs: its repositories (one, if
// named) and where its container is.
func (s *Server) diffTarget(ctx context.Context, tenantID, runID, repo string) (diffTarget, error) {
	t := diffTarget{runID: runID}
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var sp spec.RunSpec
		var plState string
		if err := tx.QueryRow(ctx, `SELECT r.state, r.spec, r.current_epoch, coalesce(p.host_id, ''), coalesce(p.state, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, runID).Scan(&t.state, &sp, &t.epoch, &t.hostID, &plState); err != nil {
			return err
		}
		if sp.Git != nil {
			t.repos = sp.Git.Repositories
		}
		if repo != "" {
			t.repos = slices.DeleteFunc(slices.Clone(t.repos), func(r spec.Repository) bool { return r.Name != repo })
			if len(t.repos) == 0 {
				return errf(http.StatusNotFound, "not_found", "the Run has no repository %q", repo)
			}
		}
		if len(t.repos) == 0 {
			return errf(http.StatusNotFound, "no_diff", "the Run has no repositories")
		}
		t.live = slices.Contains([]string{"starting", "running", "stopping"}, plState)
		var err error
		t.bases, err = gitBases(ctx, tx, runID)
		return err
	})
	return t, err
}

// gitBases are the commits a Run's repositories were cloned at: each
// repository's latest successful git.clone (a resume clones only the
// repositories it adds, so the others keep their first).
func gitBases(ctx context.Context, tx pgx.Tx, runID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (data->>'repo') data->>'repo', coalesce(data->>'commit', '')
		FROM run_events WHERE run_id = $1 AND type = $2 AND data->>'status' = 'cloned'
		ORDER BY data->>'repo', id DESC`, runID, proto.EvGitClone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var repo, commit string
		if err := rows.Scan(&repo, &commit); err != nil {
			return nil, err
		}
		out[repo] = commit
	}
	if len(out) == 0 {
		return nil, rows.Err()
	}
	return out, rows.Err()
}

// liveDiff asks the Run's runner to compute the diff in its container.
// errNotRunning: the container is not running (a snapshot has the diff).
func (s *Server) liveDiff(ctx context.Context, t diffTarget, kind string, statOnly bool) ([]RepoDiff, error) {
	if !s.hub.Streaming(t.hostID) {
		if t.state == StateRunning {
			return nil, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
		}
		return nil, errNotRunning
	}
	req := proto.DiffRequest{SubID: ids.New("diff"), Kind: kind, StatOnly: statOnly}
	for _, r := range t.repos {
		req.Repos = append(req.Repos, proto.DiffRepo{Name: r.Name, Path: r.Path, Base: t.bases[r.Name]})
	}
	ch, cancel := s.hub.Subscribe(req.SubID)
	defer cancel()
	if err := s.hub.SendLive(t.hostID, proto.Frame{Type: proto.MsgDiffRequest, RunID: t.runID, Epoch: t.epoch, Data: proto.Marshal(req)}); err != nil {
		return nil, errf(http.StatusServiceUnavailable, "host_unreachable", "%v", err)
	}
	// The runner gives the diff a minute; a little more for the relay.
	deadline := time.NewTimer(75 * time.Second)
	defer deadline.Stop()
	at := time.Now().UTC()
	got := map[string]RepoDiff{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.hub.Gone(t.hostID):
			return nil, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host disconnected")
		case <-deadline.C:
			return nil, errf(http.StatusGatewayTimeout, "diff_timeout", "the Run's host did not answer in time")
		case f, ok := <-ch:
			if !ok {
				return nil, errf(http.StatusBadGateway, "diff_failed", "the diff stream from the host was dropped")
			}
			switch f.Type {
			case proto.MsgDiffResult:
				var res proto.DiffResult
				if err := json.Unmarshal(f.Data, &res); err != nil {
					return nil, err
				}
				d := repoDiff(res.Stat, res.Patch)
				d.Source, d.At = "live", at
				got[d.Repo] = d
			case proto.MsgDiffEnd:
				var end proto.DiffEnd
				_ = json.Unmarshal(f.Data, &end)
				switch {
				case end.NotRunning:
					return nil, errNotRunning
				case end.Error != "":
					return nil, errf(http.StatusBadGateway, "diff_failed", "computing the diff in the Run's container: %s", end.Error)
				}
				return ordered(t, got, "live", at), nil
			}
		}
	}
}

// ordered lists the diffs in the spec's order; a repository the source
// had nothing for says so.
func ordered(t diffTarget, got map[string]RepoDiff, source string, at time.Time) []RepoDiff {
	out := []RepoDiff{}
	for _, r := range t.repos {
		d, ok := got[r.Name]
		if !ok {
			d = RepoDiff{Repo: r.Name, Source: source, At: at, Error: "no diff was computed for this repository"}
		}
		d.Push = r.Pushed()
		if d.FileStats == nil {
			d.FileStats = []proto.DiffFile{}
		}
		out = append(out, d)
	}
	return out
}

func repoDiff(st proto.DiffStat, patch []byte) RepoDiff {
	d := RepoDiff{Repo: st.Repo, Base: st.Base, Head: st.Head, Truncated: st.Truncated,
		Files: st.Files, Insertions: st.Insertions, Deletions: st.Deletions, FileStats: st.FileStats,
		FiltersIgnored: st.FiltersIgnored, FilteredPaths: st.FilteredPaths, Error: st.Error, ErrorCode: st.ErrorCode}
	setPatch(&d, patch)
	return d
}

func setPatch(d *RepoDiff, patch []byte) {
	if utf8.Valid(patch) {
		d.Patch = string(patch)
	} else {
		d.PatchBase64 = patch
	}
}

// snapshotDiff reads the diff stored with the latest snapshot that has one.
func (s *Server) snapshotDiff(ctx context.Context, tenantID string, t diffTarget, kind string, statOnly bool) ([]RepoDiff, error) {
	type row struct {
		d                  RepoDiff
		key, location, sum string
		size               int64
	}
	var rows []row
	var snapID string
	var at time.Time
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT s.id, s.created_at FROM snapshots s
			WHERE s.run_id = $1 AND EXISTS (SELECT 1 FROM snapshot_diffs d WHERE d.snapshot_id = s.id)
			ORDER BY s.epoch DESC, s.created_at DESC LIMIT 1`, t.runID).Scan(&snapID, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(http.StatusNotFound, "no_diff", "no snapshot of this Run has a diff (it has not stopped since it started, or its snapshots predate diffs)")
		}
		if err != nil {
			return err
		}
		q, err := tx.Query(ctx, `SELECT d.repo, d.base, d.head, d.truncated, d.files, d.insertions, d.deletions, d.file_stats,
				d.filters_ignored, d.filtered_paths, d.error, d.error_code,
				coalesce(b.s3_key, ''), coalesce(b.location, ''), d.size, d.sha256
			FROM snapshot_diffs d LEFT JOIN blobs b ON b.id = d.blob_id
			WHERE d.snapshot_id = $1 AND d.kind = $2`, snapID, kind)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, func(r pgx.CollectableRow) (row, error) {
			var x row
			err := r.Scan(&x.d.Repo, &x.d.Base, &x.d.Head, &x.d.Truncated, &x.d.Files, &x.d.Insertions, &x.d.Deletions,
				&x.d.FileStats, &x.d.FiltersIgnored, &x.d.FilteredPaths, &x.d.Error, &x.d.ErrorCode, &x.key, &x.location, &x.size, &x.sum)
			return x, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	got := map[string]RepoDiff{}
	for _, x := range rows {
		x.d.Source, x.d.SnapshotID, x.d.At = "snapshot", snapID, at.UTC()
		if !statOnly && x.d.Error == "" && x.location != "" {
			patch, err := s.readDiffBlob(ctx, x.key, x.location, x.size)
			if err != nil {
				return nil, err
			}
			setPatch(&x.d, patch)
		}
		got[x.d.Repo] = x.d
	}
	out := ordered(t, got, "snapshot", at.UTC())
	for i := range out {
		if out[i].SnapshotID == "" {
			out[i].SnapshotID = snapID
		}
	}
	return out, nil
}

// readDiffBlob reads a stored patch (zstd in S3), at most size bytes.
func (s *Server) readDiffBlob(ctx context.Context, key, location string, size int64) ([]byte, error) {
	switch location {
	case "s3":
	case "host":
		return nil, errf(http.StatusConflict, "not_uploaded", "the diff is still being uploaded from its host")
	default:
		return nil, errf(http.StatusGone, "gone", "the diff was deleted (retention)")
	}
	body, _, err := s.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	zr, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	b, err := io.ReadAll(io.LimitReader(zr, max(size, 0)+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size {
		return nil, fmt.Errorf("diff blob %s: %d bytes, expected %d", key, len(b), size)
	}
	return b, nil
}

// recordSnapshotDiffs stores a snapshot's diffs: each patch a blob of its
// placement, each repository and kind a row.
func recordSnapshotDiffs(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, snapID string, diffs []proto.SnapshotDiff) error {
	for _, d := range diffs {
		if d.Kind != proto.DiffBaseClone && d.Kind != proto.DiffBaseHead {
			continue
		}
		var blobID *string
		if d.Blob != nil {
			if err := insertBlob(ctx, tx, tenantID, runID, epoch, hostID, d.Blob.BlobID, "diff", d.Repo, d.Blob.Size, d.Blob.SHA256); err != nil {
				return err
			}
			blobID = &d.Blob.BlobID
		}
		stats, filtered := d.FileStats, d.FilteredPaths
		if stats == nil {
			stats = []proto.DiffFile{}
		}
		if filtered == nil {
			filtered = []string{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO snapshot_diffs (tenant_id, run_id, snapshot_id, epoch, repo, kind, base, head, blob_id,
				size, sha256, truncated, files, insertions, deletions, file_stats, filters_ignored, filtered_paths, error, error_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20) ON CONFLICT DO NOTHING`,
			tenantID, runID, snapID, epoch, d.Repo, d.Kind, d.Base, d.Head, blobID,
			d.PatchBytes, d.PatchSHA256, d.Truncated, d.Files, d.Insertions, d.Deletions, stats,
			d.FiltersIgnored, filtered, d.Error, d.ErrorCode); err != nil {
			return err
		}
	}
	return nil
}
