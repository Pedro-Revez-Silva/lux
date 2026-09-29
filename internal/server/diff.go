package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

type diffInput struct {
	RunPath
	Base string `query:"base" enum:"clone,head" doc:"clone (default): from the commit each repository was cloned at. head: from its HEAD."`
	Stat bool   `query:"stat" doc:"Totals only, no patches."`
}

type RepoDiff struct {
	Repo        string               `json:"repo"`
	Base        string               `json:"base" doc:"The commit diffed from."`
	Head        string               `json:"head" doc:"The checkout's HEAD commit (empty before its first commit)."`
	Patch       string               `json:"patch,omitempty" doc:"The patch (git diff format), when it is valid UTF-8."`
	PatchBase64 []byte               `json:"patchBase64,omitempty" doc:"The patch, base64, when it is not valid UTF-8 (patch is then absent)."`
	Files       int                  `json:"files"`
	Insertions  int                  `json:"insertions"`
	Deletions   int                  `json:"deletions"`
	FileStats   []proto.DiffFileStat `json:"fileStats,omitempty"`
	Truncated   bool                 `json:"truncated" doc:"Untracked files were cut (at 32 KiB each, or 8 KiB past 1 MiB in all) or are binary: the patch does not recreate them."`
	Error       string               `json:"error,omitempty" doc:"Why this repository's diff could not be computed."`
}

type RunDiff struct {
	Base  string     `json:"base" enum:"clone,head"`
	Repos []RepoDiff `json:"repos"`
}

type runDiffOutput struct {
	Body RunDiff
}

var diffWait = 75 * time.Second

func (s *Server) runDiff(ctx context.Context, in *diffInput) (*runDiffOutput, error) {
	base := in.Base
	if base == "" {
		base = proto.DiffBaseClone
	}
	var runState, plState, hostID string
	var epoch int
	err := s.db.Tx(ctx, store.Tenant(principal(ctx).TenantID), func(tx pgx.Tx) error {
		var sp spec.RunSpec
		if err := tx.QueryRow(ctx, `SELECT r.state, r.spec, r.current_epoch, coalesce(p.host_id, ''), coalesce(p.state, '')
			FROM runs r LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
			WHERE r.id = $1`, in.ID).Scan(&runState, &sp, &epoch, &hostID, &plState); err != nil {
			return err
		}
		if sp.Git == nil || len(sp.Git.Repositories) == 0 {
			return errf(http.StatusNotFound, "no_diff", "the Run has no repositories")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if (runState != StateRunning && runState != StateStopping) || (plState != "running" && plState != "stopping") {
		return nil, notRunning(runState, plState)
	}
	res, err := s.askDiff(ctx, hostID, in.ID, epoch, base)
	if err != nil {
		return nil, err
	}
	switch {
	case res.NotRunning:
		return nil, notRunning(runState, plState)
	case res.Busy:
		return nil, errf(http.StatusConflict, "diff_busy", "another diff of this Run is under way; retry when it is done")
	case res.Error != "":
		return nil, errf(http.StatusBadGateway, "diff_failed", "the diff in the Run's container: %s", res.Error)
	}
	out := &runDiffOutput{Body: RunDiff{Base: base, Repos: []RepoDiff{}}}
	for _, r := range res.Repos {
		d := RepoDiff{Repo: r.Repo, Base: r.Base, Head: r.Head, Files: r.Files, Insertions: r.Insertions,
			Deletions: r.Deletions, FileStats: r.FileStats, Truncated: r.Truncated, Error: r.Error}
		switch {
		case in.Stat:
		case utf8.Valid(r.Patch):
			d.Patch = string(r.Patch)
		default:
			d.PatchBase64 = r.Patch
		}
		out.Body.Repos = append(out.Body.Repos, d)
	}
	return out, nil
}

func (s *Server) askDiff(ctx context.Context, hostID, runID string, epoch int, base string) (proto.DiffResult, error) {
	var res proto.DiffResult
	c := s.hub.conn(hostID)
	if c == nil {
		return res, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host has no live connection to this luxd")
	}
	if !slices.Contains(c.caps, proto.CapDiff) {
		return res, errf(http.StatusServiceUnavailable, "diff_unsupported", "the Run's host runs a lux-runner without diffs; upgrade it")
	}
	subID := ids.New("diff")
	ch, cancel := s.hub.Subscribe(subID)
	defer cancel()
	if err := s.hub.SendLive(hostID, proto.Frame{Type: proto.MsgDiffRequest, RunID: runID, Epoch: epoch,
		Data: proto.Marshal(proto.DiffRequest{SubID: subID, Base: base})}); err != nil {
		return res, errf(http.StatusServiceUnavailable, "host_unreachable", "%v", err)
	}
	timer := time.NewTimer(diffWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return res, ctx.Err()
	case <-c.done:
		return res, errf(http.StatusServiceUnavailable, "host_unreachable", "the Run's host disconnected")
	case <-timer.C:
		return res, errf(http.StatusGatewayTimeout, "diff_timeout", "the Run's host did not answer in time")
	case f, ok := <-ch:
		if !ok {
			return res, errors.New("diff result dropped")
		}
		return res, json.Unmarshal(f.Data, &res)
	}
}

const keepAPatch = "resume it, or save a patch at stop with workload.beforeStop " +
	"(git add -N . && git diff --binary <base> > $LUX_ARTIFACTS/final.patch) and fetch it with lux artifacts"

func notRunning(runState, plState string) error {
	if plState == "assigned" || plState == "starting" ||
		slices.Contains([]string{StateSubmitted, StateScheduled, StateProvisioning, StateStarting, StateResuming}, runState) {
		return errf(http.StatusConflict, "run_not_running",
			"the Run is %s: its container is not up yet; its diff is available once it runs", runState)
	}
	what := "the Run is " + runState
	if runState == StateRunning || runState == StateStopping {
		what += " and its container has exited"
	}
	return errf(http.StatusConflict, "run_not_running", "%s: its diff is available only while the Run is running; %s", what, keepAPatch)
}

// gitBases are the commits the repositories of a Run's placement at epoch
// were cloned at: each one's latest successful git.clone in that placement,
// else in the placement whose snapshot it restored, and so on back (a
// resume clones only the repositories it adds). A snapshot older than the
// latest (resume --from-snapshot) leads back through its own placement,
// never through those after it. A placement scheduled before lineage was
// recorded (no snapshotId) has only its own clones: an earlier
// placement's may not be what it restored.
func gitBases(ctx context.Context, tx pgx.Tx, runID string, epoch int) (map[string]string, error) {
	m := map[string]string{}
	for e := epoch; e > 0; {
		rows, err := tx.Query(ctx, `SELECT data->>'repo', coalesce(data->>'commit', '')
			FROM run_events WHERE run_id = $1 AND epoch = $2 AND type = $3 AND data->>'status' = 'cloned'
			ORDER BY id DESC`, runID, e, proto.EvGitClone)
		if err != nil {
			return nil, err
		}
		clones, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
			var kv [2]string
			return kv, r.Scan(&kv[0], &kv[1])
		})
		if err != nil {
			return nil, err
		}
		for _, kv := range clones {
			if _, ok := m[kv[0]]; !ok {
				m[kv[0]] = kv[1]
			}
		}
		var recorded bool
		var snap *string
		err = tx.QueryRow(ctx, `SELECT data ? 'snapshotId', data->>'snapshotId' FROM run_events
			WHERE run_id = $1 AND epoch = $2 AND type = 'state' AND data->>'state' = $3
			ORDER BY id DESC LIMIT 1`, runID, e, StateScheduled).Scan(&recorded, &snap)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!recorded || snap == nil)) {
			break // no lineage, or it started from empty volumes
		}
		if err != nil {
			return nil, err
		}
		prev := 0
		if err := tx.QueryRow(ctx, `SELECT epoch FROM snapshots WHERE id = $1 AND run_id = $2`, *snap, runID).Scan(&prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		// Epochs only go back: a snapshot is always an earlier placement's.
		e = min(prev, e-1)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}
