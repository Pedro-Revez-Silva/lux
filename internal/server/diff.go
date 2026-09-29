package server

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// A running Run's diff: per repository, from its base (the commit it was
// cloned at, or its HEAD) to its working tree, computed now in the Run's
// container by its runner. A Run whose container is not running has none.

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
	At         time.Time        `json:"at" doc:"When it was computed."`
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
	if !t.live {
		return notRunning(t)
	}
	repos, err := s.liveDiff(ctx, t, kind, statOnly)
	if err != nil {
		return err
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
	runID   string
	state   string // the Run's
	plState string // its current placement's
	hostID  string
	epoch   int
	live    bool // its container should be running
	repos   []spec.Repository
	bases   map[string]string
}

// diffTarget reads what a diff of the Run needs: its repositories (one, if
// named) and where its container is.
func (s *Server) diffTarget(ctx context.Context, tenantID, runID, repo string) (diffTarget, error) {
	t := diffTarget{runID: runID}
	err := s.db.Tx(ctx, store.Tenant(tenantID), func(tx pgx.Tx) error {
		var sp spec.RunSpec
		if err := tx.QueryRow(ctx, `SELECT r.state, r.spec, r.current_epoch, coalesce(p.host_id, ''), coalesce(p.state, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, runID).Scan(&t.state, &sp, &t.epoch, &t.hostID, &t.plState); err != nil {
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
		// A stopping Run's container runs until its workload has exited:
		// the runner is asked, and says when it is not running.
		t.live = (t.state == StateRunning || t.state == StateStopping) &&
			(t.plState == "running" || t.plState == "stopping")
		var err error
		t.bases, err = gitBases(ctx, tx, runID)
		return err
	})
	return t, err
}

// keepAPatch is how to keep a Run's changes past its stop.
const keepAPatch = "resume it, or save a patch at stop with workload.beforeStop " +
	"(git add -N . && git diff --binary <base> > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts"

// notRunning is the answer for a Run whose container is not running: one
// not up yet, or one that has stopped (or ended, or is ending).
func notRunning(t diffTarget) error {
	if t.plState == "assigned" || t.plState == "starting" ||
		slices.Contains([]string{StateSubmitted, StateScheduled, StateProvisioning, StateStarting, StateResuming}, t.state) {
		return errf(http.StatusConflict, "run_not_running",
			"the Run is %s: its container is not up yet; its diff is available once it runs", t.state)
	}
	what := "the Run is " + t.state
	if t.state == StateRunning || t.state == StateStopping {
		what += " and its container has exited"
	}
	return errf(http.StatusConflict, "run_not_running",
		"%s: its diff is available only while the Run is running; %s", what, keepAPatch)
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
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var kv [2]string
		return kv, r.Scan(&kv[0], &kv[1])
	})
	if err != nil || len(out) == 0 {
		return nil, err
	}
	m := make(map[string]string, len(out))
	for _, kv := range out {
		m[kv[0]] = kv[1]
	}
	return m, nil
}

// liveDiffWait is how long luxd waits for the runner's answer: the runner
// gives a diff a minute, and a little more for the relay.
var liveDiffWait = 75 * time.Second

// liveDiff asks the Run's runner to compute the diff in its container. The
// whole request is on the one connection the host has when it starts: its
// capability, the request, the results and any cancel. A runner that
// reconnects meanwhile forgets the diff, so the request fails at once.
func (s *Server) liveDiff(ctx context.Context, t diffTarget, kind string, statOnly bool) ([]RepoDiff, error) {
	c := s.hub.conn(t.hostID)
	if c == nil {
		return nil, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
	}
	if !c.can(proto.CapDiff) {
		return nil, errf(http.StatusServiceUnavailable, "diff_unsupported", "the Run's host runs a lux-runner without diffs; its diff is available once the host is upgraded")
	}
	req := proto.DiffRequest{SubID: ids.New("diff"), Kind: kind, StatOnly: statOnly}
	for _, r := range t.repos {
		req.Repos = append(req.Repos, proto.DiffRepo{Name: r.Name, Path: r.Path, Base: t.bases[r.Name]})
	}
	ch, cancel := s.hub.subscribeOn(c, req.SubID)
	defer cancel()
	if err := c.sendLive(proto.Frame{Type: proto.MsgDiffRequest, RunID: t.runID, Epoch: t.epoch, Data: proto.Marshal(req)}, c.replaced); err != nil {
		return nil, connError(err)
	}
	// Gone before the end (the client left, or the deadline): the runner
	// stops the diff (for this request; a shared one when its last goes).
	ended := false
	defer func() {
		if !ended {
			_ = c.sendLive(proto.Frame{Type: proto.MsgDiffCancel, RunID: t.runID, Epoch: t.epoch,
				Data: proto.Marshal(proto.DiffEnd{SubID: req.SubID})}, nil)
		}
	}()
	deadline := time.NewTimer(liveDiffWait)
	defer deadline.Stop()
	at := time.Now().UTC()
	got := map[string]RepoDiff{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, connError(errHostDisconnected)
		case <-c.replaced:
			return nil, connError(errRunnerReconnected)
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
				d.At = at
				got[d.Repo] = d
			case proto.MsgDiffEnd:
				ended = true
				var end proto.DiffEnd
				_ = json.Unmarshal(f.Data, &end)
				switch {
				case end.NotRunning:
					return nil, notRunning(t)
				case end.Busy:
					return nil, errf(http.StatusTooManyRequests, "diff_busy", "another live diff of this Run is under way (a different base, repository or stat); retry when it is done")
				case end.Error != "":
					return nil, errf(http.StatusBadGateway, "diff_failed", "computing the diff in the Run's container: %s", end.Error)
				}
				return ordered(t, got, at), nil
			}
		}
	}
}

func connError(err error) error {
	switch err {
	case errRunnerReconnected:
		return errf(http.StatusServiceUnavailable, "runner_reconnected", "the Run's runner reconnected during the diff, which it does not carry over; retry")
	case errHostDisconnected:
		return errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host disconnected")
	}
	return errf(http.StatusServiceUnavailable, "host_unreachable", "%v", err)
}

// ordered lists the diffs in the spec's order; a repository the runner
// had nothing for says so.
func ordered(t diffTarget, got map[string]RepoDiff, at time.Time) []RepoDiff {
	out := []RepoDiff{}
	for _, r := range t.repos {
		d, ok := got[r.Name]
		if !ok {
			d = RepoDiff{Repo: r.Name, At: at, Error: "no diff was computed for this repository"}
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
	if utf8.Valid(patch) {
		d.Patch = string(patch)
	} else {
		d.PatchBase64 = patch
	}
	return d
}
