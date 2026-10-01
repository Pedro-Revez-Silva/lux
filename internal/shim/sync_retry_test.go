package shim

import (
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// awaitSyncRetry, the shim's wait for the runner's whole-history bundles
// before init: nil once the wait elapses, the runner's answer as soon as
// it comes (an empty one too), and nil within its 1s poll once the Run is
// stopping.
func TestAwaitSyncRetry(t *testing.T) {
	newShim := func() *Shim { return &Shim{syncCh: make(chan *proto.SyncArgs, 1)} }
	t.Run("no answer: nil after the wait", func(t *testing.T) {
		s := newShim()
		start := time.Now()
		if got := s.awaitSyncRetry(200 * time.Millisecond); got != nil {
			t.Fatalf("got %+v", got)
		}
		if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
			t.Fatalf("returned after %s", d)
		}
	})
	t.Run("answered: the args", func(t *testing.T) {
		for _, want := range []*proto.SyncArgs{{Repos: []proto.SyncRepo{{Name: "app", Bundle: "/.lux/run/sync/resume/app.bundle"}}}, {}} {
			s := newShim()
			go func() {
				time.Sleep(50 * time.Millisecond)
				s.syncCh <- want
			}()
			start := time.Now()
			if got := s.awaitSyncRetry(time.Minute); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("returned after %s", d)
			}
		}
	})
	t.Run("stopping: nil within about 1s", func(t *testing.T) {
		s := newShim()
		go func() {
			time.Sleep(100 * time.Millisecond)
			s.mu.Lock()
			s.stopping = true
			s.mu.Unlock()
		}()
		start := time.Now()
		if got := s.awaitSyncRetry(time.Minute); got != nil {
			t.Fatalf("got %+v", got)
		}
		if d := time.Since(start); d > 2500*time.Millisecond {
			t.Fatalf("returned %s after", d)
		}
	})
}
