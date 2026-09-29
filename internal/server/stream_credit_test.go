package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// A grant the host's queue refused is not lost: a reader waiting for frames
// (none can come: the runner has no credit) retries it until it goes out.
func TestAnOwedGrantIsRetriedWhileTheReaderWaits(t *testing.T) {
	s := &Server{}
	s.hub = newHub(s)
	ch := make(chan proto.Frame, 1)
	h := &hubStream{s: s, hostID: "h1", id: "st_1", ch: ch, gone: make(chan struct{}), window: 4}

	// Two frames taken, half the window: the grant fails (no connection).
	h.took()
	h.took()
	if h.taken != 2 {
		t.Fatalf("taken = %d, want the failed grant kept owed (2)", h.taken)
	}

	// The host's connection is back; no frame comes, only the retry.
	conn := &runnerConn{hostID: "h1", send: make(chan proto.Frame, 1), done: make(chan struct{})}
	s.hub.mu.Lock()
	s.hub.conns["h1"] = conn
	s.hub.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*grantRetry)
	defer cancel()
	if _, _, why := h.recv(ctx); why != recvOwed {
		t.Fatalf("recv = %v, want the owed grant retried", why)
	}
	select {
	case f := <-conn.send:
		var d proto.StreamData
		if err := json.Unmarshal(f.Data, &d); err != nil || d.Credit != 2 || f.Stream != "st_1" {
			t.Fatalf("grant = %+v %+v", f, d)
		}
	case <-time.After(time.Second):
		t.Fatal("no grant sent")
	}
	if h.taken != 0 {
		t.Fatalf("taken = %d after the grant went out", h.taken)
	}
	// Nothing owed: recv waits for frames, not the retry.
	ch <- proto.Frame{Type: proto.MsgStreamData}
	if _, ok, why := h.recv(ctx); why != recvFrame || !ok {
		t.Fatalf("recv = %v %v, want the frame", why, ok)
	}
}
