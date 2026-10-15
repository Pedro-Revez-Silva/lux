package runner

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"strconv"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// What a placement's end is reported as, from the shim's exit file as the
// shim writes it and podman's OOMKilled.
func TestOutOfMemoryIsTheRunsOwnLimit(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	exitFile := func(info proto.ExitInfo) []byte {
		b, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		name   string
		file   []byte
		err    error
		podman bool
		want   bool
	}{
		{"a nested container was OOM-killed; the Run exited 0",
			exitFile(proto.ExitInfo{ExitCode: 0, Reason: "exited", MemoryOOM: n(0)}), nil, true, false},
		{"the Run reached its limit, reclaimed, and exited 0",
			exitFile(proto.ExitInfo{ExitCode: 0, Reason: "exited", MemoryOOM: n(1)}), nil, true, false},
		{"the Run's limit fired and the workload died by SIGKILL",
			exitFile(proto.ExitInfo{ExitCode: 137, Signal: "killed", Reason: "exited", MemoryOOM: n(3)}), nil, true, true},
		{"the Run's limit fired and the workload's shell reports its child SIGKILLed",
			exitFile(proto.ExitInfo{ExitCode: 137, Reason: "exited", MemoryOOM: n(1)}), nil, true, true},
		{"a nested container was OOM-killed and the workload passed on its 137",
			exitFile(proto.ExitInfo{ExitCode: 137, Reason: "exited", MemoryOOM: n(0)}), nil, true, false},
		{"the workload was SIGKILLed with no OOM in the Run's cgroup",
			exitFile(proto.ExitInfo{ExitCode: 137, Signal: "killed", Reason: "exited", MemoryOOM: n(0)}), nil, true, false},
		{"the Run's limit fired, the workload failed otherwise",
			exitFile(proto.ExitInfo{ExitCode: 1, Reason: "exited", MemoryOOM: n(2)}), nil, true, false},
		{"stop wins after earlier memory pressure",
			exitFile(proto.ExitInfo{ExitCode: 137, Signal: "killed", Reason: "stopped", MemoryOOM: n(3)}), nil, true, false},
		{"spot reclaim wins after earlier memory pressure",
			exitFile(proto.ExitInfo{ExitCode: 137, Signal: "killed", Reason: "spot-reclaim", MemoryOOM: n(3)}), nil, true, false},
		// Without the shim's count, podman's account is the only one.
		{"a shim that predates the count; podman says OOM",
			[]byte(`{"exitCode":0,"reason":"exited","lastSeq":4}`), nil, true, true},
		{"a shim that predates the count; podman says no OOM",
			[]byte(`{"exitCode":137,"signal":"killed","reason":"exited","lastSeq":4}`), nil, false, false},
		{"no exit file (the shim was killed); podman says OOM",
			nil, fs.ErrNotExist, true, true},
		{"no exit file; podman says no OOM",
			nil, fs.ErrNotExist, false, false},
	}
	for _, c := range cases {
		e := exitFrom(137, c.file, c.err)
		if got := ranOutOfMemory(e, c.podman); got != c.want {
			t.Errorf("%s: oom %v, want %v (record %+v)", c.name, got, c.want, e)
		}
	}
	// The count travels as the shim writes it.
	e := exitFrom(0, exitFile(proto.ExitInfo{ExitCode: 137, Signal: "killed", Reason: "exited", MemoryOOM: n(5), LastSeq: 9}), nil)
	if e.Code != 137 || e.OutputSeq != 9 || e.memoryOOM == nil || *e.memoryOOM != 5 || e.signal != "killed" {
		t.Fatalf("exit record %+v", e)
	}
	if e := exitFrom(1, nil, errors.New("gone")); e.Reason != "crashed" || !e.Failed {
		t.Fatalf("no exit file: %+v", e)
	}
}

// LUX_CPUS is the spec's cpus as written; LUX_MEMORY_LIMIT the limit the
// container is given, scaled to the host's share, in whole pages;
// nothing else is added.
func TestSizeEnv(t *testing.T) {
	page := int64(os.Getpagesize())
	scale, err := newMemoryScale(32<<30+512<<20, 512<<20, 64<<30) // half
	if err != nil {
		t.Fatal(err)
	}
	limit := scale.limit(1<<30 + 3)
	got := withSizeEnv(map[string]string{"A": "1"}, 0.5, limit)
	want := map[string]string{"A": "1", "LUX_CPUS": "0.5", "LUX_MEMORY_LIMIT": strconv.FormatInt(limit, 10)}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if limit%page != 0 || limit > (1<<30+3)/2 || limit < (1<<30)/2-page {
		t.Fatalf("limit %d", limit)
	}
	for cpus, s := range map[float64]string{2: "2", 0.25: "0.25", 1.5: "1.5", 16: "16"} {
		if got := withSizeEnv(nil, cpus, 1<<30)["LUX_CPUS"]; got != s {
			t.Errorf("cpus %v: LUX_CPUS %q", cpus, got)
		}
	}
	// An unscaled limit not on a page: memory.max holds whole pages.
	if got := withSizeEnv(nil, 1, 3*page+5)["LUX_MEMORY_LIMIT"]; got != strconv.FormatInt(3*page, 10) {
		t.Errorf("unaligned: %q", got)
	}
	if got := withSizeEnv(nil, 0, 0); len(got) != 0 {
		t.Errorf("zero size: %v", got)
	}
	if _, ok := withSizeEnv(nil, 1, 1<<30)["OMP_NUM_THREADS"]; ok {
		t.Error("OMP_NUM_THREADS set")
	}
}
