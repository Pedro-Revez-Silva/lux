package egress

// Network transfer accounting. A provider bills bytes by where they go
// (same zone, another zone, its own public addresses, its free services,
// another region, the internet) and by direction, so the table also counts
// bytes per billing class (proto.NetClass), in chains that only count:
//
//   - classification sets net_<class>4 and net_<class>6 (interval sets),
//     shared by every Run and the host, filled from the NetworkClasses luxd
//     sends (Firewall.SetClasses); per other region net_p_<region>4/6, and
//     net_provider4/6, their union, which gates the per-region rules so
//     traffic to the internet is not tested against every region.
//   - per Run, named counters lux_<iface>_<key>_{tx,rx,rxd} (key a
//     proto.NetKey, '-' and ':' as '_'), and chains tx_<iface> classifying
//     by destination and rx_<iface> and rxd_<iface> by source (txp_/rxp_/
//     rxdp_ per region). The base chain count (forward hook, priority 100,
//     policy accept) sends packets from the Run's bridge to the first and
//     packets routed to it to the second.
//     Running after the egress chain and netavark's (priority 0) it sees
//     accepted L3 traffic after those known filters, not invoice-complete
//     transfer. Later hooks can drop outbound; locally dropped inbound may
//     already be provider-billed. Its only verdicts are accept and
//     jumps into these chains, so it can never change what is filtered: an
//     accept ends this base chain only. Replies count both ways: providers
//     bill them. Only the forward path counts: a Run's traffic to its own
//     host (its DNS stub, the gateway) never crosses the network.
//     Unrestricted Runs are counted the same way.
//   - count_in (forward hook, priority -300, ahead of every filter) sends
//     packets routed to a Run's bridge to rxd_<iface>: inbound as the
//     provider delivered it, dropped or not (proto.NetBytes.RxDelivered,
//     what transfer is priced by). Same verdict rule as count.
//   - the host's own traffic (the runner's link to luxd, pulls, fetches,
//     uploads) in host_out and host_in, base chains on the output and
//     input hooks at priority 100, into lux_host_<key>_{tx,rx}, and in
//     count_in_host (input, -300) into lux_host_<key>_rxd. Loopback
//     and container bridges are skipped: those bytes stay on the host, and
//     a Run's forwarded bytes are its own counters'.
//
// Precedence is proto.NetClasses' order, by rule order (lists may
// overlap across classes). What no list holds is internet when luxd sent a
// provider catalog and unknown when it did not; both counters exist, the
// last rule picks one.
//
// A classes update that keeps the layout (the set of regions, and whether
// there is a catalog) flushes and refills the sets in one transaction;
// chains and counters are untouched. A layout change also replaces the
// counting chains' rules, in the same transaction; counters are named
// objects and keep their values.
//
// Counters live in the kernel and outlast a runner restart; New reads
// those of the table it replaces and recreates them with their values, so
// a re-adopted Run counts on from where it was. A reboot loses them.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/marcioapm/lux/internal/proto"
)

// hostLocalIfaces are interfaces whose traffic never leaves the host: not
// network transfer. "lux*" are Runs' bridges, counted per Run.
var hostLocalIfaces = []string{"lo", "lux*", "podman*", "cni-*", "docker*", "virbr*"}

// singleClasses have one set per family, in precedence order; provider
// (per region) goes between region-public and provider-global.
var (
	beforeProvider = []proto.NetClass{proto.NetLocal, proto.NetZone, proto.NetRegionService, proto.NetRegionPublic}
	afterProvider  = []proto.NetClass{proto.NetProviderGlobalFree, proto.NetProviderGlobal}
)

// Counter directions: tx and rx after the filters, rxd (RxDelivered)
// inbound ahead of them.
var counterDirs = []string{"tx", "rx", "rxd"}

func nftName(s string) string { return strings.NewReplacer("-", "_", ":", "_", ".", "_").Replace(s) }

func classSet(c proto.NetClass, v6 bool) string { return "net_" + nftName(string(c)) + family(v6) }
func regionSet(region string, v6 bool) string   { return "net_p_" + nftName(region) + family(v6) }

func family(v6 bool) string {
	if v6 {
		return "6"
	}
	return "4"
}

func counterName(owner, key, dir string) string {
	return "lux_" + owner + "_" + nftName(key) + "_" + dir
}

// counterRef is what a counter counts.
type counterRef struct{ owner, key, dir string }

// layout is what the counting chains' rules depend on besides the sets'
// contents.
type layout struct {
	regions []string // sorted
	catalog bool
}

func (l layout) equal(o layout) bool {
	return l.catalog == o.catalog && slices.Equal(l.regions, o.regions)
}

func (l layout) unlisted() proto.NetClass {
	if l.catalog {
		return proto.NetInternet
	}
	return proto.NetUnknown
}

// keys are every counter key the layout counts into.
func (l layout) keys() []string {
	var out []string
	for _, c := range beforeProvider {
		out = append(out, string(c))
	}
	for _, r := range l.regions {
		out = append(out, proto.NetKey(proto.NetProvider, r))
	}
	for _, c := range afterProvider {
		out = append(out, string(c))
	}
	return append(out, string(proto.NetInternet), string(proto.NetUnknown))
}

// accounting is the Firewall's counting state; guarded by Firewall.mu.
type accounting struct {
	layout  layout
	version string
	loaded  bool                  // SetClasses has run once
	counted map[string]bool       // Run ifaces with counters and chains
	names   map[string]counterRef // every counter this Firewall made
	// kept are counters of the table New replaced, recreated with their
	// values, that no owner has taken up yet.
	kept         map[string]keptCounter
	hostKept     bool
	ifaceSubnets func() []netip.Prefix
	// ingress: count_in and count_in_host exist. Always on outside the
	// tests that prove they change no filtering.
	ingress bool
}

type keptCounter struct{ packets, bytes int64 }

// transact runs the script build makes, letting build change a copy of
// f.acct that is kept only if nft accepts the script. Call with opMu and
// mu held.
func (f *Firewall) transact(build func() string) error {
	old := f.acct
	f.acct.counted = maps.Clone(old.counted)
	f.acct.names = maps.Clone(old.names)
	f.acct.kept = maps.Clone(old.kept)
	if err := f.nft(build()); err != nil {
		f.acct = old
		return err
	}
	return nil
}

func newAccounting() accounting {
	return accounting{counted: map[string]bool{}, names: map[string]counterRef{}, kept: map[string]keptCounter{},
		ifaceSubnets: interfaceSubnets, ingress: true}
}

// classLists are NetworkClasses parsed and masked, each list without
// prefixes another one in it covers (an interval set refuses overlapping
// elements).
type classLists struct {
	single   map[proto.NetClass][]netip.Prefix
	regions  map[string][]netip.Prefix
	provider []netip.Prefix // every region's
	layout   layout
}

func parsePrefixes(c string, list []string) ([]netip.Prefix, error) {
	ps := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("network class %s: %w", c, err)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		ps = append(ps, p.Masked())
	}
	return disjoint(ps), nil
}

// effective are the lists the sets hold for nc. With neither Local nor
// Zone, or provisional classes (a Zone but no Local), the host's interface
// subnets are Local, read again on every call. Declared zone-only classes
// (a static host's) keep Local empty.
func (f *Firewall) effective(nc *proto.NetworkClasses) (classLists, error) {
	cl := classLists{single: map[proto.NetClass][]netip.Prefix{}, regions: map[string][]netip.Prefix{}}
	var in proto.NetworkClasses
	if nc != nil {
		in = *nc
	}
	for c, list := range map[proto.NetClass][]string{
		proto.NetLocal: in.Local, proto.NetZone: in.Zone, proto.NetRegionService: in.RegionService,
		proto.NetRegionPublic: in.RegionPublic, proto.NetProviderGlobal: in.ProviderGlobal,
		proto.NetProviderGlobalFree: in.ProviderGlobalFree,
	} {
		ps, err := parsePrefixes(string(c), list)
		if err != nil {
			return cl, err
		}
		cl.single[c] = ps
	}
	if len(cl.single[proto.NetLocal]) == 0 && (len(cl.single[proto.NetZone]) == 0 || in.Provisional) {
		cl.single[proto.NetLocal] = disjoint(f.acct.ifaceSubnets())
	}
	cl.layout.catalog = nc.Catalog()
	if cl.layout.catalog {
		var all []netip.Prefix
		for r, list := range in.Provider {
			// A region becomes part of set and counter names.
			if !proto.ValidNetworkRegion(r) {
				return cl, fmt.Errorf("network class provider: region %q", r)
			}
			ps, err := parsePrefixes(proto.NetKey(proto.NetProvider, r), list)
			if err != nil {
				return cl, err
			}
			cl.regions[r] = ps
			cl.layout.regions = append(cl.layout.regions, r)
			all = append(all, ps...)
		}
		slices.Sort(cl.layout.regions)
		cl.provider = disjoint(all)
	}
	return cl, nil
}

// disjoint is ps without duplicates and without prefixes inside another.
func disjoint(ps []netip.Prefix) []netip.Prefix {
	ps = slices.Clone(ps)
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if a.Addr().Is4() != b.Addr().Is4() {
			if a.Addr().Is4() {
				return -1
			}
			return 1
		}
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	var out []netip.Prefix
	for _, p := range ps {
		// Sorted by address then length: a covering prefix starts at or
		// before p, and the last kept one is the only candidate.
		if n := len(out); n > 0 && out[n-1].Bits() <= p.Bits() && out[n-1].Contains(p.Addr()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// interfaceSubnets are the subnets of this host's own addresses, but
// loopback, link-local and container bridges'.
func interfaceSubnets() []netip.Prefix {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 || slices.ContainsFunc(hostLocalIfaces, func(g string) bool { return globMatch(g, ifc.Name) }) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.IsLinkLocalUnicast() || ip.IsLoopback() {
				continue
			}
			ones, bits := ipn.Mask.Size()
			if ip.Is4() && bits == 128 {
				ones -= 96
			}
			out = append(out, netip.PrefixFrom(ip, ones).Masked())
		}
	}
	return out
}

// globMatch matches nftables' interface wildcards: a trailing '*'.
func globMatch(glob, name string) bool {
	if p, ok := strings.CutSuffix(glob, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return glob == name
}

// setsScript declares every classification set (a no-op for one that
// exists), empties it and fills it from cl.
func setsScript(cl classLists) string {
	var b strings.Builder
	fill := func(name string, v6 bool, ps []netip.Prefix) {
		fmt.Fprintf(&b, "add set inet lux %s { type ipv%s_addr; flags interval; }\nflush set inet lux %s\n", name, family(v6), name)
		var elems []string
		for _, p := range ps {
			if p.Addr().Is6() == v6 {
				elems = append(elems, p.String())
			}
		}
		// Bounded lines keep each netlink message under nft's buffer.
		for chunk := range slices.Chunk(elems, 2000) {
			fmt.Fprintf(&b, "add element inet lux %s { %s }\n", name, strings.Join(chunk, ", "))
		}
	}
	for _, v6 := range []bool{false, true} {
		for _, c := range append(slices.Clone(beforeProvider), afterProvider...) {
			fill(classSet(c, v6), v6, cl.single[c])
		}
		fill(classSet(proto.NetProvider, v6), v6, cl.provider)
		for _, r := range cl.layout.regions {
			fill(regionSet(r, v6), v6, cl.regions[r])
		}
	}
	return b.String()
}

// classifyRules fill a counting chain and its per-region chain: one rule
// per class and family matching the peer (field: daddr for tx, saddr for
// rx) into owner's counter, then the fall-through counter.
func classifyRules(l layout, chain, regionChain, owner, field, dir string) string {
	var b strings.Builder
	match := func(ch, set4, set6, action string) {
		fmt.Fprintf(&b, "add rule inet lux %s ip %s @%s %s\n", ch, field, set4, action)
		fmt.Fprintf(&b, "add rule inet lux %s ip6 %s @%s %s\n", ch, field, set6, action)
	}
	count := func(key string) string { return "counter name " + counterName(owner, key, dir) + " accept" }
	for _, c := range beforeProvider {
		match(chain, classSet(c, false), classSet(c, true), count(string(c)))
	}
	match(chain, classSet(proto.NetProvider, false), classSet(proto.NetProvider, true), "jump "+regionChain)
	for _, r := range l.regions {
		match(regionChain, regionSet(r, false), regionSet(r, true), count(proto.NetKey(proto.NetProvider, r)))
	}
	for _, c := range afterProvider {
		match(chain, classSet(c, false), classSet(c, true), count(string(c)))
	}
	fmt.Fprintf(&b, "add rule inet lux %s %s\n", chain, count(string(l.unlisted())))
	return b.String()
}

// countersScript declares owner's counters for the current layout but
// those it already has, and those kept from the replaced table (re-adding
// a counter that exists is not guaranteed to leave its value). Call with
// f.mu held.
func (f *Firewall) countersScript(owner string) string {
	var b strings.Builder
	prefix := "lux_" + owner + "_"
	for n := range f.acct.kept {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		key, dir, ok := cutDir(strings.TrimPrefix(n, prefix))
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "provider_") && key != "provider_global" && key != "provider_global_free" {
			key = "provider:" + strings.ReplaceAll(strings.TrimPrefix(key, "provider_"), "_", "-")
		} else {
			key = strings.ReplaceAll(key, "_", "-")
		}
		f.acct.names[n] = counterRef{owner, key, dir}
		delete(f.acct.kept, n)
	}
	for _, k := range f.acct.layout.keys() {
		for _, dir := range counterDirs {
			n := counterName(owner, k, dir)
			if _, ok := f.acct.names[n]; ok {
				continue
			}
			f.acct.names[n] = counterRef{owner, k, dir}
			if _, ok := f.acct.kept[n]; ok {
				delete(f.acct.kept, n)
				continue
			}
			fmt.Fprintf(&b, "add counter inet lux %s\n", n)
		}
	}
	return b.String()
}

// cutDir splits a counter name's "<key>_<dir>" tail; ok only if the first
// "_<dir>" in s ends it.
func cutDir(s string) (key, dir string, ok bool) {
	for _, d := range counterDirs {
		if k, rest, found := strings.Cut(s, "_"+d); found && rest == "" {
			return k, d, true
		}
	}
	return "", "", false
}

// runCounts are a Run's counting directions: chain <dir>_<iface> (and
// <dir>p_<iface> per region), reached through map runs_<dir>, classifying
// by addr.
var runCounts = []struct{ dir, addr string }{{"tx", "daddr"}, {"rx", "saddr"}, {"rxd", "saddr"}}

// runChainNames are a Run's counting chains and their per-region parts.
func runChainNames(iface string) []string {
	var out []string
	for _, c := range runCounts {
		out = append(out, c.dir+"_"+iface, c.dir+"p_"+iface)
	}
	return out
}

// runChains are a Run's counting chains' rules (the chains exist).
func runChains(l layout, iface string) string {
	var b strings.Builder
	for _, c := range runCounts {
		b.WriteString(classifyRules(l, c.dir+"_"+iface, c.dir+"p_"+iface, iface, c.addr, c.dir))
	}
	return b.String()
}

// runCountingScript makes a Run's counters and chains and the map entries
// reaching them; "" if it has them (a re-applied Run keeps its counts).
func (f *Firewall) runCountingScript(iface string) string {
	if f.acct.counted[iface] {
		return ""
	}
	f.acct.counted[iface] = true
	var b strings.Builder
	b.WriteString(f.countersScript(iface))
	for _, c := range runChainNames(iface) {
		fmt.Fprintf(&b, "add chain inet lux %s\n", c)
	}
	b.WriteString(runChains(f.acct.layout, iface))
	for _, c := range runCounts {
		fmt.Fprintf(&b, "add element inet lux runs_%s { %q : jump %s_%s }\n", c.dir, iface, c.dir, iface)
	}
	return b.String()
}

// hostChains are host_out's, host_in's and count_in_host's rules (the
// chains exist).
func hostChains(l layout) string {
	var b strings.Builder
	for _, g := range hostLocalIfaces {
		fmt.Fprintf(&b, "add rule inet lux host_out oifname %q accept\n", g)
		fmt.Fprintf(&b, "add rule inet lux host_in iifname %q accept\n", g)
		fmt.Fprintf(&b, "add rule inet lux count_in_host iifname %q accept\n", g)
	}
	b.WriteString(classifyRules(l, "host_out", "host_outp", "host", "daddr", "tx"))
	b.WriteString(classifyRules(l, "host_in", "host_inp", "host", "saddr", "rx"))
	b.WriteString(classifyRules(l, "count_in_host", "count_in_hostp", "host", "saddr", "rxd"))
	return b.String()
}

// hostChainNames are every host counting chain, flushed on a layout change.
var hostChainNames = []string{"host_out", "host_outp", "host_in", "host_inp", "count_in_host", "count_in_hostp"}

// countingSetup is New's part for accounting, in its transaction: kept
// counters, the sets (with no classes from luxd yet: interface subnets
// Local, the rest unknown), the count chains and the host's counting.
//
// count_in (forward) and count_in_host (input) run at priority -300,
// before lux's filters (-10), netavark's (0) and any distribution firewall
// at the usual 0: what they count is inbound as the provider delivered it,
// including what a filter then drops, since the provider bills those
// bytes too. Like count, their only verdicts are accept and jumps.
func (f *Firewall) countingSetup() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(f.acct.kept)) {
		k := f.acct.kept[n]
		fmt.Fprintf(&b, "add counter inet lux %s { packets %d bytes %d }\n", n, k.packets, k.bytes)
	}
	cl, _ := f.effective(nil)
	f.acct.layout = cl.layout
	b.WriteString(setsScript(cl))
	b.WriteString("add map inet lux runs_tx { type ifname : verdict; }\n")
	b.WriteString("add map inet lux runs_rx { type ifname : verdict; }\n")
	b.WriteString("add map inet lux runs_rxd { type ifname : verdict; }\n")
	b.WriteString("add chain inet lux count { type filter hook forward priority 100; policy accept; }\n")
	b.WriteString("add chain inet lux count_in" + ingressHook(f.acct.ingress, "forward") + "\n")
	// A Run's packets to or from another bridge on this host (another
	// container network) never cross the network.
	for _, g := range hostLocalIfaces {
		fmt.Fprintf(&b, "add rule inet lux count iifname \"lux*\" oifname %q accept\n", g)
		fmt.Fprintf(&b, "add rule inet lux count oifname \"lux*\" iifname %q accept\n", g)
		fmt.Fprintf(&b, "add rule inet lux count_in oifname \"lux*\" iifname %q accept\n", g)
	}
	b.WriteString("add rule inet lux count iifname vmap @runs_tx\n")
	b.WriteString("add rule inet lux count oifname vmap @runs_rx\n")
	b.WriteString("add rule inet lux count_in oifname vmap @runs_rxd\n")
	b.WriteString(f.countersScript("host"))
	b.WriteString("add chain inet lux host_out { type filter hook output priority 100; policy accept; }\n")
	b.WriteString("add chain inet lux host_in { type filter hook input priority 100; policy accept; }\n")
	b.WriteString("add chain inet lux count_in_host" + ingressHook(f.acct.ingress, "input") + "\n")
	b.WriteString("add chain inet lux host_outp\nadd chain inet lux host_inp\nadd chain inet lux count_in_hostp\n")
	b.WriteString(hostChains(f.acct.layout))
	return b.String()
}

// ingressHook declares an ingress counting chain as a base chain on hook
// at -300 or, when off, as a regular chain no packet reaches.
func ingressHook(on bool, hook string) string {
	if !on {
		return ""
	}
	return " { type filter hook " + hook + " priority -300; policy accept; }"
}

// SetClasses replaces the classification lists (see the file comment for
// what one transaction changes). The same version again does nothing.
func (f *Firewall) SetClasses(nc *proto.NetworkClasses) error {
	version := ""
	if nc != nil {
		version = nc.Version
	}
	f.opMu.Lock()
	defer f.opMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acct.loaded && version == f.acct.version {
		return nil
	}
	cl, err := f.effective(nc)
	if err != nil {
		return err
	}
	if err := f.transact(func() string { return f.classesScript(cl) }); err != nil {
		return fmt.Errorf("nftables network classes: %w", err)
	}
	f.acct.version, f.acct.loaded = version, true
	return nil
}

// classesScript loads cl into the sets and, on a layout change, makes the
// new layout's counters and replaces every counting chain's rules. Call
// with f.mu held.
func (f *Firewall) classesScript(cl classLists) string {
	old := f.acct.layout
	if cl.layout.equal(old) {
		return setsScript(cl)
	}
	// Counters for the new layout before the rules naming them; then
	// every counting chain's rules replaced.
	var b strings.Builder
	f.acct.layout = cl.layout
	b.WriteString(f.countersScript("host"))
	ifaces := slices.Sorted(maps.Keys(f.acct.counted))
	for _, iface := range ifaces {
		b.WriteString(f.countersScript(iface))
	}
	b.WriteString(setsScript(cl))
	for _, c := range hostChainNames {
		fmt.Fprintf(&b, "flush chain inet lux %s\n", c)
	}
	b.WriteString(hostChains(cl.layout))
	for _, iface := range ifaces {
		for _, c := range runChainNames(iface) {
			fmt.Fprintf(&b, "flush chain inet lux %s\n", c)
		}
		b.WriteString(runChains(cl.layout, iface))
	}
	// Sets of regions no longer listed: no rule names them now.
	for _, r := range old.regions {
		if !slices.Contains(cl.layout.regions, r) {
			fmt.Fprintf(&b, "delete set inet lux %s\ndelete set inet lux %s\n", regionSet(r, false), regionSet(r, true))
		}
	}
	return b.String()
}

// HostCountersKept: New found the host's counters in the table it replaced
// (the runner restarted, the kernel did not), so they still count every
// byte since the host booted. False after a reboot: they start from zero.
func (f *Firewall) HostCountersKept() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acct.hostKept
}

// ClassesVersion is the version of the lists the sets hold.
func (f *Firewall) ClassesVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acct.version
}

// removeCountingScript deletes what runCountingScript (and layout changes)
// made for iface, map entries first. Call with f.mu held.
func (f *Firewall) removeCountingScript(iface string) string {
	var b strings.Builder
	for _, c := range runCounts {
		fmt.Fprintf(&b, "delete element inet lux runs_%s { %q }\n", c.dir, iface)
	}
	for _, c := range runChainNames(iface) {
		fmt.Fprintf(&b, "delete chain inet lux %s\n", c)
	}
	for _, n := range slices.Sorted(maps.Keys(f.acct.names)) {
		if f.acct.names[n].owner == iface {
			fmt.Fprintf(&b, "delete counter inet lux %s\n", n)
			delete(f.acct.names, n)
		}
	}
	return b.String()
}

// Counts are bytes per proto.NetKey, per Run bridge interface and for the
// host, as the kernel counters hold them now. Keys with no bytes either
// way are left out.
type Counts struct {
	Runs map[string]map[string]proto.NetBytes
	Host map[string]proto.NetBytes
}

type nftCounter struct {
	Name    string `json:"name"`
	Table   string `json:"table"`
	Family  string `json:"family"`
	Packets int64  `json:"packets"`
	Bytes   int64  `json:"bytes"`
}

// parseCounters reads `nft -j list counters table inet lux`: the table's
// counters by name.
func parseCounters(raw []byte) (map[string]nftCounter, error) {
	var doc struct {
		Nftables []struct {
			Counter *nftCounter `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("nft counters: %w", err)
	}
	out := map[string]nftCounter{}
	for _, o := range doc.Nftables {
		if c := o.Counter; c != nil && c.Table == "lux" && c.Family == "inet" {
			out[c.Name] = *c
		}
	}
	return out, nil
}

// Counters reads every counter in one nft call.
func (f *Firewall) Counters() (Counts, error) {
	raw, err := f.list()
	if err != nil {
		return Counts{}, err
	}
	byName, err := parseCounters(raw)
	if err != nil {
		return Counts{}, err
	}
	out := Counts{Runs: map[string]map[string]proto.NetBytes{}, Host: map[string]proto.NetBytes{}}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, c := range byName {
		ref, ok := f.acct.names[name]
		if !ok {
			continue
		}
		m := out.Host
		if ref.owner != "host" {
			if out.Runs[ref.owner] == nil {
				out.Runs[ref.owner] = map[string]proto.NetBytes{}
			}
			m = out.Runs[ref.owner]
		}
		if c.Bytes == 0 {
			continue
		}
		nb := m[ref.key]
		switch ref.dir {
		case "tx":
			nb.Tx = c.Bytes
		case "rx":
			nb.Rx = c.Bytes
		case "rxd":
			nb.RxDelivered = c.Bytes
		}
		m[ref.key] = nb
	}
	return out, nil
}

// keepCounters reads the counters of the table New is about to replace,
// for countingSetup to recreate. No table (a first start, a reboot) keeps
// none.
func (f *Firewall) keepCounters() error {
	raw, err := f.list()
	if err != nil {
		return err
	}
	byName, err := parseCounters(raw)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, c := range byName {
		if strings.HasPrefix(n, "lux_") {
			f.acct.kept[n] = keptCounter{c.Packets, c.Bytes}
			f.acct.hostKept = f.acct.hostKept || strings.HasPrefix(n, "lux_host_")
		}
	}
	return nil
}

// PruneCounters deletes the counters kept from before a restart whose Run
// was not re-applied (call it once re-adoption is done). A re-applied Run's
// and the host's stay kept even if the current layout does not name them
// yet: the provider:<region> counters are taken up when luxd's classes
// arrive, after re-adoption.
func (f *Firewall) PruneCounters() {
	f.mu.Lock()
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(f.acct.kept)) {
		owner, _, _ := strings.Cut(strings.TrimPrefix(n, "lux_"), "_")
		if owner == "host" || f.acct.counted[owner] {
			continue
		}
		fmt.Fprintf(&b, "delete counter inet lux %s\n", n)
		delete(f.acct.kept, n)
	}
	f.mu.Unlock()
	if b.Len() > 0 {
		_ = f.nft(b.String())
	}
}
