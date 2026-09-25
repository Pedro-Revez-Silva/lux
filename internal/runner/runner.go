// Package runner is lux-runner: the agent on every host. It dials luxd,
// takes placements, runs them in hardened Podman containers with lux-shim
// as PID 1, relays their output, snapshots their state volumes whenever a
// container exits, and uploads what luxd needs to resume them elsewhere.
//
// Podman is daemonless: every container has its own conmon, so restarting
// the runner does not touch running containers. On start the runner
// re-adopts them by label.
package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/version"
)

type Config struct {
	URL     string
	Token   string
	Name    string
	DataDir string
	Shim    string
	Labels  map[string]string
	MaxRuns int
	CPUs    float64
	Memory  int64
	// Disk offered for Runs' writable layers and volumes, reserved by the
	// scheduler from their resources.disk (0: not reserved; each Run's
	// limit still applies).
	Disk int64
	// ProviderID is the cloud instance id, for provisioned hosts.
	ProviderID string
	// ForcePoll uses the HTTP polling transport even if WebSocket works.
	ForcePoll bool
	// HostTTL is how long local snapshot copies are kept after upload.
	HostTTL time.Duration
	// EC2IMDS is the EC2 instance metadata endpoint to watch for a spot
	// interruption notice ("" does not watch).
	EC2IMDS string
	// UsageEvery is how often disk and network use are sampled, which
	// bounds how far past its disk limit a Run can write (default 15s).
	UsageEvery time.Duration
	// ImageDiskHigh is the percentage of the disk holding Podman's storage
	// over which the GC removes lux's unused images, least recently used
	// first (0: off).
	ImageDiskHigh float64
	// Nested offers nested containers (sandbox.nestedContainers): the host
	// is labelled nested=true, and such Runs get what rootless Podman
	// inside them needs (see nested.go).
	Nested bool
}

type Runner struct {
	cfg   Config
	log   *slog.Logger
	pm    *podman.Podman
	conn  *conn
	api   *api
	shimV string
	// lease is luxd's host lease (its Welcome says), read by the heartbeat
	// loop: a time.Duration, 0 until then.
	lease atomic.Int64

	mu         sync.Mutex
	placements map[string]*placement // by run id
	subs       map[string]context.CancelFunc
	uploads    *uploader
	control    *serialQueues
	egress     *egress.Firewall
	images     *imageUse
	graph      string // Podman's graph root, once asked
	graphOnce  sync.Once
	// recordMu serializes read-modify-write of snapshot records (uploader,
	// discard, report).
	recordMu sync.Mutex
	streams  streams
	// nestedSeccomp is the seccomp profile for nested-containers Runs, or
	// "" if this host does not offer them.
	nestedSeccomp string
	git           *gitws.Manager
	mounts        sync.Map // volume name → mountpoint
	// evictBy is when the provider takes this host away (zero: not
	// evicting). Stops before it get a grace that leaves time to snapshot
	// and upload.
	evictBy atomic.Pointer[evicting]
	// runnerSHA256, shimSHA256: this runner's own binary and its --shim,
	// hashed once at startup (a self-update replaces the file, not this
	// process, so the hash is stable for the process's life).
	runnerSHA256, shimSHA256 string
}

// mountpoint is where a volume's data is on this host. It never changes for
// a volume's life, so it is looked up once (each lookup forks podman).
func (r *Runner) mountpoint(ctx context.Context, volume string) (string, error) {
	if mp, ok := r.mounts.Load(volume); ok {
		return mp.(string), nil
	}
	mp, err := r.pm.VolumeMountpoint(ctx, volume)
	if err != nil {
		return "", err
	}
	r.mounts.Store(volume, mp)
	return mp, nil
}

func (r *Runner) forgetMountpoint(volume string) { r.mounts.Delete(volume) }

func New(cfg Config, log *slog.Logger) (*Runner, error) {
	if cfg.Name == "" {
		h, _ := os.Hostname()
		cfg.Name = h
	}
	if cfg.MaxRuns == 0 {
		cfg.MaxRuns = 16
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = float64(runtime.NumCPU())
	}
	if cfg.Memory == 0 {
		cfg.Memory = memTotal()
	}
	if cfg.UsageEvery == 0 {
		cfg.UsageEvery = 15 * time.Second
	}
	if cfg.HostTTL == 0 {
		cfg.HostTTL = 24 * time.Hour
	}
	// tmp holds what a pull, push or build needed while it ran (registry
	// auth files among them): whatever a stopped runner left there is stale.
	os.RemoveAll(filepath.Join(cfg.DataDir, "tmp"))
	for _, d := range []string{"runs", "snapshots", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(cfg.Shim); err != nil {
		return nil, fmt.Errorf("shim binary: %w", err)
	}
	r := &Runner{
		cfg:        cfg,
		log:        log,
		pm:         podman.New(),
		placements: map[string]*placement{},
		subs:       map[string]context.CancelFunc{},
		control:    newSerialQueues(),
		streams:    streams{m: map[string]*stream{}},
		git:        gitws.New(cfg.DataDir),
	}
	r.api = newAPI(cfg.URL, cfg.Token, cfg.Name)
	r.conn = newConn(r)
	r.uploads = newUploader(r)
	r.shimV = version.Version
	blocked, err := blockedForRuns(context.Background(), cfg.URL)
	if err != nil {
		return nil, err
	}
	fw, err := egress.New(blocked)
	if err != nil {
		return nil, err
	}
	r.egress = fw
	r.images = newImageUse(cfg.DataDir)
	// Best effort: an unreadable binary (permissions, a stripped
	// /proc/self/exe on some minimal containers) just means luxd never
	// drains this runner for being outdated.
	if exe, err := os.Executable(); err == nil {
		r.runnerSHA256 = sha256File(exe)
	}
	r.shimSHA256 = sha256File(cfg.Shim)
	return r, nil
}

// sha256File is path's content sha256, hex, or "" when it cannot be read.
func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Runner) Run(ctx context.Context) error {
	if _, err := r.pm.Version(ctx); err != nil {
		return fmt.Errorf("podman is not usable: %w", err)
	}
	if r.cfg.Nested {
		for _, dev := range []string{"/dev/fuse", "/dev/net/tun"} {
			if _, err := os.Stat(dev); err != nil {
				return fmt.Errorf("--nested needs %s: %w", dev, err)
			}
		}
		host, err := r.hostSeccompProfile(ctx)
		if err != nil {
			return err
		}
		p, err := writeNestedSeccomp(r.cfg.DataDir, host)
		if err != nil {
			return err
		}
		r.nestedSeccomp = p
	}
	r.readopt(ctx)
	go r.uploads.loop(ctx)
	go r.heartbeatLoop(ctx)
	go r.usageLoop(ctx)
	go r.egress.Run(ctx)
	go r.gcLoop(ctx)
	if r.cfg.EC2IMDS != "" {
		go r.watchSpot(ctx, r.cfg.EC2IMDS)
	}
	r.conn.loop(ctx)
	return nil
}

func (r *Runner) hello(ctx context.Context) proto.Hello {
	pv, _ := r.pm.Version(ctx)
	imgs, _ := r.pm.Images(ctx)
	h := proto.Hello{
		Name:            r.cfg.Name,
		ProviderID:      r.cfg.ProviderID,
		ProtocolVersion: proto.Version,
		RunnerVersion:   version.Version,
		ShimVersion:     r.shimV,
		PodmanVersion:   pv,
		Arch:            runtime.GOARCH,
		Labels:          r.cfg.Labels,
		Nested:          r.nestedSeccomp != "",
		Capacity:        proto.Capacity{CPUs: r.cfg.CPUs, Memory: r.cfg.Memory, Disk: r.cfg.Disk, Runs: r.cfg.MaxRuns},
		Images:          imgs,
		GitMirrors:      r.git.Mirrors(),
		LocalSnapshots:  r.localSnapshots(),
		RunnerSHA256:    r.runnerSHA256,
		ShimSHA256:      r.shimSHA256,
	}
	r.mu.Lock()
	for _, p := range r.placements {
		if st := p.liveState(); st != "" {
			h.Live = append(h.Live, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st})
		}
	}
	r.mu.Unlock()
	return h
}

// onWelcome reconciles with what luxd thinks runs here: anything else is
// stale (its Run moved on while this host was away) and is stopped.
func (r *Runner) onWelcome(ctx context.Context, w proto.Welcome) {
	if w.LeaseSeconds > 0 {
		r.lease.Store(int64(w.LeaseSeconds * float64(time.Second)))
	}
	want := map[string]int{}
	for _, l := range w.Live {
		want[l.RunID] = l.Epoch
	}
	r.mu.Lock()
	var stale []*placement
	for _, p := range r.placements {
		if p.liveState() == "" {
			continue
		}
		if e, ok := want[p.runID]; !ok || e != p.epoch {
			stale = append(stale, p)
		}
	}
	r.mu.Unlock()
	for _, p := range stale {
		r.log.Warn("stopping stale placement", "run", p.runID, "epoch", p.epoch)
		p.markStale()
		go p.kill(context.WithoutCancel(ctx))
	}
}

// handleControl handles one durable message from luxd. Idempotent.
func (r *Runner) handleControl(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgAssign:
		var a proto.Assign
		if err := json.Unmarshal(f.Data, &a); err != nil {
			r.log.Error("bad assign", "err", err)
			return
		}
		r.assign(ctx, a)
	case proto.MsgStop, proto.MsgCancel:
		var s proto.StopRequest
		_ = json.Unmarshal(f.Data, &s)
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.requestStop(ctx, s.Reason)
		}
	case proto.MsgInput:
		var in proto.Input
		_ = json.Unmarshal(f.Data, &in)
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.input(ctx, in)
		}
	case proto.MsgInterrupt:
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			p.interrupt(ctx)
		}
	case proto.MsgPush:
		if p := r.placement(f.RunID, f.Epoch); p != nil {
			var req proto.Push
			_ = json.Unmarshal(f.Data, &req)
			// Not in the control queue: a push can take minutes, and a stop
			// behind it must not wait.
			go p.push(context.WithoutCancel(ctx), req)
		}
	case proto.MsgSnapshotDiscard:
		var d struct {
			RunID       string `json:"runId"`
			BeforeEpoch int    `json:"beforeEpoch"`
		}
		_ = json.Unmarshal(f.Data, &d)
		r.discard(ctx, d.RunID, d.BeforeEpoch)
	case proto.MsgDrain:
		// luxd stops this host's Runs itself; nothing to do locally.
	case proto.MsgExit:
		var e proto.ExitHost
		_ = json.Unmarshal(f.Data, &e)
		if !r.shouldExit(ctx, e) {
			return
		}
		r.log.Warn("luxd asked this host to exit", "reason", e.Reason, "code", e.Code)
		// Exit after the ack for this message has gone out (handleControl
		// returns to its caller, which sends it next), not from here: never
		// in place of the running binary. The unit's Restart=always starts
		// a fresh process, whose ExecStartPre re-downloads first.
		go func() {
			time.Sleep(2 * time.Second)
			os.Exit(e.Code)
		}()
	default:
		r.log.Warn("unknown control message", "type", f.Type)
	}
}

func (r *Runner) placement(runID string, epoch int) *placement {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.placements[runID]
	if p == nil || (epoch != 0 && p.epoch != epoch) {
		return nil
	}
	return p
}

func (r *Runner) assign(ctx context.Context, a proto.Assign) {
	r.mu.Lock()
	old := r.placements[a.RunID]
	if old != nil && old.epoch == a.Epoch {
		r.mu.Unlock()
		return // redelivered
	}
	if old != nil && old.epoch > a.Epoch {
		r.mu.Unlock()
		r.log.Warn("ignoring assign for an older epoch", "run", a.RunID, "epoch", a.Epoch, "have", old.epoch)
		return
	}
	p := newPlacement(r, a)
	r.placements[a.RunID] = p
	r.mu.Unlock()
	if old != nil && old.liveState() != "" {
		// The same Run again with a newer epoch while the old one still
		// runs here: luxd gave up on the old one.
		old.markStale()
		old.kill(ctx)
		old.waitDone(30 * time.Second)
	}
	go p.run(context.WithoutCancel(ctx))
	if r.evictBy.Load() != nil {
		// Placed here while the drain was being decided: luxd preempts it.
		go r.reportEvicting(context.WithoutCancel(ctx))
	}
}

// handleLive handles non-durable frames: output subscriptions, streams.
func (r *Runner) handleLive(ctx context.Context, f proto.Frame) {
	switch f.Type {
	case proto.MsgOutputSubscribe:
		var s proto.OutputSubscribe
		_ = json.Unmarshal(f.Data, &s)
		sctx, cancel := context.WithCancel(ctx)
		r.mu.Lock()
		r.subs[s.SubID] = cancel
		r.mu.Unlock()
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.subs, s.SubID)
			r.mu.Unlock()
		}()
		err := r.streamOutput(sctx, f.RunID, f.Epoch, s)
		end := proto.OutputEnd{SubID: s.SubID}
		if err != nil && sctx.Err() == nil {
			end.Error = err.Error()
		}
		if sctx.Err() == nil {
			_ = r.conn.Send(ctx, proto.Frame{Type: proto.MsgOutputEnd, RunID: f.RunID, Epoch: f.Epoch, Data: proto.Marshal(end)})
		}
	case proto.MsgOutputCancel:
		var s proto.OutputSubscribe
		_ = json.Unmarshal(f.Data, &s)
		r.mu.Lock()
		if c := r.subs[s.SubID]; c != nil {
			c()
		}
		r.mu.Unlock()
	}
}

// heartbeatEvery is a third of luxd's host lease (30s until its Welcome
// says), at least a second.
func (r *Runner) heartbeatEvery() time.Duration {
	lease := time.Duration(r.lease.Load())
	if lease <= 0 {
		lease = 30 * time.Second
	}
	return max(lease/3, time.Second)
}

func (r *Runner) heartbeatLoop(ctx context.Context) {
	for {
		// The interval is re-read every second: the first wait starts before
		// luxd's Welcome says the lease, and must not outlast a shorter one.
		for start := time.Now(); ; {
			left := time.Until(start.Add(r.heartbeatEvery()))
			if left <= 0 {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(left, time.Second)):
			}
		}
		hb := proto.Heartbeat{LocalSnapshots: r.localSnapshots(), GitMirrors: r.git.Mirrors(),
			RunnerSHA256: r.runnerSHA256, ShimSHA256: r.shimSHA256}
		if hu, err := podman.ReadHostUsage(r.cfg.DataDir); err == nil {
			hb.Usage = &proto.HostUsage{CPUSeconds: hu.CPUSeconds, MemoryBytes: hu.MemoryBytes, DiskBytes: hu.DiskBytes}
		}
		for _, p := range r.livePlacements() {
			if st := p.liveState(); st != "" {
				hb.Leases = append(hb.Leases, proto.LivePlacement{RunID: p.runID, Epoch: p.epoch, State: st, Usage: p.usage()})
			}
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = r.conn.Report(hctx, proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(hb)})
		cancel()
	}
}

func (r *Runner) livePlacements() []*placement {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := make([]*placement, 0, len(r.placements))
	for _, p := range r.placements {
		if p.liveState() != "" {
			ps = append(ps, p)
		}
	}
	return ps
}

// binariesMatchManifest reports whether luxd's current manifest lists this
// arch with exactly this runner's shas. An unreachable luxd or a missing
// arch never matches.
func (r *Runner) binariesMatchManifest(ctx context.Context) bool {
	var manifest map[string]map[string]string
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.api.getJSON(ctx, "/runner/bin/manifest", &manifest); err != nil {
		return false
	}
	have := manifest["linux-"+runtime.GOARCH]
	return have["lux-runner"] != "" && have["lux-runner"] == r.runnerSHA256 &&
		have["lux-shim"] != "" && have["lux-shim"] == r.shimSHA256
}

// shouldExit reports whether a MsgExit is still actionable: a redelivered
// exit can arrive after this host took new work or already caught up.
func (r *Runner) shouldExit(ctx context.Context, e proto.ExitHost) bool {
	if live := r.livePlacements(); len(live) > 0 {
		r.log.Warn("ignoring exit: this host holds live placements", "reason", e.Reason, "code", e.Code, "placements", len(live))
		return false
	}
	if r.binariesMatchManifest(ctx) {
		r.log.Warn("ignoring exit: this host's binaries already match luxd's manifest", "reason", e.Reason, "code", e.Code)
		return false
	}
	return true
}

// usageLoop samples disk and network use of live placements, in parallel,
// on a slower schedule than heartbeats: it walks volumes and forks podman.
// It is also how the disk limit is enforced, so a Run can write about one
// interval's worth past its limit before it is stopped.
func (r *Runner) usageLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.UsageEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for _, p := range r.livePlacements() {
			wg.Go(func() { p.sampleSlow(ctx) })
		}
		wg.Wait()
	}
}

// localSnapshots lists Runs whose state volumes are on this host.
func (r *Runner) localSnapshots() []proto.LocalSnapshot {
	var out []proto.LocalSnapshot
	entries, _ := os.ReadDir(filepath.Join(r.cfg.DataDir, "runs"))
	for _, e := range entries {
		st, err := readRunState(r.runDir(e.Name()))
		if err != nil || st.VolumesSnapshot == "" {
			continue
		}
		out = append(out, proto.LocalSnapshot{RunID: e.Name(), Epoch: st.VolumesEpoch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

func (r *Runner) runDir(runID string) string { return filepath.Join(r.cfg.DataDir, "runs", runID) }

func memTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}
