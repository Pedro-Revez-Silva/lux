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
//   - per Run, a chain: replies, then the hard blocks (sets blocked4 and
//     blocked6: link-local and the metadata service, loopback, the control
//     plane), then the Run's allow set, then drop. An unrestricted Run's
//     chain accepts after the hard blocks instead;
//   - input rules so a restricted Run reaches the host only on the DNS
//     stub's port;
//   - a DNS redirect: port 53 from any restricted Run goes to its stub.
//     Unrestricted Runs use the host's resolvers.
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
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

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
	mu       sync.Mutex
	lookup   func(ctx context.Context, host string) ([]netip.Addr, error) // IPv4 addresses of host
	nft      func(script string) error
	runs     map[string]*run // by bridge interface
	blocked  []netip.Prefix  // parsed once: always blocked, plus the control plane
	resolved map[string]map[netip.Addr]bool
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
	}, nftScript)
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
	if len(v4) > 0 {
		fmt.Fprintf(&b, "add element inet lux blocked4 { %s }\n", strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "add element inet lux blocked6 { %s }\n", strings.Join(v6, ", "))
	}
	if err := f.nft(b.String()); err != nil {
		return nil, fmt.Errorf("nftables: %w", err)
	}
	return f, nil
}

// newFirewall is a Firewall that resolves with lookup and loads rules with
// nft; New passes the system resolver and the nft binary.
func newFirewall(extra []netip.Prefix, lookup func(context.Context, string) ([]netip.Addr, error), nft func(string) error) *Firewall {
	f := &Firewall{lookup: lookup, nft: nft, runs: map[string]*run{}, resolved: map[string]map[netip.Addr]bool{}}
	for _, p := range alwaysBlocked {
		f.blocked = append(f.blocked, netip.MustParsePrefix(p))
	}
	f.blocked = append(f.blocked, extra...)
	return f
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
	f.Remove(iface)
	r, err := newRun(unrestricted, rules, onDNS)
	if err != nil {
		return err
	}
	c, s := chainName(iface), setName(iface)
	var b strings.Builder
	fmt.Fprintf(&b, "add chain inet lux %s\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ct state established,related accept\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ip daddr @blocked4 drop\n", c)
	fmt.Fprintf(&b, "add rule inet lux %s ip6 daddr @blocked6 drop\n", c)
	if unrestricted {
		fmt.Fprintf(&b, "add rule inet lux %s accept\n", c)
	} else {
		fmt.Fprintf(&b, "add set inet lux %s { type ipv4_addr; flags interval; }\n", s)
		fmt.Fprintf(&b, "add rule inet lux %s ip daddr @%s accept\n", c, s)
		fmt.Fprintf(&b, "add rule inet lux %s counter drop\n", c)
		if len(r.cidrs) > 0 {
			fmt.Fprintf(&b, "add element inet lux %s { %s }\n", s, join(r.cidrs))
		}
	}
	fmt.Fprintf(&b, "add element inet lux runs { %q : jump %s }\n", iface, c)
	if !unrestricted {
		fmt.Fprintf(&b, "add element inet lux run_ifaces { %q }\n", iface)
		st, err := startStub(gateway, f, iface)
		if err != nil {
			return fmt.Errorf("dns stub on %s: %w", gateway, err)
		}
		r.stub = st
	}
	if err := f.nft(b.String()); err != nil {
		if r.stub != nil {
			r.stub.close()
		}
		return fmt.Errorf("nftables for %s: %w", iface, err)
	}
	f.install(ctx, iface, r)
	return nil
}

// install makes r the Run on iface and resolves its names now (adding
// known addresses to its set); the shared loop keeps them fresh.
func (f *Firewall) install(ctx context.Context, iface string, r *run) {
	f.mu.Lock()
	f.runs[iface] = r
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
func (f *Firewall) isBlocked(ip netip.Addr) bool {
	for _, p := range f.blocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
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
// script: map entries first, then the chain and set they point to.
func (f *Firewall) Remove(iface string) {
	f.mu.Lock()
	r := f.runs[iface]
	delete(f.runs, iface)
	// A name no remaining Run allows is no longer refreshed; dropping its
	// addresses keeps f.resolved bounded by the live Runs' hosts.
	if r != nil {
		for h := range r.hosts {
			if !f.listed(h) {
				delete(f.resolved, h)
			}
		}
	}
	f.mu.Unlock()
	if r == nil {
		return
	}
	if r.stub != nil {
		r.stub.close()
	}
	script := fmt.Sprintf("delete element inet lux runs { %q }\ndelete chain inet lux %s\n", iface, chainName(iface))
	if !r.unrestricted {
		script = fmt.Sprintf("delete element inet lux run_ifaces { %q }\n", iface) + script +
			fmt.Sprintf("delete set inet lux %s\n", setName(iface))
	}
	_ = f.nft(script)
}

func join(ps []netip.Prefix) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
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
