package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/runner/disklimit"
	"github.com/marcioapm/lux/internal/spec"
)

// FreeMark is a free-space watermark: a percentage of the filesystem's
// capacity (Percent, in (0, 100)) or a number of bytes (Bytes > 0). The
// zero value is unset.
type FreeMark struct {
	Percent float64
	Bytes   int64
}

// Default watermarks of the low-free-space guard. Conservative: no shaped
// host has measured them yet.
var (
	DefaultDiskLowFree     = FreeMark{Percent: 5}
	DefaultDiskRecoverFree = FreeMark{Percent: 10}
)

// ParseFreeMark reads "5%" (a percentage of capacity, above 0 and below
// 100) or a size ("8GiB", "8Gi", a number of bytes, above 0). Its error
// does not repeat s: the caller names the flag or variable and the value.
func ParseFreeMark(s string) (FreeMark, error) {
	s = strings.TrimSpace(s)
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		p, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || math.IsNaN(p) || p <= 0 || p >= 100 {
			return FreeMark{}, errors.New("want a percentage above 0% and below 100%")
		}
		return FreeMark{Percent: p}, nil
	}
	b, err := spec.ParseBytes(s)
	if err != nil || b <= 0 {
		return FreeMark{}, errors.New("want a percentage (5%) or a size above 0 (8GiB)")
	}
	return FreeMark{Bytes: int64(b)}, nil
}

func (m FreeMark) IsZero() bool { return m == FreeMark{} }

func (m FreeMark) String() string {
	if m.Percent > 0 {
		return strconv.FormatFloat(m.Percent, 'f', -1, 64) + "%"
	}
	return spec.BytesText(m.Bytes)
}

// of is the mark in bytes of a filesystem of capacity bytes, rounded down.
func (m FreeMark) of(capacity int64) int64 {
	if m.Percent > 0 {
		return int64(float64(capacity) * m.Percent / 100)
	}
	return m.Bytes
}

// CheckFreeMarks refuses a low mark not below the recovery mark, when the
// two can be compared without the filesystem (same unit). newDiskGuard
// checks the rest against the filesystem's capacity at start.
func CheckFreeMarks(low, rec FreeMark) error {
	switch {
	case low.Percent > 0 && rec.Percent > 0 && low.Percent >= rec.Percent,
		low.Bytes > 0 && rec.Bytes > 0 && low.Bytes >= rec.Bytes:
		return fmt.Errorf("--disk-low-free %s must be below --disk-recover-free %s", low, rec)
	}
	return nil
}

// guardEvery bounds how often the guard measures the filesystem, apart
// from the disk samples: --usage-every when shorter.
const guardEvery = 2 * time.Second

// guardIdleRetry bounds how long a pass under pressure that found nothing
// to act on is trusted: until then, with nothing changed (gen), the next
// passes neither fork podman for images nor scan copies and placements.
const guardIdleRetry = time.Minute

// diskGuard is the low-free-space guard of a runner that scales disk. It
// measures the free bytes of the filesystem holding the data directory
// (watch, every guardEvery). Pressure starts when they fall below the low
// mark and ends only once they are at or above the recovery mark; each
// change is published at once. After each round of disk samples, a pass
// under pressure:
//
//  1. blocks admissions: the heartbeat carries the pressure, and luxd
//     places nothing more here;
//  2. reclaims caches toward the recovery mark: lux's unused images, as
//     --image-disk-high does, then the local copies of Runs no longer live
//     here that S3 holds (every snapshot reported and uploaded), oldest
//     exit first;
//  3. asks luxd to stop placements over their effective disk limit (within
//     their grace), largest excess first, until what is on its way back
//     covers what the filesystem lacks of the recovery mark, measured again
//     after each one. A placement at or under its limit is never stopped.
//
// What is on its way back (pending): a stop frees only the placement's
// writable layer when its container goes; its state volumes stay, beside
// their new export, as the Run's local copy until that copy's upload ends
// and step 2 discards it. So a stopping placement counts its writable
// layer, and a copy whose volumes' snapshot is uploading counts that
// snapshot's blob sizes (a lower bound, read from disk, so it survives a
// runner restart); once uploaded the copy is reclaimable, and step 2 frees
// it before any other Run is stopped. Neither counts its excess: that
// would stop too few, the space not coming back until the host TTL.
//
// A pass that finds nothing to reclaim, nothing on its way back and none to
// stop reports the pressure as unrelieved.
type diskGuard struct {
	log                *slog.Logger
	lowMark, recMark   FreeMark
	lowFree, recovFree int64

	// free measures the filesystem's free bytes (unprivileged).
	free func() (int64, error)
	// publish sends a heartbeat carrying state() soon, without waiting.
	publish func(ctx context.Context)
	// reclaim removes unused images toward freeing need bytes; whether it
	// removed any.
	reclaim func(ctx context.Context, need int64) bool
	// copies are the local copies of Runs not live here, oldest exit first.
	copies func() []localCopy
	// discard removes one reclaimable local copy, unless its Run became
	// live here meanwhile; whether it did.
	discard func(ctx context.Context, runID string) bool
	// victims are the live placements with a disk limit (overLimit picks
	// those over it).
	victims func() []diskVictim
	// stop asks luxd to stop v; at is the filesystem's state then.
	stop func(ctx context.Context, v diskVictim, at proto.DiskPressure)
	// gen changes whenever what a pass acts on may have: images,
	// placements, snapshot records, a sample going over its limit.
	gen func() uint64

	mu       sync.Mutex
	pressure *proto.DiskPressure
	// idle: the last pass under pressure found nothing, at idleGen.
	idle      bool
	idleGen   uint64
	idleSince time.Time
}

// diskVictim is a live placement with a disk limit: used bytes at its
// latest sample against limit, rw of them its writable layer. stopping: it
// is already on its way out (stopping, reported over its grace, or asked
// to stop by this guard).
type diskVictim struct {
	runID    string
	epoch    int
	used, rw int64
	limit    disklimit.Limit
	stopping bool
	p        *placement
}

func (v diskVictim) excess() int64 { return v.used - v.limit.Effective }

// localCopy is a Run's local state on this host (state volumes, snapshot
// files), its placement no longer live. reclaimable: S3 holds all of it
// (every snapshot record reported and uploaded, the volumes' own snapshot
// among them, or volumes never resumed); else awaiting is what its
// volumes' snapshot's upload, once ended, makes reclaimable (0 none).
type localCopy struct {
	runID       string
	exitAt      int64
	reclaimable bool
	awaiting    int64
}

// newDiskGuard resolves low and rec against the filesystem's capacity and
// refuses marks that are not 0 < low < recover < capacity.
func newDiskGuard(log *slog.Logger, low, rec FreeMark, capacity int64) (*diskGuard, error) {
	lo, re := low.of(capacity), rec.of(capacity)
	if lo <= 0 || lo >= re || re >= capacity {
		return nil, fmt.Errorf("--disk-low-free %s (%d bytes) and --disk-recover-free %s (%d bytes) must satisfy 0 < low < recover < the filesystem's capacity, %d bytes",
			low, lo, rec, re, capacity)
	}
	return &diskGuard{log: log, lowMark: low, recMark: rec, lowFree: lo, recovFree: re}, nil
}

// state is the heartbeat's pressure: a copy, nil when not under pressure.
func (g *diskGuard) state() *proto.DiskPressure {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pressure == nil {
		return nil
	}
	c := *g.pressure
	return &c
}

// at is the filesystem's state with free bytes, for a stop's report.
func (g *diskGuard) at(free int64) proto.DiskPressure {
	return proto.DiskPressure{FreeBytes: free, LowFreeBytes: g.lowFree, RecoverFreeBytes: g.recovFree,
		LowFree: g.lowMark.String(), RecoverFree: g.recMark.String()}
}

// observe applies one measurement to the hysteresis: entered when free
// fell below the low mark, left when it is back at the recovery mark.
func (g *diskGuard) observe(free int64) (entered, left bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.pressure == nil && free < g.lowFree:
		at := g.at(free)
		// A pass racing the previous episode's end may have left idle set.
		g.pressure, g.idle = &at, false
		return true, false
	case g.pressure != nil && free >= g.recovFree:
		g.pressure, g.idle = nil, false
		return false, true
	case g.pressure != nil:
		g.pressure.FreeBytes = free
	}
	return false, false
}

// setNoRelief records whether the latest pass found any relief, and
// reports whether that changed.
func (g *diskGuard) setNoRelief(free int64, none bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pressure == nil {
		return false
	}
	g.pressure.FreeBytes = free
	changed := g.pressure.NoRelief != none
	g.pressure.NoRelief = none
	return changed
}

// watch measures the filesystem and publishes a start or end of pressure.
// ok: free is a measurement.
func (g *diskGuard) watch(ctx context.Context) (free int64, ok bool) {
	free, err := g.free()
	if err != nil {
		g.log.Warn("disk guard: measuring the filesystem failed", "err", err)
		return 0, false
	}
	switch entered, left := g.observe(free); {
	case left:
		g.log.Info("disk pressure ended", "free", free, "recoverFree", g.recovFree)
		g.publish(ctx)
	case entered:
		g.log.Warn("disk pressure: admissions blocked", "free", free, "lowFree", g.lowFree, "recoverFree", g.recovFree)
		g.publish(ctx)
	}
	return free, true
}

// stillIdle: the last pass found nothing, nothing has changed since and it
// is recent, so this one would find nothing either.
func (g *diskGuard) stillIdle(gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.idle && gen == g.idleGen && time.Since(g.idleSince) < guardIdleRetry
}

func (g *diskGuard) setIdle(idle bool, gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.idle, g.idleGen, g.idleSince = idle, gen, time.Now()
}

// remeasure is free measured again, or free when that fails.
func (g *diskGuard) remeasure(free int64) int64 {
	if f, err := g.free(); err == nil {
		return f
	}
	return free
}

// pass measures the filesystem and, under pressure, acts as diskGuard
// says. Run after a round of disk samples: victims are as fresh as they get.
func (g *diskGuard) pass(ctx context.Context) {
	free, ok := g.watch(ctx)
	if !ok || g.state() == nil {
		return
	}
	gen := g.gen()
	if g.stillIdle(gen) {
		g.setNoRelief(free, true)
		return
	}
	relieved := g.reclaim(ctx, g.recovFree-free)
	// Always: a stopping container's removal may have freed its layer meanwhile.
	free = g.remeasure(free)
	free, pending, discarded := g.reclaimCopies(ctx, free)
	free, stopping := g.stopOverLimit(ctx, free, pending)
	relieved = relieved || discarded || stopping
	g.setIdle(!relieved, gen)
	if free >= g.recovFree {
		// Back at the recovery mark: end the pressure now, not a tick later.
		g.watch(ctx)
		return
	}
	if g.setNoRelief(free, !relieved) {
		if !relieved {
			g.log.Warn("disk pressure: nothing to reclaim and no placement over its disk limit; admissions stay blocked", "free", free)
		}
		g.publish(ctx)
	}
}

// reclaimCopies discards reclaimable local copies, oldest exit first, while
// free is short of the recovery mark. pending is what copies awaiting their
// upload will free; relieved: a copy was discarded or is awaiting.
func (g *diskGuard) reclaimCopies(ctx context.Context, free int64) (newFree, pending int64, relieved bool) {
	for _, c := range g.copies() {
		if !c.reclaimable {
			if c.awaiting > 0 {
				pending += c.awaiting
				relieved = true
			}
			continue
		}
		// Not gated on pending, unlike a stop: a discard frees space now,
		// pending only once an upload ends.
		if g.recovFree-free <= 0 || !g.discard(ctx, c.runID) {
			continue
		}
		g.log.Warn("disk pressure: discarded a local copy S3 holds", "run", c.runID, "free", free)
		relieved = true
		free = g.remeasure(free)
	}
	return free, pending, relieved
}

// stopOverLimit stops over-limit placements, largest excess first, until
// pending plus what their stops free covers what free lacks of the
// recovery mark. relieved: a placement is stopping, already or now.
func (g *diskGuard) stopOverLimit(ctx context.Context, free, pending int64) (newFree int64, relieved bool) {
	var cands []diskVictim
	for _, v := range overLimit(g.victims()) {
		if v.stopping {
			pending += v.rw
			relieved = true
			continue
		}
		cands = append(cands, v)
	}
	slices.SortFunc(cands, func(a, b diskVictim) int {
		return cmp.Or(cmp.Compare(b.excess(), a.excess()), strings.Compare(a.runID, b.runID))
	})
	for _, v := range cands {
		if g.recovFree-free-pending <= 0 {
			break
		}
		g.log.Warn("disk pressure: stopping a placement over its disk limit", "run", v.runID, "epoch", v.epoch,
			"used", v.used, "writableLayer", v.rw, "limit", v.limit.Effective, "excess", v.excess(), "free", free)
		g.stop(ctx, v, g.at(free))
		relieved = true
		// What its stop frees at once; its volumes count once its copy
		// awaits its upload, and are freed once that copy is reclaimable.
		pending += v.rw
		free = g.remeasure(free)
	}
	return free, relieved
}

// attachGuard wires g to this runner: free measures the filesystem, and
// the rest acts on the runner's images, local copies, placements and
// heartbeat.
func (r *Runner) attachGuard(g *diskGuard, free func() (int64, error)) {
	g.free = free
	g.publish = func(context.Context) { r.kickHeartbeat() }
	g.reclaim = func(ctx context.Context, need int64) bool {
		removed := r.reclaimImages(ctx, need)
		if len(removed) > 0 {
			r.log.Warn("disk pressure: removed unused images", "images", removed)
		}
		return len(removed) > 0
	}
	g.copies = r.localCopies
	// The next heartbeat's LocalSnapshots tells luxd the copy is gone.
	g.discard = r.discardIdle
	g.victims = r.diskVictims
	g.stop = r.pressureStop
	g.gen = r.diskGen
	r.guard = g
}

// startGuard measures the filesystem once, so the first heartbeat after a
// restart already carries a pressure that holds, then watches it every
// guardEvery (--usage-every when shorter), apart from the disk samples:
// a slow sample never delays blocking admissions.
func (r *Runner) startGuard(ctx context.Context) {
	if r.guard == nil {
		return
	}
	r.guard.watch(ctx)
	every := cmp.Or(r.guardTick, guardEvery)
	if r.cfg.UsageEvery > 0 {
		every = min(every, r.cfg.UsageEvery)
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			r.guard.watch(ctx)
		}
	}()
}

// guardPass runs the guard's pass, if on: usageLoop calls it after each
// round of disk samples.
func (r *Runner) guardPass(ctx context.Context) {
	if r.guard != nil {
		r.guard.pass(ctx)
	}
}

// diskGen changes whenever what the guard acts on may have changed.
func (r *Runner) diskGen() uint64 {
	n := r.diskChanges.Load()
	if r.images != nil {
		n += r.images.gen.Load()
	}
	return n
}

// diskChanged records a change diskGen must see.
func (r *Runner) diskChanged() {
	if r != nil {
		r.diskChanges.Add(1)
	}
}

// diskVictims are the live placements with a disk limit, at their latest
// conclusive disk sample (0 when unknown); overLimit picks the guard's.
func (r *Runner) diskVictims() []diskVictim {
	var out []diskVictim
	for _, p := range r.livePlacements() {
		l := p.diskLimit()
		if l.Nominal <= 0 {
			continue
		}
		p.mu.Lock()
		out = append(out, diskVictim{runID: p.runID, epoch: p.epoch, used: p.lastDisk, rw: p.lastRw, limit: l, p: p,
			stopping: p.phase != "running" || p.stopWhy != "" || p.pressureStop || p.disk.Reported()})
		p.mu.Unlock()
	}
	return out
}

// overLimit keeps the placements over their effective limit: the only
// ones the guard counts or stops. At or under it, never.
func overLimit(vs []diskVictim) []diskVictim {
	return slices.DeleteFunc(slices.Clone(vs), func(v diskVictim) bool { return v.excess() <= 0 })
}

// localCopies are the local copies of Runs whose placement here is not
// live and whose state says reported, oldest exit first: gcLoop's
// candidates, without its TTL. A copy is reclaimable only when no upload
// of the Run is pending and S3 holds what its volumes hold, as luxd's own
// rule for discarding a host's copy (uploaded and host_copy).
func (r *Runner) localCopies() []localCopy {
	records := r.snapshotRecords()
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
	var out []localCopy
	for _, e := range entries {
		st, err := readRunState(r.runDir(e.Name()))
		if err != nil || st.LastExitAt == 0 || st.Phase != "reported" {
			continue
		}
		r.mu.Lock()
		p := r.placements[e.Name()]
		r.mu.Unlock()
		if p != nil && p.liveState() != "" {
			continue
		}
		c := localCopy{runID: e.Name(), exitAt: st.LastExitAt, reclaimable: heldInS3(st, records)}
		if rec := records[st.VolumesSnapshot]; !c.reclaimable && rec != nil && rec.RunID == st.RunID && uploading(st.RunID, records) {
			// Its blobs compress what the volumes and output hold: their sum
			// is a lower bound of what is freed once uploaded, read from
			// disk, so it holds across a runner restart.
			for _, up := range rec.Uploads {
				c.awaiting += up.Size
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b localCopy) int {
		return cmp.Or(cmp.Compare(a.exitAt, b.exitAt), strings.Compare(a.runID, b.runID))
	})
	return out
}

// heldInS3: every snapshot record of st's Run is reported and uploaded,
// and its local volumes are either the recorded snapshot's or never
// resumed. Volumes whose export failed hold the Run's only copy.
func heldInS3(st *runState, records map[string]*snapshotRecord) bool {
	for _, rec := range records {
		if rec.RunID == st.RunID && (!rec.Reported || !allUploaded(rec)) {
			return false
		}
	}
	if st.VolumesSnapshot == "" {
		return st.Spec != nil && spec.RefusesResume(st.Spec.ResumePolicy)
	}
	rec := records[st.VolumesSnapshot]
	return rec != nil && rec.RunID == st.RunID
}

// uploading: a snapshot record of runID has an upload pending.
func uploading(runID string, records map[string]*snapshotRecord) bool {
	for _, rec := range records {
		if rec.RunID == runID && !allUploaded(rec) {
			return true
		}
	}
	return false
}

// pressureStop marks v's placement as stopped by the guard and reports it
// to luxd, which stops it (and retries until luxd has it).
func (r *Runner) pressureStop(ctx context.Context, v diskVictim, at proto.DiskPressure) {
	p := v.p
	p.mu.Lock()
	p.pressureStop = true
	p.mu.Unlock()
	data := map[string]any{"usedBytes": v.used, "limitBytes": v.limit.Effective, "excessBytes": v.excess(),
		"freeBytes": at.FreeBytes, "lowFreeBytes": at.LowFreeBytes, "recoverFreeBytes": at.RecoverFreeBytes}
	if v.limit.Scaled {
		data["nominalLimitBytes"], data["limitFactor"] = v.limit.Nominal, v.limit.Factor
	}
	go p.reportRetrying(context.WithoutCancel(ctx), proto.RunEvent{Type: proto.EvDiskPressureStop, Data: data})
}

// kickHeartbeat has heartbeatLoop send its next heartbeat now. Never
// blocks: kicks while one is due coalesce.
func (r *Runner) kickHeartbeat() {
	select {
	case r.hbNow <- struct{}{}:
	default:
	}
}

// sendHeartbeat sends one heartbeat now: heartbeatLoop's, on its interval
// or when kicked (the guard publishing a change of pressure).
func (r *Runner) sendHeartbeat(ctx context.Context) {
	hb := proto.Heartbeat{LocalSnapshots: r.localSnapshots(), GitMirrors: r.git.Mirrors(),
		RunnerSHA256: r.runnerSHA256, ShimSHA256: r.shimSHA256}
	if hu, err := podman.ReadHostUsage(r.cfg.DataDir); err == nil {
		hb.Usage = r.hostUsage(hu)
	}
	if p, err := r.proc.Read(); err == nil {
		hb.Runner = p
	}
	for _, p := range r.livePlacements() {
		if st := p.liveState(); st != "" {
			hb.Leases = append(hb.Leases, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st, Usage: p.usage()})
		}
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Acked: its peak RSS reached luxd. Else it carries to the next.
	if ack, err := r.conn.ReportAck(hctx, proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(hb)}); err == nil {
		r.proc.Stored()
		r.refreshNetwork(ctx, ack.NetworkVersion)
	}
}
