package egress

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// catalog is a NetworkClasses as luxd builds one for a host in zone a.
func catalog(version string) *proto.NetworkClasses {
	return &proto.NetworkClasses{
		Version: version, Source: proto.NetSourceEC2,
		Local:              []string{"10.0.0.0/24", "2001:db8:0::/64"},
		Zone:               []string{"10.0.0.0/16", "2001:db8::/48"}, // covers Local: Local wins
		RegionService:      []string{"52.216.0.0/15", "2600:1f18:1000::/40"},
		RegionPublic:       []string{"52.0.0.0/8", "2600:1f18::/32"}, // covers the service: service wins
		ProviderGlobalFree: []string{"13.32.0.0/15"},
		ProviderGlobal:     []string{"15.177.0.0/18"},
		Provider: map[string][]string{
			"eu-west-1": {"34.240.0.0/13", "2a05:d018::/36"},
			"us-west-2": {"35.80.0.0/12"},
		},
	}
}

const runIface = "luxaaaaaaaaaaaa"

func tx(owner, key string) string { return counterName(owner, key, "tx") }
func rx(owner, key string) string { return counterName(owner, key, "rx") }

// out is a Run's packet leaving through eth0 to dst; in its reply.
func out(dst string, n int64) packet {
	return packet{hook: "forward", iif: runIface, oif: "eth0", src: "10.89.0.2", dst: dst, proto: "tcp", dport: 443, bytes: n}
}
func in(src string, n int64) packet {
	return packet{hook: "forward", iif: "eth0", oif: runIface, src: src, dst: "10.89.0.2", proto: "tcp", established: true, bytes: n}
}

func setupRun(t *testing.T, k *kernel, nc *proto.NetworkClasses, unrestricted bool, rules ...spec.EgressRule) *Firewall {
	t.Helper()
	f, err := k.firewall("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if nc != nil {
		if err := f.SetClasses(nc); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), unrestricted, rules, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Remove(runIface) })
	return f
}

func TestClassificationPrecedenceAndDirection(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), true)
	cases := []struct {
		peer string
		key  string
	}{
		{"10.0.0.5", "local"},            // in Local and Zone
		{"10.0.7.5", "zone"},             // Zone only
		{"52.217.1.1", "region-service"}, // in the service and region-public
		{"52.1.1.1", "region-public"},
		{"34.241.0.1", "provider:eu-west-1"},
		{"35.81.0.1", "provider:us-west-2"},
		{"13.33.0.1", "provider-global-free"},
		{"15.177.0.1", "provider-global"},
		{"8.8.8.8", "internet"},
		{"2001:db8:0::9", "local"},
		{"2001:db8:0:5::9", "zone"},
		{"2600:1f18:1000::1", "region-service"},
		{"2600:1f18:2000::1", "region-public"},
		{"2a05:d018::1", "provider:eu-west-1"},
		{"2606:4700::1", "internet"},
	}
	expected := map[string]proto.NetBytes{}
	for i, c := range cases {
		n := int64(1000 * (i + 1))
		b := expected[c.key]
		b.Tx += n
		b.Rx += n + 1
		b.RxDelivered += n + 1
		expected[c.key] = b
		k.send(out(c.peer, n), false)
		k.send(in(c.peer, n+1), false)
		got, err := f.Counters()
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(got.Runs[runIface], expected) {
			t.Fatalf("%s: got %v want %v", c.peer, got.Runs[runIface], expected)
		}
	}
	got, _ := f.Counters()
	if _, ok := got.Runs[runIface]["unknown"]; ok {
		t.Errorf("with a catalog nothing is unknown: %v", got.Runs[runIface])
	}
	var sum int64
	for _, b := range got.Runs[runIface] {
		sum += b.Tx
	}
	var want int64
	for i := range cases {
		want += int64(1000 * (i + 1))
	}
	if sum != want {
		t.Errorf("each packet counted once: tx total %d, want %d", sum, want)
	}
	if len(got.Host) != 0 {
		t.Errorf("forwarded traffic is the Run's, not the host's: %v", got.Host)
	}
}

func TestNoCatalogIsUnknownNotInternet(t *testing.T) {
	for name, nc := range map[string]*proto.NetworkClasses{
		"older luxd":    nil,
		"empty":         {Version: "x"},
		"static, no ip": {Version: "y", Source: proto.NetSourceNone},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKernel()
			f := setupRun(t, k, nc, true)
			k.send(out("192.168.1.20", 10), false) // the host's own LAN
			k.send(out("8.8.8.8", 20), false)
			k.send(out("2606:4700::1", 30), false)
			got, _ := f.Counters()
			want := map[string]proto.NetBytes{"local": {Tx: 10}, "unknown": {Tx: 50}}
			if !maps.Equal(got.Runs[runIface], want) {
				t.Errorf("got %v, want %v", got.Runs[runIface], want)
			}
		})
	}
}

func TestDroppedAndHostTrafficIsNotCounted(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), false, spec.EgressRule{CIDR: "52.1.0.0/16"})
	if v := k.send(out("8.8.8.8", 500), false); v != "drop" {
		t.Fatalf("restricted Run reached a host it does not allow: %s", v)
	}
	if v := k.send(out("52.1.2.3", 70), false); v != "accept" {
		t.Fatalf("allowed address dropped: %s", v)
	}
	// The Run's DNS stub and gateway: input on the host, never forwarded.
	k.send(packet{hook: "input", iif: runIface, src: "10.89.0.2", dst: "10.89.0.1", proto: "udp", dport: StubPort, bytes: 900}, false)
	// Another container network on the host.
	k.send(packet{hook: "forward", iif: runIface, oif: "podman0", src: "10.89.0.2", dst: "10.88.0.5", proto: "tcp", bytes: 800}, false)
	got, _ := f.Counters()
	want := map[string]proto.NetBytes{"region-public": {Tx: 70}}
	if !maps.Equal(got.Runs[runIface], want) {
		t.Errorf("Run counted %v, want only the accepted forwarded packet %v", got.Runs[runIface], want)
	}
	if len(got.Host) != 0 {
		t.Errorf("host counted a Run's traffic: %v", got.Host)
	}
}

func TestAcceptedInterbridgeExcludedBothDirections(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), true)
	for _, bridge := range []string{"podman0", "cni-test", "docker0", "virbr0"} {
		p := out("8.8.8.8", 3200)
		p.oif = bridge
		if v := k.send(p, false); v != "accept" {
			t.Fatal(v)
		}
		p = in("8.8.8.8", 3600)
		p.iif = bridge
		if v := k.send(p, false); v != "accept" {
			t.Fatal(v)
		}
	}
	got, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs[runIface]) != 0 || len(got.Host) != 0 {
		t.Fatal(got)
	}
}

func TestHostTraffic(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), true)
	k.send(packet{hook: "output", oif: "eth0", src: "10.0.0.9", dst: "52.217.1.1", proto: "tcp", bytes: 100}, false)
	k.send(packet{hook: "input", iif: "eth0", src: "52.217.1.1", dst: "10.0.0.9", proto: "tcp", bytes: 4000}, false)
	k.send(packet{hook: "output", oif: "lo", src: "127.0.0.1", dst: "127.0.0.1", proto: "tcp", bytes: 7}, false)
	k.send(packet{hook: "output", oif: runIface, src: "10.89.0.1", dst: "10.89.0.2", proto: "udp", bytes: 9}, false)
	k.send(out("8.8.8.8", 3), false)
	got, _ := f.Counters()
	want := map[string]proto.NetBytes{"region-service": {Tx: 100, Rx: 4000, RxDelivered: 4000}}
	if !maps.Equal(got.Host, want) {
		t.Errorf("host: got %v, want %v", got.Host, want)
	}
	if !maps.Equal(got.Runs[runIface], map[string]proto.NetBytes{"internet": {Tx: 3}}) {
		t.Errorf("run: %v", got.Runs[runIface])
	}
}

// packets every filtering decision of the table is checked against.
func probes() []packet {
	var ps []packet
	for _, dst := range []string{"8.8.8.8", "52.1.2.3", "169.254.169.254", "10.0.0.5", "2606:4700::1", "fe80::1"} {
		ps = append(ps, out(dst, 1), in(dst, 1))
		p := in(dst, 1)
		p.established = false
		ps = append(ps, p)
	}
	ps = append(ps,
		packet{hook: "forward", iif: runIface, oif: "luxbbbbbbbbbbbb", src: "10.89.0.2", dst: "10.89.1.2", proto: "tcp", bytes: 1},
		packet{hook: "forward", iif: "luxnochainxxxxx", oif: "eth0", src: "10.89.2.2", dst: "8.8.8.8", proto: "tcp", bytes: 1},
		packet{hook: "input", iif: runIface, src: "10.89.0.2", dst: "10.89.0.1", proto: "udp", dport: StubPort, bytes: 1},
		packet{hook: "input", iif: runIface, src: "10.89.0.2", dst: "10.89.0.1", proto: "tcp", dport: 22, bytes: 1},
		packet{hook: "output", oif: "eth0", src: "10.0.0.9", dst: "8.8.8.8", proto: "tcp", bytes: 1},
	)
	return ps
}

func TestAccountingNeverChangesFiltering(t *testing.T) {
	for _, unrestricted := range []bool{false, true} {
		k := newKernel()
		f := setupRun(t, k, catalog("v1"), unrestricted, spec.EgressRule{CIDR: "52.1.0.0/16"})
		for _, p := range probes() {
			if with, without := k.send(p, false), k.send(p, true); with != without {
				t.Errorf("unrestricted=%t %+v: %s with counting, %s without", unrestricted, p, with, without)
			}
		}
		for _, base := range []string{"count", "count_in", "host_out", "host_in", "count_in_host"} {
			for v := range k.verdicts(base) {
				if v != "accept" && v != "return" {
					t.Errorf("%s reaches verdict %q", base, v)
				}
			}
		}
		// The filtering chains hold no billing counters: those live only in
		// the counting chains.
		for name, rules := range k.rules() {
			if name != "egress" && name != "to_host" && name != "dns" && !strings.HasPrefix(name, "run_") {
				continue
			}
			for _, r := range rules {
				if slices.Contains(r, "name") {
					t.Errorf("filter chain %s holds a named counter: %v", name, r)
				}
			}
		}
		_ = f
	}
}

// ingressRuleset is the table's every rule and chain declaration after
// setup, a Run and a catalog, with the ingress counters on or off.
func ingressRuleset(t *testing.T, ingress, unrestricted bool, gateway string) (*kernel, *Firewall, []string) {
	t.Helper()
	k := newKernel()
	var scripts []string
	f := newFirewall(nil, func(context.Context, string) ([]netip.Addr, error) { return nil, nil }, func(s string) error {
		scripts = append(scripts, s)
		return k.nft(s)
	}, k.list)
	f.acct.ifaceSubnets = func() []netip.Prefix { return nil }
	f.acct.ingress = ingress
	if err := f.setup(); err != nil {
		t.Fatal(err)
	}
	if err := f.SetClasses(catalog("v1")); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr(gateway), unrestricted, []spec.EgressRule{{CIDR: "52.1.0.0/16"}}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Remove(runIface) })
	return k, f, scripts
}

func TestIngressCountersAheadOfFilters(t *testing.T) {
	k, _, scripts := ingressRuleset(t, true, false, "10.89.0.1")
	for name, hook := range map[string]string{"count_in": "forward", "count_in_host": "input"} {
		c := k.st.chains[name]
		if c == nil || c.hook != hook || c.prio != -300 {
			t.Errorf("%s: %+v, want a base chain on %s at -300", name, c, hook)
		}
		for other, oc := range k.st.chains {
			if oc.hook == hook && other != name && oc.prio <= -300 {
				t.Errorf("%s (priority %d) runs before %s", other, oc.prio, name)
			}
		}
	}
	// The filtering: egressScript is what Apply sends for the Run's filter
	// whether or not ingress counting is on.
	want := egressScript(runIface, false, []netip.Prefix{netip.MustParsePrefix("52.1.0.0/16")})
	found := false
	for _, s := range scripts {
		found = found || strings.HasPrefix(s, want)
	}
	if !found {
		t.Errorf("Apply did not send the egress script unchanged; scripts: %q", scripts)
	}
}

// filtering is the part of a setup or Apply script before its counting
// part (which starts with the classification sets or the Run's counters).
func filtering(s string) string {
	for _, mark := range []string{"add set inet lux net_", "add counter inet lux lux_"} {
		if i := strings.Index(s, mark); i >= 0 {
			s = s[:i]
		}
	}
	return s
}

// TestIngressCountersChangeNoFiltering: the filtering scripts are
// byte-identical with and without count_in, the filter chains hold the
// same rules, and every probe gets the same verdict.
func TestIngressCountersChangeNoFiltering(t *testing.T) {
	for _, unrestricted := range []bool{false, true} {
		n := 0
		if unrestricted {
			n = 4
		}
		with, _, sw := ingressRuleset(t, true, unrestricted, fmt.Sprintf("10.89.0.%d", n+1))
		without, _, so := ingressRuleset(t, false, unrestricted, fmt.Sprintf("10.89.0.%d", n+3))
		if len(sw) != len(so) {
			t.Fatalf("%d scripts with count_in, %d without", len(sw), len(so))
		}
		for i := range sw {
			if filtering(sw[i]) != filtering(so[i]) {
				t.Errorf("unrestricted=%t script %d: filtering differs:\n%s\n---\n%s", unrestricted, i, filtering(sw[i]), filtering(so[i]))
			}
		}
		if !strings.Contains(sw[0], "hook forward priority -300") || strings.Contains(so[0], "priority -300") {
			t.Fatal("ingress switch has no effect on the setup script")
		}
		for _, c := range []string{"egress", "to_host", "dns", chainName(runIface)} {
			if !slices.EqualFunc(with.rules()[c], without.rules()[c], slices.Equal[[]string]) {
				t.Errorf("unrestricted=%t: chain %s differs with count_in", unrestricted, c)
			}
		}
		for _, p := range probes() {
			if a, b := with.send(p, false), without.send(p, false); a != b {
				t.Errorf("unrestricted=%t %+v: %s with count_in, %s without", unrestricted, p, a, b)
			}
		}
	}
}

// TestRxDeliveredCountsDropped: a packet routed to the Run that a later
// filter drops (netavark's forward chain at priority 0, modelled here,
// drops unsolicited inbound) was delivered by the provider and billed:
// rxDelivered counts it, rx does not.
func TestRxDeliveredCountsDropped(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), false, spec.EgressRule{CIDR: "52.1.0.0/16"})
	k.st.chains["netavark"] = &kchain{hook: "forward", prio: 0, rules: [][]string{
		{"ct", "state", "established,related", "accept"}, {"oifname", `"lux*"`, "drop"}}}
	unsolicited := in("8.8.8.8", 700)
	unsolicited.established = false
	if v := k.send(unsolicited, false); v != "drop" {
		t.Fatalf("unsolicited inbound: %s", v)
	}
	if v := k.send(in("52.1.2.3", 40), false); v != "accept" {
		t.Fatalf("reply: %s", v)
	}
	// The host's own input: a packet to a port the host refuses is still
	// delivered to its interface.
	k.st.chains["host_drop"] = &kchain{hook: "input", prio: 0, rules: [][]string{{"iifname", `"eth0"`, "drop"}}}
	k.send(packet{hook: "input", iif: "eth0", src: "8.8.4.4", dst: "10.0.0.9", proto: "tcp", bytes: 60}, false)
	got, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]proto.NetBytes{"internet": {RxDelivered: 700}, "region-public": {Rx: 40, RxDelivered: 40}}
	if !maps.Equal(got.Runs[runIface], want) {
		t.Errorf("run: got %v, want %v", got.Runs[runIface], want)
	}
	if h := got.Host["internet"]; h.RxDelivered != 60 || h.Rx != 0 {
		t.Errorf("host: %v, want 60 delivered and none accepted", got.Host)
	}
}

func TestClassesUpdateTouchesSetsOnly(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), true)
	k.send(out("52.1.1.1", 40), false)
	before := k.rules()
	nc := catalog("v2")
	nc.RegionPublic = []string{"52.0.0.0/8", "54.0.0.0/8", "2600:1f18::/32"}
	if err := f.SetClasses(nc); err != nil {
		t.Fatal(err)
	}
	if after := k.rules(); !maps.EqualFunc(before, after, func(a, b [][]string) bool {
		return slices.EqualFunc(a, b, slices.Equal[[]string])
	}) {
		t.Error("a classes update within the layout changed chain rules")
	}
	k.send(out("54.1.1.1", 2), false)
	got, _ := f.Counters()
	if b := got.Runs[runIface]["region-public"]; b.Tx != 42 {
		t.Errorf("region-public tx %d, want 40 kept and 2 new", b.Tx)
	}
	// The same version again loads nothing (a failing nft would show it).
	k.failOn = "flush set"
	if err := f.SetClasses(nc); err != nil {
		t.Errorf("same version reloaded: %v", err)
	}
}

func TestLayoutChangeKeepsCounters(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, nil, true)
	k.send(out("8.8.8.8", 11), false)
	filterBefore := k.rules()[chainName(runIface)]
	if err := f.SetClasses(catalog("v1")); err != nil {
		t.Fatal(err)
	}
	k.send(out("34.241.0.1", 5), false)
	k.send(out("8.8.8.8", 6), false)
	got, _ := f.Counters()
	want := map[string]proto.NetBytes{"unknown": {Tx: 11}, "provider:eu-west-1": {Tx: 5}, "internet": {Tx: 6}}
	if !maps.Equal(got.Runs[runIface], want) {
		t.Errorf("got %v, want %v", got.Runs[runIface], want)
	}
	// A region dropped from the catalog: its set goes, its counter stays
	// readable until the Run ends.
	nc := catalog("v2")
	delete(nc.Provider, "eu-west-1")
	if err := f.SetClasses(nc); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.st.sets[regionSet("eu-west-1", false)]; ok {
		t.Error("set of a dropped region kept")
	}
	k.send(out("34.241.0.1", 1), false)
	got, _ = f.Counters()
	if got.Runs[runIface]["provider:eu-west-1"].Tx != 5 || got.Runs[runIface]["internet"].Tx != 7 {
		t.Errorf("after dropping a region: %v", got.Runs[runIface])
	}
	if !slices.EqualFunc(filterBefore, k.rules()[chainName(runIface)], slices.Equal[[]string]) {
		t.Error("a layout change touched the Run's filtering chain")
	}
}

func TestRemoveReadsTheTailAndDeletesCounters(t *testing.T) {
	k := newKernel()
	f, err := k.firewall()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil); err != nil {
		t.Fatal(err)
	}
	k.send(out("8.8.8.8", 123), false)
	final := f.Remove(runIface)
	if final["unknown"].Tx != 123 {
		t.Errorf("final reading %v", final)
	}
	for n := range k.st.counters {
		if strings.Contains(n, runIface) {
			t.Errorf("counter %s outlived its Run", n)
		}
	}
	if f.Remove(runIface) != nil {
		t.Error("a second Remove read counters")
	}
}

func TestReapplyKeepsCounters(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), false, spec.EgressRule{CIDR: "8.8.8.0/24"})
	k.send(out("8.8.8.8", 10), false)
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), false, []spec.EgressRule{{CIDR: "8.8.8.0/24"}}, nil); err != nil {
		t.Fatal(err)
	}
	k.send(out("8.8.8.8", 1), false)
	got, _ := f.Counters()
	if got.Runs[runIface]["internet"].Tx != 11 {
		t.Errorf("re-Apply reset the Run's counters: %v", got.Runs[runIface])
	}
}

func TestRunnerRestartKeepsKernelCounters(t *testing.T) {
	k := newKernel()
	f1 := setupRun(t, k, catalog("v1"), true)
	k.send(out("8.8.8.8", 100), false)
	k.send(out("34.241.0.1", 30), false)
	k.send(packet{hook: "output", oif: "eth0", src: "10.0.0.9", dst: "8.8.8.8", proto: "tcp", bytes: 50}, false)
	const gone = "luxgonegonegone"
	if err := f1.Apply(context.Background(), gone, netip.MustParseAddr("10.89.3.1"), true, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A new runner on the same kernel: the table is replaced.
	f2, err := k.firewall("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if v := k.send(out("8.8.8.8", 1), false); v != "drop" {
		t.Fatalf("a Run whose rules are not loaded yet is open: %s", v)
	}
	// The runner's order: re-adopt (Apply), prune, then luxd's classes in
	// its Welcome.
	if err := f2.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil); err != nil {
		t.Fatal(err)
	}
	f2.PruneCounters()
	if err := f2.SetClasses(catalog("v1")); err != nil {
		t.Fatal(err)
	}
	k.send(out("8.8.8.8", 5), false)
	got, err := f2.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if got.Runs[runIface]["internet"].Tx != 105 || got.Runs[runIface]["provider:eu-west-1"].Tx != 30 {
		t.Errorf("re-adopted Run: %v, want 105 internet and 30 provider:eu-west-1 tx", got.Runs[runIface])
	}
	if got.Host["internet"].Tx != 50 {
		t.Errorf("host after restart: %v", got.Host)
	}
	if !f2.HostCountersKept() {
		t.Error("a restart on the same kernel reported its host counters lost")
	}
	if k.counter(tx(gone, "internet")) != -1 {
		t.Error("counters of a Run not re-adopted survived PruneCounters")
	}
}

func TestRebootStartsFromZero(t *testing.T) {
	k := newKernel()
	setupRun(t, k, catalog("v1"), true)
	k.send(out("8.8.8.8", 100), false)
	f, err := newKernel().firewall()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.Counters()
	if len(got.Runs) != 0 || len(got.Host) != 0 || f.HostCountersKept() {
		t.Errorf("a fresh kernel has counts: %+v kept %t", got, f.HostCountersKept())
	}
}

func TestFailedApplyLeavesNoCountingState(t *testing.T) {
	k := newKernel()
	f, err := k.firewall()
	if err != nil {
		t.Fatal(err)
	}
	k.failOn = "runs_tx"
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil); err == nil {
		t.Fatal("Apply succeeded on a failing nft")
	}
	k.failOn = ""
	if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil); err != nil {
		t.Fatalf("Apply after a failure: %v", err)
	}
	k.send(out("8.8.8.8", 9), false)
	if k.counter(tx(runIface, "unknown")) != 9 {
		t.Error("the retried Apply did not count")
	}
	f.Remove(runIface)
}

func TestFailedLayoutIdenticalRetry(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			k := newKernel()
			f := setupRun(t, k, nil, true)
			k.send(out("8.8.8.8", 11), false)
			if restart {
				if err := f.SetClasses(catalog("old")); err != nil {
					t.Fatal(err)
				}
				k.send(out("34.241.0.1", 7), false)
				var err error
				f, err = k.firewall()
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			k.failOn = "flush chain"
			if err := f.SetClasses(catalog("new")); err == nil {
				t.Fatal("expected rejected transaction")
			}
			k.failOn = ""
			if err := f.SetClasses(catalog("new")); err != nil {
				t.Fatal(err)
			}
			k.send(out("34.241.0.1", 23), false)
			got, err := f.Counters()
			if err != nil {
				t.Fatal(err)
			}
			want := int64(23)
			if restart {
				want += 7
			}
			if got.Runs[runIface]["provider:eu-west-1"].Tx != want || got.Runs[runIface]["unknown"].Tx != 11 {
				t.Fatalf("retry counts: %v", got.Runs[runIface])
			}
		})
	}
}

func TestStubFailureIdenticalRetry(t *testing.T) {
	k := newKernel()
	f, err := k.firewall()
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddr("127.0.0.1")
	occupied, err := net.ListenPacket("udp4", "127.0.0.1:15353")
	if err != nil {
		t.Fatal(err)
	}
	rules := []spec.EgressRule{{CIDR: "8.8.8.0/24"}}
	if err := f.Apply(context.Background(), runIface, addr, false, rules, nil); err == nil {
		t.Fatal("expected stub bind failure")
	}
	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(context.Background(), runIface, addr, false, rules, nil); err != nil {
		t.Fatal(err)
	}
	defer f.Remove(runIface)
	if v := k.send(out("8.8.8.8", 23), false); v != "accept" {
		t.Fatal(v)
	}
	got, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if got.Runs[runIface]["unknown"].Tx != 23 {
		t.Fatal(got)
	}
}

func TestRemoveFailureRetainsRegistry(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, nil, true)
	k.send(out("8.8.8.8", 30), false)
	k.failOn = "delete chain"
	if final := f.Remove(runIface); final["unknown"].Tx != 30 {
		t.Fatalf("failed Remove final: %v, want 30", final)
	}
	k.failOn = ""
	k.send(out("8.8.8.8", 5), false)
	if final := f.Remove(runIface); final["unknown"].Tx != 35 {
		t.Fatalf("retried Remove final: %v, want 35", final)
	}
	if got := k.counter(tx(runIface, "unknown")); got != -1 {
		t.Fatalf("counter remains after Remove: %d", got)
	}
}

func TestRemoveFinalReadDoesNotBlockApply(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, nil, true)
	k.send(out("8.8.8.8", 41), false)
	entered, release := make(chan struct{}), make(chan struct{})
	f.list = func() ([]byte, error) {
		close(entered)
		<-release
		return k.list()
	}
	final := make(chan map[string]proto.NetBytes, 1)
	go func() { final <- f.Remove(runIface) }()
	<-entered
	other := "luxbbbbbbbbbbbb"
	applied := make(chan error, 1)
	go func() {
		applied <- f.Apply(context.Background(), other, netip.MustParseAddr("10.89.1.1"), true, nil, nil)
	}()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Apply blocked behind Remove's final read")
	}
	close(release)
	if got := <-final; got["unknown"].Tx != 41 {
		t.Fatalf("final read: %v, want 41", got)
	}
	f.list = k.list
	f.Remove(other)
}

func TestLifecycleCatalogBarriers(t *testing.T) {
	for _, mode := range []string{"apply", "remove", "remove-failure"} {
		t.Run(mode, func(t *testing.T) {
			removing := mode != "apply"
			k := newKernel()
			f, err := k.firewall()
			if err != nil {
				t.Fatal(err)
			}
			apply := func() error {
				return f.Apply(context.Background(), runIface, netip.MustParseAddr("10.89.0.1"), true, nil, nil)
			}
			if removing {
				if err := apply(); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			setEntered := make(chan struct{}, 1)
			nft := f.nft
			var scriptsMu sync.Mutex
			var scripts []string
			f.nft = func(s string) error {
				scriptsMu.Lock()
				first := len(scripts) == 0
				scripts = append(scripts, s)
				scriptsMu.Unlock()
				if strings.Contains(s, "flush set") {
					setEntered <- struct{}{}
				}
				if first {
					close(entered)
					<-release
					if mode == "remove-failure" {
						return fmt.Errorf("rejected Remove")
					}
				}
				return nft(s)
			}
			done := make(chan error, 1)
			go func() {
				if removing {
					f.Remove(runIface)
					done <- nil
				} else {
					done <- apply()
				}
			}()
			<-entered
			started, updated := make(chan struct{}), make(chan error, 1)
			go func() { close(started); updated <- f.SetClasses(catalog("v1")) }()
			<-started
			interleaved := false
			select {
			case <-setEntered:
				interleaved = true
			case <-time.After(50 * time.Millisecond):
			}
			if interleaved {
				if err := <-updated; err != nil {
					t.Error(err)
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !interleaved {
				if err := <-updated; err != nil {
					t.Fatal(err)
				}
			}
			if f.ClassesVersion() != "v1" {
				t.Fatalf("lifecycle rollback lost committed catalog: %q", f.ClassesVersion())
			}
			if interleaved {
				t.Fatal("SetClasses ran inside held lifecycle transaction")
			}
			scriptsMu.Lock()
			if len(scripts) != 2 || !strings.Contains(scripts[1], "flush set") {
				t.Errorf("transaction order: %v", scripts)
			}
			scriptsMu.Unlock()
			if mode == "remove-failure" {
				f.Remove(runIface)
				if got := k.counter(tx(runIface, "unknown")); got != -1 {
					t.Fatalf("retried Remove left counter: %d", got)
				}
			}
			if removing {
				if err := apply(); err != nil {
					t.Fatal(err)
				}
			}
			if v := k.send(out("34.241.0.1", 23), false); v != "accept" {
				t.Fatal(v)
			}
			got, err := f.Counters()
			if err != nil {
				t.Fatal(err)
			}
			if got.Runs[runIface]["provider:eu-west-1"].Tx != 23 {
				t.Fatal(got)
			}
			f.Remove(runIface)
		})
	}
}

func TestPreservationReadFailureDoesNotReplaceTable(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, nil, true)
	k.send(out("8.8.8.8", 123), false)
	for _, invalid := range []bool{false, true} {
		broken := newFirewall(nil, f.lookup, k.nft, func() ([]byte, error) {
			if invalid {
				return []byte("invalid"), nil
			}
			return nil, fmt.Errorf("read failed")
		})
		if err := broken.setup(); err == nil {
			t.Fatal("read failure replaced table")
		}
		if k.counter(tx(runIface, "unknown")) != 123 {
			t.Fatal("existing bytes destroyed")
		}
	}
}

func TestRemovedRegionRestartFinalAndReadded(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, catalog("v1"), true)
	k.send(out("34.241.0.1", 30), false)
	k.send(packet{hook: "output", oif: "eth0", dst: "34.241.0.1", src: "10.0.0.9", bytes: 40}, false)
	nc := catalog("reduced")
	delete(nc.Provider, "eu-west-1")
	if err := f.SetClasses(nc); err != nil {
		t.Fatal(err)
	}
	f, err := k.firewall()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(context.Background(), runIface, netip.Addr{}, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.PruneCounters()
	if err := f.SetClasses(nc); err != nil {
		t.Fatal(err)
	}
	got, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if got.Host["provider:eu-west-1"].Tx != 40 || got.Runs[runIface]["provider:eu-west-1"].Tx != 30 {
		t.Fatal(got)
	}
	if err := f.SetClasses(catalog("again")); err != nil {
		t.Fatal(err)
	}
	k.send(out("34.241.0.1", 7), false)
	if final := f.Remove(runIface); final["provider:eu-west-1"].Tx != 37 {
		t.Fatal(final)
	}
}

func TestPresentZeroOwnerSnapshot(t *testing.T) {
	k := newKernel()
	f := setupRun(t, k, nil, true)
	got, err := f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := got.Runs[runIface]; !ok || m == nil || len(m) != 0 {
		t.Fatal(got)
	}
	if final := f.Remove(runIface); final == nil || len(final) != 0 {
		t.Fatal(final)
	}
	got, err = f.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Runs[runIface]; ok {
		t.Fatal(got)
	}
}

func TestInvalidRegionRefused(t *testing.T) {
	k := newKernel()
	f, err := k.firewall()
	if err != nil {
		t.Fatal(err)
	}
	nc := catalog("v1")
	nc.Provider["eu-west-1; delete table inet lux"] = []string{"1.0.0.0/8"}
	if err := f.SetClasses(nc); err == nil {
		t.Error("a region that is not a plain name was accepted")
	}
}

func TestDisjoint(t *testing.T) {
	ps := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.0.1.0/24"),
		netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("9.0.0.0/8")}
	got := disjoint(ps)
	want := []netip.Prefix{netip.MustParsePrefix("9.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("2001:db8::/32")}
	if !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}

// A recorded `nft -j list counters table inet lux` answer (nftables 1.0.9).
const recordedCounters = `{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}},
{"counter": {"family": "inet", "name": "lux_host_internet_tx", "table": "lux", "handle": 41, "packets": 12, "bytes": 3400}},
{"counter": {"family": "ip", "name": "lux_host_internet_tx", "table": "lux", "handle": 3, "packets": 1, "bytes": 999}},
{"counter": {"family": "inet", "name": "other", "table": "filter", "handle": 2, "packets": 1, "bytes": 1}}]}`

func TestParseRecordedCounters(t *testing.T) {
	got, err := parseCounters([]byte(recordedCounters))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["lux_host_internet_tx"].Bytes != 3400 || got["lux_host_internet_tx"].Packets != 12 {
		t.Errorf("got %+v", got)
	}
}
