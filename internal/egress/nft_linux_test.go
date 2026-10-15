//go:build linux

package egress

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
)

// TestRealNft loads the table through the real nft binary, in whatever
// network namespace the test runs in: set LUX_NFT_TEST=1 only inside a
// throwaway one (a container with NET_ADMIN), never on a host with a live
// runner, since it replaces table inet lux.
func TestRealNft(t *testing.T) {
	if os.Getenv("LUX_NFT_TEST") != "1" {
		t.Skip("SKIP: LUX_NFT_TEST=1 not set (needs nft and NET_ADMIN in a disposable netns)")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Fatal("LUX_NFT_TEST=1 but no nft binary")
	}
	t.Cleanup(func() { _ = nftScript("table inet lux {}\ndelete table inet lux\n") })
	newReal := func() *Firewall {
		f := newFirewall([]netip.Prefix{netip.MustParsePrefix("198.51.100.7/32")},
			func(context.Context, string) ([]netip.Addr, error) { return nil, nil }, nftScript, nftListCounters)
		if err := f.setup(); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return f
	}
	f := newReal()
	ctx := context.Background()
	gw := netip.MustParseAddr("127.0.0.1") // the stub needs an address it can bind
	if err := f.Apply(ctx, "luxrestricted01", gw, false, nil, nil); err != nil {
		t.Fatalf("restricted Apply before classes: %v", err)
	}
	realNft := f.nft
	f.nft = func(s string) error { return realNft(s + "add rule inet lux nonexistent accept\n") }
	if err := f.SetClasses(catalog("v1")); err == nil {
		t.Fatal("invalid layout transaction succeeded")
	}
	f.nft = realNft
	if err := f.SetClasses(catalog("v1")); err != nil {
		t.Fatalf("SetClasses (layout change): %v", err)
	}
	if err := f.Apply(ctx, "luxunrestrict01", netip.Addr{}, true, nil, nil); err != nil {
		t.Fatalf("unrestricted Apply: %v", err)
	}
	nc := catalog("v2")
	nc.RegionPublic = append(nc.RegionPublic, "54.0.0.0/8")
	if err := f.SetClasses(nc); err != nil {
		t.Fatalf("SetClasses (sets only): %v", err)
	}
	nc = catalog("v3")
	delete(nc.Provider, "us-west-2")
	if err := f.SetClasses(nc); err != nil {
		t.Fatalf("SetClasses (region dropped): %v", err)
	}
	c, err := f.Counters()
	if err != nil {
		t.Fatalf("Counters: %v", err)
	}
	t.Logf("counters read: runs %v host %v", c.Runs, c.Host)
	// Give a counter bytes as a restart would find it, then restart.
	if err := nftScript("delete counter inet lux " + tx("host", "internet") + "\n"); err == nil {
		t.Fatal("deleting a counter a rule uses succeeded")
	}
	conn, err := net.Dial("udp", "34.241.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, 123)); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	measured, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if measured.Host["provider:eu-west-1"].Tx != 151 {
		t.Fatal(measured)
	}
	f.Remove("luxrestricted01")
	f2 := newReal()
	if _, ok := f2.acct.kept[tx("luxunrestrict01", "internet")]; !ok {
		t.Errorf("restart did not keep the unrestricted Run's counters: %v", f2.acct.kept)
	}
	if _, ok := f2.acct.kept[tx("luxrestricted01", "internet")]; ok {
		t.Error("a removed Run's counters survived Remove")
	}
	if err := f2.SetClasses(catalog("v1")); err != nil {
		t.Fatalf("SetClasses after restart: %v", err)
	}
	if err := f2.Apply(ctx, "luxunrestrict01", netip.Addr{}, true, nil, nil); err != nil {
		t.Fatalf("re-adopt Apply: %v", err)
	}
	f2.PruneCounters()
	restored, err := f2.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Host["provider:eu-west-1"].Tx != 151 {
		t.Fatal(restored)
	}
	// Real packets from this process: the output hook, host counters.
	send := func(dst string, n int) {
		conn, err := net.Dial("udp", dst+":9")
		if err != nil {
			t.Fatalf("dial %s: %v", dst, err)
		}
		defer conn.Close()
		if _, err := conn.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f2.Counters()
	if err != nil {
		t.Fatal(err)
	}
	send("34.241.0.1", 1000)
	send("8.8.8.8", 500)
	send("127.0.0.1", 700) // loopback: not network transfer
	after, err := f2.Counters()
	if err != nil {
		t.Fatal(err)
	}
	delta := func(k string) int64 { return after.Host[k].Tx - before.Host[k].Tx }
	// UDP+IP headers: 28 bytes over the payload.
	if d := delta("provider:eu-west-1"); d != 1028 {
		t.Errorf("provider:eu-west-1 tx grew %d, want 1028", d)
	}
	if d := delta("internet"); d != 528 {
		t.Errorf("internet tx grew %d, want 528 (loopback excluded)", d)
	}
	if _, ok := after.Runs["luxunrestrict01"]; !ok {
		names, _ := f2.list()
		byName, _ := parseCounters(names)
		if _, ok := byName[tx("luxunrestrict01", "provider:eu-west-1")]; !ok {
			t.Error("re-adopted Run has no per-region counter")
		}
	}
	f2.Remove("luxunrestrict01")
}
