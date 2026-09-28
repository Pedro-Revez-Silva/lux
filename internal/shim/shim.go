// Package shim is lux-shim: PID 1 in every container.
//
// It is the only process inside the container that knows about lux. It
// runs the init script and the workload, captures output (redacted) into
// the placement's output file, runs the adapter that speaks the workload's
// protocol, accepts input and stop requests from the runner over a Unix
// socket, reaps zombies, and reports how the workload exited. It never
// talks to the network.
package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/marcioapm/lux/internal/adapter"
	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

type Shim struct {
	cfg     proto.ShimConfig
	out     *Output
	red     *Redactor
	adapter adapter.Adapter
	user    *userInfo

	mu        sync.Mutex
	started   bool
	startCh   chan proto.ShimMsg
	delivered map[string]bool
	stopping  bool
	stopWhy   string
	proc      *adapter.Process
	workPid   int
	exitCh    chan syscall.WaitStatus
	initPid   int
	initCh    chan syscall.WaitStatus
	pending   []proto.Input
	// env is the workload's environment, for exec.
	env []string
	// term is the workload's terminal, when it has one (workload.tty).
	term *terminal
	// streams: exec'd processes, by pid, whose exits the reaper reports.
	streams map[int]chan syscall.WaitStatus
}

// Main is the shim's entry point. It returns the process exit code.
func Main() int {
	cfgBytes, err := os.ReadFile(proto.ShimConfigFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: no config:", err)
		return 125
	}
	var cfg proto.ShimConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: bad config:", err)
		return 125
	}
	// Not dumpable: its memory and /proc entries (which will hold service
	// credentials) are root's and closed to ptrace, so a workload without
	// CAP_SYS_PTRACE cannot read them, even as container root.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: PR_SET_DUMPABLE:", err)
		return 125
	}
	s := &Shim{
		cfg:       cfg,
		red:       NewRedactor(nil),
		startCh:   make(chan proto.ShimMsg, 1),
		delivered: map[string]bool{},
		streams:   map[int]chan syscall.WaitStatus{},
		exitCh:    make(chan syscall.WaitStatus, 1),
		initCh:    make(chan syscall.WaitStatus, 1),
	}
	return s.run()
}

func (s *Shim) run() int {
	out, err := OpenOutput(filepath.Join(proto.ShimRunDir, proto.OutputFile(s.cfg.Epoch)), s.red)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lux-shim: output:", err)
		return 125
	}
	s.out = out

	// PID 1 must reap: orphans reparent to us.
	go s.reap()
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigs {
			s.stop("signal", 0)
		}
	}()

	ln, err := s.listen()
	if err != nil {
		return s.fail("start-failed", "socket: "+err.Error())
	}
	go s.serve(ln)

	// Wait for the runner's start (it carries the secret values).
	var start proto.ShimMsg
	select {
	case start = <-s.startCh:
	case <-time.After(10 * time.Minute):
		return s.fail("start-failed", "no start from the runner within 10 minutes")
	}
	if s.isStopping() {
		return s.finish(proto.ExitInfo{ExitCode: 0, Reason: "stopped", Message: "stopped before start"})
	}
	s.red.Set(start.Secrets)
	// Before any goroutine but this one reads cfg.MCP.
	s.cfg.ResolveMCP(start.Secrets)

	user, err := lookupUser(s.cfg.User)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	s.user = user // only this goroutine reads it before env is published
	env := s.environment(start.Secrets)
	// Exec reads s.env, under the lock, as its sign that the workload's
	// user and environment are ready.
	s.mu.Lock()
	s.env = env
	s.mu.Unlock()
	s.prepareVolumes()
	if err := s.serveServices(start.Secrets); err != nil {
		return s.fail("start-failed", err.Error())
	}
	ad, err := adapter.New(s.cfg.Adapter)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	if err := s.writeSecretFiles(start.Secrets, ad); err != nil {
		return s.fail("start-failed", "secrets: "+err.Error())
	}

	if strings.TrimSpace(s.cfg.Init) != "" {
		code, err := s.runInit(env)
		if err != nil || code != 0 {
			msg := fmt.Sprintf("init script exited with %d", code)
			if err != nil {
				msg = "init script: " + err.Error()
			}
			return s.finish(proto.ExitInfo{ExitCode: max(code, 1), Reason: "init-failed", Message: msg})
		}
	}
	if s.isStopping() {
		return s.finish(proto.ExitInfo{ExitCode: 0, Reason: "stopped", Message: "stopped during init"})
	}

	s.adapter = ad
	argv, err := ad.Command(s.cfg)
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	proc, err := s.startWorkload(argv, withAdapterEnv(env, ad, s.cfg))
	if err != nil {
		return s.fail("start-failed", err.Error())
	}
	s.out.Event(proto.EvWorkload, map[string]any{"phase": "start", "pid": proc.Cmd.Process.Pid, "command": argv})

	adDone := make(chan struct{})
	go func() {
		defer close(adDone)
		if err := ad.Run(context.Background(), proc, s.cfg, &sink{s}); err != nil {
			s.out.Event(proto.EvWarning, map[string]any{"message": "adapter: " + err.Error()})
		}
	}()
	// Inputs that arrived before the workload started, then the resume
	// input from the start message.
	s.mu.Lock()
	queued := s.pending
	s.pending = nil
	s.mu.Unlock()
	if start.Input != nil {
		queued = append([]proto.Input{*start.Input}, queued...)
	}
	for _, in := range queued {
		s.deliver(in)
	}

	ws := <-s.exitCh
	// The workload's group may have children still holding the pipes.
	_ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-adDone:
	case <-time.After(5 * time.Second):
	}
	info := proto.ExitInfo{ExitCode: exitCode(ws), Reason: "exited"}
	if ws.Signaled() {
		info.Signal = ws.Signal().String()
	}
	if s.isStopping() {
		info.Reason = "stopped"
		info.Message = s.stopWhy
	}
	return s.finish(info)
}

func (s *Shim) fail(reason, msg string) int {
	fmt.Fprintln(os.Stderr, "lux-shim:", msg)
	s.out.Event(proto.EvWarning, map[string]any{"message": msg})
	return s.finish(proto.ExitInfo{ExitCode: 125, Reason: reason, Message: msg})
}

func (s *Shim) finish(info proto.ExitInfo) int {
	// Close first: it records what was still held.
	s.out.Close()
	info.LastSeq = s.out.Seq()
	b, _ := json.Marshal(info)
	path := filepath.Join(proto.ShimRunDir, proto.ExitFile(s.cfg.Epoch))
	_ = os.WriteFile(path+".tmp", b, 0o644)
	_ = os.Rename(path+".tmp", path)
	os.Remove(proto.ShimSocket)
	return info.ExitCode
}

func (s *Shim) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// reap collects every child: the workload and init script, whose status
// is handed on, and orphans, which are just reaped.
func (s *Shim) reap() {
	sigchld := make(chan os.Signal, 16)
	signal.Notify(sigchld, syscall.SIGCHLD)
	for range sigchld {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			s.mu.Lock()
			work, init := s.workPid, s.initPid
			stream := s.streams[pid]
			delete(s.streams, pid)
			s.mu.Unlock()
			switch pid {
			case work:
				s.exitCh <- ws
			case init:
				s.initCh <- ws
			default:
				if stream != nil {
					stream <- ws
				}
			}
		}
	}
}

func (s *Shim) listen() (net.Listener, error) {
	os.Remove(proto.ShimSocket)
	ln, err := net.Listen("unix", proto.ShimSocket)
	if err != nil {
		return nil, err
	}
	// Only root in the container (the shim, and the runner through the
	// idmapped mount) may connect; the workload runs as another user.
	_ = os.Chmod(proto.ShimSocket, 0o600)
	return ln, nil
}

// serve handles runner connections. A runner that restarts reconnects;
// several connections may come and go over the container's life.
func (s *Shim) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(c)
	}
}

func (s *Shim) handleConn(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var m proto.ShimMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			enc.Encode(proto.ShimMsg{Type: "error", Error: "bad message"})
			continue
		}
		reply := proto.ShimMsg{Type: m.Type, OK: true}
		switch m.Type {
		case proto.ShimPing:
		case proto.ShimStart:
			s.mu.Lock()
			first := !s.started
			s.started = true
			s.mu.Unlock()
			if first {
				s.startCh <- m
			}
		case proto.ShimInput:
			if m.Input != nil {
				s.deliver(*m.Input)
			}
		case proto.ShimInterrupt:
			s.mu.Lock()
			ad := s.adapter
			s.mu.Unlock()
			if ad != nil {
				if err := ad.Interrupt(); err != nil {
					reply.OK, reply.Error = false, err.Error()
				}
			}
		case proto.ShimStop:
			s.stop(m.Reason, time.Duration(m.GraceSec*float64(time.Second)))
		case proto.ShimStream:
			// The connection is the stream's from now on.
			if m.Stream != nil {
				s.handleStream(sc, enc, *m.Stream)
			}
			return
		default:
			reply.OK, reply.Error = false, "unknown message "+m.Type
		}
		if err := enc.Encode(reply); err != nil {
			return
		}
	}
}

// deliver hands input to the adapter once per request id: the runner
// redelivers after reconnecting.
func (s *Shim) deliver(in proto.Input) {
	s.mu.Lock()
	if in.RequestID != "" && s.delivered[in.RequestID] {
		s.mu.Unlock()
		return
	}
	ad := s.adapter
	if ad == nil || s.proc == nil {
		s.pending = append(s.pending, in)
		s.mu.Unlock()
		return
	}
	if in.RequestID != "" {
		s.delivered[in.RequestID] = true
	}
	s.mu.Unlock()
	ad.Deliver(in)
}

// stop winds the workload down: the adapter's graceful stop, then SIGKILL
// to its whole process group after the grace period, or after shorter when
// set (the host is being taken away).
func (s *Shim) stop(reason string, shorter time.Duration) {
	s.mu.Lock()
	ad, proc, initPid := s.adapter, s.proc, s.initPid
	if s.stopping {
		s.mu.Unlock()
		// Stopping already: a shorter grace (the host is going) still
		// applies. The earlier timer firing later is harmless.
		if shorter > 0 && proc != nil {
			time.AfterFunc(shorter, func() { _ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL) })
		}
		return
	}
	s.stopping = true
	s.stopWhy = reason
	env := s.env
	s.mu.Unlock()
	s.out.Event(proto.EvStop, map[string]any{"reason": reason})
	// What the workload leaves behind, while it is still whole: before
	// anything is signalled, and inside the grace so a stop is never held
	// up by it. Only once the workload has started (there is nothing to
	// leave before), and not for init, which has no workload yet.
	if len(s.cfg.BeforeStop) > 0 && env != nil && proc != nil {
		limit := time.Duration(s.cfg.BeforeStopTimeout * float64(time.Second))
		if shorter > 0 {
			limit = min(limit, shorter/2)
		}
		start := time.Now()
		s.beforeStop(env, limit)
		if shorter > 0 {
			shorter = max(shorter-time.Since(start), time.Second)
		}
	}
	if initPid > 0 {
		_ = syscall.Kill(-initPid, syscall.SIGTERM)
	}
	if ad != nil && proc != nil {
		if err := ad.Stop(); err != nil {
			s.out.Event(proto.EvWarning, map[string]any{"message": "graceful stop: " + err.Error()})
		}
		grace := time.Duration(s.cfg.GraceSec * float64(time.Second))
		if grace <= 0 {
			grace = 30 * time.Second
		}
		if shorter > 0 {
			grace = min(grace, shorter)
		}
		time.AfterFunc(grace, func() {
			_ = syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL)
		})
	}
	// Not started yet: unblock the wait for start.
	select {
	case s.startCh <- proto.ShimMsg{Type: proto.ShimStart}:
	default:
	}
}

// beforeStop runs the spec's beforeStop command once, as the workload user
// with its environment, its output the Run's, and gives up after limit:
// the command's whole process group is killed then, and the stop goes on.
func (s *Shim) beforeStop(env []string, limit time.Duration) {
	if limit <= 0 {
		limit = 10 * time.Second
	}
	s.out.Event(proto.EvBeforeStop, map[string]any{"phase": "start"})
	cmd := s.command(s.cfg.BeforeStop, env)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	exited, err := s.startTracked(cmd.Start, &cmd.Process)
	if err != nil {
		s.out.Event(proto.EvBeforeStop, map[string]any{"phase": "done", "exitCode": -1, "error": err.Error()})
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyTo(stdout, func(b []byte) { s.out.Write("stdout", b) }) }()
	go func() { defer wg.Done(); copyTo(stderr, func(b []byte) { s.out.Write("stderr", b) }) }()
	timedOut := false
	var ws syscall.WaitStatus
	select {
	case ws = <-exited:
	case <-time.After(limit):
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		ws = <-exited
	}
	wg.Wait()
	s.out.Event(proto.EvBeforeStop, map[string]any{"phase": "done", "exitCode": exitCode(ws), "timedOut": timedOut})
}

func (s *Shim) environment(secrets map[string]string) []string {
	env := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	// Keep what the image set (PATH, LANG, …), minus the shim's own.
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	env["HOME"] = s.user.home
	env["USER"] = s.user.name
	env["LUX_RUN_ID"] = s.cfg.RunID
	env["LUX_EPOCH"] = strconv.Itoa(s.cfg.Epoch)
	if s.cfg.ArtifactsDir != "" {
		env["LUX_ARTIFACTS"] = s.cfg.ArtifactsDir
	}
	for i, svc := range s.cfg.Services {
		k, v := ServiceEnv(svc.Name)
		env[k] = v
		if svc.Loopback {
			env[k+"_URL"] = "http://" + ServiceAddr(i)
		}
	}
	for k, v := range s.cfg.Env {
		env[k] = v
	}
	for _, sec := range s.cfg.Secrets {
		if sec.As == "env" && !sec.RunnerOnly {
			env[sec.Name] = secrets[sec.Name]
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// writeSecretFiles places the Run's file secrets, and any credential files
// its adapter's agent needs (see adapter.CredentialFiles).
func (s *Shim) writeSecretFiles(values map[string]string, ad adapter.Adapter) error {
	for _, sec := range s.cfg.Secrets {
		if sec.As != "file" || sec.RunnerOnly {
			continue
		}
		if err := s.placeSecret(sec.Name, sec.Path, []byte(values[sec.Name])); err != nil {
			return err
		}
	}
	// Secrets the adapter's agent reads from files (Codex's auth.json,
	// Claude Code's MCP config).
	if cf, ok := ad.(adapter.CredentialFiles); ok {
		for path, content := range cf.CredentialFiles(s.cfg, values, s.user.home) {
			// Named with the reserved prefix (by the adapter, for a file on
			// the tmpfs): never a user secret's file.
			var err error
			if filepath.Dir(path) == proto.ShimSecretsDir {
				err = s.writeSecret(filepath.Base(path), content)
			} else {
				err = s.placeSecret(spec.ReservedSecretPrefix+filepath.Base(filepath.Dir(path))+"-"+filepath.Base(path), path, content)
			}
			if err != nil {
				return err
			}
		}
	}
	if s.user.uid != 0 {
		_ = os.Chown(proto.ShimSecretsDir, s.user.uid, s.user.gid)
	}
	return nil
}

// placeSecret writes a secret's value on the secrets tmpfs and links it
// into place. The link can live on a state volume; the value never does,
// so it is never in a snapshot.
func (s *Shim) placeSecret(name, path string, value []byte) error {
	if err := s.writeSecret(name, value); err != nil {
		return err
	}
	src := filepath.Join(proto.ShimSecretsDir, name)
	if err := s.mkdirForWorkload(filepath.Dir(path)); err != nil {
		return err
	}
	os.Remove(path)
	if err := os.Symlink(src, path); err != nil {
		return err
	}
	_ = os.Lchown(path, s.user.uid, s.user.gid)
	return nil
}

// writeSecret writes a value on the secrets tmpfs, readable only by the
// workload user.
func (s *Shim) writeSecret(name string, value []byte) error {
	src := filepath.Join(proto.ShimSecretsDir, name)
	if err := os.WriteFile(src, value, 0o400); err != nil {
		return err
	}
	_ = os.Chown(src, s.user.uid, s.user.gid)
	return nil
}

// withAdapterEnv is the workload's environment: env plus what its adapter
// adds (see adapter.Environment).
func withAdapterEnv(env []string, ad adapter.Adapter, cfg proto.ShimConfig) []string {
	e, ok := ad.(adapter.Environment)
	if !ok {
		return env
	}
	out := slices.Clone(env)
	for k, v := range e.Environment(cfg) {
		out = append(out, k+"="+v)
	}
	return out
}

// mkdirForWorkload creates dir and its missing parents, for a path the
// shim (root) makes on the workload's behalf: a secret under the user's
// home (say ~/.config/tool/key), the artifacts directory. Directories it
// creates inside the workload's own space (its home, or a volume) are the
// workload user's, so the workload can write next to them; anything else
// it creates stays root's. Directories that already existed are never
// touched.
func (s *Shim) mkdirForWorkload(dir string) error {
	var missing []string
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		_, err := os.Stat(d)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := os.Mkdir(d, 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // created meanwhile by someone else: not ours
			}
			return err
		}
		if s.workloadOwns(d) {
			_ = os.Chown(d, s.user.uid, s.user.gid)
		}
	}
	return nil
}

// workloadOwns: path is under the workload user's home or a volume.
func (s *Shim) workloadOwns(path string) bool {
	if s.user.uid == 0 {
		return false
	}
	for _, root := range append([]string{s.user.home}, s.cfg.VolumePaths...) {
		if root != "/" && (path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	return false
}

// prepareVolumes hands a fresh (root-owned) volume's root to the workload
// user, so a non-root workload can write to it.
func (s *Shim) prepareVolumes() {
	// $LUX_ARTIFACTS: the workload's to write, whoever it runs as.
	if s.cfg.ArtifactsDir != "" {
		_ = s.mkdirForWorkload(s.cfg.ArtifactsDir)
		_ = os.Chown(s.cfg.ArtifactsDir, s.user.uid, s.user.gid)
	}
	if s.user.uid == 0 {
		return
	}
	for _, p := range s.cfg.VolumePaths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == 0 {
			_ = os.Chown(p, s.user.uid, s.user.gid)
		}
	}
}

func (s *Shim) command(argv []string, env []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = s.cfg.Workdir
	if cmd.Dir == "" {
		cmd.Dir = s.user.home
	}
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/"
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if s.user.uid != 0 || s.user.gid != 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(s.user.uid), Gid: uint32(s.user.gid), Groups: s.user.groups}
	}
	// Resolve argv[0] with the workload's PATH, not the shim's.
	if !strings.Contains(argv[0], "/") {
		for _, kv := range env {
			if p, ok := strings.CutPrefix(kv, "PATH="); ok {
				for _, dir := range filepath.SplitList(p) {
					cand := filepath.Join(dir, argv[0])
					if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
						cmd.Path = cand
						cmd.Err = nil
						break
					}
				}
			}
		}
	}
	return cmd
}

func (s *Shim) runInit(env []string) (int, error) {
	s.out.Event(proto.EvInit, map[string]any{"phase": "start"})
	cmd := s.command([]string{"/bin/sh", "-c", s.cfg.Init}, env)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	s.mu.Lock()
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return -1, err
	}
	s.initPid = cmd.Process.Pid
	s.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyTo(stdout, func(b []byte) { s.out.Write("stdout", b) }) }()
	go func() { defer wg.Done(); copyTo(stderr, func(b []byte) { s.out.Write("stderr", b) }) }()
	ws := <-s.initCh
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	wg.Wait()
	s.mu.Lock()
	s.initPid = 0
	s.mu.Unlock()
	code := exitCode(ws)
	s.out.Event(proto.EvInit, map[string]any{"phase": "done", "exitCode": code})
	return code, nil
}

func (s *Shim) startWorkload(argv []string, env []string) (*adapter.Process, error) {
	cmd := s.command(argv, env)
	if s.cfg.TTY {
		return s.startWorkloadTTY(cmd, argv)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	s.workPid = cmd.Process.Pid
	s.proc = &adapter.Process{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}
	return s.proc, nil
}

// startWorkloadTTY starts a generic workload on a PTY (workload.tty): its
// terminal output goes to the output file as stdout, and to whoever is
// attached; input goes to the terminal.
func (s *Shim) startWorkloadTTY(cmd *exec.Cmd, argv []string) (*adapter.Process, error) {
	cmd.SysProcAttr.Setpgid = false // a session leader, as a PTY makes it
	s.mu.Lock()
	defer s.mu.Unlock()
	ptmx, err := pty.StartWithSize(cmd, winsize(0, 0))
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	s.workPid = cmd.Process.Pid
	s.term = newTerminal(ptmx)
	s.proc = &adapter.Process{Cmd: cmd, Stdin: ptmx, Stdout: io.NopCloser(s.term), Stderr: io.NopCloser(strings.NewReader(""))}
	return s.proc, nil
}

func copyTo(r io.Reader, f func([]byte)) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			f(append([]byte{}, buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// sink connects an adapter to the output file.
type sink struct{ s *Shim }

func (k *sink) Stdout(p []byte)            { k.s.out.Write("stdout", p) }
func (k *sink) EndMessage()                { k.s.out.EndLine("stdout") }
func (k *sink) Stderr(p []byte)            { k.s.out.Write("stderr", p) }
func (k *sink) Event(typ string, data any) { k.s.out.Event(typ, data) }
func (k *sink) Stream(typ, key, text string, wrap func(string) any) {
	k.s.out.Stream(typ, key, text, wrap)
}
func (k *sink) Session(id string) { k.s.out.Event(proto.EvSession, map[string]string{"sessionId": id}) }
func (k *sink) Activity(idle bool) {
	a := "busy"
	if idle {
		a = "idle"
	}
	k.s.out.Event(proto.EvActivity, map[string]string{"activity": a})
}
func (k *sink) InputAck(in proto.Input, err error) {
	if in.RequestID == "" {
		return
	}
	// What was delivered, up to a limit (the record stream is not for
	// whole files); secrets in it are redacted like all output.
	d := map[string]any{"requestId": in.RequestID}
	text := in.Text
	if text == "" && len(in.Raw) > 0 {
		text = string(in.Raw)
	}
	if text != "" {
		// Redacted before it is cut, so a secret straddling the cut is not
		// half kept; cut on a rune boundary.
		text = k.s.red.Redact(text)
		if len(text) > maxAckedText {
			cut := maxAckedText
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			// Not inside a redaction marker, either: the last one starting
			// before the cut must also end before it.
			if open := strings.LastIndex(text[:cut], "["); open >= 0 && strings.HasPrefix(text[open:], "[REDACTED:") &&
				!strings.Contains(text[open:cut], "]") {
				cut = open
			}
			text, d["truncated"] = text[:cut], true
		}
		d["text"] = text
	}
	if err != nil {
		d["error"] = err.Error()
	}
	k.s.out.Event(proto.EvInputAck, d)
}

// maxAckedText caps the input text an ack repeats.
const maxAckedText = 64 << 10

// ---- users ------------------------------------------------------------------

type userInfo struct {
	name     string
	uid, gid int
	home     string
	groups   []uint32
}

// lookupUser resolves the workload user against the image's /etc/passwd.
func lookupUser(spec string) (*userInfo, error) {
	b, _ := os.ReadFile("/etc/passwd")
	u, err := passwd.Lookup(spec, b)
	if err != nil {
		return nil, err
	}
	return &userInfo{name: u.Name, uid: u.UID, gid: u.GID, home: u.Home, groups: []uint32{uint32(u.GID)}}, nil
}
