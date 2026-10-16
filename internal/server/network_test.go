package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	s.recordNetwork(ctx)
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
	s.recordNetwork(ctx)
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
	s.recordNetwork(ctx)
	if getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); h.Network == nil || h.Network.PublicIPv4 != 3 {
		t.Errorf("a failed call cleared the network: %+v", h.Network)
	}
}

func TestRecordNetworkColdFailureAndPartialSubnets(t *testing.T) {
	s, keys, p := networkFixture(t)
	ctx := context.Background()
	s.recordNetwork(ctx)
	var before proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &before)
	fresh := &networkProvider{nets: map[string]HostNetwork{"i-1": {Zone: "eu-west-1a", PublicIPv4: 4, SubnetsUnavailable: true}, "i-2": hostA()}, prefixErr: errors.New("unavailable")}
	s.cfg.Providers["ec2"] = fresh
	s.recordNetwork(ctx)
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
	s.recordNetwork(ctx)
	var before, after proto.NetworkClasses
	systemScan(t, s, `SELECT net_classes FROM hosts WHERE id='h1'`, nil, &before)
	n := hostA()
	n.PublicIPv4 = 9
	s.cfg.Providers["ec2"] = &networkProvider{nets: map[string]HostNetwork{"i-1": n}, prefixErr: errors.New("unavailable")}
	s.recordNetwork(ctx)
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
	s.recordNetwork(context.Background())
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
	s.recordNetwork(ctx)
	var before, after string
	systemScan(t, s, `SELECT xmin::text FROM hosts WHERE id = 'h1'`, nil, &before)
	s.recordNetwork(ctx)
	systemScan(t, s, `SELECT xmin::text FROM hosts WHERE id = 'h1'`, nil, &after)
	if before != after {
		t.Errorf("an unchanged network was rewritten (xmin %s -> %s)", before, after)
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

	s.recordNetwork(ctx)
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

func TestRunnerNetworkEndpoint(t *testing.T) {
	s, _, _ := networkFixture(t)
	ctx := context.Background()
	raw := ids.Secret("luxh")
	execSQL(t, s, ctx, `INSERT INTO host_tokens (id, tenant_id, pool_id, token_hash) VALUES ('tok1', 't1', 'pec2', $1)`, ids.Hash(raw))
	execSQL(t, s, ctx, `UPDATE hosts SET token_id = 'tok1' WHERE id IN ('h1', 'h2')`)
	s.recordNetwork(ctx)
	get := func(name, token string) (int, proto.NetworkClasses) {
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

// Heartbeat locks hosts then placements; an exit status locks the run, then
// the placement, then the host. That inverse order deadlocks on main too, so
// 40P01 is retried here; it is not a production retry.
func TestNetworkConcurrentReportsAndExit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	serversFixture(t, s, ctx)
	start := make(chan struct{})
	done := make(chan error, 3)
	for _, net := range []map[string]proto.NetBytes{{"zone": {Rx: 10}, "internet": {Tx: 4}}, {"zone": {Tx: 20}, "provider:us-east-1": {Rx: 7}}} {
		go func() {
			<-start
			var err error
			for range 3 {
				err = s.heartbeat(ctx, "h1", proto.Heartbeat{Usage: &proto.HostUsage{Net: net}, Leases: []proto.LivePlacement{{RunID: r1, Epoch: 1, Usage: &proto.Usage{Net: net}}}})
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "40P01" {
					break
				}
			}
			done <- err
		}()
	}
	go func() {
		<-start
		var err error
		for range 3 {
			err = reportUsageStatus(s, ctx, proto.Status{State: "exited", Usage: &proto.Usage{Net: map[string]proto.NetBytes{"zone": {Rx: 10, Tx: 20}, "internet": {Tx: 4}, "provider:us-east-1": {Rx: 7}, "unknown": {Tx: 23}}}})
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "40P01" {
				break
			}
		}
		done <- err
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
