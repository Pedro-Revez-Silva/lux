// Package podman drives Podman through its CLI (--format json), not its Go
// bindings, which pull in most of Podman as a dependency.
//
// Hosts run rootful Podman with --userns=auto: every container gets its own
// unprivileged host UID range, so root in one container is a different,
// powerless UID on the host from root in the next.
package podman

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/marcioapm/lux/internal/hoststat"
)

type Podman struct {
	Bin string
}

func New() *Podman { return &Podman{Bin: "podman"} }

// ErrNotFound is returned when a container, volume or image does not exist.
var ErrNotFound = errors.New("not found")

// WaitDelay is how long a cancelled podman call (Run) has after its SIGTERM
// before it is killed.
const WaitDelay = 30 * time.Second

func (p *Podman) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, p.Bin, args...)
}

// Build runs podman build. Idmapped overlay mounts are off for it: with
// them, a build in its own user namespace commits files owned by the host
// ids of whichever range it was given, so the image depends on the host
// (root-owned files are not root's) and its id changes from build to build.
// Without them, buildah maps ownership back into the container's ids.
//
// Cancelling ctx sends SIGTERM, which podman build handles by stopping the
// step under way; SIGKILL (exec's default) would leave that step running.
//
// The cgroup manager is cgroupfs: the runner makes a build's cgroup itself
// (lux.slice/build-<run>) and passes its path as --cgroup-parent. Under
// systemd, a host's default, crun would take that path for a unit name and
// fail every RUN step ("bpf pin to /sys/fs/bpf/crun/lux_slice/...: No such
// file or directory"; with that directory, systemd rejects the unit name).
func (p *Podman) Build(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, []string{"_CONTAINERS_OVERLAY_DISABLE_IDMAP=yes"},
		append([]string{"--cgroup-manager=cgroupfs", "build"}, args...)...)
}

// Run runs podman and returns stdout. Errors include stderr.
func (p *Podman) Run(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, nil, args...)
}

func (p *Podman) run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	c := p.cmd(ctx, args...)
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
	// Cancelled: ask podman to stop (it cleans up what it started), and
	// kill it only if it has not after a while.
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
	c.WaitDelay = WaitDelay
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no such") || strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "image not known") {
			return nil, fmt.Errorf("%w: podman %s: %s", ErrNotFound, args[0], msg)
		}
		return nil, fmt.Errorf("podman %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

func (p *Podman) Version(ctx context.Context) (string, error) {
	out, err := p.Run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		out, err = p.Run(ctx, "version", "--format", "{{.Client.Version}}")
	}
	return strings.TrimSpace(string(out)), err
}

// ---- images -----------------------------------------------------------------

func (p *Podman) ImageExists(ctx context.Context, ref string) bool {
	_, err := p.Run(ctx, "image", "exists", ref)
	return err == nil
}

// Pull pulls ref; with an authfile (a containers auth.json), with the
// credentials it holds.
func (p *Podman) Pull(ctx context.Context, ref, authfile string) error {
	_, err := p.Run(ctx, withAuth([]string{"pull", "-q"}, authfile, ref)...)
	return err
}

// Push pushes a local image to dest.
func (p *Podman) Push(ctx context.Context, image, dest, authfile string) error {
	_, err := p.Run(ctx, withAuth([]string{"push", "-q"}, authfile, image, dest)...)
	return err
}

// PushDigest pushes a local image to dest and returns the manifest digest
// the registry stored it under (sha256:...), as podman wrote it to a
// digest file in dir (removed before it returns).
func (p *Podman) PushDigest(ctx context.Context, image, dest, authfile, dir string) (string, error) {
	f, err := os.CreateTemp(dir, "digest-")
	if err != nil {
		return "", err
	}
	f.Close()
	defer os.Remove(f.Name())
	if _, err := p.Run(ctx, withAuth([]string{"push", "-q", "--digestfile", f.Name()}, authfile, image, dest)...); err != nil {
		return "", err
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	d := strings.TrimSpace(string(b))
	if !digestRe.MatchString(d) {
		return "", fmt.Errorf("push %s: no manifest digest (got %q)", dest, d)
	}
	return d, nil
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func withAuth(args []string, authfile string, rest ...string) []string {
	if authfile != "" {
		args = append(args, "--authfile", authfile)
	}
	return append(args, rest...)
}

// IsManifestUnknown reports whether a pull failed because the registry
// has no such image (or repository), rather than for any other reason.
func IsManifestUnknown(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	// Registries answer 404 with MANIFEST_UNKNOWN (no such tag) or
	// NAME_UNKNOWN (no such repository).
	return strings.Contains(m, "manifest unknown") || strings.Contains(m, "name unknown")
}

// ImageInfo is what the image GC weighs (an image's id, every name it has,
// and its size) and what a placement reads of its config.
type ImageInfo struct {
	ID    string
	Names []string
	Size  int64
	User  string
	Env   []string
	// OS and Architecture: the platform of the image itself.
	OS, Architecture string
	// ImagesDir: the image metadata directory of the store this image is
	// in, "" when its inspect does not tell (then see Podman.ImagesDir).
	ImagesDir string
}

// ImageInspect returns what ref names.
func (p *Podman) ImageInspect(ctx context.Context, ref string) (ImageInfo, error) {
	out, err := p.Run(ctx, "image", "inspect", ref)
	if err != nil {
		return ImageInfo{}, err
	}
	var raw []struct {
		ID       string   `json:"Id"`
		RepoTags []string `json:"RepoTags"`
		Size     int64    `json:"Size"`
		Config   struct {
			User string   `json:"User"`
			Env  []string `json:"Env"`
		} `json:"Config"`
		OS           string `json:"Os"`
		Architecture string `json:"Architecture"`
		GraphDriver  struct {
			Name string            `json:"Name"`
			Data map[string]string `json:"Data"`
		} `json:"GraphDriver"`
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw) == 0 {
		return ImageInfo{}, fmt.Errorf("image inspect %s: %v", ref, err)
	}
	r := raw[0]
	return ImageInfo{ID: r.ID, Names: r.RepoTags, Size: r.Size, User: r.Config.User, Env: r.Config.Env,
		OS: r.OS, Architecture: r.Architecture, ImagesDir: storeImagesDir(r.GraphDriver.Name, r.GraphDriver.Data["UpperDir"])}, nil
}

// storeImagesDir is the image metadata directory of the store whose top
// layer is upperDir (<store>/<driver>/<layer>/diff): <store>/<driver>-images.
// That store is the graph root, or `imagestore` when storage.conf sets one,
// which `podman info` does not show. "" when upperDir has another shape.
func storeImagesDir(driver, upperDir string) string {
	layer := filepath.Dir(upperDir)
	drv := filepath.Dir(layer)
	if driver == "" || !filepath.IsAbs(upperDir) || filepath.Base(upperDir) != "diff" || filepath.Base(drv) != driver {
		return ""
	}
	return filepath.Join(filepath.Dir(drv), driver+"-images")
}

// ImagesDir is where containers/storage keeps image metadata under the
// graph root: <graph root>/<driver>-images, one directory per image id. An
// image in a separate image store says where it is (ImageInfo.ImagesDir).
func (p *Podman) ImagesDir(ctx context.Context) (string, error) {
	out, err := p.Run(ctx, "info", "--format", "{{.Store.GraphRoot}} {{.Store.GraphDriverName}}")
	if err != nil {
		return "", err
	}
	root, driver, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !ok || root == "" || driver == "" {
		return "", fmt.Errorf("podman info: graph root and driver %q", out)
	}
	return filepath.Join(root, driver+"-images"), nil
}

// ImagePlatforms are the platforms (os/arch[/variant]) the reference an
// image was pulled by offers, read from what containers/storage keeps
// beside the image (files named "=" + base64 of the big data key); `podman
// manifest inspect` would ask the registry. A pull through a manifest list
// or OCI index stores it as "manifest-sha256:<digest>"; an image with no
// list (single-arch, loaded, built here) has its own platform, from its
// config blob ("sha256:<id>") or else from info.
//
// A ref pinned by digest reads only the manifest of that digest. Storage
// records no tag → list association: an image reached by several tags
// keeps every tag's list and every name, merged. So a tag offers a stored
// list's platforms only when the image has that one name and one list;
// with no list it offers the image's own platform; otherwise nil
// (unknown), never a union of lists. Entries for unknown/unknown (build
// attestations) are not platforms.
func ImagePlatforms(imagesDir, ref string, info ImageInfo) ([]string, error) {
	dir := filepath.Join(imagesDir, strings.TrimPrefix(info.ID, "sha256:"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	_, pinned, _ := strings.Cut(ref, "@")
	var own *ociPlatform
	var lists [][]string
	for _, e := range entries {
		enc, ok := strings.CutPrefix(e.Name(), "=")
		if !ok {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			continue
		}
		switch k := string(key); {
		case strings.HasPrefix(k, "manifest-sha256:"):
			if pinned != "" && k != "manifest-"+pinned {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			if ps := listPlatforms(b); len(ps) > 0 {
				lists = append(lists, ps)
			}
		case k == "sha256:"+strings.TrimPrefix(info.ID, "sha256:"):
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err == nil {
				var c ociPlatform
				if json.Unmarshal(b, &c) == nil {
					own = &c
				}
			}
		}
	}
	switch {
	case len(lists) == 1 && (pinned != "" || len(info.Names) <= 1):
		return lists[0], nil
	case len(lists) > 0:
		return nil, fmt.Errorf("image %s: %d stored lists, %d names: which one %s came by is not recorded", info.ID, len(lists), len(info.Names), ref)
	}
	if own == nil || own.String() == "" {
		own = &ociPlatform{OS: info.OS, Architecture: info.Architecture}
	}
	if s := own.String(); s != "" {
		return []string{s}, nil
	}
	return nil, fmt.Errorf("image %s: no platform", info.ID)
}

// listPlatforms are a manifest list's or OCI index's platforms, sorted,
// each once (an index may list one platform several times, e.g. Windows
// images per os.version); none for an image manifest.
func listPlatforms(b []byte) []string {
	var list struct {
		Manifests []struct {
			Platform *ociPlatform `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	var out []string
	for _, m := range list.Manifests {
		if m.Platform == nil || m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
			continue
		}
		if s := m.Platform.String(); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// ociPlatform is a manifest list entry's platform, or an image config's.
type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

// String is "" for an incomplete platform, or one with a NUL or invalid
// UTF-8, which luxd could not store.
func (o ociPlatform) String() string {
	if o.OS == "" || o.Architecture == "" {
		return ""
	}
	s := o.OS + "/" + o.Architecture
	if o.Variant != "" {
		s += "/" + o.Variant
	}
	if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
		return ""
	}
	return s
}

// ContainerImages is the set of image ids any container (running or not)
// is made from.
func (p *Podman) ContainerImages(ctx context.Context) (map[string]bool, error) {
	out, err := p.Run(ctx, "ps", "-a", "--no-trunc", "--format", "{{.ImageID}}")
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, id := range fields(out) {
		m[strings.TrimPrefix(id, "sha256:")] = true
	}
	return m, nil
}

// GraphRoot is where Podman keeps images and containers.
func (p *Podman) GraphRoot(ctx context.Context) (string, error) {
	out, err := p.Run(ctx, "info", "--format", "{{.Store.GraphRoot}}")
	return strings.TrimSpace(string(out)), err
}

// ImageID returns the local image id for ref.
func (p *Podman) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := p.Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return strings.TrimSpace(string(out)), err
}

// ImagesByDigest maps each local image's manifest digest to its id,
// whatever it is named.
func (p *Podman) ImagesByDigest(ctx context.Context) (map[string]string, error) {
	out, err := p.Run(ctx, "images", "--digests", "--no-trunc", "--format", "{{.Digest}} {{.ID}}")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range fields(out) {
		if d, id, ok := strings.Cut(l, " "); ok {
			m[d] = id
		}
	}
	return m, nil
}

// ImageDigest is the manifest digest an image was pulled (or loaded) by.
func (p *Podman) ImageDigest(ctx context.Context, ref string) (string, error) {
	out, err := p.Run(ctx, "image", "inspect", "--format", "{{.Digest}}", ref)
	d := strings.TrimSpace(string(out))
	if err == nil && !strings.HasPrefix(d, "sha256:") {
		err = fmt.Errorf("image %s has no digest", ref)
	}
	return d, err
}

func (p *Podman) Images(ctx context.Context) ([]string, error) {
	out, err := p.Run(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	var imgs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" && !strings.Contains(l, "<none>") {
			imgs = append(imgs, l)
		}
	}
	return imgs, nil
}

// ---- volumes ----------------------------------------------------------------

func (p *Podman) VolumeExists(ctx context.Context, name string) bool {
	_, err := p.Run(ctx, "volume", "exists", name)
	return err == nil
}

func (p *Podman) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	a := []string{"volume", "create"}
	for k, v := range labels {
		a = append(a, "--label", k+"="+v)
	}
	_, err := p.Run(ctx, append(a, name)...)
	return err
}

func (p *Podman) VolumeRemove(ctx context.Context, name string) error {
	_, err := p.Run(ctx, "volume", "rm", "-f", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (p *Podman) VolumeMountpoint(ctx context.Context, name string) (string, error) {
	out, err := p.Run(ctx, "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	return strings.TrimSpace(string(out)), err
}

// VolumeList returns volume names with a label.
func (p *Podman) VolumeList(ctx context.Context, label string) ([]string, error) {
	out, err := p.Run(ctx, "volume", "ls", "--filter", "label="+label, "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	return fields(out), nil
}

// VolumeExport writes a tar of the volume to w.
func (p *Podman) VolumeExport(ctx context.Context, name string, w io.Writer) error {
	var stderr bytes.Buffer
	c := p.cmd(ctx, "volume", "export", name)
	c.Stdout, c.Stderr = w, &stderr
	// Cancelled: killed; a child of podman's still holding stdout does not
	// keep Run waiting past WaitDelay.
	c.WaitDelay = WaitDelay
	if err := c.Run(); err != nil {
		return fmt.Errorf("podman volume export %s: %v: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// VolumeImport replaces the volume's contents with a tar from r.
func (p *Podman) VolumeImport(ctx context.Context, name string, r io.Reader) error {
	var stderr bytes.Buffer
	c := p.cmd(ctx, "volume", "import", name, "-")
	c.Stdin, c.Stderr = r, &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("podman volume import %s: %v: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ---- networks ---------------------------------------------------------------

// Network is a created network's addressing.
type Network struct {
	Interface string
	Gateway   netip.Addr
}

// NetworkCreate creates a bridge network (if missing) with a fixed bridge
// interface name and Podman's own DNS off, and returns its addressing.
func (p *Podman) NetworkCreate(ctx context.Context, name, iface string, labels map[string]string) (Network, error) {
	a := []string{"network", "create", "--ignore", "--disable-dns", "--interface-name", iface}
	for k, v := range labels {
		a = append(a, "--label", k+"="+v)
	}
	if _, err := p.Run(ctx, append(a, name)...); err != nil {
		return Network{}, err
	}
	out, err := p.Run(ctx, "network", "inspect", "--format",
		"{{.NetworkInterface}} {{(index .Subnets 0).Gateway}}", name)
	if err != nil {
		return Network{}, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return Network{}, fmt.Errorf("network inspect %s: %q", name, out)
	}
	gw, err := netip.ParseAddr(f[1])
	if err != nil {
		return Network{}, fmt.Errorf("network %s: bad gateway %q", name, out)
	}
	return Network{Interface: f[0], Gateway: gw}, nil
}

func (p *Podman) NetworkRemove(ctx context.Context, name string) error {
	_, err := p.Run(ctx, "network", "rm", "-f", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ---- containers -------------------------------------------------------------

// Create creates a container from a full `podman create` argument list
// (without the leading "create"). Returns its id.
func (p *Podman) Create(ctx context.Context, args []string) (string, error) {
	out, err := p.Run(ctx, append([]string{"create"}, args...)...)
	return strings.TrimSpace(string(out)), err
}

func (p *Podman) Start(ctx context.Context, name string) error {
	_, err := p.Run(ctx, "start", name)
	return err
}

func (p *Podman) Stop(ctx context.Context, name string, timeout time.Duration) error {
	_, err := p.Run(ctx, "stop", "-t", strconv.Itoa(int(timeout.Seconds())), name)
	return err
}

func (p *Podman) Kill(ctx context.Context, name, signal string) error {
	_, err := p.Run(ctx, "kill", "-s", signal, name)
	return err
}

func (p *Podman) Remove(ctx context.Context, name string) error {
	_, err := p.Run(ctx, "rm", "-f", "-t", "0", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ContainerIP is a container's address on a network.
func (p *Podman) ContainerIP(ctx context.Context, name, network string) (string, error) {
	out, err := p.Run(ctx, "container", "inspect", "--format",
		fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", network), name)
	ip := strings.TrimSpace(string(out))
	if err == nil && ip == "" {
		err = fmt.Errorf("%s has no address on %s", name, network)
	}
	return ip, err
}

type ContainerState struct {
	Exists     bool
	Status     string // created | running | exited | ...
	Running    bool
	ExitCode   int
	OOMKilled  bool
	Pid        int
	StartedAt  time.Time
	FinishedAt time.Time
	CgroupPath string
}

func (p *Podman) Inspect(ctx context.Context, name string) (ContainerState, error) {
	out, err := p.Run(ctx, "container", "inspect", name)
	if errors.Is(err, ErrNotFound) {
		return ContainerState{}, nil
	}
	if err != nil {
		return ContainerState{}, err
	}
	var raw []struct {
		State struct {
			Status     string    `json:"Status"`
			Running    bool      `json:"Running"`
			ExitCode   int       `json:"ExitCode"`
			OOMKilled  bool      `json:"OOMKilled"`
			Pid        int       `json:"Pid"`
			StartedAt  time.Time `json:"StartedAt"`
			FinishedAt time.Time `json:"FinishedAt"`
			CgroupPath string    `json:"CgroupPath"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw) == 0 {
		return ContainerState{}, fmt.Errorf("inspect %s: %v", name, err)
	}
	r := raw[0]
	return ContainerState{
		Exists: true, Status: r.State.Status, Running: r.State.Running, ExitCode: r.State.ExitCode,
		OOMKilled: r.State.OOMKilled, Pid: r.State.Pid, StartedAt: r.State.StartedAt, FinishedAt: r.State.FinishedAt,
		CgroupPath: r.State.CgroupPath,
	}, nil
}

// Wait blocks until the container exits and returns its exit code.
func (p *Podman) Wait(ctx context.Context, name string) (int, error) {
	out, err := p.Run(ctx, "wait", name)
	if err != nil {
		return -1, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1, fmt.Errorf("podman wait: %q", out)
	}
	return code, nil
}

// Usage reads a running container's resource use from its cgroup (v2).
// Peaks come from the kernel (memory.peak, pids.peak), so short spikes
// between samples are not missed; the current values are for history.
type Usage struct {
	PeakMemoryBytes int64
	PeakPids        int
	CPUSeconds      float64
	MemoryBytes     int64
	Pids            int
	// Pressure stall totals in seconds (PSI): nil when the kernel has no
	// *.pressure file, which is not the same as no stall.
	CPUStall, MemoryStall, MemoryFullStall, IOStall, IOFullStall *float64
}

func CgroupUsage(cgroupPath string) (Usage, error) {
	if cgroupPath == "" {
		return Usage{}, errors.New("no cgroup")
	}
	return cgroupUsageAt("/sys/fs/cgroup" + cgroupPath), nil
}

func cgroupUsageAt(base string) Usage {
	var u Usage
	u.CPUStall, _ = readPressure(base + "/cpu.pressure")
	u.MemoryStall, u.MemoryFullStall = readPressure(base + "/memory.pressure")
	u.IOStall, u.IOFullStall = readPressure(base + "/io.pressure")
	if n, err := readInt(base + "/memory.peak"); err == nil {
		u.PeakMemoryBytes = n
	}
	if n, err := readInt(base + "/pids.peak"); err == nil {
		u.PeakPids = int(n)
	}
	if n, err := readInt(base + "/memory.current"); err == nil {
		u.MemoryBytes = n
	}
	if n, err := readInt(base + "/pids.current"); err == nil {
		u.Pids = int(n)
	}
	if b, err := os.ReadFile(base + "/cpu.stat"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
				us, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				u.CPUSeconds = float64(us) / 1e6
			}
		}
	}
	return u
}

// readPressure reads a cgroup v2 *.pressure file's "some" and "full"
// totals (microseconds in the file) as seconds. Either is nil when the file
// or its line is missing: a kernel without PSI, or cpu.pressure's "full"
// before Linux 5.13.
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=12345
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=6789
func readPressure(path string) (some, full *float64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	for _, l := range strings.Split(string(b), "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(l), " ")
		for _, f := range strings.Fields(rest) {
			v, ok := strings.CutPrefix(f, "total=")
			if !ok {
				continue
			}
			us, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				break
			}
			s := float64(us) / 1e6
			switch kind {
			case "some":
				some = &s
			case "full":
				full = &s
			}
		}
	}
	return some, full
}

// HostUsage is the whole machine's: CPU seconds used since boot (all
// cores, from /proc/stat), memory in use (MemTotal - MemAvailable), and
// the disk used and still free to unprivileged writers on the filesystem
// holding dir (DiskOK: both were read).
type HostUsage struct {
	CPUSeconds  float64
	MemoryBytes int64
	DiskBytes   int64
	DiskFree    int64
	DiskOK      bool
}

func ReadHostUsage(dir string) (HostUsage, error) {
	var u HostUsage
	cpu, err := hoststat.ReadCPU()
	if err != nil {
		return u, err
	}
	u.CPUSeconds = cpu.Seconds
	if m, err := hoststat.ReadMemory(); err == nil {
		u.MemoryBytes = m.Used
	}
	if d, err := hoststat.ReadDisk(dir); err == nil {
		u.DiskBytes, u.DiskFree, u.DiskOK = d.Used, d.Free, true
	}
	return u, nil
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func fields(out []byte) []string {
	var r []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r = append(r, l)
		}
	}
	return r
}

// Stats is what podman stats reports that the cgroup does not: network I/O.
type Stats struct {
	NetInput  int64
	NetOutput int64
}

func (p *Podman) Stats(ctx context.Context, name string) (Stats, error) {
	out, err := p.Run(ctx, "stats", "--no-stream", "--no-reset", "--format", "json", name)
	if err != nil {
		return Stats{}, err
	}
	var raw []struct {
		NetIO string `json:"net_io"` // "1.2kB / 3.4MB"
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw) == 0 {
		return Stats{}, fmt.Errorf("stats %s: %v", name, err)
	}
	in, outb, _ := strings.Cut(raw[0].NetIO, "/")
	return Stats{NetInput: parseSize(in), NetOutput: parseSize(outb)}, nil
}

// parseSize parses podman's human sizes ("1.5kB", "3MB", "0B"), decimal units.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3}, {"B", 1}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0
			}
			return int64(f * u.mult)
		}
	}
	return 0
}
