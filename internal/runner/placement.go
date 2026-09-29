package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Labels on everything the runner creates, so it can find it again.
const (
	LabelManaged = "lux.managed"
	LabelRun     = "lux.run"
	LabelTenant  = "lux.tenant"
)

// placement is one stint of a Run on this host.
type placement struct {
	r        *Runner
	runID    string
	tenantID string
	epoch    int
	assign   *proto.Assign // nil when re-adopted after a runner restart
	dir      string

	mu      sync.Mutex
	state   *runState
	phase   string // assigned | starting | running | stopping | exited | done
	stale   bool
	stopWhy string
	// cancelStart ends the steps before the container starts (an image
	// build, a pull, a clone) when the placement is stopped meanwhile.
	cancelStart context.CancelFunc
	shimConn    net.Conn
	shimEnc     *json.Encoder
	done        chan struct{}
	session     string      // latest session id the adapter reported
	user        passwd.User // who the workload runs as
	peakDisk    int64
	// diskReported: this placement was reported over its disk limit.
	diskReported bool
	netRx        int64
	netTx        int64
	cgroup       string
	ip           string // the container's address, once looked up
	// diffCancel cancels a snapshot's diff under way; diffReleased is
	// closed once its container is confirmed gone (see stopDiffs),
	// diffDone once it is reported. superseded: a later placement of the
	// Run on this host (or a discard) owns the volumes now, so no
	// snapshot diff starts; set with diffCancel read, under mu.
	diffCancel   context.CancelCauseFunc
	diffReleased chan struct{}
	diffDone     chan struct{}
	// diffHanded is closed once the diff's patches are in their snapshot
	// record (see diffHandoff).
	diffHanded chan struct{}
	superseded error
	// volumesFence is closed once the previous placement's snapshot diff
	// no longer has the Run's volumes mounted (nil: nothing to wait for).
	volumesFence <-chan struct{}
	// live is the live diff under way, if any.
	live *liveRun
}

func newPlacement(r *Runner, a proto.Assign) *placement {
	return &placement{
		r: r, runID: a.RunID, tenantID: a.TenantID, epoch: a.Epoch, assign: &a,
		dir: r.runDir(a.RunID), phase: "assigned",
		done: make(chan struct{}),
	}
}

func containerName(runID string) string    { return "lux-" + runID }
func volumeName(runID, name string) string { return "lux-" + runID + "-" + name }
func runtimeVolume(runID string) string    { return "lux-" + runID + "--rt" }
func networkName(runID string) string      { return "lux-" + runID }

// liveState is the state reported in heartbeats; "" when not live.
func (p *placement) liveState() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stale {
		return ""
	}
	switch p.phase {
	case "assigned", "starting":
		return "starting"
	case "running", "stopping":
		return p.phase
	}
	return ""
}

func (p *placement) markStale() {
	p.mu.Lock()
	p.stale = true
	if p.state != nil {
		p.state.Stale = true
		_ = writeRunState(p.dir, p.state)
	}
	p.mu.Unlock()
}

func (p *placement) isStale() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stale
}

func (p *placement) setPhase(ph string) {
	p.mu.Lock()
	p.phase = ph
	p.mu.Unlock()
}

func (p *placement) waitDone(d time.Duration) {
	select {
	case <-p.done:
	case <-time.After(d):
	}
}

func (p *placement) logf(msg string, args ...any) {
	p.r.log.Info(msg, append([]any{"run", p.runID, "epoch", p.epoch}, args...)...)
}

func (p *placement) mark(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Times == nil {
		p.state.Times = map[string]int64{}
	}
	if _, ok := p.state.Times[key]; !ok {
		p.state.Times[key] = time.Now().UnixMilli()
	}
	_ = writeRunState(p.dir, p.state)
}

func (p *placement) times() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := map[string]int64{}
	for k, v := range p.state.Times {
		t[k] = v
	}
	return t
}

// report sends a report about this placement; a stale nack fences it off.
func (p *placement) report(ctx context.Context, typ string, data any) error {
	if p.isStale() {
		return errStale
	}
	err := p.r.conn.Report(ctx, proto.Frame{Type: typ, RunID: p.runID, Epoch: p.epoch, Data: proto.Marshal(data)})
	if errors.Is(err, errStale) {
		p.logf("luxd fenced this placement off; stopping it")
		p.markStale()
		go p.kill(context.WithoutCancel(ctx))
	}
	return err
}

// reportRetrying reports an event that must reach luxd (it records state
// the Run depends on), retrying until it does, the placement is fenced off,
// or ctx ends.
func (p *placement) reportRetrying(ctx context.Context, ev proto.RunEvent) error {
	for {
		err := p.report(ctx, proto.MsgRunEvent, ev)
		if err == nil || errors.Is(err, errStale) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (p *placement) event(ctx context.Context, typ string, data map[string]any) {
	go func() {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		_ = p.report(c, proto.MsgRunEvent, proto.RunEvent{Type: typ, Data: data})
	}()
}

// run takes a placement from assignment to its final report.
func (p *placement) run(ctx context.Context) {
	defer close(p.done)
	a := p.assign
	sp := a.Spec

	prev, _ := readRunState(p.dir)
	p.state = &runState{RunID: p.runID, TenantID: p.tenantID, Epoch: p.epoch, Phase: "assigned", Times: map[string]int64{}}
	stored, _, _ := sp.SplitSecrets()
	p.state.Spec = &stored
	if prev != nil {
		p.state.VolumesSnapshot, p.state.VolumesEpoch = prev.VolumesSnapshot, prev.VolumesEpoch
	}
	// Where luxd says each repository was cloned; a clone now replaces it.
	p.state.GitBases = maps.Clone(a.GitBases)
	_ = writeRunState(p.dir, p.state)
	p.setPhase("starting")
	go p.report(ctx, proto.MsgStatus, proto.Status{State: "starting"})
	// Not a thing touched while the previous placement's snapshot diff may
	// still read the volumes (the Run's volumes are here only if it was).
	if err := p.r.volumesFree(ctx, p.runID, p.volumesFence, prev != nil); err != nil {
		p.logf("placement failed", "stage", "volumes", "err", err)
		p.finishWithoutContainer(ctx, "failed", "volumes: "+err.Error())
		return
	}

	// Until the container starts, a stop cancels whatever is under way.
	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	p.mu.Lock()
	p.cancelStart = cancelStart
	stopped := p.stopWhy != ""
	p.mu.Unlock()
	if stopped {
		cancelStart()
	}
	fail := func(stage string, err error) {
		// Nothing runs on the Run's network without a container.
		p.r.egress.Remove(bridgeName(p.runID))
		if p.pendingStop() != "" {
			p.finishWithoutContainer(ctx, "exited", "stopped before start")
			return
		}
		p.logf("placement failed", "stage", stage, "err", err)
		p.finishWithoutContainer(ctx, "failed", fmt.Sprintf("%s: %v", stage, err))
	}

	if sp.Sandbox.NestedContainers && p.r.nestedSeccomp == "" {
		fail("sandbox", errors.New("this host does not offer nested containers (lux-runner --nested)"))
		return
	}
	// The network and its egress rules first: an image build runs on it,
	// and a reused container rejoins it; both need its rules.
	network, err := p.setupNetwork(startCtx, sp)
	if err != nil {
		fail("network", err)
		return
	}
	image, err := p.ensureImage(startCtx, sp, network)
	// Pinned until its container exists (then podman itself refuses to
	// remove it): the GC must not take it while volumes and repositories
	// are being prepared.
	unpin := p.r.images.pin(image)
	defer unpin() // on any early return; idempotent
	if err != nil {
		fail("image", err)
		return
	}
	p.state.Image = image
	p.mark("imageReady")

	if p.user, err = p.workloadUser(startCtx, sp, image); err != nil {
		fail("user", err)
		return
	}
	p.mu.Lock()
	p.state.User = fmt.Sprintf("%d:%d", p.user.UID, p.user.GID)
	p.mu.Unlock()
	if err := p.prepareVolumes(startCtx, sp, a.Resume); err != nil {
		fail("volumes", err)
		return
	}
	p.mark("volumesRestored")
	if err := p.materializeRepos(startCtx, sp, p.user); err != nil {
		fail("git", err)
		return
	}

	if p.pendingStop() != "" {
		fail("stop", nil)
		return
	}

	err = p.createContainer(ctx, sp, image, network, a)
	unpin()
	if err != nil {
		fail("container", err)
		return
	}
	if err := p.r.pm.Start(ctx, containerName(p.runID)); err != nil {
		fail("start", err)
		return
	}
	p.mark("containerStarted")
	p.state.Phase = "started"
	_ = writeRunState(p.dir, p.state)
	if st, err := p.r.pm.Inspect(ctx, containerName(p.runID)); err == nil {
		p.mu.Lock()
		p.cgroup = st.CgroupPath
		p.mu.Unlock()
	}

	if err := p.startShim(ctx, a); err != nil {
		p.logf("shim start failed", "err", err)
		_ = p.r.pm.Kill(ctx, containerName(p.runID), "KILL")
	} else {
		p.setPhase("running")
		go p.report(ctx, proto.MsgStatus, proto.Status{State: "running", Times: p.times()})
	}
	// A stop that arrived while starting.
	if why := p.pendingStop(); why != "" {
		p.sendStop(ctx, why)
	}
	p.supervise(ctx)
}

// supervise follows a started container to its end: tails its output for
// adapter events, waits for it to exit, then snapshots and reports.
func (p *placement) supervise(ctx context.Context) {
	exited := make(chan struct{})
	tailDone := make(chan struct{})
	go func() { defer close(tailDone); p.tailEvents(ctx, exited) }()

	code, err := p.r.pm.Wait(ctx, containerName(p.runID))
	if err != nil {
		p.logf("podman wait failed", "err", err)
	}
	p.mark("exited")
	// Nothing on the Run's network any more: its rules and stub go (its
	// bridge, until removed, falls to the fail-closed drop).
	p.r.egress.Remove(bridgeName(p.runID))
	p.closeShim()
	// Nothing writes the output file after the container exits: the tailer
	// reads to its end and returns, so no final event is missed.
	close(exited)
	<-tailDone

	exit := p.readExit(code)
	if st, err := p.r.pm.Inspect(ctx, containerName(p.runID)); err == nil && st.OOMKilled {
		exit.Reason, exit.Message = "oom", "killed: out of memory"
	}
	p.setPhase("exited")
	p.finish(ctx, exit)
}

func (p *placement) readExit(code int) *exitRecord {
	rt, _ := p.r.mountpoint(context.Background(), runtimeVolume(p.runID))
	e := &exitRecord{Code: code, Reason: "exited"}
	b, err := os.ReadFile(filepath.Join(rt, proto.ExitFile(p.epoch)))
	if err != nil {
		e.Reason, e.Message = "crashed", "the container exited without the shim recording why"
		e.Failed = true
		return e
	}
	var info proto.ExitInfo
	if json.Unmarshal(b, &info) == nil {
		e.Code, e.Reason, e.Message, e.OutputSeq = info.ExitCode, info.Reason, info.Message, info.LastSeq
		if info.Reason == "start-failed" {
			e.Failed = true
		}
	}
	return e
}

// finish snapshots the state volumes and reports: snapshot first, so that
// by the time luxd sees the Run stopped its snapshot is recorded.
func (p *placement) finish(ctx context.Context, exit *exitRecord) {
	p.mu.Lock()
	p.state.Exit = exit
	p.state.Phase = "exited"
	p.state.LastExitAt = time.Now().UnixMilli()
	p.mu.Unlock()
	_ = writeRunState(p.dir, p.state)
	p.sampleSlow(ctx)
	usage := p.usage()

	if p.isStale() {
		p.logf("placement ended (stale; not reported)")
		p.setPhase("done")
		return
	}
	sd, err := p.snapshot(ctx)
	if err != nil {
		p.logf("snapshot failed", "err", err)
		sd = &proto.SnapshotDone{Error: err.Error(), OutputSeq: exit.OutputSeq}
	}
	for p.report(ctx, proto.MsgSnapshotDone, sd) != nil {
		if p.isStale() || ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
	// luxd has the snapshot: its blobs can be uploaded, and what the
	// workload published can go.
	if sd.Manifest.SnapshotID != "" {
		p.r.markReported(sd.Manifest.SnapshotID)
	}
	p.clearPublished(ctx)
	p.r.uploads.kick()
	// Only now the diffs: they hold up nothing above.
	if sd.Manifest.SnapshotID != "" {
		p.startSnapshotDiffs(ctx, sd.Manifest.SnapshotID)
	}

	st := proto.Status{State: "exited", ExitCode: &exit.Code, Reason: exit.Reason, Message: exit.Message,
		OutputSeq: exit.OutputSeq, Times: p.times(), Usage: usage}
	if exit.Failed {
		st.State = "failed"
	}
	for p.report(ctx, proto.MsgStatus, st) != nil {
		if p.isStale() || ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
	// Under the lock: the snapshot's diff, running by now, writes it too.
	p.mu.Lock()
	p.state.Phase = "reported"
	_ = writeRunState(p.dir, p.state)
	p.mu.Unlock()
	p.setPhase("done")
	p.logf("placement ended", "exit", exit.Code, "reason", exit.Reason)
}

// finishWithoutContainer ends a placement that never started a workload.
// Its state volumes are unchanged, so the snapshot is the one it started
// from.
func (p *placement) finishWithoutContainer(ctx context.Context, state, msg string) {
	code := 125
	if state == "exited" {
		code = 0
	}
	p.setPhase("exited")
	exit := &exitRecord{Code: code, Reason: "start-failed", Message: msg, Failed: state == "failed"}
	if state == "exited" {
		exit.Reason = "stopped"
	}
	p.mu.Lock()
	p.state.Exit = exit
	p.state.Phase = "exited"
	p.mu.Unlock()
	_ = writeRunState(p.dir, p.state)
	if p.isStale() {
		return
	}
	for p.report(ctx, proto.MsgStatus, proto.Status{State: state, ExitCode: &code, Reason: exit.Reason, Message: msg, Times: p.times()}) != nil {
		if p.isStale() || ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
	p.setPhase("done")
}

// ---- volumes ----------------------------------------------------------------

func (p *placement) prepareVolumes(ctx context.Context, sp spec.RunSpec, resume *proto.ResumeInfo) error {
	labels := map[string]string{LabelManaged: "true", LabelRun: p.runID, LabelTenant: p.tenantID}
	var refs []volumeRef
	for _, v := range sp.Volumes {
		refs = append(refs, volumeRef{Name: v.Name, Volume: volumeName(p.runID, v.Name), Path: v.Path, Kind: v.Kind})
	}
	p.state.Volumes = refs

	// The runtime volume holds the shim's socket, config and output files.
	// A named volume, because an idmapped mount needs a filesystem that
	// supports it, and the socket must be reachable from both sides.
	if !p.r.pm.VolumeExists(ctx, runtimeVolume(p.runID)) {
		if err := p.r.pm.VolumeCreate(ctx, runtimeVolume(p.runID), labels); err != nil {
			return err
		}
	}

	var manifest *proto.Manifest
	if resume != nil {
		manifest = resume.Snapshot
	}
	local := manifest != nil && p.state.VolumesSnapshot == manifest.SnapshotID
	if local {
		for _, v := range refs {
			if v.Kind == "state" && !p.r.pm.VolumeExists(ctx, v.Volume) {
				local = false
			}
		}
	}
	if local {
		p.event(ctx, "volumes.local", map[string]any{"snapshotId": manifest.SnapshotID})
	}

	for _, v := range refs {
		switch {
		case v.Kind == "ephemeral":
			// Ephemeral volumes start empty on every placement.
			_ = p.r.pm.VolumeRemove(ctx, v.Volume)
			p.r.forgetMountpoint(v.Volume)
			if err := p.r.pm.VolumeCreate(ctx, v.Volume, labels); err != nil {
				return err
			}
		case local:
			// Same host, same snapshot: nothing moves.
		default:
			_ = p.r.pm.VolumeRemove(ctx, v.Volume)
			p.r.forgetMountpoint(v.Volume)
			if err := p.r.pm.VolumeCreate(ctx, v.Volume, labels); err != nil {
				return err
			}
			if manifest == nil {
				continue
			}
			var snap *proto.VolumeSnapshot
			for i := range manifest.Volumes {
				if manifest.Volumes[i].Name == v.Name {
					snap = &manifest.Volumes[i]
				}
			}
			if snap == nil {
				continue // a volume added since: starts empty
			}
			if err := p.restoreVolume(ctx, v.Volume, snap); err != nil {
				return fmt.Errorf("restore %s: %w", v.Name, err)
			}
		}
	}
	if manifest != nil && !local {
		p.event(ctx, "volumes.restored", map[string]any{"snapshotId": manifest.SnapshotID, "from": "s3"})
	}
	if manifest != nil {
		p.state.VolumesSnapshot, p.state.VolumesEpoch = manifest.SnapshotID, manifest.Epoch
	}
	// From here the volumes are this placement's and will diverge from
	// the snapshot.
	p.state.VolumesSnapshot = ""
	return writeRunState(p.dir, p.state)
}

// restoreVolume imports one volume from a local snapshot file if this host
// still has it, else downloads it through luxd.
func (p *placement) restoreVolume(ctx context.Context, volume string, snap *proto.VolumeSnapshot) error {
	var src io.ReadCloser
	localPath := p.r.blobPath(snap.BlobID)
	if f, err := os.Open(localPath); err == nil {
		src = f
	} else {
		body, err := p.r.api.download(ctx, snap.BlobID)
		if err != nil {
			return err
		}
		src = body
	}
	defer src.Close()
	h := sha256.New()
	zr, err := zstd.NewReader(io.TeeReader(src, h))
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := p.r.pm.VolumeImport(ctx, volume, zr); err != nil {
		return err
	}
	// Drain what the decoder did not read, so the hash covers everything.
	_, _ = io.Copy(io.Discard, src)
	if got := hex.EncodeToString(h.Sum(nil)); snap.SHA256 != "" && got != snap.SHA256 {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, snap.SHA256)
	}
	return nil
}

// ---- container --------------------------------------------------------------

// hardening is applied to every container. Isolation comes from the user
// namespace (--userns=auto: a unique unprivileged host UID range per
// container), dropped capabilities, no privilege escalation, private
// namespaces, and resource limits. The shim keeps just the capabilities it
// needs to drop to the workload user; the workload gets none of them if it
// runs as non-root.
// containment is what a workload and an image build (tenant code both)
// run under: their own user namespace, few capabilities, no privilege gain.
func containment(noNewPrivileges bool) []string {
	args := []string{
		"--userns=auto:size=65536",
		"--cap-drop=ALL",
		"--cap-add=CHOWN,DAC_OVERRIDE,FOWNER,SETUID,SETGID,KILL",
	}
	if noNewPrivileges {
		args = append(args, "--security-opt=no-new-privileges")
	}
	return args
}

func hardening(sp spec.RunSpec) []string {
	// Nested containers need newuidmap's file capabilities (see nested.go).
	return append(containment(!sp.Sandbox.NestedContainers),
		// Explicit, because a host's containers.conf may default them to
		// the host's namespaces.
		"--cgroups=enabled", "--cgroupns=private", "--ipc=private", "--uts=private", "--pid=private",
		"--restart=no",
		"--hostname=lux",
		"--init=false",
		"--log-driver=none",
	)
}

func (p *placement) createContainer(ctx context.Context, sp spec.RunSpec, image string, network podman.Network, a *proto.Assign) error {
	name := containerName(p.runID)
	// A container from an earlier placement: its writable layer is only
	// worth keeping for a same-host resume with the same image; the shim
	// config is rewritten either way.
	if st, _ := p.r.pm.Inspect(ctx, name); st.Exists {
		if st.Running {
			_ = p.r.pm.Kill(ctx, name, "KILL")
			_, _ = p.r.pm.Wait(ctx, name)
		}
		id, _ := p.r.pm.ImageID(ctx, image)
		if a.Resume != nil && id != "" && id == st.ImageID && st.Labels["lux.spec"] == specHash(sp) {
			if err := p.writeShimConfig(ctx, sp, image); err != nil {
				return err
			}
			p.event(ctx, "container.reused", nil)
			return nil
		}
		if err := p.r.pm.Remove(ctx, name); err != nil {
			return err
		}
	}
	if err := p.writeShimConfig(ctx, sp, image); err != nil {
		return err
	}

	args := hardening(sp)
	args = append(args,
		"--name", name,
		"--label", LabelManaged+"=true",
		"--label", LabelRun+"="+p.runID,
		"--label", LabelTenant+"="+p.tenantID,
		"--label", "lux.spec="+specHash(sp),
		"--network", networkName(p.runID),
		"--user", "0:0",
		"--entrypoint", proto.ShimBinary,
		"-v", p.r.cfg.Shim+":"+proto.ShimBinary+":ro",
		"-v", runtimeVolume(p.runID)+":"+proto.ShimRunDir+":idmap",
		"--tmpfs", proto.ShimSecretsDir+":rw,size=16m,mode=0700,nosuid,nodev",
		// Service sockets: in memory, never in the image or a snapshot.
		"--tmpfs", proto.ShimServicesDir+":rw,size=1m,mode=0755,nosuid,nodev,noexec",
		"--cpus", fmt.Sprintf("%g", sp.Resources.CPUs),
		"--memory", fmt.Sprintf("%d", int64(sp.Resources.Memory)),
		"--memory-swap", fmt.Sprintf("%d", int64(sp.Resources.Memory)),
		"--pids-limit", fmt.Sprintf("%d", sp.Resources.Pids),
	)
	if sp.Sandbox.ReadOnlyRoot {
		args = append(args, "--read-only", "--read-only-tmpfs")
	}
	for _, v := range p.state.Volumes {
		args = append(args, "-v", v.Volume+":"+v.Path+":idmap")
	}
	args = append(args, dnsArgs(sp, network)...)
	args = append(args, p.extraArgs(sp)...)
	args = append(args, image)
	if _, err := p.r.pm.Create(ctx, args); err != nil {
		return err
	}
	return nil
}

func specHash(sp spec.RunSpec) string {
	b, _ := json.Marshal(struct {
		Image     spec.Image
		Volumes   []spec.Volume
		Resources spec.Resources
		Sandbox   spec.Sandbox
		Network   spec.Network
	}{sp.Image, sp.Volumes, sp.Resources, sp.Sandbox, sp.Network})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (p *placement) writeShimConfig(ctx context.Context, sp spec.RunSpec, image string) error {
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return err
	}
	user := sp.Workload.User
	if user == "" {
		user = fmt.Sprintf("%d:%d", p.user.UID, p.user.GID)
	}
	a := p.assign
	cfg := proto.ShimConfig{
		RunID:        p.runID,
		Epoch:        p.epoch,
		Adapter:      sp.Workload.Adapter,
		Command:      sp.Workload.Command,
		Prompt:       sp.Workload.Prompt,
		Workdir:      sp.Workload.Workdir,
		User:         user,
		TTY:          sp.Workload.TTY,
		Env:          sp.Env,
		GraceSec:     sp.Workload.Grace.Seconds(),
		Secrets:      sp.Secrets,
		ArtifactsDir: "/.lux/run/artifacts",
		MCPServers:   sp.Workload.MCPServers,
		Services:     sp.Workload.Services,
	}
	if sp.Init != nil {
		cfg.Init = sp.Init.Script
	}
	if b := sp.Workload.BeforeStop; b != nil {
		cfg.BeforeStop = b.Command
		cfg.BeforeStopTimeoutSec = b.Timeout.Seconds()
	}
	if a.Resume != nil {
		cfg.Resume = true
		cfg.SessionID = a.Resume.SessionID
		if sp.Workload.Resume != nil {
			cfg.ResumeCommand = sp.Workload.Resume.Command
		}
	}
	for _, v := range sp.Volumes {
		cfg.VolumePaths = append(cfg.VolumePaths, v.Path)
	}
	for i := range cfg.Secrets {
		cfg.Secrets[i].Value = ""
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Previous placements' exit files would confuse the new shim's owner.
	os.Remove(filepath.Join(rt, proto.ExitFile(p.epoch)))
	return os.WriteFile(filepath.Join(rt, "config.json"), b, 0o644)
}

// ---- shim socket ------------------------------------------------------------

func (p *placement) socketPath(ctx context.Context) (string, error) {
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, "shim.sock"), nil
}

func (p *placement) dialShim(ctx context.Context) error {
	path, err := p.socketPath(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			p.mu.Lock()
			p.shimConn, p.shimEnc = c, json.NewEncoder(c)
			p.mu.Unlock()
			go p.readShim(c)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("shim socket: %w", err)
		}
		st, _ := p.r.pm.Inspect(ctx, containerName(p.runID))
		if st.Exists && !st.Running {
			return errors.New("container exited before the shim was ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readShim drains replies; they only matter for debugging.
func (p *placement) readShim(c net.Conn) {
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var m proto.ShimMsg
		if json.Unmarshal(sc.Bytes(), &m) == nil && !m.OK && m.Error != "" {
			p.logf("shim error", "type", m.Type, "err", m.Error)
		}
	}
}

func (p *placement) sendShim(m proto.ShimMsg) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shimEnc == nil {
		return errors.New("shim not connected")
	}
	return p.shimEnc.Encode(m)
}

func (p *placement) closeShim() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shimConn != nil {
		p.shimConn.Close()
		p.shimConn, p.shimEnc = nil, nil
	}
}

func (p *placement) startShim(ctx context.Context, a *proto.Assign) error {
	if err := p.dialShim(ctx); err != nil {
		return err
	}
	return p.sendShim(proto.ShimMsg{Type: proto.ShimStart, Secrets: a.Secrets, Input: a.Input})
}

// ---- control ------------------------------------------------------------

func (p *placement) requestStop(ctx context.Context, reason string) {
	p.mu.Lock()
	if p.stopWhy == "" || reason == "cancel" {
		p.stopWhy = reason
	}
	phase := p.phase
	if p.state != nil {
		p.state.StopReason = p.stopWhy
		_ = writeRunState(p.dir, p.state)
	}
	if phase == "starting" && p.cancelStart != nil {
		p.cancelStart()
	}
	p.mu.Unlock()
	switch {
	case phase == "running":
		p.setPhase("stopping")
		go p.report(ctx, proto.MsgStatus, proto.Status{State: "stopping"})
		p.sendStop(ctx, reason)
	case phase == "stopping" && reason == "preempt":
		// Already stopping, perhaps with a long grace: the host is going,
		// so the shim shortens it.
		p.sendStop(ctx, reason)
	}
}

func (p *placement) pendingStop() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopWhy
}

// sendStop asks the shim for a graceful stop; if the shim cannot be
// reached, Podman stops the container (SIGTERM, then SIGKILL after grace).
func (p *placement) sendStop(ctx context.Context, reason string) {
	grace := 30 * time.Second
	if p.assign != nil {
		grace = p.assign.Spec.Workload.Grace.Duration
	}
	short := p.r.evictionGrace(grace)
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimStop, Reason: reason, GraceSec: short.Seconds()}); err != nil {
		go p.r.pm.Stop(context.WithoutCancel(ctx), containerName(p.runID), min(grace, short))
	}
}

func (p *placement) input(ctx context.Context, in proto.Input) {
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimInput, Input: &in}); err != nil {
		go p.report(ctx, proto.MsgAdapterEvent, proto.AdapterEvent{InputAck: in.RequestID, InputError: "workload not reachable: " + err.Error()})
	}
}

func (p *placement) interrupt(ctx context.Context) {
	_ = p.sendShim(proto.ShimMsg{Type: proto.ShimInterrupt})
}

// kill ends a stale placement at once: its Run lives elsewhere now.
func (p *placement) kill(ctx context.Context) {
	_ = p.r.pm.Kill(ctx, containerName(p.runID), "KILL")
}

// ---- events from the output file --------------------------------------------

// tailEvents follows the placement's output file and forwards what the
// adapter learned (session id, idle/busy, input acks) to luxd.
func (p *placement) tailEvents(ctx context.Context, exited <-chan struct{}) {
	path, err := p.outputPath(ctx, p.epoch)
	if err != nil {
		return
	}
	done := func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	_ = tailRecords(ctx, path, 0, true, done, func(rec proto.Record) error {
		if rec.Ch != "event" {
			return nil
		}
		var ev struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(rec.Event, &ev) != nil {
			return nil
		}
		var d struct {
			SessionID string `json:"sessionId"`
			Activity  string `json:"activity"`
			RequestID string `json:"requestId"`
			Error     string `json:"error"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
			Phase     string `json:"phase"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		var ae *proto.AdapterEvent
		switch ev.Type {
		case proto.EvSession:
			p.mu.Lock()
			p.session = d.SessionID
			p.mu.Unlock()
			ae = &proto.AdapterEvent{SessionID: d.SessionID}
		case proto.EvActivity:
			ae = &proto.AdapterEvent{Activity: d.Activity}
		case proto.EvInputAck:
			ae = &proto.AdapterEvent{InputAck: d.RequestID, InputError: d.Error, InputText: d.Text, InputTruncated: d.Truncated}
		case proto.EvWorkload:
			if d.Phase == "start" {
				p.mark("workloadStarted")
				go p.report(ctx, proto.MsgStatus, proto.Status{State: "running", Times: p.times()})
			}
		}
		if ae != nil {
			c, cancel := context.WithTimeout(ctx, time.Minute)
			_ = p.report(c, proto.MsgAdapterEvent, ae)
			cancel()
		}
		return nil
	})
}

func (p *placement) outputPath(ctx context.Context, epoch int) (string, error) {
	rt, err := p.r.mountpoint(ctx, runtimeVolume(p.runID))
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, proto.OutputFile(epoch)), nil
}

// tailRecords reads records with seq > since from an output file, calling
// fn for each. With follow, it waits for more until done() or ctx ends.
func tailRecords(ctx context.Context, path string, since int64, follow bool, done func() bool, fn func(proto.Record) error) error {
	var f *os.File
	for {
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		if !follow || done() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 64<<10)
	var partial []byte
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			partial = append(partial, line...)
			if partial[len(partial)-1] == '\n' {
				var rec proto.Record
				if json.Unmarshal(partial, &rec) == nil && rec.Seq > since {
					if err := fn(rec); err != nil {
						return err
					}
				}
				partial = partial[:0]
			}
		}
		if err == io.EOF {
			if !follow || done() {
				// One more pass: records written just before done.
				if follow {
					follow = false
					continue
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// ---- usage ------------------------------------------------------------------

// usage is what a heartbeat reports: the cgroup's counters, which are
// cheap file reads and kept by the kernel, plus the last disk and network
// sample (see sampleSlow).
func (p *placement) usage() *proto.Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := &proto.Usage{PeakDiskBytes: p.peakDisk, NetRxBytes: p.netRx, NetTxBytes: p.netTx}
	if p.cgroup != "" {
		if cu, err := podman.CgroupUsage(p.cgroup); err == nil {
			u.PeakMemoryBytes, u.PeakPids, u.CPUSeconds = cu.PeakMemoryBytes, cu.PeakPids, cu.CPUSeconds
			u.MemoryBytes, u.Pids = cu.MemoryBytes, cu.Pids
		}
	}
	return u
}

// sampleSlow measures what costs real work: disk use (walking the state
// volumes, and podman sizing the writable layer) and network counters.
// Run off the heartbeat path, on its own schedule.
func (p *placement) sampleSlow(ctx context.Context) {
	p.mu.Lock()
	vols := append([]volumeRef{}, p.stateVolumes()...)
	p.mu.Unlock()
	var disk int64
	for _, v := range vols {
		if mp, err := p.r.mountpoint(ctx, v.Volume); err == nil {
			disk += dirSize(mp)
		}
	}
	if out, err := p.r.pm.Run(ctx, "container", "inspect", "--size", "--format", "{{.SizeRw}}", containerName(p.runID)); err == nil {
		var n int64
		fmt.Sscan(strings.TrimSpace(string(out)), &n)
		disk += n
	}
	st, statsErr := p.r.pm.Stats(ctx, containerName(p.runID))
	p.checkDisk(ctx, disk)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peakDisk = max(p.peakDisk, disk)
	if statsErr == nil {
		p.netRx, p.netTx = max(p.netRx, st.NetInput), max(p.netTx, st.NetOutput)
	}
}

// checkDisk reports a Run over its disk limit (its writable layer and state
// volumes), once: luxd stops it, and it ends failed, snapshotted as usual.
func (p *placement) checkDisk(ctx context.Context, used int64) {
	if p.assign == nil || p.assign.Spec.Resources.Disk <= 0 {
		return
	}
	limit := int64(p.assign.Spec.Resources.Disk)
	p.mu.Lock()
	over := used > limit && !p.diskReported && p.phase == "running"
	if over {
		p.diskReported = true
	}
	p.mu.Unlock()
	if over {
		p.logf("over its disk limit", "used", used, "limit", limit)
		go p.reportRetrying(context.WithoutCancel(ctx), proto.RunEvent{Type: proto.EvDiskExceeded,
			Data: map[string]any{"usedBytes": used, "limitBytes": limit}})
	}
}

func (p *placement) stateVolumes() []volumeRef {
	if p.state == nil {
		return nil
	}
	return p.state.Volumes
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// ---- snapshot -----------------------------------------------------------

// snapshot exports every state volume (zstd), compresses the output file,
// and records them for upload. The local volumes now hold exactly this
// snapshot, so a resume here moves nothing.
func (p *placement) snapshot(ctx context.Context) (*proto.SnapshotDone, error) {
	snapID := ids.New(ids.Snapshot)
	rec := &snapshotRecord{RunID: p.runID, Epoch: p.epoch, Created: time.Now().UnixMilli()}
	sd := &proto.SnapshotDone{Manifest: proto.Manifest{SnapshotID: snapID, RunID: p.runID, Epoch: p.epoch, Volumes: []proto.VolumeSnapshot{}}}
	sd.Manifest.SessionID = p.sessionID()
	for _, v := range p.state.Volumes {
		if v.Kind != "state" {
			continue
		}
		blobID := ids.New(ids.Blob)
		size, sum, err := p.r.writeBlob(blobID, func(w io.Writer) error { return p.r.pm.VolumeExport(ctx, v.Volume, w) })
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", v.Name, err)
		}
		sd.Manifest.Volumes = append(sd.Manifest.Volumes, proto.VolumeSnapshot{Name: v.Name, Path: v.Path, BlobID: blobID, Size: size, SHA256: sum})
		rec.Uploads = append(rec.Uploads, pendingUpload{BlobID: blobID, Path: p.r.blobPath(blobID), Size: size})
	}
	// Output.
	if path, err := p.outputPath(ctx, p.epoch); err == nil {
		if f, err := os.Open(path); err == nil {
			blobID := ids.New(ids.Blob)
			size, sum, err := p.r.writeBlob(blobID, func(w io.Writer) error { _, err := io.Copy(w, f); return err })
			f.Close()
			if err == nil {
				sd.Output = &proto.BlobInfo{BlobID: blobID, Size: size, SHA256: sum}
				rec.Uploads = append(rec.Uploads, pendingUpload{BlobID: blobID, Path: p.r.blobPath(blobID), Size: size})
			}
		}
	}
	arts, err := p.collectArtifacts(ctx)
	if err != nil {
		p.event(ctx, "artifacts.failed", map[string]any{"error": err.Error()})
	}
	for _, a := range arts {
		sd.Artifacts = append(sd.Artifacts, a)
		rec.Uploads = append(rec.Uploads, pendingUpload{BlobID: a.BlobID, Path: p.r.blobPath(a.BlobID), Size: a.Size})
	}
	if p.state.Exit != nil {
		sd.OutputSeq = p.state.Exit.OutputSeq
	}
	if err := p.r.saveSnapshotRecord(snapID, rec); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.state.VolumesSnapshot, p.state.VolumesEpoch = snapID, p.epoch
	// Its diffs are owed (after the report): a restart computes them then.
	p.state.DiffsFor = snapID
	p.mu.Unlock()
	_ = writeRunState(p.dir, p.state)
	return sd, nil
}

// sessionID is the latest session id the adapter reported (seen by
// tailEvents), or the one this placement resumed.
func (p *placement) sessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == "" && p.assign != nil && p.assign.Resume != nil {
		return p.assign.Resume.SessionID
	}
	return p.session
}
