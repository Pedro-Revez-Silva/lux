package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/marcioapm/lux/internal/egress"
	"github.com/marcioapm/lux/internal/podman"
	"github.com/marcioapm/lux/internal/spec"
)

// bridgeName is a Run's bridge interface: fixed, so its egress rules (which
// match by interface) are in place before Podman creates the bridge, and
// "lux"-prefixed, so a bridge with no rules loaded is dropped (see
// package egress). Linux interface names are at most 15 bytes: "lux" and
// 12 characters of a hash of the Run id (a prefix of the id could collide).
func bridgeName(runID string) string {
	h := sha256.Sum256([]byte(runID))
	return "lux" + strings.ToLower(base32.StdEncoding.EncodeToString(h[:])[:12])
}

// setupNetwork creates the Run's network and loads its egress rules before
// the container exists. The rules are kept in the Run's state so a
// restarted runner can put them back.
func (p *placement) setupNetwork(ctx context.Context, sp spec.RunSpec) (podman.Network, error) {
	net, err := p.r.pm.NetworkCreate(ctx, networkName(p.runID), bridgeName(p.runID),
		map[string]string{LabelManaged: "true", LabelRun: p.runID})
	if err != nil {
		return net, err
	}
	es := &egressState{Interface: net.Interface, Gateway: net.Gateway.String(),
		Unrestricted: sp.Network.Unrestricted, Rules: sp.Network.Egress}
	p.mu.Lock()
	p.state.Egress = es
	p.mu.Unlock()
	if err := p.saveState(); err != nil {
		return net, err
	}
	return net, p.applyEgress(ctx, es)
}

// applyEgress loads the Run's rules and starts its DNS stub; also how a
// restarted runner restores a re-adopted Run's.
func (p *placement) applyEgress(ctx context.Context, es *egressState) error {
	gw, err := netip.ParseAddr(es.Gateway)
	if err != nil {
		return err
	}
	return p.r.egress.Apply(ctx, es.Interface, gw, es.Unrestricted, es.Rules, func(l egress.Lookup) {
		p.event(ctx, "dns", map[string]any{"name": l.Name, "allowed": l.Allowed, "answers": l.Answers})
	})
}

// blockedForRuns is what no Run may reach on this host, besides the
// always-blocked ranges: the control plane, by every address its URL's
// host resolves to now.
func blockedForRuns(ctx context.Context, luxURL string) ([]netip.Prefix, error) {
	u, err := url.Parse(luxURL)
	if err != nil {
		return nil, err
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolving the control plane %s: %w", u.Hostname(), err)
	}
	var out []netip.Prefix
	for _, a := range addrs {
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// dnsArgs point the Run's resolver at its egress stub on the gateway
// (Podman's DNS is off on its network). Unrestricted Runs get Podman's
// default: the host's resolvers.
func dnsArgs(sp spec.RunSpec, net podman.Network) []string {
	if sp.Network.Unrestricted {
		return nil
	}
	return []string{"--dns", net.Gateway.String()}
}
