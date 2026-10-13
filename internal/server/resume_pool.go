package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

type resumeTarget struct {
	PoolID string `json:"poolId" doc:"Move the Run to this pool from this resume on, by id: the same field and rule as a submit's placement.poolId (the tenant's active pool with that id, else the platform's; any other is 422 unknown_pool). The Run's pool (pool and poolId in GET) and its spec's placement.pool and placement.poolId become that pool's, so later resumes, retries and migrations stay there. Checked before anything changes, with resources as this resume sets them: 422 platform_mismatch when the platforms the image offered at its last placement and the pool's hosts' are both known and share none; 422 pool_unfit when no host the pool has or launches can ever take the Run (nested containers, placement.requires labels, cpus, memory or disk beyond a whole host). On a Run already resuming: its own pool, 202; another, 409 not_resumable. With an operator's to, that host must be in this pool (else 422 invalid_spec)."`
}

// BoundPool names a pool by its current name and its id.
type BoundPool struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// ResumePlacement is, in a resume's answer that named a pool, where the
// Run is bound from now on.
type ResumePlacement struct {
	Pool              string     `json:"pool" doc:"The pool's current name."`
	PoolID            string     `json:"poolId"`
	From              *BoundPool `json:"from,omitempty" doc:"The pool the Run left; absent when it was already there."`
	PlatformUnchecked bool       `json:"platformUnchecked,omitempty" doc:"The Run moved without its platform being checked: lux does not know the platforms its image offers (no placement reported them, or it is an image.build, rebuilt on each host) or those of the pool's hosts (none registered, and the catalog lacks its template's instance types), or either."`
}

// poolProfile is one kind of host a pool has or will launch: a host of it,
// or (ec2) the host its current template launches. A zero capacity field
// is unknown, as hostFit reads it: no limit.
type poolProfile struct {
	Labels      map[string]string
	Capacity    proto.Capacity
	LabelsKnown bool
}

// poolFacts is what lux knows of the hosts a pool can give a Run.
type poolFacts struct {
	Provider string
	// Archs: GOARCH names of its hosts' and template's platforms; empty
	// when none is known.
	Archs    []string
	Profiles []poolProfile
	// NestedMismatch: an ec2 template opts into nested containers, but its
	// latest registrations lacked nested=true (hostExpectation).
	NestedMismatch bool
}

// ec2Archs maps EC2's architecture names (DescribeInstanceTypes) to Go's,
// which runners report.
var ec2Archs = map[string]string{"x86_64": "amd64", "arm64": "arm64", "i386": "386"}

// loadPoolFacts reads, as the system (instance_types is no tenant's), what
// pool poolID offers a Run of tenantID (poolFactsTx).
func (s *Server) loadPoolFacts(ctx context.Context, poolID, tenantID string) (poolFacts, error) {
	var f poolFacts
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		f, err = s.poolFactsTx(ctx, tx, poolID, tenantID)
		return err
	})
	return f, err
}

// poolFactsTx is what pool poolID offers a Run of tenantID ("": of the
// pool's owner), in a system transaction. Profiles: a static pool's live
// hosts and latest expectationWindow registrations; an ec2 pool's live
// hosts plus the host the planner would launch for it (futureHost).
// Archs also come from the current template's registrations and its
// instance types in the catalog.
func (s *Server) poolFactsTx(ctx context.Context, tx pgx.Tx, poolID, tenantID string) (poolFacts, error) {
	var f poolFacts
	pl := poolRow{ID: poolID}
	err := tx.QueryRow(ctx, `SELECT name, provider, tenant_id, template FROM pools WHERE id = $1`, poolID).
		Scan(&pl.Name, &f.Provider, &pl.TenantID, &pl.Template)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	pl.Provider = f.Provider
	// Live hosts, then the latest registrations: two index-bounded
	// branches, so a pool's terminated history is never scanned.
	rows, err := tx.Query(ctx, `SELECT h.arch, h.labels, h.capacity, h.state IN ('ready', 'draining')
		FROM pools p CROSS JOIN LATERAL (
			(SELECT id, arch, labels, capacity, state FROM hosts
				WHERE pool_id = p.id AND (tenant_id IS NULL OR tenant_id = coalesce(nullif($2, ''), p.tenant_id))
				  AND registered_at IS NOT NULL AND state IN ('ready', 'draining'))
			UNION
			(SELECT id, arch, labels, capacity, state FROM hosts
				WHERE pool_id = p.id AND (tenant_id IS NULL OR tenant_id = coalesce(nullif($2, ''), p.tenant_id))
				  AND registered_at IS NOT NULL
				  AND (p.provider = 'static' OR provision_requested_at IS NOT NULL AND launch_template = p.template)
				ORDER BY registered_at DESC, id DESC LIMIT $3)) h
		WHERE p.id = $1`, poolID, tenantID, expectationWindow)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var arch string
		var labels map[string]string
		var capacity proto.Capacity
		var live bool
		if err := rows.Scan(&arch, &labels, &capacity, &live); err != nil {
			return f, err
		}
		if arch != "" && !slices.Contains(f.Archs, arch) {
			f.Archs = append(f.Archs, arch)
		}
		// An ec2 pool's past hosts are not hosts it has: what it
		// launches next is futureHost's.
		if live || f.Provider != "ec2" {
			f.Profiles = append(f.Profiles, poolProfile{Labels: labels, Capacity: capacity, LabelsKnown: true})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return f, err
	}
	if f.Provider == "ec2" {
		future, err := s.futureHost(ctx, tx, pl, &f)
		if err != nil {
			return f, err
		}
		f.Profiles = append(f.Profiles, future...)
	}
	slices.Sort(f.Archs)
	return f, nil
}

// futureHost is the hosts planCapacity would launch for ec2 pool pl. The
// planned host is virtual()'s: hostExpectation's capacity and shared
// labels, nested only when the template opts in and its registrations do
// not contradict it. When that host is too small for a Run, the planner
// probes with one more launch, which ec2.Launch starts as the template's
// instanceType (fallbacks only on a capacity shortage): the probe host has
// the planned labels and disk and the catalog's size of instanceType
// alone. With no registration there is only the bootstrap host, sized the
// same way; a type the catalog lacks leaves its size unknown. It adds the
// catalog's archs of every candidate type to f.Archs.
func (s *Server) futureHost(ctx context.Context, tx pgx.Tx, pl poolRow, f *poolFacts) ([]poolProfile, error) {
	expected, _, err := s.hostExpectation(ctx, tx, pl)
	if err != nil {
		return nil, err
	}
	future := poolProfile{Labels: map[string]string{}}
	if expected != nil {
		maps.Copy(future.Labels, expected.Labels)
		future.Capacity, future.LabelsKnown = expected.Capacity, true
	}
	nested := templateOffersNested(pl)
	f.NestedMismatch = nested && expected != nil && expected.nestedMismatch
	delete(future.Labels, "nested")
	if nested && !f.NestedMismatch {
		future.Labels["nested"] = "true"
	}
	var t struct {
		Region                string   `json:"region"`
		InstanceType          string   `json:"instanceType"`
		FallbackInstanceTypes []string `json:"fallbackInstanceTypes"`
	}
	_ = json.Unmarshal(pl.Template, &t)
	if t.InstanceType == "" || t.Region == "" {
		return []poolProfile{future}, nil
	}
	known, err := catalogTypes(ctx, tx, pl.Provider, t.Region, append([]string{t.InstanceType}, t.FallbackInstanceTypes...))
	if err != nil {
		return nil, err
	}
	for _, ct := range known {
		for _, g := range ct.Archs {
			if !slices.Contains(f.Archs, g) {
				f.Archs = append(f.Archs, g)
			}
		}
	}
	ct, ok := known[t.InstanceType]
	if !ok || ct.CPUs == nil || ct.Memory == nil {
		return []poolProfile{future}, nil
	}
	// Disk is never the instance type's.
	if expected == nil {
		future.Capacity.CPUs, future.Capacity.Memory = float64(*ct.CPUs), *ct.Memory
		return []poolProfile{future}, nil
	}
	probe := poolProfile{Labels: maps.Clone(future.Labels), Capacity: future.Capacity, LabelsKnown: true}
	// A new host reports its own name and arch: never a previous host's.
	delete(probe.Labels, "name")
	delete(probe.Labels, "arch")
	var runnable []string
	for _, g := range ct.Archs {
		if slices.Contains(runnerArches, g) {
			runnable = append(runnable, g)
		}
	}
	if len(runnable) == 1 {
		probe.Labels["arch"] = runnable[0]
	}
	probe.Capacity.CPUs, probe.Capacity.Memory = float64(*ct.CPUs), *ct.Memory
	return []poolProfile{future, probe}, nil
}

// platforms are f's archs as os/arch; runners are linux only.
func (f poolFacts) platforms() []string {
	out := make([]string, 0, len(f.Archs))
	for _, a := range f.Archs {
		out = append(out, "linux/"+a)
	}
	return out
}

// nestedContainers is whether a nested Run fits a profile of f, as
// checkPoolFit's nested blocker judges it; nil when f has no profile (a
// static pool no host registered in), where that check refuses nothing.
func (f poolFacts) nestedContainers() *bool {
	if len(f.Profiles) == 0 {
		return nil
	}
	nested := slices.ContainsFunc(f.Profiles, func(p poolProfile) bool { return p.Labels["nested"] == "true" })
	return &nested
}

// withPoolFacts sets each of pools' platforms and capabilities as a resume
// of tenantID's Run ("": each pool's owner's) would be checked against.
func (s *Server) withPoolFacts(ctx context.Context, tx pgx.Tx, pools []Pool, tenantID string) error {
	for i := range pools {
		f, err := s.poolFactsTx(ctx, tx, pools[i].ID, tenantID)
		if err != nil {
			return err
		}
		if ps := f.platforms(); len(ps) > 0 {
			pools[i].Platforms = ps
		}
		pools[i].Capabilities = &PoolCapabilities{NestedContainers: f.nestedContainers()}
	}
	return nil
}

// lastImagePlatforms are the image platforms of the Run's latest placement
// that reported any; nil when none did.
func lastImagePlatforms(ctx context.Context, tx pgx.Tx, runID string) ([]string, error) {
	var ps []string
	err := tx.QueryRow(ctx, `SELECT image_platforms FROM placements
		WHERE run_id = $1 AND image_platforms IS NOT NULL ORDER BY epoch DESC LIMIT 1`, runID).Scan(&ps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ps, err
}

// checkPlatform refuses a move to pool name, of facts f, none of whose
// platforms the image offers. Unknown on either side is no refusal:
// unchecked reports it.
func checkPlatform(f poolFacts, name string, imagePlatforms []string) (unchecked bool, err error) {
	if len(imagePlatforms) == 0 || len(f.Archs) == 0 {
		return true, nil
	}
	for _, p := range imagePlatforms {
		parts := strings.Split(p, "/")
		if len(parts) >= 2 && parts[0] == "linux" && slices.Contains(f.Archs, parts[1]) {
			return false, nil
		}
	}
	return false, errf(http.StatusUnprocessableEntity, "platform_mismatch", "pool %s runs %s; this Run's image offers only %s",
		name, strings.Join(f.platforms(), ", "), strings.Join(imagePlatforms, ", "))
}

// checkPoolFit refuses a move to pool name, of facts f, where no host it
// has or launches could ever take the Run, empty: hostFit's pool-decided
// blockers (nested containers, required labels, resources) on each
// profile. A pool lux knows nothing of (a static pool no host registered
// in) takes it.
func checkPoolFit(f poolFacts, name, poolID string, sp spec.RunSpec) error {
	if len(f.Profiles) == 0 {
		return nil
	}
	r := pendingRun{Spec: sp, PoolID: &poolID}
	var best []fitBlocker
	fewest := 0
	for i, pr := range f.Profiles {
		h := &candidateHost{ID: fmt.Sprint("profile-", i), PoolID: poolID, Pool: name, Labels: pr.Labels, Capacity: pr.Capacity,
			Connected: true, Caps: []string{proto.CapSyncModes}}
		if h.Labels == nil {
			h.Labels = map[string]string{}
		}
		blockers := slices.DeleteFunc(hostFit(r, h), func(b fitBlocker) bool {
			switch b.Kind {
			case kindNested, kindResource:
				return false
			case kindLabel:
				return !pr.LabelsKnown
			}
			return true
		})
		if len(blockers) == 0 {
			return nil
		}
		switch {
		case best == nil || len(blockers) < fewest:
			best, fewest = blockers, len(blockers)
		case len(blockers) == fewest:
			best = append(best, blockers...)
		}
	}
	var why []string
	for _, b := range best {
		switch b.Kind {
		case kindNested:
			switch {
			case f.NestedMismatch:
				why = append(why, "it does not offer nested containers (its template's latest hosts registered without them)")
			case f.Provider == "ec2":
				why = append(why, "it does not offer nested containers (its template has no nestedContainers, and none of its hosts runs lux-runner --nested)")
			default:
				why = append(why, "none of its hosts offers nested containers (lux-runner --nested)")
			}
		case kindLabel:
			why = append(why, "none of its hosts has the labels placement.requires names")
		case kindResource:
			largest := 0.0
			for _, pr := range f.Profiles {
				switch b.Resource {
				case "cpus":
					largest = max(largest, pr.Capacity.CPUs)
				case "memory":
					largest = max(largest, float64(pr.Capacity.Memory))
				case "disk":
					largest = max(largest, float64(pr.Capacity.Disk))
				}
			}
			if b.Resource == "cpus" {
				why = append(why, fmt.Sprintf("it needs %g cpus; its largest host has %g", b.Requested, largest))
			} else {
				why = append(why, fmt.Sprintf("it needs %s of %s; its largest host has %s", bytesText(int64(b.Requested)), b.Resource, bytesText(int64(largest))))
			}
		}
	}
	slices.Sort(why)
	why = slices.Compact(why)
	return errf(http.StatusUnprocessableEntity, "pool_unfit", "pool %s cannot take this Run: %s", name, strings.Join(why, "; "))
}

// rebindPool moves a resumed Run (its spec sp, resources as this resume
// sets them) from pool from to pool to, after checking that to can take
// it: its platform, then its fit, against f, to's facts. A refusal changes
// nothing (the caller's transaction rolls back). Already in to: nothing to
// check or change.
func rebindPool(ctx context.Context, tx pgx.Tx, runID string, sp spec.RunSpec, from BoundPool, to resolvedPool, f poolFacts) (*ResumePlacement, error) {
	moved := &ResumePlacement{Pool: to.Name, PoolID: *to.ID}
	if *to.ID == from.ID {
		return moved, nil
	}
	// A built image is rebuilt on each host, so the platform its last
	// placement reported is that build host's arch: unknown here.
	var platforms []string
	var err error
	if sp.Image.Build == nil {
		if platforms, err = lastImagePlatforms(ctx, tx, runID); err != nil {
			return nil, err
		}
	}
	if moved.PlatformUnchecked, err = checkPlatform(f, to.Name, platforms); err != nil {
		return nil, err
	}
	if err := checkPoolFit(f, to.Name, *to.ID, sp); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET pool_id = $2,
			spec = jsonb_set(spec, '{placement}', coalesce(spec->'placement', '{}') || jsonb_build_object('pool', $3::text, 'poolId', $2::text))
		WHERE id = $1`, runID, *to.ID, to.Name); err != nil {
		return nil, err
	}
	moved.From = &from
	return moved, nil
}

// pendingPool is the pool move of the resume a resuming Run waits on; nil
// when it moved none.
func pendingPool(ctx context.Context, tx pgx.Tx, runID string) (*ResumePlacement, error) {
	var ev *struct {
		From              BoundPool `json:"from"`
		To                BoundPool `json:"to"`
		PlatformUnchecked bool      `json:"platformUnchecked"`
	}
	if err := pendingResumeData(ctx, tx, runID, "pool", &ev); err != nil || ev == nil {
		return nil, err
	}
	return &ResumePlacement{Pool: ev.To.Name, PoolID: ev.To.ID, From: &ev.From, PlatformUnchecked: ev.PlatformUnchecked}, nil
}
