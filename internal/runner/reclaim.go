package runner

import (
	"context"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// reclaimGrace is how long a reclaimed spot placement may run after the
// runner first receives the notice: SIGTERM at once, SIGKILL at this.
// reclaimClock is the receipt's clock. Both are variables for tests only.
var (
	reclaimGrace = 15 * time.Second
	reclaimClock = time.Now
)

const stopSpotReclaim = "spot-reclaim"

// reclaimKillAt is the persisted kill instant: stored is the one recorded
// at first receipt (0: none yet), now that receipt.
func reclaimKillAt(stored int64, now time.Time) int64 {
	if stored > 0 {
		return stored
	}
	return now.Add(reclaimGrace).UnixMilli()
}

// fixReclaimLocked fixes the kill instant at the first notice and records
// it in the run state (the caller saves it); a later notice, or a
// re-adopted placement's, keeps the stored one. Callers hold p.mu.
func (p *placement) fixReclaimLocked() int64 {
	if p.reclaimAt == 0 && p.state != nil {
		p.reclaimAt = p.state.ReclaimKillAt
	}
	p.reclaimAt = reclaimKillAt(p.reclaimAt, reclaimClock())
	if p.state != nil {
		p.state.ReclaimKillAt = p.reclaimAt
	}
	return p.reclaimAt
}

// reclaimPastDeadline reports whether a reclamation's kill instant has
// passed: a container that comes up after it must not start its workload.
func (p *placement) reclaimPastDeadline() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reclaimAt > 0 && reclaimClock().UnixMilli() >= p.reclaimAt
}

// reclaim enforces a spot reclamation whose deadline requestStop fixed and
// persisted: the shim is told to SIGTERM now (no beforeStop, no grace), and
// the runner itself kills the container at the deadline, whether or not
// the shim or luxd are reachable. Repeated calls send the same deadline and
// arm one kill.
func (p *placement) reclaim(ctx context.Context) {
	p.mu.Lock()
	if p.reclaimAt == 0 {
		p.fixReclaimLocked()
		_ = p.saveStateLocked()
	}
	killAt, armed := p.reclaimAt, p.reclaimArmed
	p.reclaimArmed = true
	p.mu.Unlock()
	if !armed {
		p.armReclaimKill(ctx, time.UnixMilli(killAt))
	}
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimStop, Reason: stopSpotReclaim, KillAtUnixMs: killAt}); err != nil {
		if ctr := p.container(); ctr != "" {
			_ = p.r.pm.Kill(context.WithoutCancel(ctx), ctr, "TERM")
		}
	}
}

// armReclaimKill kills the container (every process in it) at killAt. A
// container not created by then is killed by run once it is
// (reclaimPastDeadline).
func (p *placement) armReclaimKill(ctx context.Context, killAt time.Time) {
	time.AfterFunc(max(0, killAt.Sub(reclaimClock())), func() {
		if ctr := p.container(); ctr != "" {
			_ = p.r.pm.Kill(context.WithoutCancel(ctx), ctr, "KILL")
		}
	})
}
