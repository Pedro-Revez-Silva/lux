package shim

import (
	"io"
	"os/exec"
	"testing"

	"github.com/creack/pty"
)

// BenchmarkTerminalThroughput reads a fast producer through a PTY as the
// workload's output is read, with and without the emulator. One op is the
// whole command; MB/s is its output.
//
//	go test -run X -bench TerminalThroughput -benchtime 1x ./internal/shim/
func BenchmarkTerminalThroughput(b *testing.B) {
	for _, p := range []struct{ name, cmd string }{
		{"yes", "yes | head -c 200M"},
		{"text", "yes 'the quick brown fox jumps over the lazy dog, 0123456789 ABCDEFGHIJ' | head -c 200M"},
	} {
		for _, emulate := range []bool{false, true} {
			name := p.name + "/none"
			if emulate {
				name = p.name + "/emulator"
			}
			b.Run(name, func(b *testing.B) {
				b.SetBytes(200 << 20)
				for b.Loop() {
					cmd := exec.Command("sh", "-c", p.cmd)
					ptmx, err := pty.StartWithSize(cmd, winsize(24, 80))
					if err != nil {
						b.Skip(err)
					}
					term := newTerminal(ptmx, 24, 80)
					if !emulate {
						term.screen = nil
					}
					n, _ := io.Copy(io.Discard, term)
					_ = cmd.Wait()
					ptmx.Close()
					if n < 200<<20 {
						b.Fatalf("read %d bytes", n)
					}
				}
			})
		}
	}
}
