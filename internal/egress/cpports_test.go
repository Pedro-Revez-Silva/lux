package egress

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	cp4 = "10.0.0.5"
	cp6 = "fd00::5"
)

// cpFirewall is a Firewall on an in-memory kernel whose control plane is
// cp4 and cp6, set up as New does.
func cpFirewall(t *testing.T) (*kernel, *Firewall) {
	t.Helper()
	return lookupFirewall(t, func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
		netip.MustParsePrefix(cp4+"/32"), netip.MustParsePrefix(cp6+"/128"))
}

// lookupFirewall is a Firewall on an in-memory kernel that resolves with
// lookup and whose control plane is controlPlane, set up as New does.
func lookupFirewall(t *testing.T, lookup func(context.Context, string) ([]netip.Addr, error), controlPlane ...netip.Prefix) (*kernel, *Firewall) {
	t.Helper()
	k := newKernel()
	f := newFirewall(controlPlane, lookup, k.nft, k.list)
	f.acct.ifaceSubnets = func() []netip.Prefix { return nil }
	if err := f.setup(); err != nil {
		t.Fatal(err)
	}
	return k, f
}

// installRestricted loads a restricted Run's chain on iface and installs
// it as Apply does, without the DNS stub Apply would start on the gateway.
func installRestricted(t *testing.T, f *Firewall, iface string, rules []spec.EgressRule) {
	t.Helper()
	r, err := newRun(false, rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.nft(egressScript(iface, false, r.cidrs)); err != nil {
		t.Fatal(err)
	}
	f.install(context.Background(), iface, r)
}

func tcpTo(iface, dst string, port int) packet {
	src := "10.89.0.2"
	if strings.Contains(dst, ":") {
		src = "fd89::2"
	}
	return packet{hook: "forward", iif: iface, oif: "eth0", src: src, dst: dst, proto: "tcp", dport: port}
}

func TestEgressScriptControlPlaneException(t *testing.T) {
	for _, unrestricted := range []bool{true, false} {
		lines := strings.Split(strings.TrimSpace(egressScript("lux1", unrestricted, nil)), "\n")
		idx := func(sub string) int {
			return slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
		}
		ex4, drop4, drop6 := idx("@cp_ports4 accept"), idx("@blocked4 drop"), idx("@blocked6 drop")
		if ex4 < 0 || drop4 < 0 || drop6 < 0 || ex4 > drop4 || ex4 > drop6 {
			t.Fatalf("unrestricted=%v: exception at %d, drops at %d/%d:\n%s", unrestricted, ex4, drop4, drop6, strings.Join(lines, "\n"))
		}
		if !strings.Contains(lines[ex4], "meta l4proto tcp") {
			t.Errorf("exception is not TCP-only: %s", lines[ex4])
		}
		ex6 := idx("@cp_ports6 accept")
		if unrestricted {
			if ex6 < 0 || ex6 > drop4 || ex6 > drop6 {
				t.Errorf("unrestricted: v6 exception at %d", ex6)
			}
			if strings.Contains(lines[ex4], "@allow_") {
				t.Errorf("unrestricted exception needs an allow set: %s", lines[ex4])
			}
		} else {
			if ex6 >= 0 {
				t.Errorf("restricted Run has a v6 exception: its IPv6 is dropped")
			}
			if !strings.Contains(lines[ex4], "ip daddr @"+setName("lux1")) {
				t.Errorf("restricted exception does not require the Run's allow set: %s", lines[ex4])
			}
			if set := idx("add set inet lux " + setName("lux1")); set < 0 || set > ex4 {
				t.Errorf("allow set declared at %d, after its use at %d", set, ex4)
			}
		}
	}
}

func TestControlPlanePortsUnrestricted(t *testing.T) {
	k, f := cpFirewall(t)
	if err := f.Apply(context.Background(), "luxu", netip.Addr{}, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []packet{tcpTo("luxu", cp4, 8090), tcpTo("luxu", cp6, 8090)} {
		if v := k.send(p, true); v != "drop" {
			t.Errorf("no ports configured: %s:%d %s", p.dst, p.dport, v)
		}
	}
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	for p, v := range map[packet]string{
		tcpTo("luxu", cp4, 8090):           "accept",
		tcpTo("luxu", cp6, 8090):           "accept",
		tcpTo("luxu", cp4, 7070):           "drop",
		tcpTo("luxu", cp6, 7070):           "drop",
		tcpTo("luxu", "127.0.0.1", 8090):   "drop",
		tcpTo("luxu", "169.254.0.1", 8090): "drop",
		tcpTo("luxu", "203.0.113.1", 443):  "accept",
		{hook: "forward", iif: "luxu", oif: "eth0", src: "10.89.0.2", dst: cp4, proto: "udp", dport: 8090}: "drop",
	} {
		if got := k.send(p, true); got != v {
			t.Errorf("%s %s:%d: %s, want %s", p.proto, p.dst, p.dport, got, v)
		}
	}
}

func TestControlPlanePortsRestricted(t *testing.T) {
	k, f := cpFirewall(t)
	for iface, rules := range map[string][]spec.EgressRule{
		"luxnone": nil,
		"luxcidr": {{CIDR: cp4 + "/32"}},
		"luxwide": {{CIDR: "10.0.0.0/8"}},
	} {
		installRestricted(t, f, iface, rules)
	}
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    packet
		want string
	}{
		// Without the address in its own egress, a Run gets no exception.
		{tcpTo("luxnone", cp4, 8090), "drop"},
		{tcpTo("luxcidr", cp4, 8090), "accept"},
		{tcpTo("luxcidr", cp4, 7070), "drop"},
		{tcpTo("luxcidr", cp6, 8090), "drop"},
		{tcpTo("luxwide", cp4, 8090), "accept"},
		{tcpTo("luxwide", cp4, 22), "drop"},
		{tcpTo("luxwide", "10.0.0.6", 22), "accept"},
	} {
		if got := k.send(c.p, true); got != c.want {
			t.Errorf("%s -> %s:%d: %s, want %s", c.p.iif, c.p.dst, c.p.dport, got, c.want)
		}
	}
}

// Changing the list rebuilds the shared pairs: Runs already running see
// the new list, and no pair of the old one remains.
func TestControlPlanePortsReplaced(t *testing.T) {
	k, f := cpFirewall(t)
	if err := f.Apply(context.Background(), "luxu", netip.Addr{}, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.SetControlPlanePorts([]int{9000, 8090, 8090}); err != nil {
		t.Fatal(err)
	}
	pairs := func() []string {
		var out []string
		for _, s := range []string{"cp_ports4", "cp_ports6"} {
			for e := range k.st.sets[s].pairs {
				out = append(out, e)
			}
		}
		slices.Sort(out)
		return out
	}
	if got, want := pairs(), []string{"10.0.0.5 . 8090", "10.0.0.5 . 9000", "fd00::5 . 8090", "fd00::5 . 9000"}; !slices.Equal(got, want) {
		t.Fatalf("pairs %v, want %v", got, want)
	}
	if _, err := f.SetControlPlanePorts([]int{9000}); err != nil {
		t.Fatal(err)
	}
	if got, want := pairs(), []string{"10.0.0.5 . 9000", "fd00::5 . 9000"}; !slices.Equal(got, want) {
		t.Fatalf("pairs %v, want %v", got, want)
	}
	if v := k.send(tcpTo("luxu", cp4, 8090), true); v != "drop" {
		t.Errorf("removed port still open: %s", v)
	}
	if v := k.send(tcpTo("luxu", cp4, 9000), true); v != "accept" {
		t.Errorf("kept port: %s", v)
	}
	if _, err := f.SetControlPlanePorts(nil); err != nil {
		t.Fatal(err)
	}
	if got := pairs(); len(got) != 0 {
		t.Fatalf("pairs after clearing: %v", got)
	}
	if v := k.send(tcpTo("luxu", cp4, 9000), true); v != "drop" {
		t.Errorf("cleared list: %s", v)
	}
	// A restarted runner's setup reloads the list in force.
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	if err := f.setup(); err != nil {
		t.Fatal(err)
	}
	if got := pairs(); len(got) != 2 {
		t.Errorf("pairs after setup: %v", got)
	}
}

func TestControlPlanePortsFailedTransactionKeepsList(t *testing.T) {
	k, f := cpFirewall(t)
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	k.failOn = "cp_ports4"
	if _, err := f.SetControlPlanePorts([]int{9000}); err == nil {
		t.Fatal("no error")
	}
	if got := f.ControlPlanePorts(); !slices.Equal(got, []int{8090}) {
		t.Errorf("list after a failed change: %v", got)
	}
}

// ports is n distinct ports from 1000.
func ports(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = 1000 + i
	}
	return out
}

// Exactly MaxControlPlanePorts apply, one pair per port and address; one
// more, or a port outside 1-65535, refuses the list and keeps the old one.
func TestControlPlanePortsLimits(t *testing.T) {
	k, f := cpFirewall(t)
	changed, err := f.SetControlPlanePorts(ports(proto.MaxControlPlanePorts))
	if err != nil || !changed {
		t.Fatalf("%d ports: changed %v, %v", proto.MaxControlPlanePorts, changed, err)
	}
	if n := len(k.st.sets["cp_ports4"].pairs) + len(k.st.sets["cp_ports6"].pairs); n != 2*proto.MaxControlPlanePorts {
		t.Errorf("%d pairs, want %d", n, 2*proto.MaxControlPlanePorts)
	}
	if changed, err := f.SetControlPlanePorts(ports(proto.MaxControlPlanePorts)); err != nil || changed {
		t.Errorf("same list again: changed %v, %v", changed, err)
	}
	for _, bad := range [][]int{ports(proto.MaxControlPlanePorts + 1), {0, 8090}, {8090, 70000}, {-1}} {
		if changed, err := f.SetControlPlanePorts(bad); err == nil || changed {
			t.Errorf("%v: changed %v, %v", bad, changed, err)
		}
	}
	if got := f.ControlPlanePorts(); !slices.Equal(got, ports(proto.MaxControlPlanePorts)) {
		t.Errorf("list after refusals: %v", got)
	}
	if _, err := f.SetControlPlanePorts([]int{8090, 8090}); err != nil {
		t.Fatal(err)
	}
	if got := f.ControlPlanePorts(); !slices.Equal(got, []int{8090}) {
		t.Errorf("duplicates: %v", got)
	}
}

// A restricted Run whose host: rule resolves to the control-plane address
// reaches the exempt port there and nothing else.
func TestControlPlanePortsRestrictedByHostname(t *testing.T) {
	k, f := lookupFirewall(t, func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "cache.example.com" {
			return []netip.Addr{netip.MustParseAddr(cp4)}, nil
		}
		return nil, nil
	}, netip.MustParsePrefix(cp4+"/32"))
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	installRestricted(t, f, "luxhost", []spec.EgressRule{{Host: "cache.example.com"}})
	for _, c := range []struct {
		port int
		want string
	}{{8090, "accept"}, {7070, "drop"}} {
		if got := k.send(tcpTo("luxhost", cp4, c.port), true); got != c.want {
			t.Errorf("%s:%d: %s, want %s", cp4, c.port, got, c.want)
		}
	}
}

// A control-plane address enters a restricted Run's set (and the stub's
// answers) only while some port is exempt; a loopback or link-local
// control-plane address never does.
func TestControlPlaneAddressAdmittedOnlyWithPorts(t *testing.T) {
	cp := netip.MustParseAddr("10.9.0.5")
	f, n, add := fixture(t, netip.PrefixFrom(cp, 32), netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("169.254.7.7/32"))
	n.addr["cp.example.com"] = cp
	n.addr["lo.example.com"] = netip.MustParseAddr("127.0.0.1")
	n.addr["ll.example.com"] = netip.MustParseAddr("169.254.7.7")
	add("lux1", false, spec.EgressRule{Host: "cp.example.com"}, spec.EgressRule{Host: "lo.example.com"}, spec.EgressRule{Host: "ll.example.com"})
	if rc, addrs := ask(t, f, "lux1", "cp.example.com"); rc != dnsmessage.RCodeNameError || addrs != nil || n.inSet("lux1", cp) {
		t.Fatalf("no ports: %v %v", rc, addrs)
	}
	if _, err := f.SetControlPlanePorts([]int{8090}); err != nil {
		t.Fatal(err)
	}
	if rc, addrs := ask(t, f, "lux1", "cp.example.com"); rc != dnsmessage.RCodeSuccess || !slices.Equal(addrs, []netip.Addr{cp}) || !n.inSet("lux1", cp) {
		t.Fatalf("with ports: %v %v", rc, addrs)
	}
	for _, name := range []string{"lo.example.com", "ll.example.com"} {
		if rc, addrs := ask(t, f, "lux1", name); rc != dnsmessage.RCodeNameError || addrs != nil || n.inSet("lux1", n.addr[name]) {
			t.Errorf("%s: %v %v", name, rc, addrs)
		}
	}
	f.mu.Lock()
	for _, ip := range []string{"127.0.0.1", "169.254.7.7"} {
		if !f.isBlocked(netip.MustParseAddr(ip)) {
			t.Errorf("%s not blocked with ports configured", ip)
		}
	}
	elems := f.pairsElements()
	f.mu.Unlock()
	if strings.Contains(elems, "127.0.0.1") || strings.Contains(elems, "169.254.") {
		t.Errorf("an always-blocked address got pairs: %s", elems)
	}
	if _, err := f.SetControlPlanePorts(nil); err != nil {
		t.Fatal(err)
	}
	if rc, addrs := ask(t, f, "lux1", "cp.example.com"); rc != dnsmessage.RCodeNameError || addrs != nil {
		t.Errorf("ports cleared, still answered: %v %v", rc, addrs)
	}
}
