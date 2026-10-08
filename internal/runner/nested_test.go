package runner

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/podman"
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
	if got := dirSize(d); got != 1010 {
		t.Errorf("dirSize = %d, want 1010", got)
	}
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
