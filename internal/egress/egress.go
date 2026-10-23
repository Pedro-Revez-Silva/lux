// Package egress enforces a Run's network egress on its host: default deny,
// allowing only the CIDRs and hostnames in its spec.
//
// Each Run gets its own Podman network: a bridge with Podman's DNS off and a
// fixed, lux-prefixed interface name, so its rules exist before the bridge
// does. One nftables table, `inet lux`, owned by the runner and never
// touched by netavark, holds:
//
//   - a forward chain, ahead of netavark's, that drops traffic between
//     Runs' bridges (whatever either allows), sends each Run's traffic to
//     its chain, and drops traffic from any lux bridge with no chain: a Run
//     whose rules are not loaded (a runner restarting) fails closed;
//   - per Run, a chain: replies, then the control plane's exempt TCP ports
//     (sets cp_ports4 and cp_ports6 of address . port, shared by every
//     Run; a restricted Run's exception also needs the address in its
//     allow set), then the hard blocks (sets blocked4 and blocked6:
//     link-local and the metadata service, loopback, the control plane),
//     then the Run's allow set, then drop. An unrestricted Run's chain
//     accepts after the hard blocks instead;
//   - input rules so a restricted Run reaches the host only on the DNS
//     stub's port;
//   - a DNS redirect: port 53 from any restricted Run goes to its stub.
//     Unrestricted Runs use the host's resolvers;
//   - network transfer counters per Run and for the host, by billing class,
//     in base chains of their own that only count (netclass.go).
//
// IPv6 from a restricted Run is dropped (allow sets are IPv4, and nothing
// else accepts); the hard blocks cover IPv6 for unrestricted Runs too.
//
// Hostnames are resolved by the runner (not the container), once per host
// name across all Runs, at start and every minute; addresses are only ever
// added while a Run lives, so a connection is not cut when a CDN rotates.
// The DNS stub answers only allowed names, from those resolved addresses
// (so an answer is always reachable), refuses the rest, and reports each
// distinct lookup once. A wildcard rule (*.<domain>) is not resolved: a
// name it matches becomes an allowed hostname on its first lookup.
package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"golang.org/x/net/dns/dnsmessage"
)

// StubPort is where the DNS stub listens on each Run's gateway.
const StubPort = 15353

// Always blocked, whatever a spec allows: link-local (including the cloud
// metadata services) and loopback.
var alwaysBlocked = []string{"169.254.0.0/16", "127.0.0.0/8", "fe80::/10", "fd00:ec2::254/128", "::1/128"}

const setupRuleset = `
table inet lux {
	map runs {
		type ifname : verdict
	}
	set run_ifaces {
		type ifname
	}
	set blocked4 {
		type ipv4_addr
		flags interval
	}
	set blocked6 {
		type ipv6_addr
		flags interval
	}
	set cp_ports4 {
		type ipv4_addr . inet_service
	}
	set cp_ports6 {
		type ipv6_addr . inet_service
	}
	chain egress {
		type filter hook forward priority -10; policy accept;
		iifname "lux*" oifname "lux*" drop
		iifname vmap @runs
		iifname "lux*" drop
	}
	chain to_host {
		type filter hook input priority -10; policy accept;
		iifname @run_ifaces meta l4proto { tcp, udp } th dport %d accept
		iifname "lux*" ct state established,related accept
		iifname "lux*" drop
	}
	chain dns {
		type nat hook prerouting priority -110; policy accept;
		iifname @run_ifaces meta l4proto { tcp, udp } th dport 53 redirect to :%d
	}
}
`

// Lookup is one distinct DNS query a Run made, for its events.
type Lookup struct {
	Name    string   `json:"name"`
	Allowed bool     `json:"allowed"`
	Answers []string `json:"answers,omitempty"`
}

// Firewall is the runner's view of the nftables table and the DNS stubs.
type Firewall struct {
	// opMu serializes lifecycle kernel transactions and accounting
	// publication. Take before mu; never hold across resolution, callbacks
	// or stub close.
	opMu    sync.Mutex
	mu      sync.Mutex
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error) // IPv4 addresses of host
	nft     func(script string) error
	list    func() ([]byte, error) // `nft -j list counters table inet lux`
	runs    map[string]*run        // by bridge interface
	blocked []netip.Prefix         // parsed once: always blocked, plus the control plane
	always  []netip.Prefix         // the always-blocked ranges alone
	// controlPlane are New's extra prefixes; cpPorts the TCP ports on them
	// Runs may reach (SetControlPlanePorts), sorted.
	controlPlane []netip.Prefix
	cpPorts      []int
	resolved     map[string]map[netip.Addr]bool
	acct         accounting
}

type run struct {
	unrestricted bool
	hosts        map[string]bool // allowed hostnames, lowercased, no trailing dot
	// wildcards are the *.<domain> rules as ".<domain>" suffixes (matched
	// with spec.SuffixMatches). A name one matches joins hosts on its first
	// lookup (admitted counts them). Runtime state only: a restarted
	// runner re-applies the stored rules with none admitted, and the
	// container asks again when the stub's 60s TTL runs out.
	wildcards []string
	admitted  int
	cidrs     []netip.Prefix
	stub      *stub
	onDNS     func(Lookup)
	seen      map[string]bool // "<name>|<allowed>" already reported
}

// maxReported caps the distinct lookups reported per Run: the names are
// the workload's to choose, and each is an event.
const maxReported = 1000

// maxWildcardNames caps the names a Run's wildcards admit: each is an nft
// set update and a name resolved every minute, and the workload picks them.
const maxWildcardNames = 256

// New sets up the nftables table. An existing one is replaced, so a
// restarted runner starts clean and re-applies its Runs; meanwhile their
// traffic is dropped, never let through. extra are addresses no Run may
// reach besides the always-blocked ranges: the control plane.
func New(extra []netip.Prefix) (*Firewall, error) {
	f := newFirewall(extra, func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	}, nftScript, nftListCounters)
	if err := f.setup(); err != nil {
		return nil, err
	}
	return f, nil
}

// setup replaces the table, keeping the counters of the one it replaces,
// and re-applies the control-plane ports in force.
func (f *Firewall) setup() error {
	if err := f.keepCounters(); err != nil {
		return err
	}
	// One transaction: create-if-missing, delete, recreate. There is never
	// a moment without the table, so a Run is never open.
	var b strings.Builder
	b.WriteString("table inet lux {}\ndelete table inet lux\n")
	fmt.Fprintf(&b, setupRuleset, StubPort, StubPort)
	var v4, v6 []string
	for _, p := range f.blocked {
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	addElements(&b, "blocked4", v4)
	addElements(&b, "blocked6", v6)
	f.mu.Lock()
	b.WriteString(f.pairsElements())
	f.mu.Unlock()
	b.WriteString(f.countingSetup())
	if err := f.nft(b.String()); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

// newFirewall is a Firewall that resolves with lookup, loads rules with nft
// and reads counters with list; New passes the system resolver and the nft
// binary.
func newFirewall(extra []netip.Prefix, lookup func(context.Context, string) ([]netip.Addr, error), nft func(string) error, list func() ([]byte, error)) *Firewall {
	f := &Firewall{lookup: lookup, nft: nft, list: list, runs: map[string]*run{}, resolved: map[string]map[netip.Addr]bool{},
		acct: newAccounting()}
	for _, p := range alwaysBlocked {
		f.always = append(f.always, netip.MustParsePrefix(p))
	}
	f.blocked = append(slices.Clone(f.always), extra...)
	f.controlPlane = slices.Clone(extra)
	return f
}

// Unloaded is a Firewall that keeps its Runs' rules in memory and loads
// nothing into nftables, and resolves no names: for tests of the runner
// that drive a placement through its network setup without root.
func Unloaded() *Firewall {
	return newFirewall(nil, func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
		func(string) error { return nil }, func() ([]byte, error) { return []byte(`{"nftables":[]}`), nil })
}

// Chain and set names derive from the bridge interface, which is already
// a valid, unique nftables identifier.
func chainName(iface string) string { return "run_" + iface }
func setName(iface string) string   { return "allow_" + iface }

// Apply loads a Run's egress rules for its bridge interface and starts its
// DNS stub on the bridge's gateway. Call it before the container starts:
// the rules match the interface by name and the stub binds the gateway
// address before the bridge has it, so both hold from the first packet.
// Idempotent. onDNS receives each distinct lookup the Run makes. With
// unrestricted, the Run may reach anything but the hard blocks, and
// resolves through the host's resolvers.
func (f *Firewall) Apply(ctx context.Context, iface string, gateway netip.Addr, unrestricted bool, rules []spec.EgressRule, onDNS func(Lookup)) error {
	// Re-applying keeps the Run's counters: only its rules are replaced.
	f.remove(iface, false)
	r, err := newRun(unrestricted, rules, onDNS)
	if err != nil {
		return err
	}
	if !unrestricted {
		st, err := startStub(gateway, f, iface)
		if err != nil {
			return fmt.Errorf("dns stub on %s: %w", gateway, err)
		}
		r.stub = st
	}
	f.opMu.Lock()
	f.mu.Lock()
	err = f.transact(func() string { return egressScript(iface, unrestricted, r.cidrs) + f.runCountingScript(iface) })
	if err == nil {
		f.runs[iface] = r
	}
	f.mu.Unlock()
	f.opMu.Unlock()
	if err != nil {
		if r.stub != nil {
			r.stub.close()
		}
		return fmt.Errorf("nftables for %s: %w", iface, err)
	}
	f.resolveRun(ctx, iface, r)
	return nil
}

// egressScript is a Run's filtering: its chain, allow set and map entries.
// Counting is separate (runCountingScript) and never changes it. The
// control-plane exception comes before the hard blocks, which would
// otherwise drop it; for a restricted Run it also needs the address in the
// Run's allow set, so it lifts the block without granting egress. The
// pairs sets are shared, so SetControlPlanePorts changes every Run's chain
// at once.
func egressScript(iface string, unrestricted bool, cidrs []netip.Prefix) string {
	c, s := chainName(iface), setName(iface)
	var b strings.Builder
	fmt.Fprintf(&b, "add chain inet lux %s\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ct state established,related accept\n", c)
	if unrestricted {
		fmt.Fprintf(&b, "add rule inet lux %s meta l4proto tcp ip daddr . tcp dport @cp_ports4 accept\n", c)
		fmt.Fprintf(&b, "add rule inet lux %s meta l4proto tcp ip6 daddr . tcp dport @cp_ports6 accept\n", c)
	} else {
		fmt.Fprintf(&b, "add set inet lux %s { type ipv4_addr; flags interval; }\n", s)
		fmt.Fprintf(&b, "add rule inet lux %s meta l4proto tcp ip daddr @%s ip daddr . tcp dport @cp_ports4 accept\n", c, s)
	}
	fmt.Fprintf(&b, "add rule inet lux %s ip daddr @blocked4 drop\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ip6 daddr @blocked6 drop\n", c)
	if unrestricted {
		fmt.Fprintf(&b, "add rule inet lux %s accept\n", c)
	} else {
		fmt.Fprintf(&b, "add rule inet lux %s ip daddr @%s accept\n", c, s)
		fmt.Fprintf(&b, "add rule inet lux %s counter drop\n", c)
		if len(cidrs) > 0 {
			fmt.Fprintf(&b, "add element inet lux %s { %s }\n", s, join(cidrs))
		}
	}
	fmt.Fprintf(&b, "add element inet lux runs { %q : jump %s }\n", iface, c)
	if !unrestricted {
		fmt.Fprintf(&b, "add element inet lux run_ifaces { %q }\n", iface)
	}
	return b.String()
}

// install makes r the Run on iface and resolves its names now (adding
// known addresses to its set); the shared loop keeps them fresh.
func (f *Firewall) install(ctx context.Context, iface string, r *run) {
	f.mu.Lock()
	f.runs[iface] = r
	f.mu.Unlock()
	f.resolveRun(ctx, iface, r)
}

func (f *Firewall) resolveRun(ctx context.Context, iface string, r *run) {
	f.mu.Lock()
	hosts := slices.Collect(maps.Keys(r.hosts))
	f.mu.Unlock()
	for _, h := range hosts {
		f.addKnown(iface, h)
		f.resolve(ctx, h)
	}
}

// newRun sorts a Run's rules: cidrs, exact hosts, and wildcards, which
// are not resolved here but on a matching lookup (answerFor).
func newRun(unrestricted bool, rules []spec.EgressRule, onDNS func(Lookup)) (*run, error) {
	r := &run{unrestricted: unrestricted, hosts: map[string]bool{}, onDNS: onDNS, seen: map[string]bool{}}
	for _, rule := range rules {
		switch {
		case rule.CIDR != "":
			p, err := netip.ParsePrefix(rule.CIDR)
			if err != nil {
				return nil, fmt.Errorf("egress cidr %q: %w", rule.CIDR, err)
			}
			r.cidrs = append(r.cidrs, p.Masked())
		case rule.IsWildcard():
			// The spec validated the rule; an invalid one admits nothing.
			if suffix, ok := spec.WildcardSuffix(rule.Host); ok {
				r.wildcards = append(r.wildcards, suffix)
			}
		case rule.Host != "":
			r.hosts[spec.NormalHost(rule.Host)] = true
		}
	}
	return r, nil
}

// addKnown gives a Run the addresses already resolved for a host (by
// another Run's rules): resolve only adds addresses that are new.
func (f *Firewall) addKnown(iface, host string) {
	f.mu.Lock()
	elems := f.knownElements(iface, host)
	f.mu.Unlock()
	if elems != "" {
		_ = f.nft(elems)
	}
}

// knownElements is the nft line adding host's resolved addresses to the
// Run's set, or "" if none are known. Call with f.mu held.
func (f *Firewall) knownElements(iface, host string) string {
	var known []string
	for ip := range f.resolved[host] {
		known = append(known, ip.String())
	}
	if len(known) == 0 {
		return ""
	}
	return fmt.Sprintf("add element inet lux %s { %s }\n", setName(iface), strings.Join(known, ", "))
}

// Run keeps allowed hostnames resolved, for every Run, until ctx ends:
// each distinct name is resolved once a minute, however many Runs allow it.
func (f *Firewall) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.refresh(ctx)
	}
}

// refreshWorkers bounds the concurrent lookups of one refresh: a Run's
// admitted names are the workload's to choose, and a slow one must not
// hold up every other Run's names for its whole timeout.
const refreshWorkers = 16

// refresh resolves every Run's distinct hostnames once and returns when
// all lookups are done.
func (f *Firewall) refresh(ctx context.Context) {
	f.mu.Lock()
	names := map[string]bool{}
	for _, r := range f.runs {
		for h := range r.hosts {
			names[h] = true
		}
	}
	f.mu.Unlock()
	var wg sync.WaitGroup
	slots := make(chan struct{}, refreshWorkers)
	for h := range names {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			f.resolve(ctx, h)
		})
	}
	wg.Wait()
}

// resolve looks a host up and adds its new addresses (never one in a
// hard-blocked range) to every Run that allows it, in one nft script.
// Returns every address known for it, or nil if no Run lists host by the
// time the lookup returns.
func (f *Firewall) resolve(ctx context.Context, host string) []netip.Addr {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := f.lookup(rctx, host)
	f.mu.Lock()
	defer f.mu.Unlock()
	// The Runs that listed host may have been removed during the lookup:
	// storing it would leave an entry nothing refreshes or prunes.
	if !f.listed(host) {
		return nil
	}
	known := f.resolved[host]
	if known == nil {
		known = map[netip.Addr]bool{}
		f.resolved[host] = known
	}
	var fresh []netip.Addr
	if err == nil {
		for _, ip := range ips {
			ip = ip.Unmap()
			if !known[ip] && !f.isBlocked(ip) && !slices.Contains(fresh, ip) {
				fresh = append(fresh, ip)
			}
		}
	}
	if len(fresh) > 0 {
		list := make([]string, len(fresh))
		for i, ip := range fresh {
			list[i] = ip.String()
		}
		var b strings.Builder
		for iface, r := range f.runs {
			if r.hosts[host] && !r.unrestricted {
				fmt.Fprintf(&b, "add element inet lux %s { %s }\n", setName(iface), strings.Join(list, ", "))
			}
		}
		// Known (so answered by the stub) only once the firewall allows
		// them; a failed add is retried on the next resolution.
		if b.Len() == 0 || f.nft(b.String()) == nil {
			for _, ip := range fresh {
				known[ip] = true
			}
		}
	}
	out := make([]netip.Addr, 0, len(known))
	for ip := range known {
		out = append(out, ip)
	}
	return out
}

// listed: some Run allows host. resolve and Remove must agree on it, or
// one re-creates what the other prunes. Call with f.mu held.
func (f *Firewall) listed(host string) bool {
	for _, r := range f.runs {
		if r.hosts[host] {
			return true
		}
	}
	return false
}

// isBlocked: an allowed hostname that resolves into a hard-blocked range
// (say the metadata address) must not open it. Call with f.mu held.
//
// A control-plane address is let into a restricted Run's set (and so
// answered by its DNS stub) while some control-plane port is exempt. That
// opens only the exempt ports: the blocked4 drop precedes the Run's plain
// allow rule, so every other port on that address stays dropped.
func (f *Firewall) isBlocked(ip netip.Addr) bool {
	if len(f.cpPorts) > 0 && f.exemptAddr(ip) {
		return false
	}
	for _, p := range f.blocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// exemptAddr: ip is one of the control plane's own addresses (a single-IP
// prefix given to New) outside the always-blocked ranges, which no port
// exemption ever opens. Only such addresses go into cp_ports4/cp_ports6.
func (f *Firewall) exemptAddr(ip netip.Addr) bool {
	for _, p := range f.always {
		if p.Contains(ip) {
			return false
		}
	}
	return slices.ContainsFunc(f.controlPlane, func(p netip.Prefix) bool { return p.IsSingleIP() && p.Addr() == ip })
}

// SetControlPlanePorts replaces the TCP ports on the control plane's
// addresses that Runs may reach, for every Run at once, in one
// transaction: the pairs sets are flushed and refilled. Duplicates
// collapse; a port outside 1-65535 or more than
// proto.MaxControlPlanePorts refuses the whole list before any nft call.
// Empty restores the full block. changed reports that the list in force
// is now a different one. On an nft error the previous list stays.
func (f *Firewall) SetControlPlanePorts(ports []int) (changed bool, err error) {
	var clean []int
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return false, fmt.Errorf("control-plane ports: %d is not a TCP port", p)
		}
		if !slices.Contains(clean, p) {
			clean = append(clean, p)
		}
	}
	slices.Sort(clean)
	if len(clean) > proto.MaxControlPlanePorts {
		return false, fmt.Errorf("control-plane ports: %d, at most %d", len(clean), proto.MaxControlPlanePorts)
	}
	f.opMu.Lock()
	defer f.opMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Equal(clean, f.cpPorts) {
		return false, nil
	}
	old := f.cpPorts
	f.cpPorts = clean
	script := "flush set inet lux cp_ports4\nflush set inet lux cp_ports6\n" + f.pairsElements()
	if err := f.nft(script); err != nil {
		f.cpPorts = old
		return false, fmt.Errorf("nftables: control-plane ports: %w", err)
	}
	if len(clean) == 0 {
		// The stub stops answering control-plane addresses. Elements already
		// in Runs' sets stay: the blocked drops cover them again.
		for _, known := range f.resolved {
			for ip := range known {
				if f.exemptAddr(ip) {
					delete(known, ip)
				}
			}
		}
	}
	return true, nil
}

// ControlPlanePorts reports the list in force, for tests and diagnostics.
func (f *Firewall) ControlPlanePorts() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cpPorts)
}

// pairsElements adds every (control-plane address, exempt port) pair to
// cp_ports4/cp_ports6, or is "" with none. Call with f.mu held.
func (f *Firewall) pairsElements() string {
	var v4, v6 []string
	for _, p := range f.controlPlane {
		ip := p.Addr()
		if !p.IsSingleIP() || !f.exemptAddr(ip) {
			continue
		}
		for _, port := range f.cpPorts {
			e := fmt.Sprintf("%s . %d", ip, port)
			if ip.Is4() {
				v4 = append(v4, e)
			} else {
				v6 = append(v6, e)
			}
		}
	}
	var b strings.Builder
	addElements(&b, "cp_ports4", v4)
	addElements(&b, "cp_ports6", v6)
	return b.String()
}

// addElements writes the line adding elems to set, or nothing with none.
func addElements(b *strings.Builder, set string, elems []string) {
	if len(elems) > 0 {
		fmt.Fprintf(b, "add element inet lux %s { %s }\n", set, strings.Join(elems, ", "))
	}
}

// answerFor is what the DNS stub answers a Run for name: nothing unless it
// is allowed; then the addresses already known (the shared loop keeps them
// fresh), resolving only on a first lookup. A name only a wildcard matches
// is admitted (added to the Run's hosts, up to maxWildcardNames) and then
// answered the same way, so its addresses are in the Run's set before the
// stub returns them. Only an A query admits: any other type for such a
// name is allowed with no addresses, because a resolver that sends A and
// AAAA together can fail the whole lookup on a refused AAAA.
func (f *Firewall) answerFor(iface, name string, qtype dnsmessage.Type) (allowed bool, addrs []netip.Addr) {
	f.mu.Lock()
	r := f.runs[iface]
	if r == nil {
		f.mu.Unlock()
		return false, nil
	}
	if !r.hosts[name] {
		if qtype != dnsmessage.TypeA {
			ok := r.admissible(name)
			f.mu.Unlock()
			return ok, nil
		}
		if !f.admit(iface, r, name) {
			f.mu.Unlock()
			return false, nil
		}
	}
	for ip := range f.resolved[name] {
		addrs = append(addrs, ip)
	}
	f.mu.Unlock()
	if len(addrs) == 0 {
		addrs = f.resolve(context.Background(), name)
	}
	return true, addrs
}

// admit adds name to a restricted Run's hosts if one of its wildcards
// matches it and the cap leaves room. Addresses already resolved for name
// (another Run's) go into the Run's set first, as addKnown does at Apply;
// if that fails, name is not admitted, and its next lookup tries again.
// Call with f.mu held.
func (f *Firewall) admit(iface string, r *run, name string) bool {
	if !r.admissible(name) {
		return false
	}
	if elems := f.knownElements(iface, name); elems != "" && f.nft(elems) != nil {
		return false
	}
	r.hosts[name] = true
	r.admitted++
	return true
}

// admissible: a restricted Run with room under the cap has a wildcard
// that matches name. Call with f.mu held.
func (r *run) admissible(name string) bool {
	return !r.unrestricted && r.admitted < maxWildcardNames && slices.ContainsFunc(r.wildcards, func(s string) bool { return spec.SuffixMatches(s, name) })
}

// report sends a lookup to the Run's events, once per (name, allowed).
func (f *Firewall) report(iface string, l Lookup) {
	f.mu.Lock()
	r := f.runs[iface]
	if r == nil || r.onDNS == nil {
		f.mu.Unlock()
		return
	}
	key := fmt.Sprintf("%s|%t", l.Name, l.Allowed)
	if r.seen[key] || len(r.seen) >= maxReported {
		f.mu.Unlock()
		return
	}
	r.seen[key] = true
	cb := r.onDNS
	f.mu.Unlock()
	cb(l)
}

// Remove tears a Run's egress down (its network is going away), in one
// script: map entries first, then the chain and set they point to, and its
// counters. It returns the counters' last reading, taken just before they
// are deleted (nil if there was none): the bytes since the last sample,
// which would otherwise be lost.
func (f *Firewall) Remove(iface string) map[string]proto.NetBytes {
	return f.remove(iface, true)
}

func (f *Firewall) remove(iface string, counters bool) map[string]proto.NetBytes {
	// The final read stays outside opMu: a full counter dump must not delay
	// other Runs' lifecycle transactions.
	var final map[string]proto.NetBytes
	f.mu.Lock()
	counted := f.acct.counted[iface]
	f.mu.Unlock()
	if counters && counted {
		if c, err := f.Counters(); err == nil {
			final = c.Runs[iface]
		}
	}
	f.opMu.Lock()
	f.mu.Lock()
	counted = f.acct.counted[iface]
	r := f.runs[iface]
	var script string
	if r != nil {
		script = fmt.Sprintf("delete element inet lux runs { %q }\ndelete chain inet lux %s\n", iface, chainName(iface))
		if !r.unrestricted {
			script = fmt.Sprintf("delete element inet lux run_ifaces { %q }\n", iface) + script +
				fmt.Sprintf("delete set inet lux %s\n", setName(iface))
		}
	}
	var err error
	if counters && counted {
		err = f.transact(func() string {
			counting := f.removeCountingScript(iface)
			delete(f.acct.counted, iface)
			return script + counting
		})
	} else if script != "" {
		err = f.nft(script)
	}
	if err != nil {
		slog.Warn("remove egress", "iface", iface, "err", err)
		f.mu.Unlock()
		f.opMu.Unlock()
		return final
	}
	delete(f.runs, iface)
	if r != nil {
		for h := range r.hosts {
			if !f.listed(h) {
				delete(f.resolved, h)
			}
		}
	}
	f.mu.Unlock()
	f.opMu.Unlock()
	if r != nil && r.stub != nil {
		r.stub.close()
	}
	return final
}

func join(ps []netip.Prefix) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
}

func nftListCounters() ([]byte, error) {
	var out, errb bytes.Buffer
	c := exec.Command("nft", "-j", "list", "counters", "table", "inet", "lux")
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		// Listing all tables distinguishes absence without interpreting localized diagnostics.
		tables, listErr := exec.Command("nft", "-j", "list", "tables").Output()
		if listErr == nil {
			var doc struct {
				Nftables []struct {
					Table struct {
						Family string `json:"family"`
						Name   string `json:"name"`
					} `json:"table"`
				} `json:"nftables"`
			}
			if json.Unmarshal(tables, &doc) == nil {
				found := false
				for _, item := range doc.Nftables {
					found = found || item.Table.Family == "inet" && item.Table.Name == "lux"
				}
				if !found {
					return []byte(`{"nftables":[]}`), nil
				}
			}
		}
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func nftScript(script string) error {
	c := exec.Command("nft", "-f", "-")
	c.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
