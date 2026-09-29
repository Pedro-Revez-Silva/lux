package runner

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

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

func TestEmptyDir(t *testing.T) {
	d, outside := t.TempDir(), t.TempDir()
	keep := filepath.Join(outside, "keep")
	for _, f := range []string{filepath.Join(d, "x", "y"), keep} {
		if err := os.MkdirAll(f, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if err := emptyDir(d); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(d); len(es) != 0 {
		t.Errorf("left %v", es)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("followed a link out: %v", err)
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

// A stopped container is reused only if it was made with the same
// arguments: a runner's engine mounts or security options change them.
func TestArgsHashCoversRunnerArgs(t *testing.T) {
	base := []string{"--name", "lux-r1", "-v", "lux-r1-home:/home/agent:idmap", "img"}
	withStore := []string{"--name", "lux-r1", "-v", "lux-r1-home:/home/agent:idmap",
		"-v", "lux-r1--docker:/home/agent/.local/share/docker:idmap", "img"}
	withAppArmor := append(slices.Clone(base[:len(base)-1]), "--security-opt=apparmor=unconfined", "img")
	if argsHash(base) != argsHash(slices.Clone(base)) {
		t.Error("the same arguments hash differently")
	}
	if argsHash(base) == argsHash(withStore) {
		t.Error("an engine mount does not change the hash")
	}
	if argsHash(base) == argsHash(withAppArmor) {
		t.Error("the AppArmor mode does not change the hash")
	}
}

// copyTree recreates an image's engine store in a volume as podman's
// copy-up would: modes, setuid bits, hardlinks as hardlinks, links as links
// (never followed out), and, as root, owners and file capabilities.
func TestCopyTree(t *testing.T) {
	src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	layer := filepath.Join(src, "overlay", "l1")
	if err := os.MkdirAll(layer, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(layer, "f")
	if err := os.WriteFile(f, []byte("layer"), 0o640); err != nil {
		t.Fatal(err)
	}
	asRoot := os.Getuid() == 0
	// Owner first: a chown clears setuid and file capabilities.
	if asRoot {
		if err := os.Chown(f, 1234, 1234); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(f, 0o640|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if asRoot {
		// cap_net_raw=ep, as security.capability v2.
		caps := []byte{0, 0, 0, 2, 0, 0x20, 0, 0, 0, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		if err := unix.Setxattr(f, "security.capability", caps, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(f, filepath.Join(layer, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "out")); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := copyTree(context.Background(), dir, dst); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(dst, "overlay", "l1", "f")
	if b, err := os.ReadFile(g); err != nil || string(b) != "layer" {
		t.Fatalf("file: %q %v", b, err)
	}
	fi, _ := os.Stat(g)
	if fi.Mode()&(os.ModePerm|os.ModeSetuid) != 0o640|os.ModeSetuid {
		t.Errorf("file mode %v, want setuid 0640", fi.Mode())
	}
	if d, _ := os.Stat(filepath.Join(dst, "overlay")); d.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", d.Mode())
	}
	if h, _ := os.Stat(filepath.Join(dst, "overlay", "l1", "hard")); h == nil || !os.SameFile(fi, h) {
		t.Error("a hardlink became a copy")
	}
	if l, err := os.Lstat(filepath.Join(dst, "out")); err != nil || l.Mode()&os.ModeSymlink == 0 {
		t.Errorf("a link was not copied as a link: %v", err)
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Errorf("wrote through a link: %v", es)
	}
	if asRoot {
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 1234 {
			t.Errorf("owner %d, want 1234", st.Uid)
		}
		if n, err := unix.Getxattr(g, "security.capability", make([]byte, 64)); err != nil || n == 0 {
			t.Errorf("file capability lost: %v", err)
		}
	}
}
