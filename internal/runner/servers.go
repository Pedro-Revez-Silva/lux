package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// A placement's servers (luxd's MsgServers): the set luxd wants now, kept
// (newest rev wins) in a file beside its state so a restarted runner has it,
// handed to the shim (which runs the commands), and health-checked here:
// every serverCheckEvery the runner dials each server's port on the
// container, and reports ready when it accepts, unreachable when a ready
// one refuses twice in a row. The shim's lux.server exit records, seen by
// tailEvents, are reported as exited. Every report carries the server's
// gen: luxd ignores one of an earlier start.

const serverCheckEvery = 3 * time.Second

// serverHealth is what the runner last reported of one server's gen.
type serverHealth struct {
	gen      int64
	reported string // ready | unreachable | exited, or "" (nothing yet)
	refused  int
}

// setServers takes luxd's set, if newer than the one it has.
func (p *placement) setServers(ctx context.Context, sv proto.Servers) {
	p.mu.Lock()
	if p.srvSet != nil && p.srvSet.Rev >= sv.Rev {
		p.mu.Unlock()
		return
	}
	p.srvSet = &sv
	p.mu.Unlock()
	// A file of its own: state.json is written by the placement's own
	// goroutine; sets come in order on the Run's control queue.
	if b, err := json.Marshal(sv); err == nil {
		if err := os.MkdirAll(p.dir, 0o700); err == nil {
			_ = writeFileAtomic(filepath.Join(p.dir, serversFile(p.epoch)), b, 0o600)
		}
	}
	p.sendServers()
}

// serversFile holds a placement's servers set, for a restarted runner.
func serversFile(epoch int) string { return fmt.Sprintf("servers-%d.json", epoch) }

// loadServers reads a placement's servers set back (nil: none).
func loadServers(dir string, epoch int) *proto.Servers {
	b, err := os.ReadFile(filepath.Join(dir, serversFile(epoch)))
	if err != nil {
		return nil
	}
	var sv proto.Servers
	if json.Unmarshal(b, &sv) != nil {
		return nil
	}
	return &sv
}

// servers is the set luxd wants now (nil: none yet).
func (p *placement) servers() *proto.Servers {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.srvSet
}

// sendServers hands the shim the commands to run. Before the shim is
// connected this does nothing: startShim sends the set right after start.
func (p *placement) sendServers() {
	sv := p.servers()
	if sv == nil {
		return
	}
	var run []proto.ServerSpec
	for _, s := range sv.Servers {
		if len(s.Command) > 0 {
			run = append(run, s)
		}
	}
	if err := p.sendShim(proto.ShimMsg{Type: proto.ShimServers, Servers: nonNilServers(run)}); err != nil && p.liveState() == "running" {
		p.logf("servers: shim not reachable", "err", err)
	}
}

func nonNilServers(s []proto.ServerSpec) []proto.ServerSpec {
	if s == nil {
		return []proto.ServerSpec{}
	}
	return s
}

// serverPortAllowed: a tunnel may reach any of the Run's servers' ports.
func (p *placement) serverPortAllowed(port int) bool {
	sv := p.servers()
	return sv != nil && slices.Contains(sv.Ports, port)
}

// checkServers health-checks the placement's servers until ctx ends or
// the placement is no longer running.
func (p *placement) checkServers(ctx context.Context) {
	health := map[string]*serverHealth{}
	t := time.NewTicker(serverCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-t.C:
		}
		if p.liveState() != "running" {
			continue
		}
		sv := p.servers()
		if sv == nil {
			continue
		}
		ip, err := p.containerIP(ctx)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, s := range sv.Servers {
			seen[s.Name] = true
			h := health[s.Name]
			if h == nil || h.gen != s.Gen {
				h = &serverHealth{gen: s.Gen}
				health[s.Name] = h
			}
			if p.serverExited(s.Name, s.Gen) {
				continue
			}
			open := dialOK(ctx, ip, s.Port)
			var report string
			switch {
			case open:
				h.refused = 0
				if h.reported != "ready" {
					report = "ready"
				}
			case h.reported == "ready":
				h.refused++
				if h.refused >= 2 {
					report = "unreachable"
				}
			}
			if report == "" {
				continue
			}
			if p.reportServer(ctx, map[string]any{"name": s.Name, "gen": s.Gen, "state": report}) == nil {
				h.reported = report
			}
		}
		for name := range health {
			if !seen[name] {
				delete(health, name)
			}
		}
	}
}

func dialOK(ctx context.Context, ip string, port int) bool {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (p *placement) reportServer(ctx context.Context, data map[string]any) error {
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return p.report(c, proto.MsgRunEvent, proto.RunEvent{Type: proto.EvServerState, Data: data})
}

// serverExited: the shim reported this gen of the server exited.
func (p *placement) serverExited(name string, gen int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.exited[name]
	return ok && g == gen
}

// onServerRecord handles a ch=server lifecycle record from the output
// file: an exit is reported to luxd.
func (p *placement) onServerRecord(ctx context.Context, rec proto.Record) {
	if rec.Event == nil {
		return
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Phase    string `json:"phase"`
			Gen      int64  `json:"gen"`
			ExitCode *int   `json:"exitCode"`
			Error    string `json:"error"`
		} `json:"data"`
	}
	if json.Unmarshal(rec.Event, &ev) != nil || ev.Type != proto.EvServer || ev.Data.Phase != "exit" {
		return
	}
	p.mu.Lock()
	if p.exited == nil {
		p.exited = map[string]int64{}
	}
	p.exited[rec.Server] = ev.Data.Gen
	p.mu.Unlock()
	d := map[string]any{"name": rec.Server, "gen": ev.Data.Gen, "state": "exited"}
	if ev.Data.ExitCode != nil {
		d["exitCode"] = *ev.Data.ExitCode
	}
	if ev.Data.Error != "" {
		d["error"] = ev.Data.Error
	}
	_ = p.reportServer(ctx, d)
}
