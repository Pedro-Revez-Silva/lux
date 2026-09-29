package shim

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// The shim creates missing parents for the workload: the workload's own
// under its home or a volume, root's elsewhere, and never touches a
// directory that already existed. Needs root, as the shim always is:
//
//	docker run --rm -v $PWD:/src -w /src golang go test ./internal/shim -run MkdirForWorkload
func TestMkdirForWorkload(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "agent")
	vol := filepath.Join(root, "workspace")
	for _, d := range []string{home, vol, filepath.Join(home, "existing")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Shim{
		user: &userInfo{uid: 1000, gid: 1000, home: home},
		cfg:  proto.ShimConfig{VolumePaths: []string{vol}},
	}
	for _, d := range []string{
		filepath.Join(home, ".config", "tool"),
		filepath.Join(home, "existing", "sub"),
		filepath.Join(vol, "a", "b"),
		filepath.Join(root, "etc", "tool"),
	} {
		if err := s.mkdirForWorkload(d); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]uint32{
		filepath.Join(home, ".config"):         1000,
		filepath.Join(home, ".config", "tool"): 1000,
		filepath.Join(home, "existing"):        0, // existed: untouched
		filepath.Join(home, "existing", "sub"): 1000,
		filepath.Join(vol, "a", "b"):           1000,
		filepath.Join(root, "etc"):             0, // outside home and volumes
		filepath.Join(root, "etc", "tool"):     0,
		home:                                   0,
	} {
		if got := lstatUID(t, p); got != want {
			t.Errorf("%s: uid %d, want %d", p, got, want)
		}
	}
}

// An engine store the runner mounts under the home made its missing parents
// as root: prepareVolumes hands those (cfg.MadeParents) to the workload. Not
// a directory the image ships root-owned on purpose, not a spec volume's
// parent, and never through a link, at any depth.
func TestPrepareVolumesOwnParents(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home", "agent")
	store := filepath.Join(home, ".local", "share", "docker")
	shipped := filepath.Join(home, "sys", "share", "containers") // /home/agent/sys: the image's, root's
	managed := filepath.Join(home, ".config", "tool")            // a spec volume under a root-owned dir
	usr := filepath.Join(root, "usr", "share")
	for _, d := range []string{store, shipped, managed, usr, filepath.Join(home, "x")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The workload replaced an intermediate directory with a link to /usr,
	// as it can on a state home: .cache/share/containers resolves there.
	if err := os.Symlink(filepath.Join(root, "usr"), filepath.Join(home, ".cache")); err != nil {
		t.Fatal(err)
	}
	s := &Shim{
		user: &userInfo{uid: 1000, gid: 1000, home: home},
		cfg: proto.ShimConfig{
			VolumePaths: []string{store, shipped, managed},
			OwnParents:  []string{store, shipped, filepath.Join(home, ".cache", "share", "containers"), filepath.Join(root, "var", "lib", "docker")},
			// What the runner found missing from the image. .cache/share
			// is listed, as a workload's link could make it look missing:
			// the link still stops the walk.
			MadeParents: []string{filepath.Join(home, ".local"), filepath.Join(home, ".local", "share"),
				filepath.Join(home, "sys", "share"), filepath.Join(home, ".cache"), filepath.Join(home, ".cache", "share")},
		},
	}
	s.prepareVolumes()
	for p, want := range map[string]uint32{
		store:                                  1000,
		filepath.Join(home, ".local"):          1000,
		filepath.Join(home, ".local", "share"): 1000,
		home:                                   0, // the image's: untouched
		filepath.Join(home, "sys"):             0, // the image ships it root's: stays so
		filepath.Join(home, "sys", "share"):    1000,
		managed:                                1000,
		filepath.Join(home, ".config"):         0, // a spec volume's parent: the image's
		filepath.Join(root, "usr"):             0, // behind the link
		usr:                                    0,
		filepath.Join(home, ".cache"):          0, // the link itself: never chowned
	} {
		if got := lstatUID(t, p); got != want {
			t.Errorf("%s: uid %d, want %d", p, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "var")); err == nil {
		t.Errorf("made %s, outside the home", filepath.Join(root, "var"))
	}
}

// With the spec's HOME (cfg.Home) the walk starts there, not at the user's
// passwd home, which is where the runner put the stores.
func TestPrepareVolumesOwnParentsSpecHome(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	passwdHome := filepath.Join(root, "home", "agent")
	specHome := filepath.Join(root, "w")
	store := filepath.Join(specHome, ".local", "share", "docker")
	for _, d := range []string{passwdHome, store} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Shim{
		user: &userInfo{uid: 1000, gid: 1000, home: passwdHome},
		cfg: proto.ShimConfig{VolumePaths: []string{store}, OwnParents: []string{store}, Home: specHome,
			MadeParents: []string{filepath.Join(specHome, ".local"), filepath.Join(specHome, ".local", "share")}},
	}
	s.prepareVolumes()
	for p, want := range map[string]uint32{
		filepath.Join(specHome, ".local"):          1000,
		filepath.Join(specHome, ".local", "share"): 1000,
		specHome: 0, // the home itself: not the mount's to hand over
	} {
		if got := lstatUID(t, p); got != want {
			t.Errorf("%s: uid %d, want %d", p, got, want)
		}
	}
}

// lstatUID is path's owner, not following a link; "" (absent) fails the
// test, so a check for root's cannot pass on a path never made.
func lstatUID(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Uid
}
