package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// podmanForDiskSamples answers sampleSlow's probes: `container inspect
// --size` prints $0.size, or fails while $0.failsize exists; `volume
// inspect` fails while $0.failvolume exists, and answers a mountpoint that
// does not exist while $0.novol exists. stats has no network.
const podmanForDiskSamples = `#!/bin/sh
case "$1 $2" in
"container inspect")
  [ -e "$0.failsize" ] && { echo "no such container" >&2; exit 125; }
  cat "$0.size" ;;
"volume inspect")
  [ -e "$0.failvolume" ] && { echo "no such volume" >&2; exit 125; }
  [ -e "$0.novol" ] && { echo "$0.gone"; exit 0; }
  echo "$0.vol" ;;
*)
  case "$1" in
  stats) echo '[{"net_io":"0B / 0B"}]' ;;
  *) exit 1 ;;
  esac ;;
esac
`

const (
	diskLimit = int64(10 << 30)
	diskOver  = diskLimit + 1<<30
	diskGrace = 5 * time.Minute
)

const (
	msgExceeded    = "over its disk limit"
	msgProbeFailed = "a disk use probe failed; " // a prefix
)

// diskFixture is a running placement of run1, limited to diskLimit with
// diskGrace, reporting to a fakeLuxd; logs collects the runner's log.
type diskFixture struct {
	r    *Runner
	bin  string
	luxd *fakeLuxd
	logs *lockedWriter
	ctx  context.Context
}

func newDiskFixture(t *testing.T) *diskFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(podmanForDiskSamples), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin+".vol", 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &lockedWriter{w: &bytes.Buffer{}}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewJSONHandler(logs, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, egress: egress.Unloaded()}
	r.conn = newConn(r)
	r.conn.polling = true
	luxd := &fakeLuxd{r: r}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { luxd.poll(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &diskFixture{r: r, bin: bin, luxd: luxd, logs: logs, ctx: ctx}
}

// placement is a new running placement of run1 at epoch, its container
// ctr-1 and no volumes.
func (f *diskFixture) placement(epoch int) *placement {
	sp := spec.RunSpec{Resources: spec.Resources{Disk: spec.Bytes(diskLimit), DiskGrace: spec.SetDuration(diskGrace)}}
	p := newPlacement(f.r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: epoch, Spec: sp}, nil)
	p.state = &runState{RunID: "run1", Epoch: epoch, Container: "ctr-1"}
	p.setPhase("running")
	return p
}

// withVolume gives p one state volume, which the fake podman mounts at
// $bin.vol (empty) unless told otherwise.
func withVolume(p *placement) {
	p.state.Volumes = []volumeRef{{Name: "data", Volume: "lux-run1-data", Kind: "state"}}
}

// events is every disk.exceeded event luxd got so far.
func (f *diskFixture) events() []proto.RunEvent { return f.runEvents(proto.EvDiskExceeded) }

// runEvents is every run event of type typ luxd got so far.
func (f *diskFixture) runEvents(typ string) []proto.RunEvent {
	f.luxd.mu.Lock()
	defer f.luxd.mu.Unlock()
	var out []proto.RunEvent
	for _, fr := range f.luxd.frames {
		var ev proto.RunEvent
		if fr.Type == proto.MsgRunEvent && json.Unmarshal(fr.Data, &ev) == nil && ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// wantEvents fails unless exactly n disk.exceeded events have been reported,
// and returns luxd's. checkDisk logs msgExceeded synchronously before it
// reports in the background, so the logged count settles "none" and "no
// more" without waiting; luxd's count is then polled up to a deadline, and
// drained briefly for an extra one in flight.
func (f *diskFixture) wantEvents(t *testing.T, what string, n int) []proto.RunEvent {
	t.Helper()
	if got := len(f.records(msgExceeded)); got != n {
		t.Fatalf("%s: %d reports logged, want %d", what, got, n)
	}
	for deadline := time.Now().Add(10 * time.Second); len(f.events()) < n && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	evs := f.events()
	if len(evs) != n {
		t.Fatalf("%s: luxd got %d events %+v, want %d", what, len(evs), evs, n)
	}
	return evs
}

// records is every runner log record whose message is msg.
func (f *diskFixture) records(msg string) []map[string]any {
	return f.matching(func(m string) bool { return m == msg })
}

func (f *diskFixture) recordsPrefixed(prefix string) []map[string]any {
	return f.matching(func(m string) bool { return strings.HasPrefix(m, prefix) })
}

func (f *diskFixture) matching(keep func(msg string) bool) []map[string]any {
	f.logs.mu.Lock()
	text := f.logs.w.String()
	f.logs.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) != nil {
			continue
		}
		if m, _ := rec["msg"].(string); keep(m) {
			out = append(out, rec)
		}
	}
	return out
}

// wentOver is the "remaining" of every log record of a sample going over
// (slog's JSON encoding of a time.Duration is nanoseconds).
func (f *diskFixture) wentOver() []time.Duration {
	var out []time.Duration
	for _, rec := range f.records("over its disk limit; stopping it unless it drops back under within its grace") {
		rem, _ := rec["remaining"].(float64)
		out = append(out, time.Duration(rem))
	}
	return out
}

func (f *diskFixture) flag(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(f.bin+"."+name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// size sets what `container inspect --size` answers.
func (f *diskFixture) size(t *testing.T, n int64) {
	t.Helper()
	f.sizeRaw(t, strconv.FormatInt(n, 10))
}

func (f *diskFixture) sizeRaw(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(f.bin+".size", []byte(s+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// wantEventData fails unless ev reports used bytes against diskLimit and
// diskGrace, ceiling as given.
func wantEventData(t *testing.T, ev proto.RunEvent, used int64, ceiling bool) {
	t.Helper()
	want := map[string]any{"usedBytes": float64(used), "limitBytes": float64(diskLimit), "graceSeconds": diskGrace.Seconds(), "ceiling": ceiling}
	if !reflect.DeepEqual(ev.Data, want) {
		t.Errorf("event data %v, want %v", ev.Data, want)
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// Within its grace a Run over its disk is not reported; once over for the
// whole grace it is, once, with what it used and was held to; a later
// sample over sends nothing more.
func TestCheckDiskReportsOnceAfterTheGrace(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	t0 := time.Now()
	p.checkDisk(f.ctx, diskOver, t0)
	p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace-time.Second))
	f.wantEvents(t, "within the grace", 0)
	p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
	evs := f.wantEvents(t, "grace elapsed", 1)
	wantEventData(t, evs[0], diskOver, false)
	p.checkDisk(f.ctx, 3*diskLimit, t0.Add(diskGrace+15*time.Second))
	f.wantEvents(t, "after the report", 1)
}

// A placement that is not running is never sampled against its disk, even
// at the ceiling.
func TestCheckDiskOnlyWhileRunning(t *testing.T) {
	f := newDiskFixture(t)
	p := f.placement(1)
	p.setPhase("exited")
	p.checkDisk(f.ctx, 3*diskLimit, time.Now())
	f.wantEvents(t, "exited at 3× its disk", 0)
}

// A sample whose probe failed, its partial sum at or under the limit, must
// not restart the grace; a successful sample under the limit does.
func TestFailedDiskProbeKeepsTheGrace(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(*testing.T, *diskFixture)
		volume bool
		peak   int64 // the partial sum: what the probes that answered measured
	}{
		{"container inspect --size fails", func(t *testing.T, f *diskFixture) { f.size(t, 1); f.flag(t, "failsize") }, false, 0},
		{"inspect --size answers no number", func(t *testing.T, f *diskFixture) { f.sizeRaw(t, "<no value>") }, false, 0},
		{"inspect --size answers garbage", func(t *testing.T, f *diskFixture) { f.sizeRaw(t, "garbage") }, false, 0},
		{"a volume's mountpoint fails", func(t *testing.T, f *diskFixture) { f.size(t, 1); f.flag(t, "failvolume") }, true, 1},
		{"a volume's mountpoint has gone", func(t *testing.T, f *diskFixture) { f.size(t, 1); f.flag(t, "novol") }, true, 1},
		// At the limit a partial sum is not over it, so it proves nothing.
		{"a partial sum exactly at the limit", func(t *testing.T, f *diskFixture) { f.size(t, diskLimit); f.flag(t, "failvolume") }, true, diskLimit},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDiskFixture(t)
			p := f.placement(1)
			if c.volume {
				withVolume(p)
			}
			c.setup(t, f)
			t0 := time.Now()
			p.checkDisk(f.ctx, diskOver, t0)
			p.sampleSlow(f.ctx)
			p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
			f.wantEvents(t, "over at the grace's end after a failed probe", 1)
			p.mu.Lock()
			peak := p.peakDisk
			p.mu.Unlock()
			if peak != c.peak {
				t.Errorf("peak disk %d after the partial sample, want %d", peak, c.peak)
			}
		})
	}
	// The same steps with every probe answering: the sample under the limit
	// restarts the grace, so the sample at its end is the first over again.
	t.Run("control: all probes succeed", func(t *testing.T) {
		f := newDiskFixture(t)
		p := f.placement(1)
		withVolume(p)
		f.size(t, 1)
		t0 := time.Now()
		p.checkDisk(f.ctx, diskOver, t0)
		p.sampleSlow(f.ctx)
		p.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
		f.wantEvents(t, "the sample under the limit restarts the grace", 0)
	})
}

// A partial sum is a lower bound: while one probe keeps failing, what the
// others measure over the limit is held to the grace and the ceiling.
func TestPartialDiskSampleOverTheLimitCounts(t *testing.T) {
	setup := func(t *testing.T, used int64) (*diskFixture, *placement) {
		f := newDiskFixture(t)
		p := f.placement(1)
		withVolume(p)
		f.flag(t, "failvolume")
		f.size(t, used)
		return f, p
	}
	t.Run("within the grace", func(t *testing.T) {
		f, p := setup(t, diskOver)
		p.sampleSlow(f.ctx)
		p.sampleSlow(f.ctx)
		f.wantEvents(t, "two partial samples over", 0)
		if got := f.wentOver(); len(got) != 1 || got[0] != diskGrace {
			t.Errorf("went-over logs' remaining %v, want one of %s: the first partial sample over starts the grace", got, diskGrace)
		}
	})
	t.Run("after the grace", func(t *testing.T) {
		f, p := setup(t, diskOver)
		p.checkDisk(f.ctx, diskOver, time.Now().Add(-diskGrace))
		p.sampleSlow(f.ctx)
		evs := f.wantEvents(t, "a partial sample over at the grace's end", 1)
		wantEventData(t, evs[0], diskOver, false)
		p.sampleSlow(f.ctx)
		f.wantEvents(t, "another partial sample over", 1)
	})
	t.Run("at the ceiling", func(t *testing.T) {
		f, p := setup(t, 2*diskLimit)
		p.sampleSlow(f.ctx)
		evs := f.wantEvents(t, "a partial sample at 2× the limit", 1)
		wantEventData(t, evs[0], 2*diskLimit, true)
	})
}

// A failing probe is logged once per placement, with its error, and only
// while a disk check would run.
func TestDiskProbeFailureLoggedOnce(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		f := newDiskFixture(t)
		p := f.placement(1)
		withVolume(p)
		f.size(t, 1)
		f.flag(t, "failvolume")
		p.sampleSlow(f.ctx)
		p.sampleSlow(f.ctx)
		recs := f.recordsPrefixed(msgProbeFailed)
		if len(recs) != 1 {
			t.Fatalf("%d probe-failed records %v, want one", len(recs), recs)
		}
		if e, _ := recs[0]["err"].(string); !strings.Contains(e, "no such volume") {
			t.Errorf("probe-failed err %q, want podman's \"no such volume\"", e)
		}
	})
	t.Run("no disk limit", func(t *testing.T) {
		f := newDiskFixture(t)
		p := f.placement(1)
		p.assign.Spec.Resources.Disk = 0
		f.flag(t, "failsize")
		p.sampleSlow(f.ctx)
		if recs := f.recordsPrefixed(msgProbeFailed); len(recs) != 0 {
			t.Errorf("no disk limit: %d probe-failed records %v, want none", len(recs), recs)
		}
	})
	t.Run("exited", func(t *testing.T) {
		f := newDiskFixture(t)
		p := f.placement(1)
		p.setPhase("exited")
		f.flag(t, "failsize")
		p.sampleSlow(f.ctx)
		if recs := f.recordsPrefixed(msgProbeFailed); len(recs) != 0 {
			t.Errorf("exited: %d probe-failed records %v, want none", len(recs), recs)
		}
	})
}

// On a runner that scales disk, checkDisk and sampleSlow hold a placement
// to its effective limit: a sample between the effective and the nominal
// limit goes over and, past the grace, is reported with both limits and
// the factor; a sample at the effective limit is not over; there is no
// ceiling (the low-free-space guard bounds the host instead).
func TestCheckDiskHoldsToTheEffectiveLimit(t *testing.T) {
	// 160 GiB nominal, 160 GiB filesystem, 10 GiB headroom: 10 GiB asked
	// holds to 9.375 GiB.
	const effective = diskLimit / 16 * 15
	scaled := func(t *testing.T) (*diskFixture, *placement) {
		f := newDiskFixture(t)
		f.r.disk = mustDiskScale(t, 160*gib, 10*gib, 160*gib)
		return f, f.placement(1)
	}
	t.Run("between effective and nominal, past the grace", func(t *testing.T) {
		f, p := scaled(t)
		used := diskLimit - 1
		t0 := time.Now()
		p.checkDisk(f.ctx, used, t0)
		p.checkDisk(f.ctx, used, t0.Add(diskGrace))
		evs := f.wantEvents(t, "under nominal, over effective", 1)
		want := map[string]any{"usedBytes": float64(used), "limitBytes": float64(effective), "graceSeconds": diskGrace.Seconds(),
			"ceiling": false, "nominalLimitBytes": float64(diskLimit), "limitFactor": 0.9375}
		if !reflect.DeepEqual(evs[0].Data, want) {
			t.Errorf("event data %v, want %v", evs[0].Data, want)
		}
	})
	t.Run("at the effective limit, through sampleSlow", func(t *testing.T) {
		f, p := scaled(t)
		f.size(t, effective)
		t0 := time.Now()
		p.checkDisk(f.ctx, effective+1, t0.Add(-diskGrace))
		p.sampleSlow(f.ctx)
		p.checkDisk(f.ctx, effective+1, t0)
		f.wantEvents(t, "the sample at the effective limit restarts the grace", 0)
	})
	t.Run("no ceiling on a scaled limit", func(t *testing.T) {
		f, p := scaled(t)
		p.checkDisk(f.ctx, 2*effective, time.Now())
		p.checkDisk(f.ctx, 10*effective, time.Now().Add(diskGrace-time.Second))
		f.wantEvents(t, "at 2× and 10× effective within the grace", 0)
	})
	t.Run("a partial sample between effective and nominal counts", func(t *testing.T) {
		f, p := scaled(t)
		withVolume(p)
		f.flag(t, "failvolume")
		f.size(t, effective+1)
		p.sampleSlow(f.ctx)
		if got := f.wentOver(); len(got) != 1 {
			t.Errorf("went-over logs %v, want one: a partial sum over the effective limit is conclusive", got)
		}
	})
}

// A new placement of the Run (a resume) starts its grace from zero: its
// first sample over has the whole grace left, even though the last
// placement was reported.
func TestNewPlacementGetsAFreshGrace(t *testing.T) {
	f := newDiskFixture(t)
	t0 := time.Now()
	p1 := f.placement(1)
	p1.checkDisk(f.ctx, diskOver, t0)
	p1.checkDisk(f.ctx, diskOver, t0.Add(diskGrace))
	f.wantEvents(t, "first placement", 1)
	p2 := f.placement(2)
	t1 := t0.Add(diskGrace + time.Minute)
	p2.checkDisk(f.ctx, diskOver, t1)
	p2.checkDisk(f.ctx, diskOver, t1.Add(diskGrace-time.Second))
	f.wantEvents(t, "second placement within its grace", 1)
	if got := f.wentOver(); len(got) != 2 || got[1] != diskGrace {
		t.Errorf("went-over logs' remaining %v, want the second %s", got, diskGrace)
	}
	p2.checkDisk(f.ctx, diskOver, t1.Add(diskGrace))
	f.wantEvents(t, "second placement past its grace", 2)
}
