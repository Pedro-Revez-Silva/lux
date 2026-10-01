package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

// Two syncs of one Run under way at once keep their own bundles: preparing
// the second leaves the first's for its shim to fetch, and each is removed
// when its own sync is done.
func TestConcurrentSyncsKeepTheirBundles(t *testing.T) {
	f := newSyncFixture(t, `true`)
	p, sp := f.p, f.sp
	rt := filepath.Join(p.r.cfg.DataDir, "rt")
	refs := []proto.SyncRef{{Repo: "app", Ref: "main"}}
	ctx := context.Background()

	first, _ := p.prepareSync(ctx, sp, refs, "s1", false)
	second, _ := p.prepareSync(ctx, sp, refs, "s2", false)
	if first == nil || second == nil || first.Repos[0].Bundle == second.Repos[0].Bundle {
		t.Fatalf("bundles: %+v %+v", first, second)
	}
	host := func(b string) string { return filepath.Join(rt, strings.TrimPrefix(b, proto.ShimRunDir+"/")) }
	for _, a := range []*proto.SyncArgs{first, second} {
		if _, err := os.Stat(host(a.Repos[0].Bundle)); err != nil {
			t.Fatalf("a sync's bundle is gone before its shim ran: %v", err)
		}
	}
	p.removeSyncBundles(ctx, "s1")
	if _, err := os.Stat(host(first.Repos[0].Bundle)); !os.IsNotExist(err) {
		t.Fatalf("s1's bundle after s1: %v", err)
	}
	if _, err := os.Stat(host(second.Repos[0].Bundle)); err != nil {
		t.Fatalf("s2's bundle after s1: %v", err)
	}
	// A request id is never a path.
	if d := syncSubdir("../../etc"); strings.ContainsAny(d, "./") {
		t.Fatalf("subdir of a hostile id: %q", d)
	}
}
