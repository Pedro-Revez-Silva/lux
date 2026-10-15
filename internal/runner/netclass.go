package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"

	"github.com/marcioapm/lux/internal/proto"
)

// netBaseline turns kernel counters that may restart from zero (a reboot,
// a counter recreated) into totals that only grow: reported = Base + Last,
// and a reading below Last folds Last into Base first. A maximum alone
// would hide the bytes after a reset until the counter passed its old
// value. Kept in the run state (and, for the host, in boot-scoped
// DataDir/network.json), so a restarted runner continues from it.
type netBaseline struct {
	Base map[string]proto.NetBytes `json:"base,omitempty"`
	Last map[string]proto.NetBytes `json:"last,omitempty"`
}

func (n *netBaseline) observe(current map[string]proto.NetBytes) map[string]proto.NetBytes {
	n.initBase()
	// Missing zero counters still participate in reset detection.
	keys := map[string]proto.NetBytes{}
	maps.Copy(keys, n.Last)
	maps.Copy(keys, current)
	for k := range keys {
		c, last, b := current[k], n.Last[k], n.Base[k]
		if c.Rx < last.Rx {
			b.Rx += last.Rx
		}
		if c.Tx < last.Tx {
			b.Tx += last.Tx
		}
		n.Base[k] = b
	}
	n.Last = maps.Clone(current)
	return n.value()
}

func (n *netBaseline) initBase() {
	if n.Base == nil {
		n.Base = map[string]proto.NetBytes{}
	}
}

func (n *netBaseline) value() map[string]proto.NetBytes {
	out := maps.Clone(n.Base)
	if out == nil {
		out = map[string]proto.NetBytes{}
	}
	for k, v := range n.Last {
		b := out[k]
		b.Rx += v.Rx
		b.Tx += v.Tx
		out[k] = b
	}
	for k, v := range out {
		if v.Rx == 0 && v.Tx == 0 {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reconcile raises the total to at least what luxd recorded (a Welcome's
// Live usage, or its HostNet): a host that rebooted lost its counters and
// the runner's in-memory baseline with them. Call it after a reading, so
// Last is the kernel's now and is not counted twice.
func (n *netBaseline) reconcile(reported map[string]proto.NetBytes) {
	n.initBase()
	for k, v := range reported {
		b, c := n.Base[k], n.Last[k]
		b.Rx = max(b.Rx, v.Rx-c.Rx)
		b.Tx = max(b.Tx, v.Tx-c.Tx)
		n.Base[k] = b
	}
}

// seed adds what luxd recorded to the total: after a reboot, every byte
// luxd knows of was counted by counters that no longer exist.
func (n *netBaseline) seed(reported map[string]proto.NetBytes) {
	n.initBase()
	for k, v := range reported {
		b := n.Base[k]
		n.Base[k] = proto.NetBytes{Rx: b.Rx + v.Rx, Tx: b.Tx + v.Tx}
	}
}

type hostNetworkState struct {
	Boot   string      `json:"boot"`
	Seeded bool        `json:"seeded"`
	Net    netBaseline `json:"net"`
}

func (r *Runner) restoreHostNetwork(boot string) error {
	r.hostNetBoot = boot
	raw, err := os.ReadFile(filepath.Join(r.cfg.DataDir, "network.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state hostNetworkState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("host network state: %w", err)
	}
	if state.Boot == boot {
		r.hostNet, r.hostNetSeeded = state.Net, state.Seeded
		r.hostNetKnown = true
	}
	return nil
}

func (r *Runner) saveHostNetwork() error {
	if r.cfg.DataDir == "" {
		return nil
	}
	raw, err := json.Marshal(hostNetworkState{r.hostNetBoot, r.hostNetSeeded, r.hostNet})
	if err == nil {
		err = writeFileAtomic(filepath.Join(r.cfg.DataDir, "network.json"), raw, 0o600)
	}
	if err != nil {
		r.log.Error("save host network baseline", "err", err)
	}
	return err
}

func (n netBaseline) clone() netBaseline {
	return netBaseline{Base: maps.Clone(n.Base), Last: maps.Clone(n.Last)}
}

// welcomeNetwork loads luxd's classes and takes its records as floors: the
// host's traffic (seeded once if the host rebooted, else a floor) and each
// re-adopted placement's. Every Welcome (each reconnect) comes here; only
// the first may seed, since a seed adds. A seed whose state cannot be
// written is undone: reported without its offset on disk, a runner restart
// in the same boot would seed it again on top of luxd's floor.
func (r *Runner) welcomeNetwork(w proto.Welcome) {
	if err := r.egress.SetClasses(w.Network); err != nil {
		r.log.Warn("network classes", "err", err)
	}
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.sampleNetworkLocked()
	if !r.hostNetSeeded && (r.hostNetKnown || !r.egress.HostCountersKept()) {
		unseeded := r.hostNet.clone()
		r.hostNet.seed(w.HostNet)
		r.hostNetSeeded = true
		if r.saveHostNetwork() != nil {
			r.hostNet, r.hostNetSeeded = unseeded, false
		}
	} else {
		r.hostNet.reconcile(w.HostNet)
		r.hostNetSeeded = true
		_ = r.saveHostNetwork()
	}
	for _, l := range w.Live {
		if p := r.placement(l.RunID, l.Epoch); p != nil && l.Usage != nil {
			p.mu.Lock()
			p.netClasses.reconcile(l.Usage.Net)
			p.saveNetLocked()
			p.mu.Unlock()
		}
	}
}

// observeNet takes a kernel reading of the placement's counters, and keeps
// the baseline in its run state (written only when the reading changed): a
// restarted runner continues from it.
func (p *placement) observeNet(current map[string]proto.NetBytes) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if maps.Equal(p.netClasses.Last, current) {
		return
	}
	p.netClasses.observe(current)
	p.saveNetLocked()
}

func (p *placement) saveNetLocked() {
	if p.state != nil {
		p.state.Net = p.netClasses
		if err := p.saveStateLocked(); err != nil {
			p.logf("save network baseline", "err", err)
		}
	}
}

// sampleNetwork serializes nft snapshots with final reads: an older tick
// cannot arrive after Remove's final reading and look like a reset.
func (r *Runner) sampleNetwork() {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.sampleNetworkLocked()
}

func (r *Runner) sampleNetworkLocked() {
	if r.egress == nil {
		return
	}
	c, err := r.egress.Counters()
	if err != nil {
		r.log.Warn("network counters", "err", err)
		return
	}
	r.hostNet.observe(c.Host)
	_ = r.saveHostNetwork()
	for _, p := range r.livePlacements() {
		if current, ok := c.Runs[bridgeName(p.runID)]; ok {
			p.observeNet(current)
		}
	}
}

// removeNetwork removes the Run's egress and counters, taking their final
// reading first: the bytes since the last sample, which the exit status
// then carries.
func (p *placement) removeNetwork() {
	r := p.r
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.egress == nil {
		return
	}
	if final := r.egress.Remove(bridgeName(p.runID)); final != nil {
		p.observeNet(final)
	}
}

// refreshNetwork fetches the host's classes when a heartbeat's ack names a
// version other than the one loaded (its Welcome carried the first).
func (r *Runner) refreshNetwork(ctx context.Context, version string) {
	if r.egress == nil || r.api == nil || r.egress.ClassesVersion() == version {
		return
	}
	var nc proto.NetworkClasses
	if err := r.api.getJSON(ctx, "/runner/v1/network?name="+url.QueryEscape(r.cfg.Name), &nc); err != nil {
		r.log.Warn("network catalog", "err", err)
		return
	}
	if err := r.egress.SetClasses(&nc); err != nil {
		r.log.Warn("network catalog", "err", err)
	}
}
