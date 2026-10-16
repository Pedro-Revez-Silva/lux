package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/server"
)

// zone and region-public price both directions; internet and
// provider:<region> outbound only; .rx/.tx one direction.
func TestParseTransferPrices(t *testing.T) {
	got, err := parseTransferPrices("internet=0.0010, zone=0.0, region-public.rx=0.01,provider:eu-west-1=0.02", "0.005", "USD")
	if err != nil {
		t.Fatal(err)
	}
	want := &server.DataTransferPrices{Currency: "USD", PublicIPv4PerHour: "0.005",
		Internet: server.PerGB{Tx: "0.0010"}, Zone: server.PerGB{Rx: "0.0", Tx: "0.0"}, RegionPublic: server.PerGB{Rx: "0.01"},
		Provider: map[string]server.PerGB{"eu-west-1": {Tx: "0.02"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if p, err := parseTransferPrices("", "", ""); p != nil || err != nil {
		t.Errorf("no flags: %+v %v", p, err)
	}
	for _, bad := range []string{"internet", "lan=1", "zone.in=1", "provider:=1"} {
		if _, err := parseTransferPrices(bad, "", "USD"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := parseTransferPrices("internet=1", "", ""); err == nil || !strings.Contains(err.Error(), "--currency") {
		t.Errorf("no currency: %v", err)
	}
}

func TestTransferPriceLines(t *testing.T) {
	lines := transferPriceLines(&server.DataTransferPrices{Currency: "USD", Internet: server.PerGB{Tx: "0.09"},
		Zone: server.PerGB{Rx: "0.01", Tx: "0.01"}, PublicIPv4PerHour: "0.005"})
	got := strings.Join(lines, "\n")
	for _, w := range []string{"internet", "tx  0.09 USD/GB", "zone", "rx  0.01 USD/GB", "public IPv4", "0.005 USD/h"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	if len(lines) != 4 {
		t.Errorf("%d lines, want 4:\n%s", len(lines), got)
	}
}
