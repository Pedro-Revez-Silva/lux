package cli

import (
	"net/http"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marcioapm/lux/internal/proto"
)

// lux attach asks luxd for a restore with --restore only; without a
// terminal on stdin it sends no size.
func TestAttachRestoreQuery(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"attach", "run_1"}, ""},
		{[]string{"attach", "--restore", "run_1"}, "restore=true"},
	} {
		urls := make(chan string, 1)
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			urls <- r.URL.Path + "?" + r.URL.RawQuery
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			_ = wsjson.Write(r.Context(), ws, proto.StreamData{Channel: "stdout", Data: []byte("screen")})
			_ = wsjson.Write(r.Context(), ws, proto.StreamData{Error: "the workload exited"})
		})
		out, err := runCLI(t, h, tc.args...)
		if err == nil || err.Error() != "the workload exited" {
			t.Fatalf("%v: error %v", tc.args, err)
		}
		if out != "screen" {
			t.Fatalf("%v: output %q", tc.args, out)
		}
		if got := <-urls; got != "/v1/runs/run_1/attach?"+tc.want {
			t.Fatalf("%v: dialled %q, want query %q", tc.args, got, tc.want)
		}
	}
}

// attach --restore sends the terminal's size, clamped to 200×500, the
// most luxd accepts.
func TestAttachPathClampsSize(t *testing.T) {
	for _, tc := range []struct {
		restore    bool
		rows, cols int
		want       string
	}{
		{false, 300, 900, "/v1/runs/r/attach"},
		{true, 0, 0, "/v1/runs/r/attach?restore=true"},
		{true, 30, 100, "/v1/runs/r/attach?cols=100&restore=true&rows=30"},
		{true, 300, 900, "/v1/runs/r/attach?cols=500&restore=true&rows=200"},
		{true, 150, 900, "/v1/runs/r/attach?cols=500&restore=true&rows=150"},
		{true, 300, 80, "/v1/runs/r/attach?cols=80&restore=true&rows=200"},
	} {
		if got := attachPath("r", tc.restore, tc.rows, tc.cols); got != tc.want {
			t.Fatalf("restore %v, %dx%d: %q, want %q", tc.restore, tc.rows, tc.cols, got, tc.want)
		}
	}
}
