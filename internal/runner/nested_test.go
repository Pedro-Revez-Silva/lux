package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/shim"
	"github.com/marcioapm/lux/internal/spec"
)

func TestNestedVolumes(t *testing.T) {
	agent := passwd.User{Name: "agent", UID: 1000, GID: 1000, Home: "/home/agent"}
	root := passwd.User{Name: "root", Home: "/root"}
	nested := func(env map[string]string, vols ...spec.Volume) spec.RunSpec {
		return spec.RunSpec{Sandbox: spec.Sandbox{NestedContainers: true}, Env: env, Volumes: vols}
	}
	state := func(path string) spec.Volume { return spec.Volume{Name: "v", Path: path, Kind: "state"} }
	both := []string{"/home/agent/.local/share/docker", "/home/agent/.local/share/containers"}
	for _, c := range []struct {
		name string
		sp   spec.RunSpec
		user passwd.User
		want []string
	}{
		{"not nested", spec.RunSpec{}, agent, nil},
		{"rootless", nested(nil), agent, both},
		{"rootful", nested(nil), root, []string{"/var/lib/docker", "/var/lib/containers"}},
		{"XDG_DATA_HOME", nested(map[string]string{"XDG_DATA_HOME": "/work/.data/"}), agent,
			[]string{"/work/.data/docker", "/work/.data/containers"}},
		{"HOME", nested(map[string]string{"HOME": "/w"}), agent, []string{"/w/.local/share/docker", "/w/.local/share/containers"}},
		{"relative XDG_DATA_HOME: ignored, as engines do", nested(map[string]string{"XDG_DATA_HOME": "data"}), agent, both},
		{"under a state home: still their own", nested(nil, state("/home/agent")), agent, both},
		{"a volume at a store: left to it", nested(nil, state("/home/agent/.local/share/docker")), agent,
			[]string{"/home/agent/.local/share/containers"}},
		{"a volume around both stores: left to it", nested(nil, state("/home/agent/.local/share")), agent, nil},
		{"a volume inside a store: left to it", nested(nil, state("/home/agent/.local/share/docker/volumes")), agent,
			[]string{"/home/agent/.local/share/containers"}},
		// Above the home: not about the stores, which stay out of its snapshot.
		{"a state volume at /home", nested(nil, state("/home")), agent, both},
		{"a state /workspace holding XDG_DATA_HOME", nested(map[string]string{"XDG_DATA_HOME": "/workspace/.data"}, state("/workspace")), agent,
			[]string{"/workspace/.data/docker", "/workspace/.data/containers"}},
		{"a passwd home with a trailing slash", nested(nil, state("/home/agent")), passwd.User{UID: 1000, Home: "/home/agent/"}, both},
		{"no home in passwd: nowhere to put them", nested(nil), passwd.User{UID: 1000, Home: ""}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, r := range nestedVolumes("r1", c.sp, c.user, c.sp.Env) {
				if r.Kind != "ephemeral" {
					t.Errorf("%s: kind %q, want ephemeral", r.Path, r.Kind)
				}
				got = append(got, r.Path)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The image's XDG_DATA_HOME reaches the workload (its HOME never does), and
// the spec's env wins over it.
func TestWorkloadEnv(t *testing.T) {
	info := podman.ImageInfo{Env: []string{"PATH=/bin", "HOME=/image-home", "XDG_DATA_HOME=/data"}}
	if got := workloadEnv(spec.RunSpec{}, info); !maps.Equal(got, map[string]string{"XDG_DATA_HOME": "/data"}) {
		t.Errorf("image only: %v", got)
	}
	sp := spec.RunSpec{Env: map[string]string{"HOME": "/w", "XDG_DATA_HOME": "/spec"}}
	if got := workloadEnv(sp, info); !maps.Equal(got, map[string]string{"HOME": "/w", "XDG_DATA_HOME": "/spec"}) {
		t.Errorf("spec over image: %v", got)
	}
}

// The engine volumes' names ("lux-<run>--docker") cannot be a spec
// volume's: validation refuses a name starting with "-".
func TestNestedVolumeNamesCannotCollide(t *testing.T) {
	for _, r := range nestedVolumes("r1", spec.RunSpec{Sandbox: spec.Sandbox{NestedContainers: true}},
		passwd.User{UID: 1000, Home: "/home/agent"}, nil) {
		name := r.Volume[len("lux-r1-"):] // what volumeName would have been given
		sp := spec.RunSpec{Image: spec.Image{Ref: "x"}, Workload: spec.Workload{Adapter: "generic", Command: []string{"true"}},
			Volumes: []spec.Volume{{Name: name, Path: "/v", Kind: "state"}}}
		if err := sp.Normalize(spec.Defaults{}); err == nil {
			t.Errorf("a spec volume named %q is accepted: it would be %s", name, r.Volume)
		}
	}
}

func TestDirSizeCountsHardlinksOnce(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(d, "a"), filepath.Join(d, "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "c"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := dirSize(context.Background(), d); got != 1010 || err != nil {
		t.Errorf("dirSize = %d, %v, want 1010", got, err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dirSize(ended, d); err == nil {
		t.Error("dirSize with its context ended: nil error, want the walk cut short")
	}
}

// dirSize's error is its root's: a missing root is an error, an unreadable
// directory under it is skipped.
func TestDirSizeErrorIsTheRoots(t *testing.T) {
	if _, err := dirSize(context.Background(), filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Error("dirSize of a missing root: nil error")
	}
	t.Run("an unreadable subdirectory is skipped", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-0 directory anyway")
		}
		d := t.TempDir()
		sub := filepath.Join(d, "sub")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "hidden"), make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "a"), make([]byte, 10), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) }) // so TempDir can remove it
		if got, err := dirSize(context.Background(), d); got != 10 || err != nil {
			t.Errorf("dirSize = %d, %v, want 10, nil", got, err)
		}
	})
}

func TestAppArmorRestrictsUserns(t *testing.T) {
	d := t.TempDir()
	for content, want := range map[string]bool{"1\n": true, "0\n": false} {
		f := filepath.Join(d, "sysctl")
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := appArmorRestrictsUserns(f); got != want {
			t.Errorf("%q: %v, want %v", content, got, want)
		}
	}
	if appArmorRestrictsUserns(filepath.Join(d, "absent")) {
		t.Error("no sysctl (no AppArmor): restricted")
	}
}

// Only the parents neither the image nor a restored volume has count as made
// by a store's mount; outside HOME, only the data home itself.
func TestMadeParents(t *testing.T) {
	img, homeVol := t.TempDir(), t.TempDir()
	// The image has /home/agent and /workspace but nothing below.
	for _, d := range []string{"home/agent", "workspace"} {
		if err := os.MkdirAll(filepath.Join(img, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A restored state home already has ~/.local (the workload's, or root's
	// on purpose): not the mount's to hand over. ~/.local/share it lacks.
	if err := os.MkdirAll(filepath.Join(homeVol, ".local"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(img)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mountpoint := func(v volumeRef) (string, error) { return homeVol, nil }
	stores := func(share string) []volumeRef {
		return []volumeRef{{Path: share + "/docker"}, {Path: share + "/containers"}}
	}
	for _, c := range []struct {
		name   string
		stores []volumeRef
		vols   []volumeRef
		want   []string
	}{
		{"no volumes: both parents are the mount's", stores("/home/agent/.local/share"), nil,
			[]string{"/home/agent/.local/share", "/home/agent/.local"}},
		{"a restored home has ~/.local", stores("/home/agent/.local/share"),
			[]volumeRef{{Name: "home", Volume: "v", Path: "/home/agent", Kind: "state"}},
			[]string{"/home/agent/.local/share"}},
		// Only the configured data home itself: /workspace/.data is not the
		// workload's to have, even if the mount made it.
		{"XDG_DATA_HOME outside HOME", stores("/workspace/.data/share"), nil,
			[]string{"/workspace/.data/share"}},
		{"XDG_DATA_HOME right under /", stores("/data"), nil, []string{"/data"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := madeParents(c.stores, c.vols, "/home/agent", root, mountpoint)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A nested Run with sandbox.cgroupDelegation has a shim config that points
// Podman inside at a containers.conf in the runtime volume whose hooks_dir
// registers lux-shim oci-defaults; a plain Run and a nested Run without
// delegation get none of it, and a spec's own CONTAINERS_CONF_OVERRIDE is
// kept. The hook is run here, in process, with the arguments it was
// registered with: an inner container without limits gets the Run's
// scaled memory limit and its CPU quota. That Podman reads containers.conf
// and hooks_dir is covered end to end by test_run_size.py's "none:" row.
func TestNestedRunsGetInnerDefaults(t *testing.T) {
	const gib = int64(1 << 30)
	// Half the requested memory is allocatable: the hook must get the
	// scaled limit, not resources.memory.
	scale, err := newMemoryScale(32*gib+512<<20, 512<<20, 64*gib)
	if err != nil {
		t.Fatal(err)
	}
	if scale.limit(gib) >= gib {
		t.Fatalf("scale.limit(1GiB) = %d: the test needs a scaled host", scale.limit(gib))
	}
	for _, c := range []struct {
		name       string
		nested     bool
		delegation bool
		cpus       float64
		specEnv    map[string]string
		wantEnv    string
		wantQuota  string
	}{
		{"nested", true, true, 1.5, nil, proto.ShimOCIConf, "150000"},
		// As podman --cpus 0.29 sets the Run's own cpu.max (measured:
		// 28999): truncated, so the default is never above the Run's.
		{"nested, 0.29 CPUs", true, true, 0.29, nil, proto.ShimOCIConf, "28999"},
		{"nested, spec's own", true, true, 1.5, map[string]string{"CONTAINERS_CONF_OVERRIDE": "/mine.conf"}, "/mine.conf", "150000"},
		// Without delegation inner containers stay in the Run's cgroup.
		{"nested, not delegated", true, false, 1.5, nil, "", ""},
		{"plain", false, false, 1.5, nil, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := t.TempDir()
			r := &Runner{mem: scale, shimOCIDefaults: true, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r.mounts.Store(runtimeVolume("run_1"), rt)
			p := &placement{r: r, runID: "run_1", epoch: 1, assign: &proto.Assign{}, state: &runState{}}
			sp := spec.RunSpec{Env: c.specEnv, Resources: spec.Resources{CPUs: c.cpus, Memory: spec.Bytes(gib)},
				Sandbox: spec.Sandbox{NestedContainers: c.nested, CgroupDelegation: c.delegation}}
			if err := p.writeShimConfig(context.Background(), sp, nil); err != nil {
				t.Fatal(err)
			}
			if got := shimConfigEnv(t, rt)["CONTAINERS_CONF_OVERRIDE"]; got != c.wantEnv {
				t.Fatalf("CONTAINERS_CONF_OVERRIDE %q, want %q", got, c.wantEnv)
			}
			b, err := os.ReadFile(filepath.Join(rt, "oci", "hooks.d", "lux-run-limits.json"))
			if !c.delegation {
				if _, serr := os.Stat(filepath.Join(rt, "oci")); err == nil || serr == nil {
					t.Fatal("a Run without cgroup delegation got the hook")
				}
				return
			}
			var hook struct {
				Hook struct {
					Path string
					Args []string
				}
				When   struct{ Always bool }
				Stages []string
			}
			if err := json.Unmarshal(b, &hook); err != nil {
				t.Fatal(err)
			}
			// Podman runs a hook only when its when matches, at its stages.
			if !hook.When.Always || !slices.Equal(hook.Stages, []string{"precreate"}) || hook.Hook.Path != proto.ShimBinary ||
				len(hook.Hook.Args) < 2 || hook.Hook.Args[1] != proto.ShimOCIDefaultsCmd {
				t.Fatalf("hook %+v", hook)
			}
			var out bytes.Buffer
			if code := shim.OCIDefaults(hook.Hook.Args[2:], strings.NewReader(`{"linux":{"resources":{}}}`), &out); code != 0 {
				t.Fatalf("the hook exited %d on %v", code, hook.Hook.Args)
			}
			var got struct {
				Linux struct {
					Resources struct {
						Memory struct{ Limit, Swap json.Number }
						CPU    struct{ Quota, Period json.Number }
					}
				}
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			res := got.Linux.Resources
			want := strconv.FormatInt(scale.limit(gib), 10)
			if res.Memory.Limit.String() != want || res.Memory.Swap.String() != want ||
				res.CPU.Quota.String() != c.wantQuota || res.CPU.Period.String() != "100000" {
				t.Fatalf("an inner container without limits got %+v, want memory %s, quota %s/100000", res, want, c.wantQuota)
			}
		})
	}
}

func shimConfigEnv(t *testing.T, rt string) map[string]string {
	t.Helper()
	var cfg proto.ShimConfig
	b, _ := os.ReadFile(filepath.Join(rt, "config.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Env
}

// With a --shim that does not list oci-defaults-v1 (an older or custom
// one), a delegated nested Run gets no hook and no CONTAINERS_CONF_OVERRIDE: its
// inner containers behave as before the hook. The runner warns once, and
// only for a delegated Run: plain and non-delegated nested Runs never use
// the hook, so they do not warn.
func TestNestedRunsWithoutTheShimsOCIDefaults(t *testing.T) {
	var logs bytes.Buffer
	r := &Runner{log: slog.New(slog.NewTextHandler(&logs, nil))}
	write := func(sp spec.RunSpec) string {
		t.Helper()
		rt := t.TempDir()
		r.mounts.Store(runtimeVolume("run_1"), rt)
		p := &placement{r: r, runID: "run_1", epoch: 1, assign: &proto.Assign{}, state: &runState{}}
		if err := p.writeShimConfig(context.Background(), sp, nil); err != nil {
			t.Fatal(err)
		}
		return rt
	}
	res := spec.Resources{CPUs: 2, Memory: 1 << 30}
	write(spec.RunSpec{Resources: res})
	write(spec.RunSpec{Resources: res, Sandbox: spec.Sandbox{NestedContainers: true}})
	if n := strings.Count(logs.String(), proto.ShimCapOCIDefaults); n != 0 {
		t.Fatalf("%d warnings for Runs without cgroup delegation, want 0:\n%s", n, logs.String())
	}
	sp := spec.RunSpec{Resources: res, Sandbox: spec.Sandbox{NestedContainers: true, CgroupDelegation: true}}
	for i := range 2 {
		rt := write(sp)
		if env, ok := shimConfigEnv(t, rt)["CONTAINERS_CONF_OVERRIDE"]; ok {
			t.Fatalf("placement %d: CONTAINERS_CONF_OVERRIDE %q without the shim's hook", i, env)
		}
		if _, err := os.Stat(filepath.Join(rt, "oci")); err == nil {
			t.Fatalf("placement %d: hook written for a shim without it", i)
		}
	}
	if n := strings.Count(logs.String(), proto.ShimCapOCIDefaults); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, logs.String())
	}
}

// The runner registers the hook only for a --shim that lists it among its
// capabilities: this tree's shim does; an older one (no capabilities
// subcommand: it starts as PID 1 without a config and exits 125), one
// listing only reclaim-v1, and a missing one do not.
func TestShimKnowsOCIDefaults(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "lux-shim")
	if out, err := exec.Command("go", "build", "-o", built, "github.com/marcioapm/lux/cmd/lux-shim").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	if !shimKnowsOCIDefaults(built) {
		t.Fatal("this tree's lux-shim: no oci-defaults")
	}
	old := filepath.Join(dir, "old-shim")
	writeFile(t, old, "#!/bin/sh\necho 'lux-shim: no config' >&2; exit 125\n")
	reclaimOnly := filepath.Join(dir, "reclaim-shim")
	writeFile(t, reclaimOnly, "#!/bin/sh\n[ \"$1\" = capabilities ] && echo '[\""+proto.ShimCapReclaim+"\"]'\n")
	for _, f := range []string{old, reclaimOnly} {
		if err := os.Chmod(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{old, reclaimOnly, filepath.Join(dir, "missing")} {
		if shimKnowsOCIDefaults(s) {
			t.Errorf("%s: oci-defaults", filepath.Base(s))
		}
	}
	if !shimKnowsReclaim(reclaimOnly) {
		t.Error("the reclaim-only shim's reclaim-v1 was not seen")
	}
}
