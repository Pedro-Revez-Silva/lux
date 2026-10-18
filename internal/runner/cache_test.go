package runner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/passwd"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// fakeCacheVolumes stores real files for each named volume under $0.volumes.
const fakeCacheVolumes = `"volume exists") test -d "$0.volumes/$3" ;;
"volume create") for n; do :; done; mkdir -p "$0.volumes/$n" ;;
"volume inspect") echo "$0.volumes/$5" ;;
"volume rm")
  shift 2
  for arg; do case "$arg" in -f|--force) ;; *) n="$arg" ;; esac; done
  rm -r "$0.volumes/$n" ;;`

// fakeCacheRm removes the sentinel $0.cache.<name>; without -f it refuses
// while $0.busy.<name> exists, as Podman refuses an attached volume.
const fakeCacheRm = `"volume rm")
  shift 2
  force=false
  for arg; do case "$arg" in -f|--force) force=true ;; *) n="$arg" ;; esac; done
  [ -e "$0.busy.$n" ] && [ "$force" = false ] && exit 1
  rm "$0.cache.$n" ;;`

// fakePodman is a podman script handling cases ("$1 $2" patterns) and
// failing every other call.
func fakePodman(cases ...string) string {
	return "#!/bin/sh\ncase \"$1 $2\" in\n" + strings.Join(cases, "\n") + "\n*) exit 1 ;;\nesac\n"
}

func gomodCacheSpec() spec.RunSpec {
	return spec.RunSpec{Volumes: []spec.Volume{{Name: "gomod", Path: "/cache", Kind: "cache"}}}
}

func writeCacheUse(t *testing.T, f *finishFixture, metadata string) {
	t.Helper()
	writeFile(t, filepath.Join(f.r.cfg.DataDir, "caches.json"), metadata)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freeOnceGone reports the recovery mark reached once path is removed.
func freeOnceGone(path string) func() (int64, error) {
	return func() (int64, error) {
		if exists(path) {
			return 0, nil
		}
		return 100, nil
	}
}

func noFree() (int64, error) { return 0, nil }

func TestCacheTenantIsolationAndResume(t *testing.T) {
	f := newFinishFixture(t)
	writeFile(t, f.bin, fakePodman(fakeCacheVolumes))
	prepare := func(run, tenant string, uid, gid int, name string) *placement {
		p := &placement{r: f.r, runID: run, tenantID: tenant, epoch: 1, dir: f.r.runDir(run), state: &runState{RunID: run}, user: passwd.User{UID: uid, GID: gid}}
		sp := spec.RunSpec{Volumes: []spec.Volume{{Name: name, Path: "/cache", Kind: "cache"}}}
		if err := p.prepareVolumes(context.Background(), sp, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	module := func(p *placement) string {
		return filepath.Join(f.bin+".volumes", p.state.Volumes[0].Volume, "module")
	}
	writeFile(t, module(prepare("a", "tenant-a", 1000, 1000, "gomod")), "cached")
	for _, c := range []struct {
		run, tenant, name string
		uid, gid          int
		want              bool
	}{
		{"b", "tenant-a", "gomod", 1000, 1000, true},
		{"a", "tenant-a", "gomod", 1000, 1000, true},
		{"c", "tenant-b", "gomod", 1000, 1000, false},
		{"d", "tenant-a", "gomod", 1001, 1000, false},
		{"e", "tenant-a", "gomod", 1000, 1001, false},
		{"f", "tenant-a", "npm", 1000, 1000, false},
		{"g", "tenant-a", "gomod", 1000, 1000, true},
	} {
		b, err := os.ReadFile(module(prepare(c.run, c.tenant, c.uid, c.gid, c.name)))
		if c.want && (err != nil || string(b) != "cached") || !c.want && !os.IsNotExist(err) {
			t.Fatalf("%+v: %q %v", c, b, err)
		}
	}
}

func TestCacheFreeHoldDoesNotWaitForLifecycle(t *testing.T) {
	f := newFinishFixture(t)
	u := f.r.cacheUse()
	u.mu.Lock()
	defer u.mu.Unlock()
	done := make(chan struct{})
	go func() {
		f.p.holdCaches(spec.RunSpec{})()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cache-free hold/release blocked on cache lifecycle")
	}
	// A fresh runner needs no cache bookkeeping at all for a cache-free Run.
	(&placement{}).holdCaches(spec.RunSpec{})()
}

func TestCacheDiscoveryTimeoutDoesNotBlockPlacement(t *testing.T) {
	oldTimeout := cacheDiscoveryTimeout
	cacheDiscoveryTimeout = 50 * time.Millisecond
	t.Cleanup(func() { cacheDiscoveryTimeout = oldTimeout })

	f := newFinishFixture(t)
	writeCacheUse(t, f, `{"known":1}`)
	// The listing exits only on SIGTERM from context cancellation; the cap
	// keeps a mutated (uncancelled) run from leaking the process.
	writeFile(t, f.bin, fakePodman(`"volume ls")
  trap 'exit 1' TERM
  i=0
  while [ "$i" -lt 3000 ]; do sleep 0.01; i=$((i + 1)); done
  printf 'leaked\n' ;;`))
	done := make(chan struct{})
	go func() {
		f.p.holdCaches(gomodCacheSpec())()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cache placement remained blocked by volume discovery")
	}
	u := f.r.cacheUse()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.last) != 2 || u.last["known"] != 1 || u.last[cacheVolume("t1", "gomod", 0, 0)] == 0 {
		t.Fatalf("cache metadata after discovery timeout: %+v", u.last)
	}
}

func TestCacheMetadataPreparationAndRelease(t *testing.T) {
	for _, metadata := range []string{"missing", "null", "{", "{}"} {
		t.Run(metadata, func(t *testing.T) {
			f := newFinishFixture(t)
			writeFile(t, f.bin, fakePodman(`"volume exists") exit 1 ;;`, `"volume create"|"volume ls") ;;`))
			if metadata != "missing" {
				writeCacheUse(t, f, metadata)
			}
			sp := gomodCacheSpec()
			release := f.p.holdCaches(sp)
			if err := f.p.prepareVolumes(context.Background(), sp, nil); err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestCacheDiscoveryReclaimsLostMetadata(t *testing.T) {
	const timestamps = `{"recent":100,"old":1}`
	for _, metadata := range []string{"missing", "{", "null", timestamps} {
		t.Run(metadata, func(t *testing.T) {
			f := newFinishFixture(t)
			writeFile(t, f.bin, fakePodman(`"volume ls")
  case "$4" in
  label=lux.managed=true) printf 'unknown\nattached\nrecent\nold\nnot-cache\n' ;;
  label=lux.cache=true) printf 'unknown\nattached\nrecent\nold\na-unmanaged\n' ;;
  *) exit 1 ;;
  esac ;;`, fakeCacheRm))
			if metadata != "missing" {
				writeCacheUse(t, f, metadata)
			}
			for _, n := range []string{"unknown", "attached", "recent", "old", "not-cache", "a-unmanaged"} {
				writeFile(t, f.bin+".cache."+n, "sentinel")
			}
			writeFile(t, f.bin+".busy.attached", "")
			if !f.r.reclaimCaches(context.Background(), freeOnceGone(f.bin+".cache.unknown"), 100) {
				t.Fatal("lost cache was not discovered and reclaimed")
			}
			for _, n := range []string{"attached", "not-cache", "a-unmanaged"} {
				if b, err := os.ReadFile(f.bin + ".cache." + n); err != nil || string(b) != "sentinel" {
					t.Fatalf("removed protected %s: %q %v", n, b, err)
				}
			}
			if metadata != timestamps {
				return
			}
			if !exists(f.bin+".cache.old") || !exists(f.bin+".cache.recent") {
				t.Fatal("lost existing timestamps")
			}
			if !f.r.reclaimCaches(context.Background(), freeOnceGone(f.bin+".cache.old"), 100) {
				t.Fatal("known old cache not reclaimed")
			}
			if !exists(f.bin + ".cache.recent") {
				t.Fatal("valid timestamp ordering lost")
			}
		})
	}
}

func TestCacheDiscoveryFailureRetainsKnown(t *testing.T) {
	f := newFinishFixture(t)
	writeCacheUse(t, f, `{"known":1}`)
	writeFile(t, f.bin+".cache.known", "sentinel")
	writeFile(t, f.bin+".cache.unknown", "sentinel")
	writeFile(t, f.bin, fakePodman(`"volume ls") echo unavailable >&2; exit 1 ;;`, fakeCacheRm))
	if !f.r.reclaimCaches(context.Background(), noFree, 100) {
		t.Fatal("listing failure lost known cache")
	}
	if b, err := os.ReadFile(f.bin + ".cache.unknown"); err != nil || string(b) != "sentinel" {
		t.Fatal("listing failure removed unknown cache", err)
	}
}

func TestCachePlacementHoldsUntilLastFailedStart(t *testing.T) {
	f := newFinishFixture(t)
	for _, d := range []string{f.bin + ".root", f.bin + ".volumes"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	readyPath := f.bin + ".ready"
	if err := syscall.Mkfifo(readyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	arrived := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(ready)
		for scanner.Scan() {
			arrived <- scanner.Text()
		}
	}()
	// Each create reports its container name on $0.ready, then fails once
	// the test writes to $0.release.<name>.
	script := strings.Replace(podmanForAStart, `"volume exists") exit 1 ;;
"volume create") ;;`, fakeCacheVolumes+`
"volume ls") ;;`, 1)
	script = strings.Replace(script, "create) echo ctr-1 > \"$0.ctr\"; echo ctr-1 ;;", `create)
  shift
  while [ "$1" != --name ]; do shift; done
  n="$2"
  echo "$n" > "$0.ready"
  read release < "$0.release.$n"
  echo "fixture refused create" >&2
  exit 1 ;;`, 1)
	writeFile(t, f.bin, script)
	sp := gomodCacheSpec()
	sp.Image = spec.Image{Ref: "img"}
	sp.Network = spec.Network{Unrestricted: true}
	ctx := context.Background()
	var placements []*placement
	for _, id := range []string{"cache-a", "cache-b"} {
		if err := syscall.Mkfifo(f.bin+".release.lux-"+id, 0o600); err != nil {
			t.Fatal(err)
		}
		p := newPlacement(f.r, proto.Assign{RunID: id, TenantID: "t1", Epoch: 1, Spec: sp}, nil)
		placements = append(placements, p)
		go p.run(ctx)
	}
	release := func(p *placement) {
		t.Helper()
		pipe, err := os.OpenFile(f.bin+".release.lux-"+p.runID, os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(pipe, "fail")
		pipe.Close()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Fatal("placement did not finish")
		}
	}
	for range placements {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("placement did not reach pre-create barrier")
		}
	}
	defer func() {
		for _, p := range placements {
			select {
			case <-p.done:
			default:
				release(p)
			}
		}
	}()
	sentinel := filepath.Join(f.bin+".volumes", cacheVolume("t1", "gomod", 0, 0), "module")
	writeFile(t, sentinel, "cached")
	reclaimKeeps := func() {
		t.Helper()
		f.r.reclaimCaches(ctx, noFree, 100)
		if b, err := os.ReadFile(sentinel); err != nil || string(b) != "cached" {
			t.Fatalf("placement cache lost: %q %v", b, err)
		}
	}
	reclaimKeeps()
	release(placements[0])
	reclaimKeeps()
	release(placements[1])
	f.r.reclaimCaches(ctx, noFree, 100)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("cache not reclaimed after last placement: %v", err)
	}
}

func TestCacheNeverInSnapshot(t *testing.T) {
	f := newFinishFixture(t)
	f.release()
	f.p.state.Volumes = append(f.p.state.Volumes, volumeRef{Name: "gomod", Volume: "cache", Path: "/cache", Kind: "cache"})
	f.p.assign.Spec.Artifacts.Paths = []string{"/cache/**"}
	f.r.mounts.Store("cache", f.data)
	sd, err := f.p.snapshot(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.Artifacts) != 0 {
		t.Fatalf("cache uploaded as artifacts: %+v", sd.Artifacts)
	}
	if len(sd.Manifest.Volumes) != 1 || sd.Manifest.Volumes[0].Name != "data" {
		t.Fatalf("manifest: %+v", sd.Manifest)
	}
}

func TestCacheReclaimLRUAndMounted(t *testing.T) {
	f := newFinishFixture(t)
	writeFile(t, f.bin, fakePodman(fakeCacheRm))
	u := f.r.cacheUse()
	u.last = map[string]int64{"old": 1, "new": 2, "mounted": 0, "starting": 0}
	u.held["starting"] = 1
	for n := range u.last {
		writeFile(t, f.bin+".cache."+n, "cache")
	}
	writeFile(t, f.bin+".busy.mounted", "")
	if !f.r.reclaimCaches(context.Background(), freeOnceGone(f.bin+".cache.old"), 100) {
		t.Fatal("nothing reclaimed")
	}
	for _, n := range []string{"new", "mounted", "starting"} {
		if !exists(f.bin + ".cache." + n) {
			t.Fatalf("removed %s", n)
		}
	}
	if exists(f.bin + ".cache.old") {
		t.Fatal("old remains")
	}
}

func TestCacheExcludedFromRunDisk(t *testing.T) {
	f := newFinishFixture(t)
	f.p.state.Volumes = []volumeRef{{Name: "cache", Volume: "cache", Path: "/cache", Kind: "cache"}}
	f.r.mounts.Store("cache", f.data)
	f.p.sampleSlow(context.Background())
	if got := f.p.usage(); got != nil && got.PeakDiskBytes != 0 {
		t.Fatalf("cache charged to Run: %+v", got)
	}
}

func TestSpotReleaseKeepsCache(t *testing.T) {
	f := newFinishFixture(t)
	writeFile(t, f.bin+".vol.cache", "")
	if err := f.p.removeWorkloadDisk(context.Background(), "", []volumeRef{{Volume: "cache", Kind: "cache"}}); err != nil {
		t.Fatal(err)
	}
	if !exists(f.bin + ".vol.cache") {
		t.Fatal("cache removed")
	}
}
