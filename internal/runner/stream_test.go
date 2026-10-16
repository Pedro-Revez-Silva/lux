package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marcioapm/lux/internal/proto"
)

// A tunnel's output waits for credit: a window's worth, then only as luxd
// grants more. Without a window, nothing waits.
func TestStreamCredit(t *testing.T) {
	if newCredit(0) != nil {
		t.Fatal("no window, yet flow control")
	}
	var none *credit
	if err := none.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := newCredit(2)
	ctx := context.Background()
	for range 2 {
		if err := c.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.take(short); err == nil {
		t.Fatal("took past the window")
	}
	done := make(chan error)
	go func() { done <- c.take(ctx) }()
	time.Sleep(20 * time.Millisecond)
	c.grant(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a grant did not wake the taker")
	}
}

// streamRig is a Runner with one running placement whose shim socket is
// served by the test (shim), and a luxd WebSocket whose received stream
// frames arrive on frames.
type streamRig struct {
	r      *Runner
	shim   net.Listener
	frames chan proto.Frame
}

func newStreamRig(t *testing.T) *streamRig {
	t.Helper()
	// A unix socket path is limited to 108 bytes: t.TempDir's may be longer.
	dir, err := os.MkdirTemp("", "lux-stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	shim, err := net.Listen("unix", filepath.Join(dir, "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shim.Close() })

	frames := make(chan proto.Frame, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			var f proto.Frame
			if readFrame(req.Context(), ws, &f) != nil {
				return
			}
			frames <- f
		}
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })

	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		placements: map[string]*placement{}, streams: streams{m: map[string]*stream{}}}
	r.conn = newConn(r)
	r.conn.ws = ws
	r.mounts.Store(runtimeVolume("run_s"), dir)
	r.placements["run_s"] = &placement{r: r, runID: "run_s", epoch: 1, phase: "running"}
	return &streamRig{r: r, shim: shim, frames: frames}
}

// open starts a stream of the given kind as handleStreamFrame does, with
// an unbuffered input: a send into it returns only once the stream's input
// goroutine has finished writing the frame before. It returns once the
// fake shim has the stream's connection and its ShimStream line.
func (g *streamRig) open(t *testing.T, kind string) (*stream, *net.UnixConn, <-chan struct{}) {
	t.Helper()
	sctx, cancel := context.WithCancelCause(context.Background())
	st := &stream{input: make(chan proto.StreamData), cancel: cancel}
	g.r.streams.put("str_1", st)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		g.r.runStream(sctx, "run_s", 1, "str_1", proto.StreamOpen{Kind: kind, Command: []string{"true"}}, st)
	}()
	return st, g.accept(t, kind), ended
}

// accept takes the stream's connection on the fake shim and checks its
// ShimStream line names the stream's kind.
func (g *streamRig) accept(t *testing.T, kind string) *net.UnixConn {
	t.Helper()
	c, err := g.shim.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m proto.ShimMsg
	if err := json.Unmarshal(line, &m); err != nil || m.Type != proto.ShimStream || m.Stream == nil || m.Stream.Kind != kind {
		t.Fatalf("shim got %q, want a ShimStream of kind %q", line, kind)
	}
	return c.(*net.UnixConn)
}

// received waits for the stream to end and returns every stream frame luxd
// received for it, in order.
func (g *streamRig) received(t *testing.T, ended <-chan struct{}) []proto.Frame {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never ended")
	}
	// Frames are written before runStream returns; a ping on the same
	// connection, received after them, says they have all arrived.
	if err := g.r.conn.Send(context.Background(), proto.Frame{Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var out []proto.Frame
	for {
		select {
		case f := <-g.frames:
			if f.Type == "ping" {
				return out
			}
			out = append(out, f)
		case <-time.After(10 * time.Second):
			t.Fatal("luxd's frames never arrived")
		}
	}
}

// closes waits for the stream to end and returns every stream.close luxd
// received for it.
func (g *streamRig) closes(t *testing.T, ended <-chan struct{}) []proto.StreamData {
	t.Helper()
	var out []proto.StreamData
	for _, f := range g.received(t, ended) {
		if f.Type == proto.MsgStreamClose {
			out = append(out, streamData(t, f))
		}
	}
	return out
}

func streamData(t *testing.T, f proto.Frame) proto.StreamData {
	t.Helper()
	var d proto.StreamData
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// feed sends each of ds into the stream's input in turn, stopping early if
// the stream ends.
func feed(t *testing.T, st *stream, ended <-chan struct{}, ds []proto.StreamData) {
	t.Helper()
	for _, d := range ds {
		select {
		case st.input <- d:
		case <-ended:
			return
		case <-time.After(10 * time.Second):
			t.Fatal("the stream's input goroutine stopped taking input")
		}
	}
}

func wantOneExitZeroClose(t *testing.T, closes []proto.StreamData) {
	t.Helper()
	if len(closes) != 1 {
		t.Fatalf("%d stream.close frames %+v, want 1", len(closes), closes)
	}
	if c := closes[0]; c.Error != "" || c.ExitCode == nil || *c.ExitCode != 0 {
		t.Fatalf("stream.close %+v, want exit code 0 and no error", c)
	}
}

// An exec'd command that exits without reading its stdin: the shim stops
// reading the stream's connection, and the client's EOF (and any input)
// fails to be written. That is not the stream's end: the shim's exit line
// is, and it reaches luxd as the one stream.close.
func TestStreamExecExitsBeforeItsInput(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t, "exec")

	// Stands in for the shim having closed before the runner's write:
	// every write now fails with EPIPE while the exit line can still be
	// sent.
	if err := shim.CloseRead(); err != nil {
		t.Fatal(err)
	}
	// The second feed returns once the first frame's write has failed; the
	// third, once the second's has.
	feed(t, st, ended, []proto.StreamData{{EOF: true}, {Data: []byte("late")}, {Data: []byte("later")}})
	if _, err := shim.Write([]byte(`{"exitCode":0}` + "\n")); err != nil {
		t.Fatalf("the runner closed the shim connection before the exit line: %v", err)
	}
	shim.Close()

	wantOneExitZeroClose(t, g.closes(t, ended))
}

// The same with the real shim's order: the exit line written and the
// connection closed before the input arrives.
func TestStreamExecExitedAndClosedBeforeItsInput(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t, "exec")
	if _, err := shim.Write([]byte(`{"exitCode":0}` + "\n")); err != nil {
		t.Fatal(err)
	}
	shim.Close()
	feed(t, st, ended, []proto.StreamData{{EOF: true}, {Data: []byte("late")}})

	wantOneExitZeroClose(t, g.closes(t, ended))
}

// A shim that goes away mid-stream, with no exit line, still ends the
// stream with an error, input written to it or not; its output before
// that is relayed.
func TestStreamShimGoneMidStream(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t, "exec")
	if _, err := shim.Write([]byte(`{"ch":"stdout","data":"aGk="}` + "\n")); err != nil {
		t.Fatal(err)
	}
	shim.Close()
	feed(t, st, ended, []proto.StreamData{{Data: []byte("more")}, {EOF: true}})

	frames := g.received(t, ended)
	if len(frames) != 2 || frames[0].Type != proto.MsgStreamData || frames[1].Type != proto.MsgStreamClose {
		t.Fatalf("stream frames %+v, want one stream.data then one stream.close", frames)
	}
	if d := streamData(t, frames[0]); d.Channel != "stdout" || string(d.Data) != "hi" {
		t.Fatalf("stream.data %+v, want stdout %q", d, "hi")
	}
	if c := streamData(t, frames[1]); c.ExitCode != nil || c.Error != "the container went away" {
		t.Fatalf("stream.close %+v, want error %q and no exit code", c, "the container went away")
	}
}

// The attached workload exits while the client is still typing: its
// "the workload exited" line, not the failed write, ends the stream.
func TestStreamAttachWorkloadExitedBeforeItsInput(t *testing.T) {
	g := newStreamRig(t)
	st, shim, ended := g.open(t, "attach")
	if _, err := shim.Write([]byte(`{"error":"the workload exited"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	shim.Close()
	feed(t, st, ended, []proto.StreamData{{Data: []byte("ls\n")}, {Data: []byte("late")}})

	closes := g.closes(t, ended)
	if len(closes) != 1 || closes[0].Error != "the workload exited" || closes[0].ExitCode != nil {
		t.Fatalf("stream.close frames %+v, want one with error %q and no exit code", closes, "the workload exited")
	}
}

// Through handleStreamFrame, with its bounded input: input the shim no
// longer reads is drained and dropped, so more of it than the buffer holds
// does not end the stream as having fallen behind; the exit line does.
func TestStreamExecInputPastTheBufferAfterItsTargetStopped(t *testing.T) {
	g := newStreamRig(t)
	ctx := context.Background()
	g.r.handleStreamFrame(ctx, proto.Frame{Type: proto.MsgStreamOpen, RunID: "run_s", Epoch: 1, Stream: "str_1",
		Data: proto.Marshal(proto.StreamOpen{Kind: "exec", Command: []string{"true"}})})
	shim := g.accept(t, "exec")
	st := g.r.streams.get("str_1")
	if st == nil {
		t.Fatal("stream.open registered no stream")
	}
	// runStream removes the stream as the last thing it does.
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for g.r.streams.get("str_1") != nil {
			time.Sleep(time.Millisecond)
		}
	}()
	// Stands in for the shim having closed before the runner's write.
	if err := shim.CloseRead(); err != nil {
		t.Fatal(err)
	}
	deliver := func(n int) {
		for i := range n {
			g.r.handleStreamFrame(ctx, proto.Frame{Type: proto.MsgStreamData, RunID: "run_s", Epoch: 1, Stream: "str_1",
				Data: proto.Marshal(proto.StreamData{Data: []byte{byte(i)}})})
		}
	}
	// Each batch fits the buffer; between them the input goroutine must
	// empty it, or the stream must have ended.
	drained := func() {
		deadline := time.Now().Add(10 * time.Second)
		for len(st.input) > 0 {
			select {
			case <-ended:
				return
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d input frames never drained", len(st.input))
			}
			time.Sleep(time.Millisecond)
		}
	}
	for range 3 {
		deliver(streamInputBuffer)
		drained()
	}
	if _, err := shim.Write([]byte(`{"exitCode":0}` + "\n")); err != nil {
		t.Fatalf("the runner closed the shim connection before the exit line: %v", err)
	}
	shim.Close()

	wantOneExitZeroClose(t, g.closes(t, ended))
}
