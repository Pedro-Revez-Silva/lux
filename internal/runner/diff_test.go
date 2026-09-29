package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/gitws"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// diffRunner is a Runner whose podman is script (a shell script given
// podman's arguments), with a running placement of run1 at epoch 1 that
// has repository app.
func diffRunner(t *testing.T, script string) (*Runner, *placement) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		pm: &podman.Podman{Bin: bin}, placements: map[string]*placement{}, git: gitws.New(dir)}
	sp := &spec.RunSpec{Git: &spec.Git{Repositories: []spec.Repository{{Name: "app", Path: "/workspace/repos/app"}}}}
	p := &placement{r: r, runID: "run1", epoch: 1, dir: r.runDir("run1"), phase: "running",
		state: &runState{RunID: "run1", Epoch: 1, User: "1000:1000", Spec: sp, GitBases: map[string]string{"app": "c0ffee"}}}
	r.placements["run1"] = p
	return r, p
}

// The runner runs the shim in the container as the workload user, with
// each repository's path and clone commit, and returns what it printed.
func TestDiffRunsTheShim(t *testing.T) {
	r, _ := diffRunner(t, `printf '%s\n' "$@" > "$0.args"; echo '{"repos":[{"repo":"app","base":"c0ffee","head":"h","files":1}]}'`)
	res := r.diff(context.Background(), "run1", 1, proto.DiffBaseClone)
	if res.Error != "" || len(res.Repos) != 1 || res.Repos[0].Files != 1 {
		t.Fatalf("%+v", res)
	}
	args, _ := os.ReadFile(r.pm.Bin + ".args")
	want := "exec\n--user\n1000:1000\n--workdir\n/\nlux-run1\n/.lux/bin/lux-shim\ndiff\n" +
		`{"base":"clone","repos":[{"name":"app","path":"/workspace/repos/app","base":"c0ffee"}]}` + "\n"
	if string(args) != want {
		t.Fatalf("podman %s", args)
	}
	if res := r.diff(context.Background(), "run1", 2, proto.DiffBaseClone); !res.NotRunning {
		t.Fatalf("another epoch: %+v", res)
	}
}

// A second diff of a placement while one is under way is busy; once that
// one ends, the next runs.
func TestDiffBusy(t *testing.T) {
	r, _ := diffRunner(t, `touch "$0.started"; while [ ! -e "$0.go" ]; do sleep 0.01; done; echo '{"repos":[]}'`)
	first := make(chan proto.DiffResult)
	go func() { first <- r.diff(context.Background(), "run1", 1, proto.DiffBaseHead) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(r.pm.Bin + ".started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first diff never started")
		}
	}
	if res := r.diff(context.Background(), "run1", 1, proto.DiffBaseHead); !res.Busy {
		t.Fatalf("second: %+v", res)
	}
	os.WriteFile(r.pm.Bin+".go", nil, 0o644)
	if res := <-first; res.Busy || res.Error != "" {
		t.Fatalf("first: %+v", res)
	}
	if res := r.diff(context.Background(), "run1", 1, proto.DiffBaseHead); res.Busy || res.Error != "" {
		t.Fatalf("after: %+v", res)
	}
}

// A diff that outlasts diffTimeout fails, saying so, and frees the slot.
func TestDiffTimeout(t *testing.T) {
	r, _ := diffRunner(t, `exec sleep 30`)
	defer func(d time.Duration) { diffTimeout = d }(diffTimeout)
	diffTimeout = 200 * time.Millisecond
	start := time.Now()
	res := r.diff(context.Background(), "run1", 1, proto.DiffBaseHead)
	if !strings.Contains(res.Error, "took longer than 200ms") || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
	if res := r.diff(context.Background(), "run1", 1, proto.DiffBaseHead); res.Busy {
		t.Fatal("still busy after the timeout")
	}
}

// A placement that is not running has no diff.
func TestDiffNotRunning(t *testing.T) {
	r, p := diffRunner(t, `echo '{"repos":[]}'`)
	p.phase = "exited"
	if res := r.diff(context.Background(), "run1", 1, proto.DiffBaseHead); !res.NotRunning {
		t.Fatalf("%+v", res)
	}
}

// A clone's commit, and who the workload is, are in the run state a
// restarted runner re-adopts its placement from.
func TestDiffBasesPersist(t *testing.T) {
	_, p := diffRunner(t, `true`)
	p.setGitBase("lib", "beef")
	st, err := readRunState(p.dir)
	if err != nil || st.GitBases["lib"] != "beef" || st.GitBases["app"] != "c0ffee" || st.User != "1000:1000" {
		t.Fatalf("%+v %v", st, err)
	}
}

// The runner says it answers diffs.
func TestHelloHasDiffCapability(t *testing.T) {
	r, _ := diffRunner(t, `echo '[]'`)
	if h := r.hello(context.Background()); !slices.Contains(h.Capabilities, proto.CapDiff) {
		t.Fatalf("%+v", h.Capabilities)
	}
}
