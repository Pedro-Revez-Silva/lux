package shim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// Servers: commands the runner asks the shim to keep running beside the
// workload (a Run's servers with a command). Each runs detached from the
// workload, in its own process group, as the workload's user with its
// environment plus the server's own, in its workdir. Its output goes to
// the output file as ch=server records naming it; its start and exit as
// lux.server events there too, which is how the runner learns it exited.
//
// The runner sends the whole set it wants (ShimServers) whenever it
// changes. A server not in it, or there with another gen, is stopped; one
// there and not running is started, once per gen: one that exited stays
// exited until the runner sends a new gen (a restart).

type serverProc struct {
	gen  int64
	pgid int
	done chan struct{}
}

type servers struct {
	mu sync.Mutex
	// ready: the workload's user and environment are known and init is
	// done; before, a set is only kept.
	ready bool
	want  []proto.ServerSpec
	procs map[string]*serverProc
	// started: every name/gen the shim has started, so an exited one is
	// not started again for the same gen.
	started map[string]int64
}

// setServers takes the runner's set and reconciles to it once the shim
// is ready.
func (s *Shim) setServers(want []proto.ServerSpec) {
	s.srv.mu.Lock()
	s.srv.want = want
	ready := s.srv.ready
	s.srv.mu.Unlock()
	if ready {
		s.reconcileServers()
	}
}

// serversReady starts the servers the runner asked for so far: called
// once init is done, before the workload starts.
func (s *Shim) serversReady() {
	s.srv.mu.Lock()
	s.srv.ready = true
	s.srv.mu.Unlock()
	s.reconcileServers()
}

func (s *Shim) reconcileServers() {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	want := map[string]proto.ServerSpec{}
	for _, sv := range s.srv.want {
		if len(sv.Command) > 0 {
			want[sv.Name] = sv
		}
	}
	for name, p := range s.srv.procs {
		if w, ok := want[name]; !ok || w.Gen != p.gen {
			stopServerProc(p)
			delete(s.srv.procs, name)
		}
	}
	for name, w := range want {
		if s.srv.procs[name] != nil || s.srv.started[name] == w.Gen {
			continue
		}
		s.srv.started[name] = w.Gen
		if p := s.startServer(w); p != nil {
			s.srv.procs[name] = p
		}
	}
}

// stopServerProc stops a server's process group: TERM, then KILL.
func stopServerProc(p *serverProc) {
	_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	}()
}

// startServer starts one server's command. Must be called with srv.mu
// held. A command that cannot start is recorded as exited at once.
func (s *Shim) startServer(sv proto.ServerSpec) *serverProc {
	s.mu.Lock()
	env := s.env
	s.mu.Unlock()
	for k, v := range sv.Env {
		env = append(env, k+"="+v)
	}
	cmd := s.command(sv.Command, env)
	if dir := sv.Workdir; dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(s.user.home, dir)
		}
		cmd.Dir = dir
	}
	exitEvent := func(code int, msg string) {
		d := map[string]any{"phase": "exit", "gen": sv.Gen, "exitCode": code}
		if msg != "" {
			d["error"] = msg
		}
		s.out.ServerEvent(sv.Name, d)
	}
	if fi, err := os.Stat(cmd.Dir); err != nil || !fi.IsDir() {
		exitEvent(127, fmt.Sprintf("workdir %s: not a directory", cmd.Dir))
		return nil
	}
	stdout, e1 := cmd.StdoutPipe()
	stderr, e2 := cmd.StderrPipe()
	if e1 != nil || e2 != nil {
		exitEvent(127, "pipes: "+fmt.Sprint(e1, e2))
		return nil
	}
	exited, err := s.startTracked(cmd.Start, &cmd.Process)
	if err != nil {
		exitEvent(127, err.Error())
		return nil
	}
	p := &serverProc{gen: sv.Gen, pgid: cmd.Process.Pid, done: make(chan struct{})}
	s.out.ServerEvent(sv.Name, map[string]any{"phase": "start", "gen": sv.Gen, "pid": cmd.Process.Pid})
	last := &lastLine{}
	drain := copyOutputs([]io.Reader{stdout, stderr}, func(i int, b []byte) {
		if i == 1 {
			last.Write(b)
			s.out.WriteServer(sv.Name, "stderr", b)
		} else {
			s.out.WriteServer(sv.Name, "stdout", b)
		}
	})
	go func() {
		ws := <-exited
		// Whatever it left behind in its group goes with it.
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
		drain()
		close(p.done)
		s.srv.mu.Lock()
		if s.srv.procs[sv.Name] == p {
			delete(s.srv.procs, sv.Name)
		}
		s.srv.mu.Unlock()
		exitEvent(exitCode(ws), s.red.Redact(last.String()))
	}()
	return p
}

// lastLine keeps the last non-empty line written to it (at most 1000
// bytes of it): a server's error, when it exits.
type lastLine struct {
	mu   sync.Mutex
	cur  []byte
	last string
}

func (l *lastLine) Write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.cur = append(l.cur, p...)
			if len(l.cur) > 4096 {
				l.cur = l.cur[len(l.cur)-4096:]
			}
			return
		}
		l.cur = append(l.cur, p[:i]...)
		if t := strings.TrimSpace(string(l.cur)); t != "" {
			l.last = t
		}
		l.cur = l.cur[:0]
		p = p[i+1:]
	}
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.last
	if t := strings.TrimSpace(string(l.cur)); t != "" {
		s = t
	}
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return strings.ToValidUTF8(s, "")
}
