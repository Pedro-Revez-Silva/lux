package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

func (s *Server) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		if err := s.scheduleOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("schedule", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

type candidateHost struct {
	ID        string
	TenantID  *string
	Pool      string
	Labels    map[string]string
	Capacity  proto.Capacity
	Images    []string
	Mirrors   []string
	UsedCPUs  float64
	UsedMem   int64
	UsedDisk  int64
	UsedRuns  int
	Tenants   []string // tenants with live placements here
	Shared    bool
	Connected bool
}

type pendingRun struct {
	ID            string
	TenantID      string
	State         string
	Spec          spec.RunSpec
	SnapshotID    *string
	SessionID     string
	Epoch         int
	PendingInput  json.RawMessage
	ImageResolved *proto.ImageResolution
	HasSecrets    bool
	// An operator's say: the host it must go to, or one it must not.
	PlaceOn   string
	AvoidHost string
}

// scheduleOnce considers every Run waiting for a host, a batch at a time,
// with SKIP LOCKED so several luxd instances can schedule at once. The
// batches walk the queue by (updated_at, id), so Runs that cannot be
// placed do not hide the ones behind them.
func (s *Server) scheduleOnce(ctx context.Context) error {
	var after cursorPos
	for range 50 {
		next, more, err := s.scheduleBatch(ctx, after)
		if err != nil || !more {
			return err
		}
		after = next
	}
	return nil
}

type cursorPos struct {
	updated time.Time
	id      string
}

// secretsGrace is how long a Run with secrets waits for this luxd to hold
// its values before they are declared lost. Submit and resume cache them
// before committing, so a Run this instance accepted always has them; the
// grace is for Runs accepted by another instance (which schedules them
// itself) and for this instance having restarted. It is the host lease:
// how long luxd waits on anything that may still be in flight elsewhere.
func (s *Server) secretsGrace() time.Duration { return s.cfg.LeaseDuration }

// scheduleBatch looks at up to 20 waiting Runs after pos, placing what it
// can and recording why the rest wait. Everything it writes is committed.
func (s *Server) scheduleBatch(ctx context.Context, pos cursorPos) (cursorPos, bool, error) {
	var notify []string
	var last cursorPos
	var n int
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, state, spec, snapshot_id, session_id, current_epoch, pending_input, image_resolved,
				jsonb_array_length(secrets) > 0, coalesce(place_on, ''), coalesce(avoid_host, ''), updated_at, updated_at < now() - $3::interval
			FROM runs WHERE state IN `+queuedRunStates+` AND NOT cancel_requested
			  AND (updated_at, id) > ($1, $2)
			ORDER BY updated_at, id FOR UPDATE SKIP LOCKED LIMIT 20`,
			pos.updated, pos.id, interval(s.secretsGrace()))
		if err != nil {
			return err
		}
		type item struct {
			r        pendingRun
			updated  time.Time
			graceful bool
		}
		var items []item
		for rows.Next() {
			var it item
			r := &it.r
			if err := rows.Scan(&r.ID, &r.TenantID, &r.State, &r.Spec, &r.SnapshotID, &r.SessionID, &r.Epoch, &r.PendingInput,
				&r.ImageResolved, &r.HasSecrets, &r.PlaceOn, &r.AvoidHost, &it.updated, &it.graceful); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
		}
		rows.Close()
		n = len(items)
		if n == 0 {
			return nil
		}
		last = cursorPos{items[n-1].updated, items[n-1].r.ID}
		hosts, err := s.candidateHosts(ctx, tx)
		if err != nil {
			return err
		}
		for _, it := range items {
			r := it.r
			// Secret values live only in memory. If this luxd does not hold
			// them past the grace period (it restarted, or they went to an
			// instance that is gone), the Run cannot start until someone
			// supplies them again: it stops, resumable with its secrets.
			if _, ok := s.secrets.get(r.ID); r.HasSecrets && !ok {
				if it.graceful {
					if err := setRunState(ctx, tx, r.TenantID, r.ID, StateStopped, "secrets must be supplied again: resume with them", r.Epoch); err != nil {
						return err
					}
				}
				continue
			}
			h, wait, err := s.pickHost(ctx, tx, r, hosts)
			if err != nil {
				return err
			}
			if h == nil {
				if err := s.noHost(ctx, tx, r, wait); err != nil {
					return err
				}
				continue
			}
			if err := s.assign(ctx, tx, r, h); err != nil {
				return err
			}
			notify = append(notify, h.ID)
		}
		return nil
	})
	if err != nil {
		return pos, false, err
	}
	for _, h := range notify {
		s.hub.Notify(h)
	}
	return last, n == 20, nil
}

func (s *Server) candidateHosts(ctx context.Context, tx pgx.Tx) ([]*candidateHost, error) {
	rows, err := tx.Query(ctx, `
		SELECT h.id, h.tenant_id, h.pool, h.labels, h.capacity, coalesce(h.caches->'images', '[]'),
			coalesce(h.caches->'gitMirrors', '[]'),
			coalesce((SELECT bool_or(p.shared) FROM pools p WHERE p.tenant_id IS NULL AND p.name = h.pool), false)
		FROM hosts h
		WHERE h.state = 'ready' AND NOT h.draining AND h.last_heartbeat > now() - $1::interval`,
		interval(s.cfg.LeaseDuration))
	if err != nil {
		return nil, err
	}
	var hosts []*candidateHost
	byID := map[string]*candidateHost{}
	for rows.Next() {
		h := &candidateHost{}
		var images []string
		if err := rows.Scan(&h.ID, &h.TenantID, &h.Pool, &h.Labels, &h.Capacity, &images, &h.Mirrors, &h.Shared); err != nil {
			rows.Close()
			return nil, err
		}
		h.Images = images
		h.Connected = s.hub.Reachable(h.ID)
		hosts = append(hosts, h)
		byID[h.ID] = h
	}
	rows.Close()
	used, err := tx.Query(ctx, `SELECT host_id, tenant_id, resources FROM placements
		WHERE state IN `+livePlacementStates+``)
	if err != nil {
		return nil, err
	}
	defer used.Close()
	for used.Next() {
		var hostID, tenantID string
		var res spec.Resources
		if err := used.Scan(&hostID, &tenantID, &res); err != nil {
			return nil, err
		}
		if h := byID[hostID]; h != nil {
			h.UsedCPUs += res.CPUs
			h.UsedMem += int64(res.Memory)
			h.UsedDisk += int64(res.Disk)
			h.UsedRuns++
			h.Tenants = append(h.Tenants, tenantID)
		}
	}
	return hosts, used.Err()
}

// pickHost chooses where a Run goes. Hard constraints first (tenancy, pool,
// labels, capacity), then affinity: the host holding the Run's snapshot
// wins outright (a resume there moves nothing), then hosts with the image
// cached, then soft label preferences.
//
// wait is a human-readable reason when no host is chosen.
func (s *Server) pickHost(ctx context.Context, tx pgx.Tx, r pendingRun, hosts []*candidateHost) (*candidateHost, string, error) {
	// Where is the snapshot we must resume from, and may another host use it?
	var snapHost string
	var snapUploaded, snapAvailable, snapHostCopy bool
	if r.SnapshotID != nil {
		err := tx.QueryRow(ctx, `SELECT coalesce(host_id, ''), uploaded, available, host_copy FROM snapshots WHERE id = $1`,
			*r.SnapshotID).Scan(&snapHost, &snapUploaded, &snapAvailable, &snapHostCopy)
		if err != nil {
			return nil, "", err
		}
		if !snapAvailable {
			return nil, "snapshot unavailable", nil
		}
		if !snapHostCopy {
			snapHost = ""
		}
	}

	type scored struct {
		h     *candidateHost
		score int
	}
	var ok []scored
	reason := "no host matches"
	if r.PlaceOn != "" {
		// A chosen host that can no longer take Runs (drained, lost, gone:
		// not a candidate) must not hold the Run forever: the choice
		// lapses, and the Run goes wherever it may.
		if slices.ContainsFunc(hosts, func(h *candidateHost) bool { return h.ID == r.PlaceOn }) {
			reason = "waiting for its chosen host"
		} else {
			if _, err := tx.Exec(ctx, `UPDATE runs SET place_on = NULL WHERE id = $1`, r.ID); err != nil {
				return nil, "", err
			}
			r.PlaceOn = ""
		}
	}
	for _, h := range hosts {
		if !h.Connected {
			continue
		}
		// Chosen by an operator: that host, whatever its pool, labels or
		// sharing (it still needs the tenancy and capacity). A host to
		// avoid (the one a migration left) is only scored down: rather
		// back where it was than nowhere.
		chosen := r.PlaceOn != ""
		if chosen && h.ID != r.PlaceOn {
			continue
		}
		if h.Pool != r.Spec.Placement.Pool && !chosen {
			continue
		}
		// Tenancy: a tenant's own hosts; a shared platform pool; or a
		// platform host no other tenant is using right now.
		if h.TenantID != nil && *h.TenantID != r.TenantID {
			continue
		}
		if h.TenantID == nil && !h.Shared {
			other := false
			for _, t := range h.Tenants {
				if t != r.TenantID {
					other = true
				}
			}
			if other {
				continue
			}
		}
		if !labelsMatch(h.Labels, r.Spec.Placement.Requires) && !chosen {
			continue
		}
		if r.Spec.Sandbox.NestedContainers && h.Labels["nested"] != "true" {
			continue
		}
		res := r.Spec.Resources
		if h.Capacity.Runs > 0 && h.UsedRuns >= h.Capacity.Runs ||
			h.Capacity.CPUs > 0 && h.UsedCPUs+res.CPUs > h.Capacity.CPUs ||
			h.Capacity.Memory > 0 && h.UsedMem+int64(res.Memory) > h.Capacity.Memory ||
			h.Capacity.Disk > 0 && h.UsedDisk+int64(res.Disk) > h.Capacity.Disk {
			reason = "waiting for capacity"
			continue
		}
		// A snapshot only on another host, not yet uploaded: that host must
		// finish the upload first (it just reported it; it is alive).
		if snapHost != "" && h.ID != snapHost && !snapUploaded {
			reason = "waiting for snapshot upload"
			continue
		}
		// Moving away (a migration): its snapshot's host is the one it
		// left, so wait for the upload rather than go straight back.
		if h.ID == r.AvoidHost && snapHost == h.ID && !snapUploaded {
			reason = "waiting for snapshot upload"
			continue
		}
		sc := 0
		if snapHost == h.ID {
			sc += 1000
		}
		if r.Spec.Image.Ref != "" {
			for _, img := range h.Images {
				if img == r.Spec.Image.Ref {
					sc += 100
				}
			}
		}
		// Mirrors cached: a clone there is a local copy plus a fetch.
		if r.Spec.Git != nil {
			for _, repo := range r.Spec.Git.Repositories {
				if slices.Contains(h.Mirrors, repo.URL) {
					sc += 50
				}
			}
		}
		for k, v := range r.Spec.Placement.Prefers {
			if h.Labels[k] == v {
				sc += 10
			}
		}
		// Spread: fewer running Runs first.
		sc -= h.UsedRuns
		if h.ID == r.AvoidHost {
			sc -= 100000
		}
		ok = append(ok, scored{h, sc})
	}
	if len(ok) == 0 {
		return nil, reason, nil
	}
	sort.SliceStable(ok, func(i, j int) bool { return ok[i].score > ok[j].score })
	return ok[0].h, "", nil
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// noHost records why a Run is waiting, and asks a provider for a host if
// its pool has one.
func (s *Server) noHost(ctx context.Context, tx pgx.Tx, r pendingRun, wait string) error {
	if wait == "snapshot unavailable" {
		return setRunState(ctx, tx, r.TenantID, r.ID, StateLost, "its snapshot is no longer available", r.Epoch)
	}
	var provider string
	err := tx.QueryRow(ctx, `SELECT provider FROM pools WHERE name = $1 AND (tenant_id = $2 OR tenant_id IS NULL) AND NOT retired
		ORDER BY tenant_id NULLS LAST LIMIT 1`, r.Spec.Placement.Pool, r.TenantID).Scan(&provider)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if provider != "" && provider != "static" && wait != "waiting for snapshot upload" {
		if r.State != StateProvisioning {
			return setRunState(ctx, tx, r.TenantID, r.ID, StateProvisioning, "waiting for a host", r.Epoch)
		}
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE runs SET state_reason = $2 WHERE id = $1 AND state_reason <> $2`, r.ID, wait)
	return err
}

// assign creates the next placement: a new epoch, fenced. Everything the
// previous placement reports from now on is stale.
func (s *Server) assign(ctx context.Context, tx pgx.Tx, r pendingRun, h *candidateHost) error {
	epoch := r.Epoch + 1
	placementID := ids.New(ids.Placement)
	_, err := tx.Exec(ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, 'assigned', $6, now() + $7::interval)`,
		placementID, r.TenantID, r.ID, h.ID, epoch, r.Spec.Resources,
		// Generous first lease: pulling or building the image can be slow,
		// and heartbeats renew it once the runner has the placement.
		interval(s.cfg.LeaseDuration*4))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET current_epoch = $2, state = 'scheduled', state_reason = '', pending_input = NULL,
			place_on = NULL, avoid_host = NULL,
			first_scheduled_at = coalesce(first_scheduled_at, now()), updated_at = now()
		WHERE id = $1`, r.ID, epoch); err != nil {
		return err
	}
	if err := addEvent(ctx, tx, r.TenantID, r.ID, epoch, "state", map[string]any{"state": StateScheduled, "host": h.ID}); err != nil {
		return err
	}

	a := proto.Assign{RunID: r.ID, TenantID: r.TenantID, Epoch: epoch, Spec: r.Spec, ImageResolved: r.ImageResolved}
	if r.SnapshotID != nil || r.SessionID != "" {
		a.Resume = &proto.ResumeInfo{SessionID: r.SessionID}
		if r.SnapshotID != nil {
			var m proto.Manifest
			if err := tx.QueryRow(ctx, `SELECT manifest FROM snapshots WHERE id = $1`, *r.SnapshotID).Scan(&m); err != nil {
				return err
			}
			a.Resume.Snapshot = &m
		}
	}
	if len(r.PendingInput) > 0 {
		var in proto.Input
		if err := json.Unmarshal(r.PendingInput, &in); err == nil {
			a.Input = &in
		}
	}
	// Secrets are attached when the message is sent, from memory.
	if err := enqueue(ctx, tx, h.ID, r.ID, epoch, proto.MsgAssign, a); err != nil {
		return err
	}
	h.UsedRuns++
	h.UsedCPUs += r.Spec.Resources.CPUs
	h.UsedMem += int64(r.Spec.Resources.Memory)
	h.UsedDisk += int64(r.Spec.Resources.Disk)
	h.Tenants = append(h.Tenants, r.TenantID)
	return nil
}

// requestResume queues a stopped or lost Run for a new placement.
func (s *Server) requestResume(ctx context.Context, tx pgx.Tx, tenantID, runID string, input *proto.Input, why string) error {
	var in any
	if input != nil {
		in = input
	}
	// A resumed Run is not finished, whatever it was: retention must not
	// treat its blobs as those of a terminal Run. Without input, it keeps
	// what was pending (a migration's).
	_, err := tx.Exec(ctx, `UPDATE runs SET state = 'resuming', state_reason = $3, pending_input = coalesce($2, pending_input), updated_at = now(),
			exit_code = NULL, finished_at = NULL
		WHERE id = $1`, runID, in, why)
	if err != nil {
		return err
	}
	return addEvent(ctx, tx, tenantID, runID, 0, "state", map[string]any{"state": StateResuming, "reason": why})
}
