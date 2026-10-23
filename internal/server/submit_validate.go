package server

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// validateSubmit is POST /v1/runs?validateOnly=true after submit's own
// validation and normalization: the pool and shaped size the Run would get,
// then whether it would start now, decided by the scheduler's own admission
// (spotAdmission, or hostFit for a normal Run) over the same candidate hosts.
// It writes nothing: no Run, event, secret, capacity history or pledge.
func (s *Server) validateSubmit(ctx context.Context, p Principal, stored spec.RunSpec, raw spec.Resources) (*submitRunOutput, error) {
	answer := &SubmitAnswer{Valid: true}
	var r pendingRun
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		if err := checkRunQuota(ctx, tx, p.TenantID); err != nil {
			return err
		}
		rp, err := resolveSubmitPool(ctx, tx, p.TenantID, &stored)
		if err != nil {
			return err
		}
		stored.Placement.Pool = rp.Name
		if _, _, err := resolveSubmitSize(ctx, tx, rp, raw, &stored.Resources); err != nil {
			return err
		}
		answer.EffectiveResources = &stored.Resources
		answer.EffectivePool = rp.Name
		if rp.ID != nil {
			answer.EffectivePoolID = *rp.ID
		}
		// Shape: hostFit's shaped-pool gate, as the scheduler sets it.
		sh, err := poolShape(ctx, tx, rp.ID)
		if err != nil {
			return err
		}
		r = pendingRun{TenantID: p.TenantID, Spec: stored, PoolID: rp.ID, Shape: sh}
		s.markManagedAuth(&r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Host capacity is the system's; the answer names only the eligible
	// host and per-kind counts, never another tenant's Runs or usage.
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION READ ONLY`); err != nil {
			return err
		}
		ids, err := s.eligibleHostIDs(ctx, tx, []*string{r.PoolID}, []string{r.TenantID}, []string{""})
		if err != nil {
			return err
		}
		hs, err := s.candidateHosts(ctx, tx, ids)
		if err != nil {
			return err
		}
		var adm admission
		if r.Spec.Spot {
			demand, ok, err := normalDemand(ctx, tx, hs)
			if err != nil {
				return err
			}
			s.markManagedAuth(slicePtrs(demand)...)
			if ok {
				adm = spotAdmission(r, hs, demand, s.now())
			} else {
				adm.Blockers = []AdmissionBlocker{{Code: blockNormalPending, Hosts: len(hs)}}
			}
		} else {
			adm = normalAdmission(r, hs)
		}
		answer.Blockers = adm.Blockers
		if adm.Host != nil {
			answer.WouldStartNow, answer.EligibleHost = true, adm.Host.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &submitRunOutput{Status: http.StatusOK, Body: answer}, nil
}
