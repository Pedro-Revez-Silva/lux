package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/sizing"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// Fractional pool sizes (docs/design/sizes.md): a shaped pool's ladder, a
// Run's size request, and how both are persisted.

// PoolShape is an operator-declared nominal host shape; pools.shape.
type PoolShape struct {
	Family      string     `json:"family" doc:"The size family, a lowercase slug (e.g. m): the first part of every size name. Pool metadata, never inferred from the provider."`
	Arch        string     `json:"arch" enum:"arm64,amd64" doc:"The hosts' architecture, the last part of every size name."`
	CPUs        float64    `json:"cpus" doc:"Nominal CPUs of a whole host: a power of two."`
	Memory      spec.Bytes `json:"memory" doc:"Nominal memory of a whole host, in the terms Runs ask in."`
	Disk        spec.Bytes `json:"disk" doc:"Nominal disk of a whole host (from its launch template, not its instance type)."`
	MinCPUs     float64    `json:"minCpus" doc:"The smallest size's CPUs: a power of two, at least 0.5. The ladder halves the whole host down to it."`
	DefaultCPUs float64    `json:"defaultCpus" doc:"The CPUs of the size a Run that asks for no resources gets: one of the ladder's."`
}

// PoolSize is one entry of a shaped pool's ladder.
type PoolSize struct {
	Name     string     `json:"name" example:"m-2-arm64"`
	CPUs     float64    `json:"cpus"`
	Memory   spec.Bytes `json:"memory" doc:"Bytes."`
	Disk     spec.Bytes `json:"disk" doc:"Bytes."`
	Fraction float64    `json:"fraction" doc:"Of a whole host: 1, 0.5, 0.25, ..."`
	Default  bool       `json:"default,omitempty" doc:"The size a Run that asks for no resources gets."`
}

func (sh PoolShape) model() (*sizing.Model, error) {
	return sizing.New(sizing.Shape{
		Dimensions: sizing.Dimensions{CPUs: sh.CPUs, Memory: sh.Memory, Disk: sh.Disk},
		Family:     sh.Family, Arch: sh.Arch, MinCPUs: sh.MinCPUs, DefaultCPUs: sh.DefaultCPUs,
	})
}

// storedModel is the model of pool's stored shape. Every write checks the
// shape (CheckPoolShape), so New refuses a stored one only if its rules
// tightened after it was stored: a 500 pool_shape_invalid naming the pool.
func (sh PoolShape) storedModel(pool string) (*sizing.Model, error) {
	m, err := sh.model()
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "pool_shape_invalid", "pool %s: stored shape: %v", pool, err)
	}
	return m, nil
}

// sizes is sh's ladder, smallest first. A stored shape New refuses (see
// storedModel) lists no sizes rather than failing the whole pool listing;
// submits and resumes into the pool get pool_shape_invalid. scanPool has no
// logger to report it with.
func (sh PoolShape) sizes() []PoolSize {
	m, err := sh.model()
	if err != nil {
		return nil
	}
	def := m.Default().Name
	var out []PoolSize
	for _, e := range m.List() {
		out = append(out, PoolSize{Name: e.Name, CPUs: e.CPUs, Memory: e.Memory, Disk: e.Disk, Fraction: e.Fraction, Default: e.Name == def})
	}
	return out
}

// hasSize reports whether name is a size of sh's current ladder whose
// cpus, memory and disk are cur's.
func (sh PoolShape) hasSize(name string, cur spec.Resources) bool {
	m, err := sh.model()
	return err == nil && sizeCurrent(m, name, cur)
}

// sizeCurrent: a shape change can keep a size's name and change its
// memory or disk, so the entry's dimensions must match too.
func sizeCurrent(m *sizing.Model, name string, cur spec.Resources) bool {
	e, err := m.Parse(name)
	return err == nil && e.CPUs == cur.CPUs && e.Memory == cur.Memory && e.Disk == cur.Disk
}

// shapeKeys are ParseShape's keys, in the order its errors list them.
var shapeKeys = []string{"family", "arch", "cpus", "memory", "disk", "min-cpus", "default-cpus"}

// ParseShape reads `luxd admin create-pool --shape` and `lux pools set
// --shape`: family=m,arch=arm64,cpus=8,memory=32Gi,disk=160Gi,min-cpus=0.5,default-cpus=2,
// every key required. Values are checked by ValidShape, not here.
func ParseShape(s string) (*PoolShape, error) {
	var sh PoolShape
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !slices.Contains(shapeKeys, k) {
			return nil, fmt.Errorf("shape %q: want %s=VALUE pairs", s, strings.Join(shapeKeys, "|"))
		}
		var err error
		switch k {
		case "family":
			sh.Family = v
		case "arch":
			sh.Arch = v
		case "cpus":
			sh.CPUs, err = strconv.ParseFloat(v, 64)
		case "min-cpus":
			sh.MinCPUs, err = strconv.ParseFloat(v, 64)
		case "default-cpus":
			sh.DefaultCPUs, err = strconv.ParseFloat(v, 64)
		case "memory":
			err = json.Unmarshal([]byte(strconv.Quote(v)), &sh.Memory)
		case "disk":
			err = json.Unmarshal([]byte(strconv.Quote(v)), &sh.Disk)
		}
		if err != nil {
			return nil, fmt.Errorf("shape %q: %s: invalid value %q", s, k, v)
		}
		seen[k] = true
	}
	var missing []string
	for _, k := range shapeKeys {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("shape %q: missing %s", s, strings.Join(missing, ", "))
	}
	return &sh, nil
}

// ValidShape is sh's model, or a 422 invalid_pool for a shape sizing.New
// refuses.
func ValidShape(sh PoolShape) (*sizing.Model, error) {
	m, err := sh.model()
	if err != nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_pool", "%s", err.Error())
	}
	return m, nil
}

// catalogType is an instance type as the instance-type catalog describes
// it. CPUs and Memory are nil when unknown; Archs are Go's names
// (ec2Archs) of its architectures, in catalog order, those not mapped left
// out.
type catalogType struct {
	CPUs   *int
	Memory *int64
	Archs  []string
}

// catalogTypes are the catalog's entries for types in provider's region;
// a type it lacks is absent.
func catalogTypes(ctx context.Context, tx pgx.Tx, provider, region string, types []string) (map[string]catalogType, error) {
	rows, err := tx.Query(ctx, `SELECT instance_type, vcpus, memory_bytes, coalesce(architectures, '{}') FROM instance_types
		WHERE provider = $1 AND region = $2 AND instance_type = ANY($3)`, provider, region, types)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]catalogType{}
	for rows.Next() {
		var name string
		var ct catalogType
		var archs []string
		if err := rows.Scan(&name, &ct.CPUs, &ct.Memory, &archs); err != nil {
			return nil, err
		}
		for _, a := range archs {
			if g := ec2Archs[a]; g != "" && !slices.Contains(ct.Archs, g) {
				ct.Archs = append(ct.Archs, g)
			}
		}
		out[name] = ct
	}
	return out, rows.Err()
}

// CheckPoolShape validates a pool's shape (nil: none) and, for an ec2
// pool, its template's instanceType and fallbackInstanceTypes against it,
// as far as the instance-type catalog describes them: each must match the
// shape's architecture and have at least its CPUs and memory. Disk comes
// from the launch template, which every type shares, so it is not the
// type's to check. A type the catalog lacks is not checked.
func CheckPoolShape(ctx context.Context, db *store.Store, provider string, template map[string]any, sh *PoolShape) error {
	if sh == nil {
		return nil
	}
	m, err := ValidShape(*sh)
	if err != nil {
		return err
	}
	if provider != "ec2" {
		return nil
	}
	region, _ := template["region"].(string)
	primary, _ := template["instanceType"].(string)
	if region == "" || primary == "" {
		return nil
	}
	types := []string{primary}
	if list, ok := template["fallbackInstanceTypes"].([]any); ok {
		for _, v := range list {
			if t, ok := v.(string); ok {
				types = append(types, t)
			}
		}
	}
	return db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		known, err := catalogTypes(ctx, tx, provider, region, types)
		if err != nil {
			return err
		}
		for i, t := range types {
			ct, ok := known[t]
			if !ok || ct.CPUs == nil || ct.Memory == nil {
				continue
			}
			// The shape's arch when the type offers it, else the first it
			// offers; a type with no arch lux knows is not refused for it.
			arch := sh.Arch
			if !slices.Contains(ct.Archs, sh.Arch) && len(ct.Archs) > 0 {
				arch = ct.Archs[0]
			}
			d := sizing.Dimensions{CPUs: float64(*ct.CPUs), Memory: spec.Bytes(*ct.Memory), Disk: sh.Disk}
			if err := m.ValidateFallback(arch, d); err != nil {
				field := "template.instanceType"
				if i > 0 {
					field = fmt.Sprintf("template.fallbackInstanceTypes[%d]", i-1)
				}
				return errf(http.StatusUnprocessableEntity, "invalid_pool", "invalid pool shape: %s %s: %s", field, t, err.Error())
			}
		}
		return nil
	})
}

// SizeRequest is what a Run asked for in resources, as the sizing model
// reads it: only the fields given (a zero is not given).
type SizeRequest struct {
	Size   string      `json:"size,omitempty"`
	CPUs   *float64    `json:"cpus,omitempty"`
	Memory *spec.Bytes `json:"memory,omitempty" doc:"Bytes."`
	Disk   *spec.Bytes `json:"disk,omitempty" doc:"Bytes."`
}

// SizeResolved is the size a Run has, and its resources.
type SizeResolved struct {
	Size   string     `json:"size"`
	CPUs   float64    `json:"cpus"`
	Memory spec.Bytes `json:"memory" doc:"Bytes."`
	Disk   spec.Bytes `json:"disk" doc:"Bytes."`
}

// RunSizing is a Run's request and resolution on a shaped pool.
type RunSizing struct {
	Requested *SizeRequest  `json:"requested,omitempty" doc:"What its submit asked for in resources.size, cpus, memory and disk, only those given (empty: none, the pool's default size). Absent for a Run submitted to an unshaped pool. A resume's request is in its answer's resize and its resume.requested event."`
	Resolved  *SizeResolved `json:"resolved,omitempty" doc:"The size the Run has now, and its cpus, memory and disk (as spec.resources). Absent while it is in an unshaped pool, or while its pool's current sizes have no size of that name with the Run's cpus, memory and disk (the shape changed since, even if the name stayed): its next resume resolves it again."`
}

// sizeRequestOf is r's size request: its size, cpus, memory and disk
// that are not zero.
func sizeRequestOf(r spec.Resources) SizeRequest {
	var q SizeRequest
	q.Size = r.Size
	if r.CPUs != 0 {
		q.CPUs = &r.CPUs
	}
	if r.Memory != 0 {
		q.Memory = &r.Memory
	}
	if r.Disk != 0 {
		q.Disk = &r.Disk
	}
	return q
}

func (q SizeRequest) model() sizing.Request {
	var r sizing.Request
	r.Size = sizing.Optional[string]{Present: q.Size != "", Value: q.Size}
	if q.CPUs != nil {
		r.CPUs = sizing.Optional[float64]{Present: true, Value: *q.CPUs}
	}
	if q.Memory != nil {
		r.Memory = sizing.Optional[spec.Bytes]{Present: true, Value: *q.Memory}
	}
	if q.Disk != nil {
		r.Disk = sizing.Optional[spec.Bytes]{Present: true, Value: *q.Disk}
	}
	return r
}

func (q SizeRequest) equal(o SizeRequest) bool {
	return q.Size == o.Size && samePtr(q.CPUs, o.CPUs) && samePtr(q.Memory, o.Memory) && samePtr(q.Disk, o.Disk)
}

// samePtr: both nil, or both set to equal values.
func samePtr[T comparable](a, b *T) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// sizingError is the API error for a sizing.Resolve failure in pool.
func sizingError(pool string, err error) error {
	var over *sizing.OversizedError
	switch {
	case errors.As(err, &over):
		return errf(http.StatusUnprocessableEntity, "size_too_large", "pool %s: %s", pool, err.Error())
	case errors.Is(err, sizing.ErrConflict):
		return errf(http.StatusUnprocessableEntity, "size_conflict", "pool %s: resources.size cannot be combined with cpus, memory or disk", pool)
	case errors.Is(err, sizing.ErrUnknownSize):
		return errf(http.StatusUnprocessableEntity, "unknown_size", "pool %s: %s", pool, err.Error())
	case errors.Is(err, sizing.ErrInvalidRequest):
		return invalidSpec(&spec.ValidationError{Problems: []string{"resources: cpus, memory and disk must be greater than 0"}})
	}
	return err
}

func errSizeUnsupported(pool string) error {
	return errf(http.StatusUnprocessableEntity, "size_unsupported", "pool %s has no shape, so no sizes: ask with resources.cpus, memory and disk", pool)
}

// poolShape is pool id's shape; nil for an unshaped pool or none.
func poolShape(ctx context.Context, tx pgx.Tx, poolID *string) (*PoolShape, error) {
	if poolID == nil || *poolID == "" {
		return nil, nil
	}
	var sh *PoolShape
	err := tx.QueryRow(ctx, `SELECT shape FROM pools WHERE id = $1`, *poolID).Scan(&sh)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return sh, err
}

// resolveSubmitSize resolves a submit's resources as asked (raw: before
// luxd's defaults) in its pool. An unshaped pool refuses a size and leaves
// res as Normalize filled it; a shaped one sets res's cpus, memory and
// disk to the size it resolves to (pids are not the size's). The returned
// request and size are nil for an unshaped pool.
func resolveSubmitSize(ctx context.Context, tx pgx.Tx, rp resolvedPool, raw spec.Resources, res *spec.Resources) (*SizeRequest, *string, error) {
	sh, err := poolShape(ctx, tx, rp.ID)
	if err != nil {
		return nil, nil, err
	}
	if sh == nil {
		if raw.Size != "" {
			return nil, nil, errSizeUnsupported(rp.Name)
		}
		return nil, nil, nil
	}
	m, err := sh.storedModel(rp.Name)
	if err != nil {
		return nil, nil, err
	}
	req := sizeRequestOf(raw)
	got, err := m.Resolve(req.model())
	if err != nil {
		return nil, nil, sizingError(rp.Name, err)
	}
	res.CPUs, res.Memory, res.Disk = got.Resolved.CPUs, got.Resolved.Memory, got.Resolved.Disk
	return &req, &got.Resolved.Name, nil
}

// checkReplay refuses an idempotent re-submit whose resources differ from
// the shaped Run's submit asked for (requested, never resolved: two
// requests that round to one size are still different requests). A Run
// submitted to an unshaped pool records no request, and is returned as it
// always was.
func checkReplay(existing *Run, raw spec.Resources) error {
	if existing.Sizing == nil || existing.Sizing.Requested == nil {
		return nil
	}
	if !existing.Sizing.Requested.equal(sizeRequestOf(raw)) {
		return errf(http.StatusConflict, "idempotency_conflict",
			"Idempotency-Key was used for Run %s, submitted with other resources", existing.ID)
	}
	return nil
}

// savedDisk is the peak disk use of the placement that took the snapshot
// the Run resumes from, and whether that placement reported its final
// sample (exited). The runner reports the snapshot before the exit status
// carrying its final disk sample, and only that status makes a placement
// 'exited'. In any other state (lost between the two) the peak may
// predate data the snapshot holds.
func savedDisk(ctx context.Context, tx pgx.Tx, runID string) (peak *int64, final, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT p.peak_disk_bytes, p.state = 'exited' FROM runs r
		JOIN snapshots sn ON sn.id = r.snapshot_id AND sn.run_id = r.id
		JOIN placements p ON p.id = sn.placement_id
		WHERE r.id = $1`, runID).Scan(&peak, &final)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	return peak, final, err == nil, err
}

// diskShrinkRefusal says why the Run may not shrink to disk: "" when its
// saved state fits disk with diskShrinkFloor's headroom. peak is set only
// when a final measurement refused it (the peak disk use it read).
func diskShrinkRefusal(ctx context.Context, tx pgx.Tx, runID string, disk spec.Bytes) (reason string, peak *int64, err error) {
	measured, final, found, err := savedDisk(ctx, tx, runID)
	switch {
	case err != nil:
		return "", nil, err
	case found && !final:
		return "the placement that took the snapshot it resumes from ended without reporting its final disk use", nil, nil
	case measured == nil:
		return "no disk use is recorded for the snapshot it resumes from", nil, nil
	case int64(disk) < diskShrinkFloor(*measured):
		return fmt.Sprintf("its saved state used up to %s, which needs at least %s of disk (that plus max(25%%, 1 GiB))",
			bytesText(*measured), bytesText(diskShrinkFloor(*measured))), measured, nil
	}
	return "", nil, nil
}

// resizeShaped resolves a resume's request r in the shaped pool (its
// model m) and writes the size into the Run: its spec's cpus, memory and
// disk and its resolved_size. cur is what the Run has; asked is what the
// resume asked for (zero for a pool move that asked for nothing). A size
// with less disk than the Run has must still hold its saved state with
// the shrink headroom unshaped pools use; otherwise it is refused, never
// kept at the old disk, which would break the size's coupling.
func resizeShaped(ctx context.Context, tx pgx.Tx, runID, pool string, cur spec.Resources, m *sizing.Model, asked spec.Resources, r sizing.Request) (*Resize, error) {
	got, err := m.Resolve(r)
	if err != nil {
		return nil, sizingError(pool, err)
	}
	e := got.Resolved
	rz := &Resize{Requested: asked, Applied: cur, Size: e.Name}
	rz.Applied.CPUs, rz.Applied.Memory, rz.Applied.Disk = e.CPUs, e.Memory, e.Disk
	set := map[string]any{"cpus": e.CPUs, "memory": e.Memory, "disk": e.Disk}
	if asked.DiskGrace.Set {
		rz.Applied.DiskGrace = asked.DiskGrace
		set["diskGrace"] = asked.DiskGrace
	}
	if e.Disk < cur.Disk {
		// TODO(sizes phase 4): round up to the smallest size whose disk
		// fits the saved state, instead of refusing.
		reason, _, err := diskShrinkRefusal(ctx, tx, runID, e.Disk)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return nil, errSizeBelowState(e, reason)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET resolved_size = $3,
			spec = jsonb_set(spec, '{resources}', coalesce(spec->'resources', '{}') || $2::jsonb) WHERE id = $1`,
		runID, set, e.Name); err != nil {
		return nil, err
	}
	return rz, nil
}

func errSizeBelowState(e sizing.Entry, why string) error {
	return errf(http.StatusUnprocessableEntity, "size_below_saved_state",
		"size %s has %s of disk, less than the Run has now: %s; a shaped pool never keeps a larger disk than its size, so choose a size with more disk",
		e.Name, bytesText(int64(e.Disk)), why)
}

// resumeSizing is what resumeSize sizes a Run against.
type resumeSizing struct {
	runID    string
	from     BoundPool      // the pool the Run is in now
	dest     BoundPool      // the pool it is in from this resume on
	shape    *PoolShape     // dest's; nil: unshaped
	resolved string         // runs.resolved_size; "": none
	cur      spec.Resources // the Run's stored resources
}

func (z resumeSizing) moving() bool { return z.dest.ID != z.from.ID }

// resumeSize applies a resume's resources and pool move to a Run's size,
// before the move itself (rebindPool) checks the Run fits in z.dest. An
// unshaped destination keeps today's rules (resizeRun) and drops the Run's
// size. A shaped one resolves what the resume asked; asked nothing, it
// rounds the Run's current cpus, memory and disk up to one of its sizes on
// a move, or when the Run's size is no longer on the pool's ladder with
// those resources (its shape changed). nil, nil: nothing to change.
func resumeSize(ctx context.Context, tx pgx.Tx, z resumeSizing, req *resumeResources) (*Resize, error) {
	var asked spec.Resources
	if req != nil {
		var err error
		if asked, err = req.check(); err != nil {
			return nil, err
		}
	}
	if z.shape == nil {
		if asked.Size != "" {
			return nil, errSizeUnsupported(z.dest.Name)
		}
		// The Run kept a size from a shaped pool: the one it leaves, or its
		// own before its shape was removed. An unshaped pool has no sizes.
		if z.resolved != "" {
			if _, err := tx.Exec(ctx, `UPDATE runs SET resolved_size = NULL WHERE id = $1`, z.runID); err != nil {
				return nil, err
			}
		}
		if req == nil {
			return nil, nil
		}
		return resizeRun(ctx, tx, z.runID, z.cur, asked)
	}
	m, err := z.shape.storedModel(z.dest.Name)
	if err != nil {
		return nil, err
	}
	// diskGrace is no size dimension: a resume asking only for it keeps
	// the Run's size.
	switch {
	case sizeRequestOf(asked) != (SizeRequest{}):
		return resizeShaped(ctx, tx, z.runID, z.dest.Name, z.cur, m, asked, sizeRequestOf(asked).model())
	case z.moving(), z.resolved != "" && !sizeCurrent(m, z.resolved, z.cur):
		held := sizeRequestOf(spec.Resources{CPUs: z.cur.CPUs, Memory: z.cur.Memory, Disk: z.cur.Disk})
		return resizeShaped(ctx, tx, z.runID, z.dest.Name, z.cur, m, asked, held.model())
	case req != nil:
		return resizeRun(ctx, tx, z.runID, z.cur, asked)
	}
	return nil, nil
}
