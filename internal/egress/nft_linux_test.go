//go:build linux

package egress

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	for _, ports := range [][]int{{8090, 9000}, {8090}, nil} {
		if _, err := f.SetControlPlanePorts(ports); err != nil {
			t.Fatalf("SetControlPlanePorts(%v): %v", ports, err)
		}
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
	realDroppedInbound(t, f2)
}

// realDroppedInbound injects one UDP packet from an internet address into
// a veth whose peer holds a local address, while another table drops it
// on the input hook at priority 0 (a distribution firewall): the ingress
// counter at -300 sees it, host_in at 100 does not.
func realDroppedInbound(t *testing.T, f *Firewall) {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		t.Fatal("LUX_NFT_TEST=1 but no ip binary (iproute2)")
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	run("link", "add", "nfttv0", "type", "veth", "peer", "name", "nfttv1")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "nfttv0").Run() })
	run("addr", "add", "192.0.2.1/24", "dev", "nfttv0")
	run("link", "set", "nfttv0", "up")
	run("link", "set", "nfttv1", "up")
	for _, k := range []string{"all", "nfttv0"} {
		_ = os.WriteFile("/proc/sys/net/ipv4/conf/"+k+"/rp_filter", []byte("0"), 0)
	}
	if err := nftScript("table inet luxtestdrop {}\ndelete table inet luxtestdrop\n" +
		"add table inet luxtestdrop\nadd chain inet luxtestdrop in { type filter hook input priority 0; policy accept; }\n" +
		"add rule inet luxtestdrop in iifname \"nfttv0\" udp dport 9 drop\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nftScript("delete table inet luxtestdrop\n") })
	before, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	dst, err := net.InterfaceByName("nfttv0")
	if err != nil {
		t.Fatal(err)
	}
	src, err := net.InterfaceByName("nfttv1")
	if err != nil {
		t.Fatal(err)
	}
	const payload = 200
	frame := udpFrame(dst.HardwareAddr, src.HardwareAddr, [4]byte{8, 8, 4, 4}, [4]byte{192, 0, 2, 1}, payload)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: src.Index, Halen: 6}); err != nil {
		t.Fatal(err)
	}
	// An injected frame is received in softirq, after Sendto returns: poll
	// rxDelivered (counted first, at -300) until it moves, for at most 5s.
	var d, r int64
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		after, err := f.Counters()
		if err != nil {
			t.Fatal(err)
		}
		d = after.Host["internet"].RxDelivered - before.Host["internet"].RxDelivered
		r = after.Host["internet"].Rx - before.Host["internet"].Rx
		if d != 0 || time.Now().After(deadline) {
			break
		}
	}
	if d != payload+28 || r != 0 {
		t.Fatalf("dropped inbound: rxDelivered grew %d (want %d), rx grew %d (want 0)", d, payload+28, r)
	}
}

// udpFrame is an Ethernet frame holding an IPv4 UDP datagram to port 9.
func udpFrame(dstMAC, srcMAC net.HardwareAddr, src, dst [4]byte, payload int) []byte {
	ipLen := 20 + 8 + payload
	b := make([]byte, 14+ipLen)
	copy(b[0:6], dstMAC)
	copy(b[6:12], srcMAC)
	binary.BigEndian.PutUint16(b[12:], 0x0800)
	ip := b[14:]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:], ^uint16(sum))
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:], 40000)
	binary.BigEndian.PutUint16(udp[2:], 9)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+payload))
	return b
}
