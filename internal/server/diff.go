package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// container while it runs; otherwise read from its latest snapshot, whose
// diff state (snapshot_diff_state) decides the answer: an older
// snapshot's diff is given only when asked for by id.

type diffInput struct {
	RunPath
	Snapshot string `query:"snapshot" doc:"This snapshot's diff, not the latest one's (nor a live one)."`
	Repo     string `query:"repo" doc:"Only this repository."`
	Base     string `query:"base" enum:"clone,head" doc:"clone (default): from the commit each repository was cloned at. head: from its HEAD (uncommitted work only)."`
	Stat     string `query:"stat" doc:"true for only the stats, no patches." example:"true"`
	Accept   string `header:"Accept" doc:"text/x-diff for the patches alone, concatenated."`
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
	// What the patch cannot carry.
	NormalizedPaths []string `json:"normalizedPaths,omitempty" doc:"Files git converts before comparing them (text, eol, core.autocrlf, working-tree-encoding): the patch has git's stored form, and a change of line endings alone does not show (at most 100)."`
	DirtySubmodules []string `json:"dirtySubmodules,omitempty" doc:"Submodules with uncommitted changes or untracked files of their own, which the patch does not carry (only a changed commit of a submodule is in it; at most 100)."`
	Error           string   `json:"error,omitempty" doc:"Why this repository's diff could not be computed."`
	ErrorCode       string   `json:"errorCode,omitempty" enum:"base_unreachable" doc:"base_unreachable: the commit it was cloned at is no longer in its history (base=head still works)."`
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
	if t.live && in.Snapshot == "" {
		repos, err = s.liveDiff(ctx, t, kind, statOnly)
		if errors.Is(err, errNotRunning) {
			t.live = false
		} else if err != nil {
			return err
		}
	}
	if !t.live || in.Snapshot != "" {
		if repos, err = s.snapshotDiff(ctx, p.TenantID, t, in.Snapshot, kind, statOnly); err != nil {
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
	if !s.hub.Can(t.hostID, proto.CapDiff) {
		return nil, errf(http.StatusServiceUnavailable, "diff_unsupported", "the Run's host runs a lux-runner without diffs; its diff is available once the host is upgraded")
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
	// Gone before the end (the client left, or the deadline): the runner
	// stops the diff (for this request; a shared one when its last goes).
	ended := false
	defer func() {
		if !ended {
			_ = s.hub.SendLive(t.hostID, proto.Frame{Type: proto.MsgDiffCancel, RunID: t.runID, Epoch: t.epoch,
				Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID})})
		}
	}()
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
				ended = true
				var end proto.DiffEnd
				_ = json.Unmarshal(f.Data, &end)
				switch {
				case end.NotRunning:
					return nil, errNotRunning
				case end.Busy:
					return nil, errf(http.StatusTooManyRequests, "diff_busy", "another live diff of this Run is under way (a different base, repository or stat); retry when it is done")
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
		FiltersIgnored: st.FiltersIgnored, FilteredPaths: st.FilteredPaths, NormalizedPaths: st.NormalizedPaths,
		DirtySubmodules: st.DirtySubmodules, Error: st.Error, ErrorCode: st.ErrorCode}
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

// Snapshot diff states (snapshot_diff_state).
const (
	diffPending     = "pending"
	diffComplete    = "complete"
	diffSkipped     = "skipped"
	diffFailed      = "failed"
	diffUnsupported = "unsupported"
)

const (
	// diffRetryAfter is what a diff_pending answer says to wait.
	diffRetryAfter = 5
	// diffReportLostAfter: a snapshot still pending this long has lost its
	// report (the runner gives up after 10 minutes); the sweep fails it.
	diffReportLostAfter = 15 * time.Minute
	// diffReportLost is the reason it is failed with.
	diffReportLost = "report_lost"
)

// snapshotDiff reads a snapshot's stored diff: snapID's, or the Run's
// latest snapshot's. That snapshot alone answers, whatever its state: an
// older one's diff would be stale.
func (s *Server) snapshotDiff(ctx context.Context, tenantID string, t diffTarget, snapID, kind string, statOnly bool) ([]RepoDiff, error) {
	type row struct {
		d                  RepoDiff
		key, location, sum string
		size               int64
		foreign            bool
	}
	var rows []row
	var at time.Time
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var err error
		if snapID == "" {
			err = tx.QueryRow(ctx, `SELECT id, created_at FROM snapshots WHERE run_id = $1
				ORDER BY epoch DESC, created_at DESC, id DESC LIMIT 1`, t.runID).Scan(&snapID, &at)
			if errors.Is(err, pgx.ErrNoRows) {
				return errf(http.StatusNotFound, "no_diff", "the Run has no snapshot yet (it has not stopped since it started)")
			}
		} else {
			err = tx.QueryRow(ctx, `SELECT created_at FROM snapshots WHERE id = $1 AND run_id = $2`, snapID, t.runID).Scan(&at)
			if errors.Is(err, pgx.ErrNoRows) {
				return errf(http.StatusNotFound, "not_found", "the Run has no snapshot %s", snapID)
			}
		}
		if err != nil {
			return err
		}
		var state, reason string
		err = tx.QueryRow(ctx, `SELECT state, reason FROM snapshot_diff_state WHERE snapshot_id = $1`, snapID).Scan(&state, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			state, reason = diffUnsupported, "no diff was recorded with it (it predates diffs, or the Run had no repositories then)"
		} else if err != nil {
			return err
		}
		switch state {
		case diffComplete:
		case diffPending:
			return &HTTPError{Status: http.StatusConflict, Code: "diff_pending", RetryAfter: diffRetryAfter, SnapshotID: snapID,
				Message: fmt.Sprintf("the diff of snapshot %s is still being computed; try again in %ds", snapID, diffRetryAfter)}
		default:
			return &HTTPError{Status: http.StatusNotFound, Code: "diff_unavailable", Reason: reason, SnapshotID: snapID,
				Message: fmt.Sprintf("snapshot %s has no diff (%s): %s", snapID, state, reason)}
		}
		names := make([]string, len(t.repos))
		for i, r := range t.repos {
			names[i] = r.Name
		}
		// The blob must be this Run's own (its tenant's): a row naming
		// another's is refused on report, and never read here either.
		q, err := tx.Query(ctx, `SELECT d.repo, d.base, d.head, d.truncated, d.files, d.insertions, d.deletions, d.file_stats,
				d.filters_ignored, d.filtered_paths, d.normalized_paths, d.dirty_submodules, d.error, d.error_code,
				coalesce(b.s3_key, ''), coalesce(b.location, ''), d.size, d.sha256,
				d.blob_id IS NOT NULL AND (b.id IS NULL OR b.tenant_id <> d.tenant_id OR b.run_id <> d.run_id OR b.kind <> 'diff')
			FROM snapshot_diffs d LEFT JOIN blobs b ON b.id = d.blob_id
			WHERE d.snapshot_id = $1 AND d.kind = $2 AND d.repo = ANY($3)`, snapID, kind, names)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, func(r pgx.CollectableRow) (row, error) {
			var x row
			err := r.Scan(&x.d.Repo, &x.d.Base, &x.d.Head, &x.d.Truncated, &x.d.Files, &x.d.Insertions, &x.d.Deletions,
				&x.d.FileStats, &x.d.FiltersIgnored, &x.d.FilteredPaths, &x.d.NormalizedPaths, &x.d.DirtySubmodules,
				&x.d.Error, &x.d.ErrorCode, &x.key, &x.location, &x.size, &x.sum, &x.foreign)
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
		if !statOnly && x.d.Error == "" && x.size > 0 {
			var patch []byte
			var err error
			if x.foreign {
				err = errors.New("its blob is not this Run's")
			} else {
				patch, err = s.readDiffBlob(ctx, x.key, x.location, x.size, x.sum)
			}
			var ae *HTTPError
			switch {
			case errors.As(err, &ae):
				return nil, err // not uploaded yet, or deleted: the request's answer
			case err != nil:
				// This repository's patch cannot be had: its error, not an
				// empty patch, and not the other repositories' failure.
				s.log.Warn("diff blob", "run", t.runID, "snapshot", snapID, "repo", x.d.Repo, "err", err)
				x.d.Error = "its stored patch cannot be read: " + err.Error()
			default:
				setPatch(&x.d, patch)
			}
		}
		got[x.d.Repo] = x.d
	}
	out := ordered(t, got, "snapshot", at.UTC())
	for i := range out {
		if out[i].SnapshotID == "" {
			out[i].SnapshotID = snapID
		}
		if _, ok := got[out[i].Repo]; !ok {
			out[i].Error = "the Run did not have this repository at this snapshot"
		}
	}
	return out, nil
}

// readDiffBlob reads a stored patch (zstd in S3): exactly size bytes whose
// sha256 is sum, as the runner reported them.
func (s *Server) readDiffBlob(ctx context.Context, key, location string, size int64, sum string) ([]byte, error) {
	switch location {
	case "s3":
	case "":
		return nil, errors.New("no blob was recorded for it")
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
	if got := sha256.Sum256(b); hex.EncodeToString(got[:]) != sum {
		return nil, fmt.Errorf("diff blob %s: sha256 %x, expected %s", key, got, sum)
	}
	return b, nil
}

// expectDiffs records, with a snapshot luxd has just accepted, the diff
// it expects for it: pending, for the repositories its placement had (the
// Run's own, and those a resume added and cloned by then), when its runner
// computes diffs; unsupported when it does not; nothing for a Run without
// repositories.
func expectDiffs(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, snapID string) error {
	var sp spec.RunSpec
	var capable bool
	if err := tx.QueryRow(ctx, `SELECT r.spec, coalesce('diff' = ANY(h.capabilities), false)
		FROM runs r LEFT JOIN hosts h ON h.id = $2 WHERE r.id = $1`, runID, hostID).Scan(&sp, &capable); err != nil {
		return err
	}
	if sp.Git == nil || len(sp.Git.Repositories) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT data->>'repo' FROM run_events
		WHERE run_id = $1 AND type = $2 AND data->>'status' = 'cloned' AND epoch <= $3`, runID, proto.EvGitClone, epoch)
	if err != nil {
		return err
	}
	cloned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	repos := []string{}
	for _, r := range sp.Git.Repositories {
		if r.AddedBy == "" || slices.Contains(cloned, r.Name) {
			repos = append(repos, r.Name)
		}
	}
	state, reason := diffPending, ""
	if !capable {
		state, reason = diffUnsupported, "the host's lux-runner does not compute diffs"
	}
	_, err = tx.Exec(ctx, `INSERT INTO snapshot_diff_state (snapshot_id, tenant_id, run_id, state, reason, repos)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, snapID, tenantID, runID, state, reason, repos)
	return err
}

// errDiffReport: a snapshot.diffs report luxd will not store (nacked,
// not stale: the runner tries again, then gives up).
type errDiffReport struct{ msg string }

func (e *errDiffReport) Error() string { return e.msg }

// applySnapshotDiffs stores a snapshot's diffs, reported after the
// snapshot itself: each patch a blob of its placement, each repository and
// kind a row, the snapshot's diff state, and a diff.failed or diff.skipped
// event for what is missing. A report is taken whole or not at all.
func applySnapshotDiffs(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, sd proto.SnapshotDiffs) error {
	var snapEpoch int
	var snapHost string
	err := tx.QueryRow(ctx, `SELECT epoch, coalesce(host_id, '') FROM snapshots WHERE id = $1 AND run_id = $2`, sd.SnapshotID, runID).Scan(&snapEpoch, &snapHost)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("snapshot.diffs for unknown snapshot %s of %s", sd.SnapshotID, runID)
	}
	if err != nil {
		return err
	}
	if snapEpoch != epoch || snapHost != hostID {
		return fmt.Errorf("snapshot.diffs for %s from epoch %d on %s: it is epoch %d's on %s", sd.SnapshotID, epoch, hostID, snapEpoch, snapHost)
	}
	sum := sha256.Sum256(proto.Marshal(sd))
	digest := hex.EncodeToString(sum[:])
	var state, reason, applied string
	var repos []string
	err = tx.QueryRow(ctx, `SELECT state, reason, repos, report_sha256 FROM snapshot_diff_state WHERE snapshot_id = $1 FOR UPDATE`,
		sd.SnapshotID).Scan(&state, &reason, &repos, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return &errDiffReport{fmt.Sprintf("snapshot.diffs for %s: no diff is expected for it", sd.SnapshotID)}
	}
	if err != nil {
		return err
	}
	if sd.Lost != "" {
		return diffsLost(ctx, tx, tenantID, runID, epoch, sd, state)
	}
	// A redelivery is the same report; any other for a snapshot whose diff
	// is settled is refused.
	switch {
	case applied == digest:
		return nil
	case state == diffPending, state == diffUnsupported, state == diffFailed && reason == diffReportLost:
	default:
		return &errDiffReport{fmt.Sprintf("snapshot.diffs for %s: its diff is %s already", sd.SnapshotID, state)}
	}
	next, why := diffComplete, ""
	switch {
	case sd.Skipped != "":
		next, why = diffSkipped, sd.Skipped
	case sd.Error != "":
		next, why = diffFailed, sd.Error
	default:
		if err := wholeReport(sd, repos); err != nil {
			return err
		}
	}
	if err := recordSnapshotDiffs(ctx, tx, tenantID, hostID, runID, epoch, sd.SnapshotID, sd.Diffs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE snapshot_diff_state SET state = $2, reason = $3, report_sha256 = $4, updated_at = now()
		WHERE snapshot_id = $1`, sd.SnapshotID, next, why, digest); err != nil {
		return err
	}
	if sd.CleanupFailed != "" {
		if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvDiffCleanupFailed, map[string]any{"snapshotId": sd.SnapshotID, "error": sd.CleanupFailed}); err != nil {
			return err
		}
	}
	switch next {
	case diffSkipped:
		return addEvent(ctx, tx, tenantID, runID, epoch, proto.EvDiffSkipped, map[string]any{"snapshotId": sd.SnapshotID, "reason": sd.Skipped})
	case diffFailed:
		return addEvent(ctx, tx, tenantID, runID, epoch, proto.EvDiffFailed, map[string]any{"snapshotId": sd.SnapshotID, "error": sd.Error})
	}
	failed := map[string]bool{}
	for _, d := range sd.Diffs {
		if d.Error == "" || failed[d.Repo] {
			continue
		}
		failed[d.Repo] = true
		data := map[string]any{"snapshotId": sd.SnapshotID, "repo": d.Repo, "kind": d.Kind, "error": d.Error}
		if d.ErrorCode != "" {
			data["errorCode"] = d.ErrorCode
		}
		if err := addEvent(ctx, tx, tenantID, runID, epoch, proto.EvDiffFailed, data); err != nil {
			return err
		}
	}
	return nil
}

// wholeReport checks a report has exactly one diff per expected
// repository and kind, and nothing else.
func wholeReport(sd proto.SnapshotDiffs, repos []string) error {
	type key struct{ repo, kind string }
	want := map[key]bool{}
	for _, r := range repos {
		for _, k := range []string{proto.DiffBaseClone, proto.DiffBaseHead} {
			want[key{r, k}] = true
		}
	}
	got := map[key]bool{}
	for _, d := range sd.Diffs {
		k := key{d.Repo, d.Kind}
		if !want[k] || got[k] {
			return &errDiffReport{fmt.Sprintf("snapshot.diffs for %s: an unexpected or repeated diff for %q (%s)", sd.SnapshotID, d.Repo, d.Kind)}
		}
		got[k] = true
	}
	if len(got) != len(want) {
		return &errDiffReport{fmt.Sprintf("snapshot.diffs for %s: %d of the %d expected diffs", sd.SnapshotID, len(got), len(want))}
	}
	return nil
}

// diffsLost records that a snapshot's patches never reached luxd (the
// runner lost their files before uploading them): its diff has failed.
// Only a complete diff has patches; anything else (a redelivery) stays.
func diffsLost(ctx context.Context, tx pgx.Tx, tenantID, runID string, epoch int, sd proto.SnapshotDiffs, state string) error {
	if state != diffComplete {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE snapshot_diff_state SET state = 'failed', reason = $2, updated_at = now()
		WHERE snapshot_id = $1`, sd.SnapshotID, "patches lost before upload: "+sd.Lost); err != nil {
		return err
	}
	return addEvent(ctx, tx, tenantID, runID, epoch, proto.EvDiffFailed, map[string]any{"snapshotId": sd.SnapshotID, "error": "patches lost before upload: " + sd.Lost})
}

// reapLostDiffReports fails the diffs whose report never came: their
// runner gave up (it tries for 10 minutes), or its host is gone.
func (s *Server) reapLostDiffReports(ctx context.Context) error {
	return s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE snapshot_diff_state ds SET state = 'failed', reason = $2, updated_at = now()
			FROM snapshots sn
			WHERE ds.state = 'pending' AND ds.updated_at < now() - $1::interval AND sn.id = ds.snapshot_id
			RETURNING ds.tenant_id, ds.run_id, sn.epoch, ds.snapshot_id`, interval(diffReportLostAfter), diffReportLost)
		if err != nil {
			return err
		}
		type lost struct {
			Tenant, Run string
			Epoch       int
			Snap        string
		}
		gone, err := pgx.CollectRows(rows, pgx.RowToStructByPos[lost])
		if err != nil {
			return err
		}
		for _, l := range gone {
			if err := addEvent(ctx, tx, l.Tenant, l.Run, l.Epoch, proto.EvDiffFailed,
				map[string]any{"snapshotId": l.Snap, "error": "its diff report never arrived"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// refusedBlobError: a report names a blob id that is not a new one of its
// own: another Run's (perhaps another tenant's), or reused within it.
type refusedBlobError struct {
	snapID, blobID, why string
}

func (e *refusedBlobError) Error() string {
	return fmt.Sprintf("snapshot.diffs for %s refused: blob %s %s", e.snapID, e.blobID, e.why)
}

// claimDiffBlob records a new blob for a patch of snapshot snapID. An id
// that exists already is accepted only as a redelivery: the same Run's
// diff blob of this snapshot, with the same size and sha256. Anything else
// could make this snapshot's diff read another's bytes.
func claimDiffBlob(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, snapID string, d proto.SnapshotDiff) error {
	b := d.Blob
	var bTenant, bRun, bKind, bSum string
	var bSize int64
	var ours bool
	err := tx.QueryRow(ctx, `SELECT b.tenant_id, b.run_id, b.kind, b.size, b.sha256,
			EXISTS (SELECT 1 FROM snapshot_diffs d WHERE d.blob_id = b.id AND d.snapshot_id = $2)
		FROM blobs b WHERE b.id = $1 FOR UPDATE`, b.BlobID, snapID).Scan(&bTenant, &bRun, &bKind, &bSize, &bSum, &ours)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO blobs (id, tenant_id, run_id, epoch, kind, name, size, sha256, location, host_id)
			VALUES ($1, $2, $3, $4, 'diff', $5, $6, $7, 'host', $8)`, b.BlobID, tenantID, runID, epoch, d.Repo, b.Size, b.SHA256, hostID)
		return err
	}
	if err != nil {
		return err
	}
	if bTenant != tenantID || bRun != runID || bKind != "diff" || !ours || bSize != b.Size || bSum != b.SHA256 {
		return &refusedBlobError{snapID, b.BlobID, "exists already and is not this snapshot's"}
	}
	return nil
}

// recordSnapshotDiffs stores a snapshot's diffs: each patch a blob of its
// placement, each repository and kind a row.
func recordSnapshotDiffs(ctx context.Context, tx pgx.Tx, tenantID, hostID, runID string, epoch int, snapID string, diffs []proto.SnapshotDiff) error {
	seen := map[string]bool{}
	for _, d := range diffs {
		if d.Kind != proto.DiffBaseClone && d.Kind != proto.DiffBaseHead {
			continue
		}
		var blobID *string
		if d.Blob != nil {
			if seen[d.Blob.BlobID] {
				return &refusedBlobError{snapID, d.Blob.BlobID, "is named twice"}
			}
			seen[d.Blob.BlobID] = true
			if len(d.PatchSHA256) != 64 || d.PatchBytes <= 0 {
				return fmt.Errorf("snapshot.diffs for %s: %s (%s) has a blob but no patch size or sha256", snapID, d.Repo, d.Kind)
			}
			if err := claimDiffBlob(ctx, tx, tenantID, hostID, runID, epoch, snapID, d); err != nil {
				return err
			}
			blobID = &d.Blob.BlobID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO snapshot_diffs (tenant_id, run_id, snapshot_id, epoch, repo, kind, base, head, blob_id,
				size, sha256, truncated, files, insertions, deletions, file_stats, filters_ignored, filtered_paths,
				normalized_paths, dirty_submodules, error, error_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22) ON CONFLICT DO NOTHING`,
			tenantID, runID, snapID, epoch, d.Repo, d.Kind, d.Base, d.Head, blobID,
			d.PatchBytes, d.PatchSHA256, d.Truncated, d.Files, d.Insertions, d.Deletions, nonNil(d.FileStats),
			d.FiltersIgnored, nonNil(d.FilteredPaths), nonNil(d.NormalizedPaths), nonNil(d.DirtySubmodules), d.Error, d.ErrorCode); err != nil {
			return err
		}
	}
	return nil
}
