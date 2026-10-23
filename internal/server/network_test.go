package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
)

// networkProvider answers Network from nets and Prefixes from prefixes (or
// fails with their errors), and counts calls.
type networkProvider struct {
	fakeLaunchProvider
	nets        map[string]HostNetwork
	netErr      error
	prefixes    ProviderPrefixes
	prefixErr   error
	netCalls    [][]string
	prefixCalls int
}

func (p *networkProvider) Network(_ context.Context, _ json.RawMessage, ids []string) (map[string]HostNetwork, error) {
	p.netCalls = append(p.netCalls, slices.Clone(ids))
	if p.netErr != nil {
		return nil, p.netErr
	}
	out := map[string]HostNetwork{}
	for _, id := range ids {
		if n, ok := p.nets[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

func (p *networkProvider) Prefixes(context.Context, json.RawMessage) (ProviderPrefixes, error) {
	p.prefixCalls++
	return p.prefixes, p.prefixErr
}

func hostA() HostNetwork {
	return HostNetwork{SubnetCIDRs: []string{"10.0.0.0/24"}, Zone: "eu-west-1a", PublicIPv4: 1, VPCSubnets: []NetworkSubnet{
		{CIDR: "10.0.0.0/24", Zone: "eu-west-1a"}, {CIDR: "2001:db8::/64", Zone: "eu-west-1a"},
		{CIDR: "10.0.1.0/24", Zone: "eu-west-1b"}, {CIDR: "10.0.2.0/24", Zone: "eu-west-1c"}}}
}

func catalogEU(at time.Time) ProviderPrefixes {
	return ProviderPrefixes{Source: proto.NetSourceEC2, FetchedAt: at,
		RegionService: []string{"52.218.0.0/17"}, RegionPublic: []string{"52.16.0.0/15"},
		ProviderGlobal: []string{"15.177.0.0/18"}, ProviderGlobalFree: []string{"13.32.0.0/15"},
		Provider: map[string][]string{"us-east-1": {"3.80.0.0/12"}}}
}

func TestNetworkClassesFromHostAndCatalog(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nc := networkClasses(hostA(), catalogEU(at))
	if !slices.Equal(nc.Local, []string{"10.0.0.0/24", "2001:db8::/64"}) || !slices.Equal(nc.Zone, []string{"10.0.1.0/24", "10.0.2.0/24"}) {
		t.Errorf("local %v zone %v: Local must be the host's zone only", nc.Local, nc.Zone)
	}
	if nc.Source != proto.NetSourceEC2 || nc.FetchedAt == nil || !nc.FetchedAt.Equal(at) || !nc.Catalog() {
		t.Errorf("provenance %q %v", nc.Source, nc.FetchedAt)
	}
	if !slices.Equal(nc.RegionService, []string{"52.218.0.0/17"}) || !slices.Equal(nc.RegionPublic, []string{"52.16.0.0/15"}) ||
		!slices.Equal(nc.ProviderGlobal, []string{"15.177.0.0/18"}) || !slices.Equal(nc.ProviderGlobalFree, []string{"13.32.0.0/15"}) || !slices.Equal(nc.Provider["us-east-1"], []string{"3.80.0.0/12"}) {
		t.Errorf("provider lists %+v", nc)
	}
	if again := networkClasses(hostA(), catalogEU(at)); again.Version != nc.Version || nc.Version == "" {
		t.Errorf("version not stable: %q %q", nc.Version, again.Version)
	}
	moved := hostA()
	moved.Zone = "eu-west-1b"
	if networkClasses(moved, catalogEU(at)).Version == nc.Version {
		t.Error("a host in another zone has the same version")
	}
	// No catalog: Source none, so the runner counts unlisted traffic as
	// unknown, never internet.
	bare := networkClasses(hostA(), ProviderPrefixes{})
	if bare.Source != proto.NetSourceNone || bare.Catalog() || bare.FetchedAt != nil || len(bare.Local) == 0 {
		t.Errorf("without a catalog: %+v", bare)
	}
}

// networkFixture: tenant t1 with an ec2 pool and two live provider hosts,
// a terminated one and a static host.
func networkFixture(t *testing.T) (*Server, map[string]string, *networkProvider) {
	t.Helper()
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES ('pec2', 't1', 'burst', 'ec2', '{"region":"eu-west-1"}')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, pool_id, name, state, provider_id, provision_requested_at, launch_template) VALUES
		('h1', 't1', 'pec2', 'h1', 'ready', 'i-1', now(), '{"region":"eu-west-1"}'),
		('h2', 't1', 'pec2', 'h2', 'ready', 'i-2', now(), '{"region":"eu-west-1"}'),
		('gone', 't1', 'pec2', 'gone', 'terminated', 'i-4', now(), '{"region":"eu-west-1"}')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, name, state) VALUES ('static1', 't1', 'static1', 'ready')`)
	p := &networkProvider{nets: map[string]HostNetwork{"i-1": hostA()}, prefixes: catalogEU(time.Now().UTC().Truncate(time.Second))}
	s.cfg.Providers = map[string]Provider{"ec2": p}
	return s, keys, p
}

func TestRecordNetworkRefreshesLiveProviderHosts(t *testing.T) {
	s, keys, p := networkFixture(t)
	ctx := context.Background()
	s.recordNetwork(ctx, false)
	if fmt.Sprint(p.netCalls) != "[[i-1 i-2]]" {
		t.Errorf("calls %v: one per region, live provider hosts only", p.netCalls)
	}
	var h Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); code != http.StatusOK || h.Network == nil ||
		h.Network.PublicIPv4 != 1 || h.Network.Zone != "eu-west-1a" || len(h.Network.VPCSubnets) != 4 {
		t.Fatalf("GET host: %d %+v", code, h.Network)
	}
	var h2 Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h2", &h2); code != http.StatusOK || h2.Network != nil {
		t.Errorf("unknown network shown: %d %+v", code, h2.Network)
	}
	var v1 string
	systemScan(t, s, `SELECT net_classes->>'version' FROM hosts WHERE id = 'h1'`, nil, &v1)

	// An address attached later: the next pass refreshes the stored value.
	n := hostA()
	n.PublicIPv4 = 3
	p.nets["i-1"] = n
	s.recordNetwork(ctx, false)
	if getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); h.Network.PublicIPv4 != 3 {
		t.Errorf("publicIPv4 not refreshed: %+v", h.Network)
	}
	// Same classes: same version (publicIPv4 is not a list).
	var v2 string
	systemScan(t, s, `SELECT net_classes->>'version' FROM hosts WHERE id = 'h1'`, nil, &v2)
	if v1 != v2 {
		t.Errorf("version changed with no list change: %s -> %s", v1, v2)
	}

	// A failing call keeps what is stored.
	p.netErr = errors.New("UnauthorizedOperation")
	s.recordNetwork(ctx, false)
	if getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); h.Network == nil || h.Network.PublicIPv4 != 3 {
		t.Errorf("a failed call cleared the network: %+v", h.Network)
	}
}

func TestRecordNetworkColdFailureAndPartialSubnets(t *testing.T) {
	s, keys, p := networkFixture(t)
	ctx := context.Background()
	s.recordNetwork(ctx, false)
	var before proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &before)
	fresh := &networkProvider{nets: map[string]HostNetwork{"i-1": {Zone: "eu-west-1a", PublicIPv4: 4, SubnetsUnavailable: true}, "i-2": hostA()}, prefixErr: errors.New("unavailable")}
	s.cfg.Providers["ec2"] = fresh
	s.recordNetwork(ctx, false)
	var n HostNetwork
	var after proto.NetworkClasses
	systemScan(t, s, `SELECT network,net_classes FROM hosts WHERE id='h1'`, nil, &n, &after)
	if n.PublicIPv4 != 4 || !slices.Equal(n.SubnetCIDRs, p.nets["i-1"].SubnetCIDRs) || len(n.VPCSubnets) != 4 {
		t.Fatal(n)
	}
	if after.Version == before.Version || after.Catalog() || len(after.Zone) != 0 || !slices.Equal(after.Local, before.Local) {
		t.Fatalf("partial discovery must retain only known Local: %+v", after)
	}
	var h Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); code != http.StatusOK || h.Network == nil || !h.Network.SubnetsUnavailable {
		t.Fatalf("partial network not visible: %d %+v", code, h.Network)
	}
	var other HostNetwork
	systemScan(t, s, `SELECT network FROM hosts WHERE id='h2'`, nil, &other)
	if len(other.VPCSubnets) != 4 {
		t.Fatal(other)
	}
}

func TestRecordNetworkColdPrefixFailureCompleteHost(t *testing.T) {
	s, keys, _ := networkFixture(t)
	ctx := context.Background()
	s.recordNetwork(ctx, false)
	var before, after proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &before)
	n := hostA()
	n.PublicIPv4 = 9
	s.cfg.Providers["ec2"] = &networkProvider{nets: map[string]HostNetwork{"i-1": n}, prefixErr: errors.New("unavailable")}
	s.recordNetwork(ctx, false)
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &after)
	if after.Version != before.Version || !after.Catalog() {
		t.Fatalf("cold prefix failure lost catalog: before=%+v after=%+v", before, after)
	}
	var h Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); code != http.StatusOK || h.Network == nil || h.Network.PublicIPv4 != 9 {
		t.Fatalf("host facts not refreshed: %d %+v", code, h.Network)
	}
}

func TestRecordNetworkColdPartialHasNoCatalog(t *testing.T) {
	s, _, p := networkFixture(t)
	p.nets = map[string]HostNetwork{"i-1": {SubnetsUnavailable: true, PublicIPv4: 1}}
	s.recordNetwork(context.Background(), false)
	var nc proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &nc)
	if nc.Catalog() || len(nc.Local) != 0 || len(nc.Zone) != 0 {
		t.Fatalf("cold partial discovery installed classes: %+v", nc)
	}
}

// Unchanged answers write nothing: the row's xmin stays.
func TestRecordNetworkWritesOnlyChanges(t *testing.T) {
	s, _, _ := networkFixture(t)
	ctx := context.Background()
	s.recordNetwork(ctx, false)
	var before, after string
	systemScan(t, s, `SELECT xmin::text FROM hosts WHERE id = 'h1'`, nil, &before)
	s.recordNetwork(ctx, false)
	systemScan(t, s, `SELECT xmin::text FROM hosts WHERE id = 'h1'`, nil, &after)
	if before != after {
		t.Errorf("an unchanged network was rewritten (xmin %s -> %s)", before, after)
	}
}

// A host that registers is discovered by the next provision() pass that is
// not a provider check; once it has a catalog, those passes ask nothing.
func TestProvisionDiscoversNewHostNetwork(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() WHERE id = 'h1'`)
	provisionPass(t, s)
	if fmt.Sprint(p.netCalls) != "[[i-1]]" || p.prefixCalls != 1 {
		t.Fatalf("calls %v prefixes %d: want the registered host without a catalog only", p.netCalls, p.prefixCalls)
	}
	var nc proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id = 'h1'`, nil, &nc)
	if !nc.Catalog() || !slices.Equal(nc.Local, []string{"10.0.0.0/24", "2001:db8::/64"}) {
		t.Fatalf("net_classes after one pass: %+v", nc)
	}
	var regionRows int
	systemScan(t, s, `SELECT count(*) FROM network_catalog WHERE provider = 'ec2' AND region = 'eu-west-1'`, nil, &regionRows)
	if regionRows != 1 {
		t.Fatalf("the narrow pass stored %d region catalogs", regionRows)
	}
	// Past the retry spacing, so only the stored catalog can stop a call.
	now = now.Add(2 * netRetryEvery)
	provisionPass(t, s)
	if len(p.netCalls) != 1 || p.prefixCalls != 1 {
		t.Errorf("calls %v prefixes %d: a pass with nothing missing called the provider", p.netCalls, p.prefixCalls)
	}
}

// A host whose discovery fails is asked again no more often than every
// netRetryEvery, and not at all past netNewHostWindow (the full pass then
// covers it).
func TestProvisionNewHostNetworkRetrySpacing(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() WHERE id = 'h1'`)
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() - interval '6 minutes' WHERE id = 'h2'`)
	p.netErr = errors.New("RequestLimitExceeded")
	var at []time.Duration
	for off := time.Duration(0); off <= 25*time.Second; off += time.Second {
		now = now.Add(time.Second)
		before := len(p.netCalls)
		provisionPass(t, s)
		if len(p.netCalls) > before {
			at = append(at, off)
		}
	}
	if want := []time.Duration{0, 10 * time.Second, 20 * time.Second}; !slices.Equal(at, want) {
		t.Fatalf("failing host asked at %v, want %v", at, want)
	}
	for _, ids := range p.netCalls {
		if !slices.Equal(ids, []string{"i-1"}) {
			t.Fatalf("calls %v: a host registered before the window was asked", p.netCalls)
		}
	}
	// A partial answer (Source none) is still missing a catalog: retried.
	p.netErr = nil
	p.prefixErr, p.prefixes = errors.New("unavailable"), ProviderPrefixes{}
	now = now.Add(netRetryEvery)
	provisionPass(t, s)
	var nc proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id = 'h1'`, nil, &nc)
	if nc.Source != proto.NetSourceNone {
		t.Fatalf("net_classes %+v", nc)
	}
	p.prefixErr, p.prefixes = nil, catalogEU(time.Now().UTC().Truncate(time.Second))
	now = now.Add(netRetryEvery)
	provisionPass(t, s)
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id = 'h1'`, nil, &nc)
	if !nc.Catalog() {
		t.Fatalf("Source none not retried: %+v", nc)
	}
}

// Every Welcome carries runners.control_plane_ports, none when unset.
func TestWelcomeCarriesControlPlanePorts(t *testing.T) {
	s, _, _ := networkFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ('tok1', 't1', 'pec2', 'hash1')`)
	execSQL(t, s, ctx, `UPDATE hosts SET token_id = 'tok1' WHERE id = 'h1'`)
	tok := &hostToken{ID: "tok1", TenantID: new("t1"), PoolID: new("pec2")}
	hello := proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-1"}
	w, err := s.registerHost(ctx, tok, hello)
	if err != nil || w.ControlPlanePorts != nil {
		t.Fatalf("none configured: %v %v", w.ControlPlanePorts, err)
	}
	s.cfg.ControlPlanePorts = []int{8090}
	w, err = s.registerHost(ctx, tok, hello)
	if err != nil || !slices.Equal(w.ControlPlanePorts, []int{8090}) {
		t.Fatalf("configured: %v %v", w.ControlPlanePorts, err)
	}
}

// Hello never asks the provider; it hands the runner what is stored, and
// the heartbeat's ack names its version.
func TestWelcomeAndAckCarryNetwork(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ('tok1', 't1', 'pec2', 'hash1')`)
	execSQL(t, s, ctx, `UPDATE hosts SET token_id = 'tok1' WHERE id = 'h1'`)
	tok := &hostToken{ID: "tok1", TenantID: new("t1"), PoolID: new("pec2")}
	w, err := s.registerHost(ctx, tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Network != nil || p.prefixCalls != 0 || len(p.netCalls) != 0 {
		t.Fatalf("hello: network %+v, provider calls %d/%d", w.Network, p.prefixCalls, len(p.netCalls))
	}
	ack := s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(proto.Heartbeat{
		Usage: &proto.HostUsage{Net: map[string]proto.NetBytes{"unknown": {Rx: 10, Tx: 20}}}})})
	if ack.Type != proto.MsgAck || len(ack.Data) != 0 {
		t.Errorf("ack with nothing to say: %s %s", ack.Type, ack.Data)
	}

	s.recordNetwork(ctx, false)
	w, err = s.registerHost(ctx, tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Network == nil || !w.Network.Catalog() || len(w.Network.Provider["us-east-1"]) != 1 {
		t.Fatalf("welcome network %+v", w.Network)
	}
	if !maps.Equal(w.HostNet, map[string]proto.NetBytes{"unknown": {Rx: 10, Tx: 20}}) {
		t.Errorf("welcome hostNet %v", w.HostNet)
	}
	ack = s.handleReport(ctx, "h1", proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(proto.Heartbeat{})})
	var a proto.Ack
	if err := json.Unmarshal(ack.Data, &a); err != nil || a.NetworkVersion != w.Network.Version {
		t.Errorf("ack %s, want version %s", ack.Data, w.Network.Version)
	}
}

// getRunnerNetwork is GET /runner/v1/network for host name with token.
func getRunnerNetwork(t *testing.T, s *Server, name, token string) (int, proto.NetworkClasses) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/runner/v1/network?name="+name, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var nc proto.NetworkClasses
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &nc)
	}
	return w.Code, nc
}

func heartbeatAckVersion(t *testing.T, s *Server, hostID string) string {
	t.Helper()
	ack := s.handleReport(context.Background(), hostID, proto.Frame{Type: proto.MsgHeartbeat, Data: proto.Marshal(proto.Heartbeat{})})
	var a proto.Ack
	if len(ack.Data) > 0 {
		if err := json.Unmarshal(ack.Data, &a); err != nil {
			t.Fatal(err)
		}
	}
	return a.NetworkVersion
}

// hostTokenFor issues token tok1 (pool pec2) to hostIDs and returns its secret.
func hostTokenFor(t *testing.T, s *Server, hostIDs ...string) string {
	t.Helper()
	raw := ids.Secret("luxh")
	execSQL(t, s, context.Background(), `INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ('tok1', 't1', 'pec2', $1)`, ids.Hash(raw))
	execSQL(t, s, context.Background(), `UPDATE hosts SET token_id = 'tok1' WHERE id = ANY($1)`, hostIDs)
	return raw
}

// helloH1 registers h1 (provider id i-1) under tok1 with caps.
func helloH1(t *testing.T, s *Server, caps ...string) proto.Welcome {
	t.Helper()
	tok := &hostToken{ID: "tok1", TenantID: new("t1"), PoolID: new("pec2")}
	w, err := s.registerHost(context.Background(), tok, proto.Hello{Name: "h1", ProtocolVersion: proto.Version, Arch: "arm64", ProviderID: "i-1", Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A new host is sent provisional classes at registration, built from its
// region's network_catalog row, which a provisioner pass stored: no
// provider call on hello, never another provider's or region's row, only to
// a runner with CapProvisionalNet, never stored in hosts.net_classes. Once
// discovery stores the host's own, the ack's version moves and the
// endpoint serves those.
func TestProvisionalNetworkAtRegistration(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	raw := hostTokenFor(t, s, "h1")
	foreign := `{"source":"ec2-ip-ranges","regionPublic":["198.51.100.0/24"]}`
	execSQL(t, s, ctx, `INSERT INTO network_catalog (provider, region, catalog, fetched_at, version) VALUES
		('ec2', 'us-east-1', $1, now(), 'other-region'), ('gce', 'eu-west-1', $1, now(), 'other-provider')`, foreign)
	p.nets = map[string]HostNetwork{} // the host's own network is not discovered yet

	// No row for ec2/eu-west-1 yet: nothing, as before.
	if w := helloH1(t, s, proto.CapProvisionalNet); w.Network != nil {
		t.Fatalf("welcome without a region catalog: %+v", w.Network)
	}
	if v := heartbeatAckVersion(t, s, "h1"); v != "" {
		t.Fatalf("ack version %q without a region catalog", v)
	}

	s.recordNetwork(ctx, false)
	var xmin1, xmin2 string
	systemScan(t, s, `SELECT coalesce((SELECT xmin::text FROM network_catalog WHERE provider = 'ec2' AND region = 'eu-west-1'), '')`, nil, &xmin1)
	s.recordNetwork(ctx, false)
	s.recordNetwork(ctx, true)
	systemScan(t, s, `SELECT coalesce((SELECT xmin::text FROM network_catalog WHERE provider = 'ec2' AND region = 'eu-west-1'), '')`, nil, &xmin2)
	if xmin1 != xmin2 {
		t.Errorf("an unchanged region catalog was rewritten (xmin %s -> %s)", xmin1, xmin2)
	}
	// A changed catalog replaces the row under a new version.
	var before, after string
	systemScan(t, s, `SELECT version FROM network_catalog WHERE provider = 'ec2' AND region = 'eu-west-1'`, nil, &before)
	p.prefixes.RegionPublic = append(slices.Clone(p.prefixes.RegionPublic), "54.72.0.0/15")
	s.recordNetwork(ctx, false)
	systemScan(t, s, `SELECT version FROM network_catalog WHERE provider = 'ec2' AND region = 'eu-west-1'`, nil, &after)
	if after == before {
		t.Fatalf("a changed region catalog kept version %s", before)
	}

	netCalls, prefixCalls := len(p.netCalls), p.prefixCalls
	w := helloH1(t, s, proto.CapProvisionalNet)
	if len(p.netCalls) != netCalls || p.prefixCalls != prefixCalls {
		t.Fatalf("hello called the provider: network %d -> %d, prefixes %d -> %d", netCalls, len(p.netCalls), prefixCalls, p.prefixCalls)
	}
	nc := w.Network
	want := p.prefixes
	if nc == nil || !nc.Provisional || !nc.Catalog() || nc.Source != want.Source || nc.FetchedAt == nil || !nc.FetchedAt.Equal(want.FetchedAt) || nc.Version != after {
		t.Fatalf("provisional welcome: %+v, want version %s", nc, after)
	}
	if !slices.Equal(nc.RegionService, want.RegionService) || !slices.Equal(nc.RegionPublic, want.RegionPublic) ||
		!slices.Equal(nc.ProviderGlobal, want.ProviderGlobal) || !slices.Equal(nc.ProviderGlobalFree, want.ProviderGlobalFree) ||
		!maps.EqualFunc(nc.Provider, want.Provider, slices.Equal) {
		t.Errorf("region half %+v, want %+v", nc, want)
	}
	if len(nc.Local) != 0 || !slices.Equal(nc.Zone, []string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}) {
		t.Errorf("local %v zone %v", nc.Local, nc.Zone)
	}
	var stored *string
	systemScan(t, s, `SELECT net_classes::text FROM hosts WHERE id = 'h1'`, nil, &stored)
	if stored != nil {
		t.Fatalf("provisional classes stored: %s", *stored)
	}
	provisional := nc.Version
	settled := *nc
	settled.Provisional = false
	if netClassesVersion(settled) == provisional {
		t.Errorf("provisional version %s equals the same lists' without Provisional", provisional)
	}
	if v := heartbeatAckVersion(t, s, "h1"); v != provisional {
		t.Errorf("ack version %q, want the provisional %q", v, provisional)
	}
	if code, got := getRunnerNetwork(t, s, "h1", raw); code != http.StatusOK || !reflect.DeepEqual(got, *nc) {
		t.Errorf("endpoint while undiscovered: %d %+v, want the welcome's %+v", code, got, *nc)
	}

	// A runner without the capability would price its own subnet as zone.
	if w := helloH1(t, s); w.Network != nil {
		t.Fatalf("welcome to a runner without %s: %+v", proto.CapProvisionalNet, w.Network)
	}
	if v := heartbeatAckVersion(t, s, "h1"); v != "" {
		t.Errorf("ack version %q for a runner without the capability", v)
	}

	helloH1(t, s, proto.CapProvisionalNet)
	p.nets = map[string]HostNetwork{"i-1": hostA()}
	s.netTried = nil
	s.recordNetwork(ctx, true)
	discovered := heartbeatAckVersion(t, s, "h1")
	if discovered == "" || discovered == provisional {
		t.Fatalf("ack version after discovery %q (provisional %q)", discovered, provisional)
	}
	code, got := getRunnerNetwork(t, s, "h1", raw)
	if code != http.StatusOK || got.Provisional || got.Version != discovered || !slices.Equal(got.Local, []string{"10.0.0.0/24", "2001:db8::/64"}) {
		t.Errorf("endpoint after discovery: %d %+v", code, got)
	}
}

// A host with stored classes is served them even when they are Source none
// (a failed prefix fetch): provisional classes are only for a host with
// none stored, so they never replace a discovered Local and Zone.
func TestStoredSourceNoneClassesBeatProvisional(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	raw := hostTokenFor(t, s, "h1")
	catalog := p.prefixes
	p.prefixes, p.prefixErr = ProviderPrefixes{}, errors.New("unavailable")
	s.recordNetwork(ctx, false)
	if err := s.storeRegionCatalog(ctx, "ec2", "eu-west-1", catalog); err != nil {
		t.Fatal(err)
	}
	var stored proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id = 'h1'`, nil, &stored)
	if stored.Source != proto.NetSourceNone || len(stored.Local) == 0 || len(stored.Zone) == 0 {
		t.Fatalf("setup: stored %+v, want Source none with Local and Zone", stored)
	}
	w := helloH1(t, s, proto.CapProvisionalNet)
	if w.Network == nil || w.Network.Provisional || !reflect.DeepEqual(*w.Network, stored) {
		t.Fatalf("welcome %+v, want the stored %+v", w.Network, stored)
	}
	if v := heartbeatAckVersion(t, s, "h1"); v != stored.Version {
		t.Errorf("ack version %q, want the stored %q", v, stored.Version)
	}
	if code, got := getRunnerNetwork(t, s, "h1", raw); code != http.StatusOK || got.Provisional || !reflect.DeepEqual(got, stored) {
		t.Errorf("endpoint: %d %+v, want the stored %+v", code, got, stored)
	}
}

// The full pass stores the region catalog of a pool with no hosts, so the
// first host of a new pool's region registers with one already there.
func TestRecordNetworkWarmsEmptyPoolRegion(t *testing.T) {
	s, _, p := networkFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES
		('pus', 't1', 'us', 'ec2', '{"region":"us-east-1"}'),
		('pold', 't1', 'old', 'ec2', '{"region":"ap-south-1"}')`)
	execSQL(t, s, ctx, `UPDATE pools SET retired = true WHERE id = 'pold'`)
	s.recordNetwork(ctx, true)
	var n int
	systemScan(t, s, `SELECT count(*) FROM network_catalog`, nil, &n)
	if n != 0 || p.prefixCalls != 0 {
		t.Fatalf("narrow pass with nothing missing: %d rows, %d prefix calls", n, p.prefixCalls)
	}
	// A host the narrow pass discovers: its region only, no warm-up.
	execSQL(t, s, ctx, `UPDATE hosts SET registered_at = now() WHERE id = 'h1'`)
	s.recordNetwork(ctx, true)
	var regions []string
	systemScan(t, s, `SELECT array_agg(region ORDER BY region) FROM network_catalog`, nil, &regions)
	if !slices.Equal(regions, []string{"eu-west-1"}) || p.prefixCalls != 1 {
		t.Fatalf("narrow pass: regions %v, %d prefix calls; want the host's region once", regions, p.prefixCalls)
	}
	execSQL(t, s, ctx, `DELETE FROM network_catalog`)
	p.prefixCalls = 0
	s.recordNetwork(ctx, false)
	rows := map[string]bool{}
	systemScan(t, s, `SELECT count(*) FROM network_catalog`, nil, &n)
	for _, r := range []string{"eu-west-1", "us-east-1", "ap-south-1"} {
		var ok bool
		systemScan(t, s, `SELECT EXISTS (SELECT 1 FROM network_catalog WHERE provider = 'ec2' AND region = $1)`, []any{r}, &ok)
		rows[r] = ok
	}
	if !rows["eu-west-1"] || !rows["us-east-1"] || rows["ap-south-1"] || n != 2 || p.prefixCalls != 2 {
		t.Errorf("rows %v (%d), prefix calls %d: want the live pools' regions once each", rows, n, p.prefixCalls)
	}
	// No real source: nothing is stored.
	execSQL(t, s, ctx, `DELETE FROM network_catalog`)
	p.prefixes = ProviderPrefixes{}
	s.recordNetwork(ctx, false)
	if systemScan(t, s, `SELECT count(*) FROM network_catalog`, nil, &n); n != 0 {
		t.Errorf("%d rows from a catalog with no source", n)
	}
}

func TestRunnerNetworkEndpoint(t *testing.T) {
	s, _, _ := networkFixture(t)
	ctx := context.Background()
	raw := hostTokenFor(t, s, "h1", "h2")
	s.recordNetwork(ctx, false)
	get := func(name, token string) (int, proto.NetworkClasses) { return getRunnerNetwork(t, s, name, token) }
	if code, nc := get("h1", raw); code != http.StatusOK || !nc.Catalog() || len(nc.Zone) != 2 {
		t.Errorf("h1: %d %+v", code, nc)
	}
	if code, nc := get("h2", raw); code != http.StatusOK || nc.Source != proto.NetSourceNone || nc.Catalog() {
		t.Errorf("h2 (nothing known): %d %+v", code, nc)
	}
	if code, _ := get("h1", "luxh_wrong"); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	if code, _ := get("static1", raw); code == http.StatusOK {
		t.Error("a host of another token answered")
	}
}

// Placement and host classes merge per key and direction by maximum, from
// heartbeats and exit statuses alike; absent keys are kept; the Run's usage
// sums its placements; and GET shows them. A platform host's own traffic,
// static catalog and transfer prices are shown to operators only, on each
// of the three read paths; a tenant's own host shows its own.
func TestHostNetworkTotalsVisibility(t *testing.T) {
	s := testServer(t)
	keys := poolFixture(t, s, context.Background())
	execSQL(t, s, context.Background(), `UPDATE hosts SET host_net_classes='{"internet":{"tx":123}}',
		network_static = '{"local":["10.9.0.0/24"]}', transfer_prices = '{"currency":"USD","internet":{"tx":"0.001"}}' WHERE id IN ('hp','ha')`)
	operatorOnly := func(h Host) bool {
		return h.NetClasses["internet"].Tx == 123 && h.StaticNetwork != nil && h.TransferPrices != nil
	}
	hidden := func(h Host) bool {
		return h.NetClasses == nil && h.StaticNetwork == nil && h.TransferPrices == nil
	}
	for _, c := range []struct {
		key, query string
		platform   bool
	}{{"a", "", false}, {"b", "", false}, {"op", "", true}, {"op", "tenant=a", false}} {
		for _, endpoint := range []string{"/v1/hosts/hp", "/v1/hosts", "/v1/hosts?limit=100"} {
			path := endpoint
			if c.query != "" {
				if strings.Contains(path, "?") {
					path += "&"
				} else {
					path += "?"
				}
				path += c.query
			}
			var raw json.RawMessage
			if code := getJSON(t, s, keys[c.key], path, &raw); code != 200 {
				t.Fatalf("%s: %d", path, code)
			}
			var hosts []Host
			if endpoint == "/v1/hosts/hp" {
				var h Host
				if err := json.Unmarshal(raw, &h); err != nil {
					t.Fatal(err)
				}
				hosts = []Host{h}
			} else {
				var page struct {
					Items []Host `json:"hosts"`
				}
				if err := json.Unmarshal(raw, &hosts); err != nil {
					if err := json.Unmarshal(raw, &page); err != nil {
						t.Fatal(err)
					}
					hosts = page.Items
				}
			}
			seen := false
			for _, h := range hosts {
				if h.ID == "hp" {
					seen = true
					if c.platform && !operatorOnly(h) || !c.platform && !hidden(h) {
						t.Fatalf("%s %s leaked/omitted: %v %+v %+v", c.key, path, h.NetClasses, h.StaticNetwork, h.TransferPrices)
					}
				}
				if h.ID == "ha" && !operatorOnly(h) {
					t.Fatalf("owner's own network missing: %v %+v %+v", h.NetClasses, h.StaticNetwork, h.TransferPrices)
				}
			}
			if !seen {
				t.Fatalf("platform host not found: %s %s", path, raw)
			}
		}
	}
}

// A platform static pool's default transfer prices and public IPv4 charge
// are operator-only, like its hosts'; a tenant's own pool shows its own.
func TestPoolTransferPricesVisibility(t *testing.T) {
	s := testServer(t)
	keys := poolFixture(t, s, context.Background())
	execSQL(t, s, context.Background(), `UPDATE pools SET transfer_prices = '{"currency":"USD","internet":{"tx":"0.001"},"publicIPv4PerHour":"0.004"}'
		WHERE id IN ('p-shared', 'p-a')`)
	for _, c := range []struct {
		key, query string
		platform   bool
	}{{"a", "", false}, {"b", "", false}, {"op", "", true}, {"op", "?tenant=a", false}} {
		var list PoolList
		if code := getJSON(t, s, keys[c.key], "/v1/pools"+c.query, &list); code != 200 {
			t.Fatalf("%s: %d", c.key, code)
		}
		seen := false
		for _, pl := range list.Pools {
			switch {
			case pl.ID == "p-shared":
				seen = true
				if got := pl.TransferPrices != nil; got != c.platform {
					t.Errorf("%s%s: platform pool transferPrices %+v, want shown %v", c.key, c.query, pl.TransferPrices, c.platform)
				}
			case pl.ID == "p-a" && (pl.TransferPrices == nil || pl.TransferPrices.PublicIPv4PerHour != "0.004"):
				t.Errorf("%s%s: own pool transferPrices %+v", c.key, c.query, pl.TransferPrices)
			}
		}
		if !seen {
			t.Fatalf("%s%s: platform pool not listed", c.key, c.query)
		}
	}
}

// Heartbeats and an exit report for the same host and placement at once:
// every counter is kept (lux_net_max on both rows) and nothing deadlocks
// (TestStatusReportsLockTheHostFirst has the order).
func TestNetworkConcurrentReportsAndExit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	start := make(chan struct{})
	done := make(chan error, 3)
	for _, net := range []map[string]proto.NetBytes{{"zone": {Rx: 10}, "internet": {Tx: 4}}, {"zone": {Tx: 20}, "provider:us-east-1": {Rx: 7}}} {
		go func() {
			<-start
			done <- s.heartbeat(ctx, "h1", proto.Heartbeat{Usage: &proto.HostUsage{Net: net}, Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, Usage: &proto.Usage{Net: net}}}})
		}()
	}
	go func() {
		<-start
		done <- reportUsageStatus(s, ctx, proto.Status{State: "exited", Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Rx: 10, Tx: 20}, "internet": {Tx: 4}, "provider:us-east-1": {Rx: 7}, "unknown": {Tx: 23}}}})
	}()
	close(start)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var host, placement map[string]proto.NetBytes
	var state string
	systemScan(t, s, `SELECT host_net_classes FROM hosts WHERE id='h1'`, nil, &host)
	systemScan(t, s, `SELECT net_classes,state FROM placements WHERE run_id=$1 AND epoch=1`, []any{r1}, &placement, &state)
	want := map[string]proto.NetBytes{"zone": {Rx: 10, Tx: 20}, "internet": {Tx: 4}, "provider:us-east-1": {Rx: 7}}
	if !maps.Equal(host, want) {
		t.Fatal(host)
	}
	want["unknown"] = proto.NetBytes{Tx: 23}
	if !maps.Equal(placement, want) || state != "exited" {
		t.Fatalf("%s %v", state, placement)
	}
}

func TestNetClassesMergeAndExposure(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	beat := func(run, host map[string]proto.NetBytes) {
		t.Helper()
		var hu *proto.HostUsage
		if host != nil {
			hu = &proto.HostUsage{Net: host}
		}
		if err := s.heartbeat(ctx, "h1", proto.Heartbeat{Usage: hu,
			Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, State: "running", Usage: &proto.Usage{Net: run}}}}); err != nil {
			t.Fatal(err)
		}
	}
	beat(map[string]proto.NetBytes{"zone": {Rx: 10, Tx: 5}, "provider:us-east-1": {Tx: 7}}, map[string]proto.NetBytes{"internet": {Tx: 100}})
	beat(map[string]proto.NetBytes{"zone": {Rx: 8, Tx: 9}}, map[string]proto.NetBytes{"internet": {Tx: 90, Rx: 4}})
	beat(nil, nil) // an older runner: nothing changes
	if err := reportUsageStatus(s, ctx, proto.Status{State: "running", Usage: &proto.Usage{Net: map[string]proto.NetBytes{"unknown": {Rx: 1}, "zone": {Tx: 3}}}}); err != nil {
		t.Fatal(err)
	}
	var placement, host map[string]proto.NetBytes
	systemScan(t, s, `SELECT net_classes FROM placements WHERE run_id = $1 AND epoch = 1`, []any{r1}, &placement)
	systemScan(t, s, `SELECT host_net_classes FROM hosts WHERE id = 'h1'`, nil, &host)
	wantP := map[string]proto.NetBytes{"zone": {Rx: 10, Tx: 9}, "provider:us-east-1": {Tx: 7}, "unknown": {Rx: 1}}
	if !maps.Equal(placement, wantP) {
		t.Errorf("placement %v, want %v", placement, wantP)
	}
	if wantH := map[string]proto.NetBytes{"internet": {Tx: 100, Rx: 4}}; !maps.Equal(host, wantH) {
		t.Errorf("host %v, want %v", host, wantH)
	}

	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 2 WHERE id = $1`, r1)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, net_classes) VALUES
		('p2', 't1', $1, 'h1', 2, 'running', '{"zone": {"rx": 1, "tx": 1}}')`, r1)
	var run Run
	if code := getJSON(t, s, key, "/v1/runs/"+r1, &run); code != http.StatusOK {
		t.Fatalf("GET run: %d", code)
	}
	if !maps.Equal(run.Placements[0].NetClasses, wantP) {
		t.Errorf("placement netClasses %v", run.Placements[0].NetClasses)
	}
	wantSum := map[string]proto.NetBytes{"zone": {Rx: 11, Tx: 10}, "provider:us-east-1": {Tx: 7}, "unknown": {Rx: 1}}
	if !maps.Equal(run.Usage.NetClasses, wantSum) {
		t.Errorf("run usage netClasses %v, want %v", run.Usage.NetClasses, wantSum)
	}
}

// lux_net_max is the merge rule alone, against the database.
func TestNetMaxFunction(t *testing.T) {
	s := testServer(t)
	cases := []struct{ a, b, want string }{
		{`{"zone": {"rx": 5, "tx": 1}}`, `{"zone": {"rx": 2, "tx": 9}, "local": {"rx": 3, "tx": 0}}`, `{"local": {"rx": 3, "tx": 0}, "zone": {"rx": 5, "tx": 9}}`},
		{`null`, `{"zone": {"rx": 1, "tx": 2}}`, `{"zone": {"rx": 1, "tx": 2}}`},
		{`{"zone": {"rx": 1, "tx": 2}}`, `null`, `{"zone": {"rx": 1, "tx": 2}}`},
		{`{"zone": {"rx": 9000000000, "tx": 0}}`, `{}`, `{"zone": {"rx": 9000000000, "tx": 0}}`},
		{`{"internet": {"rx": 1, "tx": 0, "rxDelivered": 7}}`, `{"internet": {"rx": 2, "tx": 0, "rxDelivered": 5}}`, `{"internet": {"rx": 2, "tx": 0, "rxDelivered": 7}}`},
		{`{"internet": {"rx": 1, "tx": 0}}`, `{"internet": {"rx": 0, "tx": 0, "rxDelivered": 4}}`, `{"internet": {"rx": 1, "tx": 0, "rxDelivered": 4}}`},
	}
	for _, c := range cases {
		var got string
		var equal bool
		systemScan(t, s, `SELECT lux_net_max($1::jsonb, $2::jsonb)::text, lux_net_max($1::jsonb, $2::jsonb) = $3::jsonb`,
			[]any{c.a, c.b, c.want}, &got, &equal)
		if !equal {
			t.Errorf("lux_net_max(%s, %s) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
	var none *string
	systemScan(t, s, `SELECT lux_net_max(NULL, 'null'::jsonb)::text`, nil, &none)
	if none != nil {
		t.Errorf("both empty: %s, want NULL", *none)
	}
}
