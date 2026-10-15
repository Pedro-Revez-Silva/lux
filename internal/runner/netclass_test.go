package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/proto"
)

func TestNetBaselineResetsAndReadoption(t *testing.T) {
	n := netBaseline{}
	sample := func(rx, tx int64) map[string]proto.NetBytes {
		return map[string]proto.NetBytes{"internet": {Rx: rx, Tx: tx}}
	}
	check := func(current, want map[string]proto.NetBytes) {
		t.Helper()
		got := n.observe(current)
		if !maps.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	check(sample(100, 200), sample(100, 200))
	check(sample(120, 250), sample(120, 250))
	check(sample(5, 10), sample(125, 260))
	check(sample(10, 20), sample(130, 270))
	// Baseline survives restart; surviving kernel counters are not added twice.
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	n = netBaseline{}
	if err = json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	check(sample(15, 30), sample(135, 280))
	n.reconcile(sample(140, 290))
	check(sample(20, 40), sample(145, 300))
	// Reboot: kernel gone; fold the last reading in before new bytes.
	check(nil, sample(145, 300))
	check(sample(3, 4), sample(148, 304))
}

func TestNetWelcomeFloorDoesNotDoubleCount(t *testing.T) {
	n := netBaseline{}
	n.observe(map[string]proto.NetBytes{"provider:eu-west-1": {Tx: 100}})
	n.reconcile(map[string]proto.NetBytes{"provider:eu-west-1": {Tx: 90}, "unknown": {Rx: 7}})
	got := n.value()
	if got["provider:eu-west-1"].Tx != 100 || got["unknown"].Rx != 7 {
		t.Fatal(got)
	}
	n.reconcile(got)
	if !maps.Equal(n.value(), got) {
		t.Fatal("repeated welcome added twice")
	}
}

func TestHostRebootReportGrowthRestart(t *testing.T) {
	dir := t.TempDir()
	makeRunner := func(boot string) *Runner {
		r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
		if err := r.restoreHostNetwork(boot); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeRunner("old")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 100}})
	r.hostNetSeeded = true
	r.saveHostNetwork()
	r = makeRunner("new")
	r.welcomeNetwork(proto.Welcome{HostNet: map[string]proto.NetBytes{"internet": {Tx: 100}}})
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 20}})
	r.saveHostNetwork()
	if r.hostNet.value()["internet"].Tx != 120 {
		t.Fatal(r.hostNet.value())
	}
	// Ten unsent bytes survive in the kernel; only the prior sample is durable.
	r = makeRunner("new")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 30}})
	r.hostNet.reconcile(map[string]proto.NetBytes{"internet": {Tx: 120}})
	if r.hostNet.value()["internet"].Tx != 130 {
		t.Fatal(r.hostNet.value())
	}
}

func TestNewBootIgnoresOldOffset(t *testing.T) {
	dir := t.TempDir()
	makeRunner := func(boot string) *Runner {
		t.Helper()
		r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
		if err := r.restoreHostNetwork(boot); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeRunner("old")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 100}})
	r.hostNetSeeded = true
	if err := r.saveHostNetwork(); err != nil {
		t.Fatal(err)
	}
	r = makeRunner("new")
	r.hostNet.observe(map[string]proto.NetBytes{"internet": {Tx: 150}})
	r.welcomeNetwork(proto.Welcome{HostNet: map[string]proto.NetBytes{"internet": {Tx: 100}}})
	if got := r.hostNet.value()["internet"].Tx; got != 250 {
		t.Fatalf("new boot: %d, want 250", got)
	}
}

func TestWelcomeFloorPersistedWithoutGrowth(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	p := &placement{r: r, runID: "run1", epoch: 2, phase: "running", dir: t.TempDir(), state: &runState{RunID: "run1", Epoch: 2}}
	p.observeNet(map[string]proto.NetBytes{"zone": {Tx: 10}})
	r.placements["run1"] = p
	r.welcomeNetwork(proto.Welcome{Live: []proto.LivePlacement{{RunID: "run1", Epoch: 2, Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Tx: 40}}}}}})
	state, err := readRunState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Net.value()["zone"].Tx != 40 {
		t.Fatal(state.Net.value())
	}
}

// fakeNftRunner is a Runner whose firewall is egress.New against an nft on
// PATH that accepts every script and lists counters from a file: the
// production read path, with counter values the test sets.
func fakeNftRunner(t *testing.T) (*Runner, *placement, func(unknownTx int64)) {
	t.Helper()
	dir := t.TempDir()
	counters := filepath.Join(dir, "counters.json")
	script := "#!/bin/sh\ncase \"$*\" in\n*list*counters*) cat " + counters + " ;;\n*) cat >/dev/null ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nft"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(counters, []byte(`{"nftables":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fw, err := egress.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: fw, placements: map[string]*placement{}}
	p := &placement{r: r, runID: "run1", epoch: 1, phase: "running", dir: t.TempDir(), state: &runState{RunID: "run1", Epoch: 1}}
	r.placements["run1"] = p
	iface := bridgeName("run1")
	if err := fw.Apply(context.Background(), iface, netip.Addr{}, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	set := func(tx int64) {
		t.Helper()
		doc := fmt.Sprintf(`{"nftables":[{"counter":{"family":"inet","table":"lux","name":"lux_%s_unknown_tx","packets":1,"bytes":%d}}]}`, iface, tx)
		if err := os.WriteFile(counters, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r, p, set
}

// The exit status carries p.usage(): it must hold the final reading taken
// at removal, once, however often cleanup runs.
func TestExitUsageConsumesFinalReadOnce(t *testing.T) {
	r, p, set := fakeNftRunner(t)
	set(100)
	r.sampleNetwork()
	if got := p.usage().Net["unknown"].Tx; got != 100 {
		t.Fatalf("sampled %d, want 100", got)
	}
	set(123) // 23 bytes after the last sample
	p.removeNetwork()
	if got := p.usage().Net["unknown"].Tx; got != 123 {
		t.Fatalf("exit usage %d, want 123", got)
	}
	p.removeNetwork()
	r.sampleNetwork()
	if got := p.usage().Net["unknown"].Tx; got != 123 {
		t.Fatalf("repeated cleanup: %d, want 123", got)
	}
	st, err := readRunState(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Net.value()["unknown"].Tx; got != 123 {
		t.Fatalf("persisted %d, want 123", got)
	}
}

// A successful read of a present owner whose counters are zero is a reset.
func TestSamplerPresentZeroIsAReset(t *testing.T) {
	r, p, set := fakeNftRunner(t)
	for _, v := range []int64{100, 0, 150} {
		set(v)
		r.sampleNetwork()
	}
	if got := p.usage().Net["unknown"].Tx; got != 250 {
		t.Fatalf("100 -> 0 -> 150: %d, want 250", got)
	}
}

// A seed that cannot be persisted is not reported: the runner stays
// unseeded, and the next Welcome seeds once, durably.
func TestUnpersistedSeedIsUndone(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	// A regular file where the data directory should be: every write fails.
	if err := os.WriteFile(dataDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dataDir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	if err := r.restoreHostNetwork("boot-1"); err == nil {
		t.Fatal("reading state under a file succeeded")
	}
	r.hostNetBoot = "boot-1"
	recorded := map[string]proto.NetBytes{"internet": {Tx: 1000}}
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	if r.hostNetSeeded || r.hostNet.value() != nil {
		t.Fatalf("unpersisted seed reported: seeded=%t value=%v", r.hostNetSeeded, r.hostNet.value())
	}
	if err := os.Remove(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	if got := r.hostNet.value(); !maps.Equal(got, recorded) || !r.hostNetSeeded {
		t.Fatalf("after a writable Welcome: %v seeded=%t, want %v once", got, r.hostNetSeeded, recorded)
	}
	restarted := &Runner{cfg: Config{DataDir: dataDir}, log: r.log, egress: egress.Unloaded(), placements: map[string]*placement{}}
	if err := restarted.restoreHostNetwork("boot-1"); err != nil {
		t.Fatal(err)
	}
	if got := restarted.hostNet.value(); !maps.Equal(got, recorded) || !restarted.hostNetSeeded {
		t.Fatalf("restored %v seeded=%t", got, restarted.hostNetSeeded)
	}
}

func TestSampleBeforeWelcomeThenRestartSeedsOnce(t *testing.T) {
	r, _, _ := fakeNftRunner(t)
	dir := t.TempDir()
	r.cfg.DataDir = dir
	if err := r.restoreHostNetwork("boot-1"); err != nil {
		t.Fatal(err)
	}
	// The firewall's preserved host counter is available before Welcome.
	counters := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "counters.json")
	if err := os.WriteFile(counters, []byte(`{"nftables":[{"counter":{"family":"inet","table":"lux","name":"lux_host_internet_tx","bytes":50}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fw, err := egress.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	r.egress = fw
	r.sampleNetwork()
	if got := r.hostNet.value()["internet"].Tx; got != 50 {
		t.Fatalf("before Welcome: %d, want 50", got)
	}
	restart := func() *Runner {
		t.Helper()
		fw, err := egress.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		next := &Runner{cfg: Config{DataDir: dir}, log: r.log, egress: fw, placements: map[string]*placement{}}
		if err := next.restoreHostNetwork("boot-1"); err != nil {
			t.Fatal(err)
		}
		return next
	}
	r = restart()
	if !r.egress.HostCountersKept() {
		t.Fatal("same-boot restart did not preserve host counters")
	}
	w := proto.Welcome{HostNet: map[string]proto.NetBytes{"internet": {Tx: 1000}}}
	r.welcomeNetwork(w)
	r.welcomeNetwork(w)
	if got := r.hostNet.value()["internet"].Tx; got != 1050 {
		t.Fatalf("after Welcome twice: %d, want 1050", got)
	}
	r = restart()
	if got := r.hostNet.value()["internet"].Tx; got != 1050 || !r.hostNetSeeded {
		t.Fatalf("restored %d seeded=%t, want 1050 seeded=true", got, r.hostNetSeeded)
	}
}

func TestCorruptHostNetworkStateRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	if err := r.restoreHostNetwork("boot-1"); err == nil {
		t.Fatal("corrupt network.json accepted")
	}
}

func TestHostSamplePersistsResetFold(t *testing.T) {
	r, _, _ := fakeNftRunner(t)
	dir := t.TempDir()
	r.cfg.DataDir = dir
	if err := r.restoreHostNetwork("boot-1"); err != nil {
		t.Fatal(err)
	}
	counters := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "counters.json")
	for _, v := range []int64{100, 0, 40} {
		doc := fmt.Sprintf(`{"nftables":[{"counter":{"family":"inet","table":"lux","name":"lux_host_internet_tx","bytes":%d}}]}`, v)
		if err := os.WriteFile(counters, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		r.sampleNetwork()
	}
	restarted := &Runner{cfg: Config{DataDir: dir}, log: r.log, egress: egress.Unloaded(), placements: map[string]*placement{}}
	if err := restarted.restoreHostNetwork("boot-1"); err != nil {
		t.Fatal(err)
	}
	if got := restarted.hostNet.value()["internet"].Tx; got != 140 {
		t.Fatalf("restored %d, want 140", got)
	}
}

func TestUsageNetCompatibility(t *testing.T) {
	raw, err := json.Marshal(proto.Usage{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["net"]; ok {
		t.Fatal("empty net not omitted")
	}
	want := proto.Usage{Net: map[string]proto.NetBytes{"unknown": {Rx: 3, Tx: 4}, "provider:eu-west-1": {Tx: 9}}}
	raw, err = json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got proto.Usage
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Net, want.Net) {
		t.Fatal(got.Net)
	}
}

// A host that rebooted (Unloaded keeps no counters, as after a reboot)
// starts its total from what luxd recorded; a reconnect does not add it
// again.
func TestWelcomeSeedsHostTrafficOnce(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	recorded := map[string]proto.NetBytes{"internet": {Rx: 500, Tx: 70}}
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	r.welcomeNetwork(proto.Welcome{HostNet: recorded})
	if got := r.hostNet.value(); !maps.Equal(got, recorded) {
		t.Errorf("after a reboot and a reconnect: %v, want %v", got, recorded)
	}
}

// A placement re-adopted from Live continues from what luxd recorded when
// that is more than the kernel's counters hold.
func TestWelcomeRaisesReadoptedPlacement(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), egress: egress.Unloaded(), placements: map[string]*placement{}}
	p := &placement{r: r, runID: "run1", epoch: 2, phase: "running"}
	p.netClasses.observe(map[string]proto.NetBytes{"zone": {Tx: 10}})
	r.placements["run1"] = p
	r.welcomeNetwork(proto.Welcome{Live: []proto.LivePlacement{{RunID: "run1", Epoch: 2, State: "running",
		Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Tx: 40}, "provider:eu-west-1": {Rx: 3}}}}}})
	want := map[string]proto.NetBytes{"zone": {Tx: 40}, "provider:eu-west-1": {Rx: 3}}
	if got := p.usage().Net; !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	p.netClasses.observe(map[string]proto.NetBytes{"zone": {Tx: 15}})
	if got := p.usage().Net["zone"].Tx; got != 45 {
		t.Errorf("after 5 more bytes: %d, want 45", got)
	}
}
