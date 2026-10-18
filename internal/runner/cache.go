package runner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/spec"
)

const labelCache = "lux.cache"

var cacheDiscoveryTimeout = 10 * time.Second

type cacheUse struct {
	mu   sync.Mutex
	last map[string]int64 // volume -> last mount or release, Unix ms
	held map[string]int
	path string
}

// cacheUse loads caches.json once, then adds managed cache volumes it lacks
// as least recently used so they stay reclaimable.
func (r *Runner) cacheUse() *cacheUse {
	r.cacheOnce.Do(func() {
		u := &cacheUse{last: map[string]int64{}, held: map[string]int{}, path: filepath.Join(r.cfg.DataDir, "caches.json")}
		if b, err := os.ReadFile(u.path); err == nil {
			var last map[string]int64
			if err := json.Unmarshal(b, &last); err != nil || last == nil {
				r.log.Warn("invalid cache use metadata; starting with empty cache map", "path", u.path, "err", err)
			} else {
				u.last = last
			}
		}
		r.caches = u
		ctx, cancel := context.WithTimeout(context.Background(), cacheDiscoveryTimeout)
		defer cancel()
		managed, managedErr := r.pm.VolumeList(ctx, LabelManaged+"=true")
		cached, cacheErr := r.pm.VolumeList(ctx, labelCache+"=true")
		if managedErr != nil || cacheErr != nil {
			r.log.Warn("discovering cache volumes failed", "managed_err", managedErr, "cache_err", cacheErr)
			return
		}
		for _, name := range cached {
			if _, ok := u.last[name]; !ok && slices.Contains(managed, name) {
				u.last[name] = 0
			}
		}
	})
	return r.caches
}

func cacheVolume(tenant, name string, uid, gid int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d:%d", tenant, name, uid, gid)))
	return fmt.Sprintf("lux-cache-%x", h[:])
}

func (p *placement) cacheVolume(name string) string {
	return cacheVolume(p.tenantID, name, p.user.UID, p.user.GID)
}

func (u *cacheUse) save() error {
	b, err := json.Marshal(u.last)
	if err != nil {
		return err
	}
	return writeFileAtomic(u.path, b, 0o600)
}

// Hold through setup and container removal, including failed starts. This
// serializes lifecycle changes only; workloads may write concurrently.
func (p *placement) holdCaches(sp spec.RunSpec) func() {
	var names []string
	for _, v := range sp.Volumes {
		if v.Kind == "cache" {
			names = append(names, p.cacheVolume(v.Name))
		}
	}
	if len(names) == 0 {
		return func() {}
	}
	u := p.r.cacheUse()
	u.mu.Lock()
	for _, n := range names {
		u.held[n]++
	}
	u.mu.Unlock()
	return func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		for _, n := range names {
			u.held[n]--
			u.last[n] = time.Now().UnixMilli()
		}
		if err := u.save(); err != nil {
			p.logf("saving cache use failed", "err", err)
		}
		p.r.diskChanged()
	}
}

func (p *placement) prepareCache(ctx context.Context, v volumeRef) error {
	u := p.r.cacheUse()
	u.mu.Lock()
	defer u.mu.Unlock()
	hit := p.r.pm.VolumeExists(ctx, v.Volume)
	if !hit {
		if err := p.r.pm.VolumeCreate(ctx, v.Volume, map[string]string{LabelManaged: "true", LabelTenant: p.tenantID, labelCache: "true"}); err != nil {
			return err
		}
	}
	u.last[v.Volume] = time.Now().UnixMilli()
	if err := u.save(); err != nil {
		return err
	}
	p.r.diskChanged()
	p.event(ctx, "volume.cache", map[string]any{"name": v.Name, "hit": hit})
	return nil
}

// Non-forced removal lets Podman protect every attached container, including
// ones re-adopted after a restart or left by a failed removal.
func (r *Runner) reclaimCaches(ctx context.Context, free func() (int64, error), recoverFree int64) bool {
	u := r.cacheUse()
	u.mu.Lock()
	defer u.mu.Unlock()
	names := slices.SortedFunc(maps.Keys(u.last), func(a, b string) int {
		return cmp.Or(cmp.Compare(u.last[a], u.last[b]), cmp.Compare(a, b))
	})
	removed := false
	for _, n := range names {
		f, err := free()
		if err != nil || f >= recoverFree {
			break
		}
		if u.held[n] > 0 {
			continue
		}
		if _, err := r.pm.Run(ctx, "volume", "rm", n); err != nil {
			continue
		}
		delete(u.last, n)
		r.forgetMountpoint(n)
		removed = true
		r.log.Info("disk pressure: removed cache volume", "volume", n)
	}
	if removed {
		if err := u.save(); err != nil {
			r.log.Warn("saving cache use failed", "err", err)
		}
		r.diskChanged()
	}
	return removed
}
