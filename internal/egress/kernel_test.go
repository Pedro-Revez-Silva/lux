package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// kernel is an in-memory nftables for the subset of the language this
// package writes: it applies scripts as one transaction (any error leaves
// it unchanged), refuses what nft refuses here (a missing object, deleting
// one still referenced, overlapping interval elements), and evaluates
// packets through the base chains of a hook, counting into named counters.
// The real nft is exercised by nft_linux_test.go where it is available.
type kernel struct {
	mu sync.Mutex
	st *kstate
	// failOn: a script containing it fails, as an nft error would.
	failOn string
}

type kstate struct {
	exists   bool
	chains   map[string]*kchain
	sets     map[string]*kset
	vmaps    map[string]map[string]string // map: ifname -> verdict
	counters map[string]*kcounter
}

type kchain struct {
	hook  string // "" for a regular chain
	prio  int
	rules [][]string
}

type kset struct {
	ifname bool
	v6     bool
	elems  []netip.Prefix
	names  map[string]bool
}

type kcounter struct{ packets, bytes int64 }

func newKernel() *kernel { return &kernel{st: emptyState()} }

func emptyState() *kstate {
	return &kstate{chains: map[string]*kchain{}, sets: map[string]*kset{}, vmaps: map[string]map[string]string{}, counters: map[string]*kcounter{}}
}

func (s *kstate) clone() *kstate {
	c := emptyState()
	c.exists = s.exists
	for n, ch := range s.chains {
		rules := make([][]string, len(ch.rules))
		for i, r := range ch.rules {
			rules[i] = slices.Clone(r)
		}
		c.chains[n] = &kchain{hook: ch.hook, prio: ch.prio, rules: rules}
	}
	for n, st := range s.sets {
		c.sets[n] = &kset{ifname: st.ifname, v6: st.v6, elems: slices.Clone(st.elems), names: maps.Clone(st.names)}
	}
	for n, m := range s.vmaps {
		c.vmaps[n] = maps.Clone(m)
	}
	for n, k := range s.counters {
		v := *k
		c.counters[n] = &v
	}
	return c
}

// firewall is a Firewall on k, with ifaceSubnets as the host's interface
// subnets, set up as New does (keeping the counters of a table k holds).
func (k *kernel) firewall(ifaceSubnets ...string) (*Firewall, error) {
	f := newFirewall(nil, func(context.Context, string) ([]netip.Addr, error) { return nil, nil }, k.nft, k.list)
	f.acct.ifaceSubnets = func() []netip.Prefix {
		var out []netip.Prefix
		for _, s := range ifaceSubnets {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}
	return f, f.setup()
}

func (k *kernel) nft(script string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failOn != "" && strings.Contains(script, k.failOn) {
		return fmt.Errorf("nft: refused (test)")
	}
	next := k.st.clone()
	if err := next.apply(script); err != nil {
		return err
	}
	k.st = next
	return nil
}

func (s *kstate) apply(script string) error {
	lines := strings.Split(script, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "":
		case line == "table inet lux {}":
			s.exists = true
		case line == "delete table inet lux":
			if !s.exists {
				return fmt.Errorf("no table")
			}
			*s = *emptyState()
		case line == "table inet lux {":
			s.exists = true
			n, err := s.block(lines[i+1:])
			if err != nil {
				return err
			}
			i += n
		default:
			if err := s.command(line); err != nil {
				return fmt.Errorf("%q: %w", line, err)
			}
		}
	}
	return nil
}

// block reads a table block's body up to its closing brace; it returns
// the lines consumed.
func (s *kstate) block(lines []string) (int, error) {
	var kind, name string
	var body []string
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case kind == "" && line == "}":
			return i + 1, nil
		case kind == "":
			f := strings.Fields(line)
			if len(f) != 3 || f[2] != "{" {
				return 0, fmt.Errorf("block: %q", line)
			}
			kind, name, body = f[0], f[1], nil
		case line == "}":
			decl := strings.Join(body, " ")
			var err error
			switch kind {
			case "map":
				err = s.command("add map inet lux " + name + " { " + decl + " }")
			case "set":
				err = s.command("add set inet lux " + name + " { " + decl + " }")
			case "chain":
				if len(body) > 0 && strings.HasPrefix(body[0], "type ") {
					err = s.command("add chain inet lux " + name + " { " + body[0] + " }")
					body = body[1:]
				} else {
					err = s.command("add chain inet lux " + name)
				}
				for _, r := range body {
					if err == nil {
						err = s.command("add rule inet lux " + name + " " + r)
					}
				}
			}
			if err != nil {
				return 0, err
			}
			kind = ""
		default:
			body = append(body, line)
		}
	}
	return 0, fmt.Errorf("unterminated table block")
}

func (s *kstate) command(line string) error {
	if !s.exists {
		return fmt.Errorf("no table")
	}
	f := strings.Fields(line)
	if len(f) < 5 || f[2] != "inet" || f[3] != "lux" {
		return fmt.Errorf("unsupported")
	}
	verb, name := f[0]+" "+f[1], f[4]
	rest := strings.TrimSpace(strings.Join(f[5:], " "))
	switch verb {
	case "add chain":
		if _, ok := s.chains[name]; ok {
			return nil
		}
		c := &kchain{}
		if rest != "" {
			var hook string
			var prio int
			if _, err := fmt.Sscanf(rest, "{ type filter hook %s priority %d; policy accept; }", &hook, &prio); err != nil {
				if _, err2 := fmt.Sscanf(rest, "{ type nat hook %s priority %d; policy accept; }", &hook, &prio); err2 != nil {
					return fmt.Errorf("chain declaration: %v", err)
				}
			}
			c.hook, c.prio = hook, prio
		}
		s.chains[name] = c
	case "flush chain":
		c, ok := s.chains[name]
		if !ok {
			return fmt.Errorf("no chain %s", name)
		}
		c.rules = nil
	case "delete chain":
		// nft deletes the chain's rules with it; only a jump to it blocks.
		if _, ok := s.chains[name]; !ok {
			return fmt.Errorf("no chain %s", name)
		}
		if s.referenced("jump " + name) {
			return fmt.Errorf("chain %s in use", name)
		}
		delete(s.chains, name)
	case "add rule":
		c, ok := s.chains[name]
		if !ok {
			return fmt.Errorf("no chain %s", name)
		}
		toks := tokenize(rest)
		for i, t := range toks {
			switch {
			case strings.HasPrefix(t, "@"):
				_, isSet := s.sets[t[1:]]
				_, isMap := s.vmaps[t[1:]]
				if !isSet && !isMap {
					return fmt.Errorf("no set %s", t)
				}
			case t == "name" && i > 0 && toks[i-1] == "counter":
				if _, ok := s.counters[toks[i+1]]; !ok {
					return fmt.Errorf("no counter %s", toks[i+1])
				}
			case t == "jump":
				if _, ok := s.chains[toks[i+1]]; !ok {
					return fmt.Errorf("no chain %s", toks[i+1])
				}
			}
		}
		c.rules = append(c.rules, toks)
	case "add set":
		if _, ok := s.sets[name]; ok {
			return nil
		}
		switch {
		case strings.Contains(rest, "type ifname"):
			s.sets[name] = &kset{ifname: true, names: map[string]bool{}}
		case strings.Contains(rest, "type ipv4_addr") && strings.Contains(rest, "flags interval"):
			s.sets[name] = &kset{}
		case strings.Contains(rest, "type ipv6_addr") && strings.Contains(rest, "flags interval"):
			s.sets[name] = &kset{v6: true}
		default:
			return fmt.Errorf("set declaration %q", rest)
		}
	case "flush set":
		st, ok := s.sets[name]
		if !ok {
			return fmt.Errorf("no set %s", name)
		}
		st.elems, st.names = nil, map[string]bool{}
	case "delete set":
		if _, ok := s.sets[name]; !ok {
			return fmt.Errorf("no set %s", name)
		}
		if s.referenced("@" + name) {
			return fmt.Errorf("set %s in use", name)
		}
		delete(s.sets, name)
	case "add map":
		if _, ok := s.vmaps[name]; !ok {
			s.vmaps[name] = map[string]string{}
		}
	case "add element", "delete element":
		body, ok := strings.CutPrefix(rest, "{ ")
		body, ok2 := strings.CutSuffix(body, " }")
		if !ok || !ok2 {
			return fmt.Errorf("elements %q", rest)
		}
		del := verb == "delete element"
		if m, ok := s.vmaps[name]; ok {
			for e := range strings.SplitSeq(body, ", ") {
				k, v, _ := strings.Cut(e, " : ")
				k = strings.Trim(k, `"`)
				if del {
					if _, ok := m[k]; !ok {
						return fmt.Errorf("no element %s in %s", k, name)
					}
					delete(m, k)
					continue
				}
				if c, ok := strings.CutPrefix(v, "jump "); !ok || s.chains[c] == nil {
					return fmt.Errorf("map element %q", e)
				}
				m[k] = v
			}
			return nil
		}
		st, ok := s.sets[name]
		if !ok {
			return fmt.Errorf("no set %s", name)
		}
		for e := range strings.SplitSeq(body, ", ") {
			if st.ifname {
				k := strings.Trim(e, `"`)
				if del {
					if !st.names[k] {
						return fmt.Errorf("no element %s", k)
					}
					delete(st.names, k)
				} else {
					st.names[k] = true
				}
				continue
			}
			p, err := netip.ParsePrefix(e)
			if err != nil {
				a, err2 := netip.ParseAddr(e)
				if err2 != nil {
					return err
				}
				p = netip.PrefixFrom(a, a.BitLen())
			}
			if p.Addr().Is6() != st.v6 {
				return fmt.Errorf("%s in %s: wrong family", e, name)
			}
			if p != p.Masked() {
				return fmt.Errorf("%s: host bits set", e)
			}
			if del {
				i := slices.Index(st.elems, p)
				if i < 0 {
					return fmt.Errorf("no element %s", e)
				}
				st.elems = slices.Delete(st.elems, i, i+1)
				continue
			}
			if slices.Contains(st.elems, p) {
				continue
			}
			for _, q := range st.elems {
				if q.Overlaps(p) {
					return fmt.Errorf("%s overlaps %s in %s", p, q, name)
				}
			}
			st.elems = append(st.elems, p)
		}
	case "add counter":
		if _, ok := s.counters[name]; ok {
			return nil
		}
		c := &kcounter{}
		if rest != "" {
			if _, err := fmt.Sscanf(rest, "{ packets %d bytes %d }", &c.packets, &c.bytes); err != nil {
				return fmt.Errorf("counter %q: %v", rest, err)
			}
		}
		s.counters[name] = c
	case "delete counter":
		if _, ok := s.counters[name]; !ok {
			return fmt.Errorf("no counter %s", name)
		}
		if s.referenced("name " + name) {
			return fmt.Errorf("counter %s in use", name)
		}
		delete(s.counters, name)
	default:
		return fmt.Errorf("unsupported")
	}
	return nil
}

// referenced: some rule or map element holds ref ("jump c", "@set",
// "name counter").
func (s *kstate) referenced(ref string) bool {
	for _, c := range s.chains {
		for _, r := range c.rules {
			if strings.Contains(" "+strings.Join(r, " ")+" ", " "+ref+" ") {
				return true
			}
		}
	}
	for _, m := range s.vmaps {
		for _, v := range m {
			if v == ref {
				return true
			}
		}
	}
	return false
}

// tokenize splits a rule, keeping "{ tcp, udp }" as one token.
func tokenize(rule string) []string {
	var out []string
	for rule = strings.TrimSpace(rule); rule != ""; rule = strings.TrimSpace(rule) {
		if strings.HasPrefix(rule, "{") {
			end := strings.Index(rule, "}")
			out = append(out, rule[:end+1])
			rule = rule[end+1:]
			continue
		}
		tok, rest, _ := strings.Cut(rule, " ")
		out = append(out, tok)
		rule = rest
	}
	return out
}

// list answers as `nft -j list counters table inet lux` does.
func (k *kernel) list() ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.st.exists {
		return []byte(`{"nftables":[]}`), nil
	}
	objs := []map[string]any{{"metainfo": map[string]any{"version": "1.0.9", "json_schema_version": 1}}}
	for _, n := range slices.Sorted(maps.Keys(k.st.counters)) {
		c := k.st.counters[n]
		objs = append(objs, map[string]any{"counter": map[string]any{"family": "inet", "name": n, "table": "lux", "handle": 1, "packets": c.packets, "bytes": c.bytes}})
	}
	return json.Marshal(map[string]any{"nftables": objs})
}

// packet is one packet at a hook.
type packet struct {
	hook        string // forward, input, output
	iif, oif    string
	src, dst    string
	proto       string // tcp, udp
	dport       int
	established bool
	bytes       int64
}

// send evaluates p through its hook's base chains in priority order; with
// skipCounting, the counting chains (priority 100) are left out. It
// returns the final verdict.
func (k *kernel) send(p packet, skipCounting bool) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var base []string
	for n, c := range k.st.chains {
		if c.hook == p.hook && !(skipCounting && c.prio == 100) {
			base = append(base, n)
		}
	}
	slices.SortFunc(base, func(a, b string) int { return k.st.chains[a].prio - k.st.chains[b].prio })
	for _, n := range base {
		if v := k.st.run(n, p, 0); v == "drop" {
			return "drop"
		}
	}
	return "accept"
}

func (s *kstate) run(chain string, p packet, depth int) string {
	if depth > 8 {
		panic("jump loop")
	}
	for _, r := range s.chains[chain].rules {
		if v, done := s.rule(r, p, depth); done {
			return v
		}
	}
	return ""
}

// rule evaluates one rule; done when it reached a verdict.
func (s *kstate) rule(r []string, p packet, depth int) (string, bool) {
	addr := func(a string) netip.Addr {
		x, err := netip.ParseAddr(a)
		if err != nil {
			panic(err)
		}
		return x
	}
	for i := 0; i < len(r); i++ {
		switch t := r[i]; t {
		case "iifname", "oifname":
			name := p.iif
			if t == "oifname" {
				name = p.oif
			}
			arg := r[i+1]
			i++
			if arg == "vmap" {
				v, ok := s.vmaps[strings.TrimPrefix(r[i+1], "@")][name]
				i++
				if !ok {
					return "", false
				}
				c, _ := strings.CutPrefix(v, "jump ")
				if out := s.run(c, p, depth+1); out != "" {
					return out, true
				}
				return "", false
			}
			if set, ok := strings.CutPrefix(arg, "@"); ok {
				if !s.sets[set].names[name] {
					return "", false
				}
				continue
			}
			if !globMatch(strings.Trim(arg, `"`), name) {
				return "", false
			}
		case "ip", "ip6":
			field, set := r[i+1], strings.TrimPrefix(r[i+2], "@")
			i += 2
			a := addr(p.dst)
			if field == "saddr" {
				a = addr(p.src)
			}
			if a.Is6() != (t == "ip6") {
				return "", false
			}
			if !slices.ContainsFunc(s.sets[set].elems, func(q netip.Prefix) bool { return q.Contains(a) }) {
				return "", false
			}
		case "ct":
			i += 2
			if !p.established {
				return "", false
			}
		case "meta":
			list := r[i+2]
			i += 2
			if !strings.Contains(list, p.proto) || p.proto == "" {
				return "", false
			}
		case "th":
			n, _ := strconv.Atoi(r[i+2])
			i += 2
			if p.dport != n {
				return "", false
			}
		case "counter":
			if i+1 < len(r) && r[i+1] == "name" {
				c := s.counters[r[i+2]]
				c.packets++
				c.bytes += p.bytes
				i += 2
			}
		case "accept", "drop":
			return t, true
		case "jump":
			if out := s.run(r[i+1], p, depth+1); out != "" {
				return out, true
			}
			return "", false
		default:
			panic(fmt.Sprintf("kernel: unsupported token %q in %v", t, r))
		}
	}
	return "", false
}

// verdicts are every verdict reachable from chain: its rules' and those of
// the chains it jumps to or reaches through a map, "jump" excluded.
func (k *kernel) verdicts(chain string) map[string]bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(c string) {
		if seen[c] {
			return
		}
		seen[c] = true
		for _, r := range k.st.chains[c].rules {
			for i, t := range r {
				switch {
				case t == "accept" || t == "drop" || t == "return" || t == "reject" || t == "queue" || t == "goto":
					out[t] = true
				case t == "jump":
					walk(r[i+1])
				case t == "vmap":
					for _, v := range k.st.vmaps[strings.TrimPrefix(r[i+1], "@")] {
						to, _ := strings.CutPrefix(v, "jump ")
						walk(to)
					}
				}
			}
		}
	}
	walk(chain)
	return out
}

// counter is a counter's bytes, -1 if it does not exist.
func (k *kernel) counter(name string) int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	if c, ok := k.st.counters[name]; ok {
		return c.bytes
	}
	return -1
}

// rules is every chain's rules, for comparing the table's filtering
// before and after a change.
func (k *kernel) rules() map[string][][]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string][][]string{}
	for n, c := range k.st.chains {
		out[n] = slices.Clone(c.rules)
	}
	return out
}
