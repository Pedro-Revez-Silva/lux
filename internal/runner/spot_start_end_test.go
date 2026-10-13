package runner

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// startEndRig is a reclaimRig whose fake podman keeps volumes and the
// container as files: `volume create` makes $0.vol.<name> (and, for
// $0.holdvol's name, touches $0.volhold and blocks), `volume rm` removes
// it unless $0.failvolrm exists, `rm -f` of ctr-1 removes $0.ctr, and
// `start` fails while $0.failstart exists.
func startEndRig(t *testing.T, flags ...string) *reclaimRig {
	t.Helper()
	g := newReclaimRig(t, true, flags...)
	script := strings.NewReplacer(
		`"volume create") ;;`,
		`"volume create") for a; do n=$a; done; touch "$0.vol.$n"
  if [ "$n" = "$(cat "$0.holdvol" 2>/dev/null)" ]; then touch "$0.volhold"; while :; do sleep 0.02; done; fi ;;`,
		`"volume rm") ;;`,
		`"volume rm") [ -e "$0.failvolrm" ] && exit 1; rm -f "$0.vol.$4" ;;`,
		`"rm -f") ;;`,
		`"rm -f") [ "$5" = ctr-1 ] && rm -f "$0.ctr"; : ;;`,
		`    touch "$0.starting"`,
		`    touch "$0.starting"; [ -e "$0.failstart" ] && { echo "start refused" >&2; exit 1; }`,
	).Replace(podmanForReclaim)
	if err := os.WriteFile(g.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return g
}

// startEndSpec has an ephemeral and two state volumes; nested adds the
// container engines' volumes.
func startEndSpec(spot, nested bool) spec.RunSpec {
	sp := spec.RunSpec{Spot: spot, Image: spec.Image{Ref: "img"}, Network: spec.Network{Unrestricted: true},
		Volumes: []spec.Volume{{Name: "e", Path: "/e", Kind: "ephemeral"}, {Name: "a", Path: "/a", Kind: "state"}, {Name: "b", Path: "/b", Kind: "state"}}}
	if spot {
		sp.ResumePolicy = spec.ResumeNever
	}
	sp.Sandbox.NestedContainers = nested
	return sp
}

func (g *reclaimRig) place(ctx context.Context, sp spec.RunSpec) *placement {
	p := newPlacement(g.r, proto.Assign{RunID: "run1", TenantID: "t1", Epoch: 1, Spec: sp}, nil)
	g.r.mu.Lock()
	g.r.placements["run1"] = p
	g.r.mu.Unlock()
	go p.run(ctx)
	return p
}

// present lists which of the named volumes exist, and the container.
func (g *reclaimRig) present(volumes ...string) []string {
	var out []string
	for _, v := range volumes {
		if _, err := os.Stat(g.bin + ".vol." + v); err == nil {
			out = append(out, v)
		}
	}
	if _, err := os.Stat(g.bin + ".ctr"); err == nil {
		out = append(out, "container ctr-1")
	}
	return out
}

func isEnd(st proto.Status) bool { return st.State == "exited" || st.State == "failed" }

// endStatus waits for a final (exited or failed) status to be sent.
func (g *reclaimRig) endStatus(t *testing.T) proto.Status {
	t.Helper()
	var end proto.Status
	eventually(t, "no end report", func() bool {
		sts := g.luxd.statuses()
		i := slices.IndexFunc(sts, isEnd)
		if i >= 0 {
			end = sts[i]
		}
		return i >= 0
	})
	return end
}

func (g *reclaimRig) ends() int {
	n := 0
	for _, st := range g.luxd.statuses() {
		if isEnd(st) {
			n++
		}
	}
	return n
}

var (
	specVolumes   = []string{volumeName("run1", "e"), volumeName("run1", "a"), volumeName("run1", "b")}
	engineVolumes = []string{"lux-run1--docker", "lux-run1--containers"}
)

// A spot placement that ends during its start frees its disk at luxd with
// its end report: the volumes made so far, and a container created but
// never started, are removed before it is sent.
func TestSpotStartEndReleasesDisk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flags  []string
		nested bool
		end    proto.Status
	}{
		// Reclaimed while the second state volume is being created.
		{name: "reclaimed while volumes are made", end: proto.Status{State: "exited", Reason: "stopped"}},
		// The container was created, and podman failed to start it.
		{name: "container start failed", flags: []string{"failstart"}, nested: true, end: proto.Status{State: "failed", Reason: "start-failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortReclaim(t, 2*time.Second)
			g := startEndRig(t, tc.flags...)
			g.r.nestedSeccomp = "/dev/null"
			reclaimed := tc.flags == nil
			if reclaimed {
				writeFile(t, g.bin+".holdvol", volumeName("run1", "b"))
			}
			p := g.place(g.ctx, startEndSpec(true, tc.nested))
			if reclaimed {
				g.waitFile(t, "volhold")
				if got := g.present(specVolumes...); len(got) < 2 {
					t.Fatalf("volumes %v when reclaimed: want some made already", got)
				}
				p.requestStop(g.ctx, stopSpotReclaim)
			}
			end := g.endStatus(t)
			left := g.present(append(slices.Clone(specVolumes), engineVolumes...)...)
			if end.State != tc.end.State || end.Reason != tc.end.Reason {
				t.Fatalf("end report %+v, want %s: %s", end, tc.end.State, tc.end.Reason)
			}
			if len(left) != 0 {
				t.Fatalf("at the end report %v still exist; podman:\n%s", left, strings.Join(podmanCalls(g.bin), "\n"))
			}
			calls := podmanCalls(g.bin)
			if !reclaimed {
				for _, v := range engineVolumes {
					if !slices.Contains(calls, "volume rm -f "+v) {
						t.Fatalf("engine volume %s never removed; podman:\n%s", v, strings.Join(calls, "\n"))
					}
				}
				created := slices.IndexFunc(calls, func(l string) bool { return strings.HasPrefix(l, "create ") })
				if created < 0 || !slices.Contains(calls[created:], "rm -f -t 0 ctr-1") {
					t.Fatalf("the created container was not removed; podman:\n%s", strings.Join(calls, "\n"))
				}
			}
			g.luxd.release(g.ctx)
			mustEnd(t, p, "the placement never ended")
			if n := g.ends(); n != 1 {
				t.Fatalf("%d end reports, want 1", n)
			}
		})
	}
}

// A spot placement whose disk could not be released when its start ended
// keeps its end unreported across a runner restart; the re-adopted
// placement takes no snapshot, removes what is left, and reports the end
// once.
func TestSpotStartEndReleaseSurvivesRestart(t *testing.T) {
	shortReclaim(t, 2*time.Second)
	old := releaseRetry
	releaseRetry = 50 * time.Millisecond
	t.Cleanup(func() { releaseRetry = old })
	g := startEndRig(t, "failstart", "failvolrm")
	g.luxd.release(g.ctx)
	ctx, cancel := context.WithCancel(g.ctx)
	p := g.place(ctx, startEndSpec(true, false))
	eventually(t, "no volume removal retries", func() bool {
		return strings.Count(strings.Join(podmanCalls(g.bin), "\n"), "volume rm -f "+volumeName("run1", "e")) >= 3
	})
	if n := g.ends(); n != 0 {
		t.Fatalf("%d end reports with the volumes still there", n)
	}
	if st, err := readRunState(p.dir); err != nil || st.Phase != "exited" || !st.ReleasePending {
		t.Fatalf("run state %+v (%v): want exited with its release pending", st, err)
	}
	cancel()
	mustEnd(t, p, "the first runner's placement did not stop")

	g.r.mu.Lock()
	g.r.placements = map[string]*placement{}
	g.r.mu.Unlock()
	g.r.readopt(g.ctx)
	g.r.mu.Lock()
	q := g.r.placements["run1"]
	g.r.mu.Unlock()
	if q == nil {
		t.Fatal("the placement was not re-adopted")
	}
	time.Sleep(200 * time.Millisecond)
	if n := g.ends(); n != 0 {
		t.Fatalf("%d end reports after the restart with the volumes still there", n)
	}
	if err := os.Remove(g.bin + ".failvolrm"); err != nil {
		t.Fatal(err)
	}
	mustEnd(t, q, "the re-adopted placement never ended")
	if left := g.present(specVolumes...); len(left) != 0 {
		t.Fatalf("%v still exist", left)
	}
	g.luxd.mu.Lock()
	frames := slices.Clone(g.luxd.frames)
	g.luxd.mu.Unlock()
	for _, fr := range frames {
		if fr.Type == proto.MsgSnapshotDone {
			t.Fatalf("a snapshot was reported: %+v", fr)
		}
	}
	sts := g.luxd.statuses()
	if n := g.ends(); n != 1 || sts[len(sts)-1].State != "failed" || sts[len(sts)-1].Reason != "start-failed" {
		t.Fatalf("statuses %+v: want one end, failed: start-failed", sts)
	}
}

// A Run that is not spot keeps its volumes and its container when its
// start fails: it may resume on this host from them.
func TestNormalStartFailureKeepsVolumes(t *testing.T) {
	g := startEndRig(t, "failstart")
	p := g.place(g.ctx, startEndSpec(false, false))
	end := g.endStatus(t)
	if end.State != "failed" || end.Reason != "start-failed" {
		t.Fatalf("end report %+v, want failed: start-failed", end)
	}
	want := append(slices.Clone(specVolumes), "container ctr-1")
	if got := g.present(specVolumes...); !slices.Equal(got, want) {
		t.Fatalf("at the end report %v exist, want %v", got, want)
	}
	g.luxd.release(g.ctx)
	mustEnd(t, p, "the placement never ended")
	calls := podmanCalls(g.bin)
	created := slices.IndexFunc(calls, func(l string) bool { return strings.HasPrefix(l, "create ") })
	for _, l := range calls[created:] {
		if strings.HasPrefix(l, "volume rm") || strings.HasPrefix(l, "rm ") {
			t.Fatalf("podman %q after the container was created:\n%s", l, strings.Join(calls, "\n"))
		}
	}
	if st, err := readRunState(p.dir); err != nil || st.ReleasePending {
		t.Fatalf("run state %+v (%v): a normal Run has no release", st, err)
	}
}
