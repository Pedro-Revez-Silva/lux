package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// chooseHost resolves an operator's choice of host (resume --to, migrate
// --to) to its id; nil when there is none. Tenants cannot choose: where a
// Run goes is the scheduler's (and its spec's) to say.
func (s *Server) chooseHost(ctx context.Context, p Principal, ref string) (*string, error) {
	if ref == "" {
		return nil, nil
	}
	if !p.Operator {
		return nil, errf(http.StatusForbidden, "forbidden", "only operators choose hosts")
	}
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		// The Run's tenant (p's, by now) sees its own hosts and the
		// platform's: any other is not a host it could run on.
		id, err = s.resolveHost(ctx, tx, p, ref, false)
		if err != nil {
			return err
		}
		var state string
		var draining bool
		if err := tx.QueryRow(ctx, `SELECT state, draining FROM hosts WHERE id = $1`, id).Scan(&state, &draining); err != nil {
			return err
		}
		if state != "ready" || draining {
			return errf(http.StatusConflict, "host_unavailable", "host %s is %s: it takes no new Runs", ref, state)
		}
		return nil
	})
	if errors.Is(err, errNotFound) {
		return nil, errf(http.StatusNotFound, "not_found", "no host %s this Run could use", ref)
	}
	return &id, err
}

type migrateRequest struct {
	To    string       `json:"to,omitempty" doc:"The host to move to (id or name); by default, any other."`
	Input *resumeInput `json:"input,omitempty" doc:"A message for the workload once it runs again."`
}

type migrateRunInput struct {
	RunPath
	Body *migrateRequest
}

// migrateRun moves a running Run to another host: it is stopped (its state
// snapshotted), then resumed at once, away from the host it was on, or on
// the one chosen: the path a drain takes, for one Run. An agent resumes its
// session; input, if given, is delivered once it runs. Operators only.
func (s *Server) migrateRun(ctx context.Context, in *migrateRunInput) (*acceptedRun, error) {
	p := principal(ctx)
	req := migrateRequest{}
	if in.Body != nil {
		req = *in.Body
	}
	placeOn, err := s.chooseHost(ctx, p, req.To)
	if err != nil {
		return nil, err
	}
	var hostID string
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var state, current, stopping, policy string
		if err := tx.QueryRow(ctx, `SELECT r.state, coalesce(p.host_id, ''), coalesce(p.stop_reason, ''), `+runResumePolicySQL+` FROM runs r
				LEFT JOIN placements p ON p.run_id = r.id AND p.epoch = r.current_epoch
				WHERE r.id = $1 AND r.tenant_id = $2 FOR UPDATE OF r`, in.ID, p.TenantID).Scan(&state, &current, &stopping, &policy); err != nil {
			return err
		}
		switch {
		// Before the state: a stopped Run with such a policy gets
		// not_movable too, since no move of it is ever possible.
		case spec.FailsOnMove(policy):
			// Moved, it would fail: the operator stops or cancels it instead.
			return errf(http.StatusConflict, "not_movable", "the Run's resumePolicy is %s: it cannot continue on another host", policy)
		case state != StateRunning && state != StateStopping:
			return errf(http.StatusConflict, "not_running", "run is %s: only a running Run can be migrated (a stopped one: resume --to)", state)
		case stopping != "":
			// Its stop decides what becomes of it: one reason per stop.
			// A force-evicted Run lands here; a merely cordoned host's does not.
			return errf(http.StatusConflict, "stopping", "the Run is already being stopped (%s)", stopping)
		case state != StateRunning:
			return errf(http.StatusConflict, "not_running", "run is %s", state)
		case placeOn != nil && *placeOn == current:
			return errf(http.StatusConflict, "same_host", "the Run is already on that host")
		}
		var input *proto.Input
		if req.Input != nil && req.Input.Text != "" {
			input = &proto.Input{RequestID: ids.New("in"), Text: req.Input.Text}
		}
		// Where it goes next, and what it is told there: read when the stop
		// completes (placementExited) and it is resumed.
		if _, err := tx.Exec(ctx, `UPDATE runs SET place_on = $2, avoid_host = nullif($3, ''), pending_input = $4 WHERE id = $1`,
			in.ID, placeOn, current, input); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, p.TenantID, in.ID, 0, "migrate.requested",
			map[string]any{"by": p.Actor(), "from": current, "to": placeOn}); err != nil {
			return err
		}
		hostID, err = s.requestStop(ctx, tx, p.TenantID, in.ID, "migrate")
		return err
	})
	if err != nil {
		return nil, err
	}
	if hostID != "" {
		s.hub.Notify(hostID)
	}
	run, err := s.loadRun(ctx, p.TenantID, in.ID, false)
	if err != nil {
		return nil, err
	}
	return &acceptedRun{http.StatusAccepted, run}, nil
}
