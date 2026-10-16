package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// eun1Transfer is the provider's answer for eu-north-1 as ec2.Prices
// parses the recorded Pricing API fixture (internal/ec2/testdata), with two
// destination regions of its 37.
func eun1Transfer() DataTransferPrices {
	return DataTransferPrices{Currency: "USD",
		Internet:     PerGB{Tx: "0.0900000000"},
		Zone:         PerGB{Rx: "0.0100000000", Tx: "0.0100000000"},
		RegionPublic: PerGB{Rx: "0.0100000000", Tx: "0.0100000000"},
		Provider: map[string]PerGB{
			"eu-west-1":    {Tx: "0.0200000000"},
			"mx-central-1": {Tx: "0.0500000000"},
		},
		PublicIPv4PerHour: "0.0050000000"}
}

func eun1TransferTrimmed() DataTransferPrices {
	return DataTransferPrices{Currency: "USD",
		Internet:          PerGB{Tx: "0.09"},
		Zone:              PerGB{Rx: "0.01", Tx: "0.01"},
		RegionPublic:      PerGB{Rx: "0.01", Tx: "0.01"},
		Provider:          map[string]PerGB{"eu-west-1": {Tx: "0.02"}, "mx-central-1": {Tx: "0.05"}},
		PublicIPv4PerHour: "0.005"}
}

// The answer round-trips through price_cache, is served from it while
// fresh, and a stale copy stands in for a failed refresh.
func TestDataTransferPriceCache(t *testing.T) {
	p := &fakePriceProvider{transfer: map[string]DataTransferPrices{"eu-north-1": eun1Transfer()}}
	s := providerPriceServer(t, p)
	ctx := context.Background()
	want := eun1TransferTrimmed()
	got, err := s.dataTransferPrice(ctx, "ec2", "eu-north-1")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("first: %+v %v", got, err)
	}
	if got, err := s.dataTransferPrice(ctx, "ec2", "eu-north-1"); err != nil || !reflect.DeepEqual(got, want) || len(p.transferCalls) != 1 {
		t.Fatalf("cached: %+v %v, calls %v", got, err, p.transferCalls)
	}
	execSQL(t, s, ctx, `UPDATE price_cache SET fetched_at = now() - interval '2 hours'`)
	p.transferErr = errors.New("throttled")
	if got, err := s.dataTransferPrice(ctx, "ec2", "eu-north-1"); err != nil || !reflect.DeepEqual(got, want) || len(p.transferCalls) != 2 {
		t.Errorf("stale after failure: %+v %v, calls %v", got, err, p.transferCalls)
	}
	if _, err := s.dataTransferPrice(ctx, "ec2", "us-east-1"); err == nil {
		t.Error("nothing cached and the provider failing: no error")
	}
	// A refresh that drops a destination drops its cached row.
	p.transferErr = nil
	next := eun1Transfer()
	delete(next.Provider, "mx-central-1")
	p.transfer["eu-north-1"] = next
	execSQL(t, s, ctx, `UPDATE price_cache SET fetched_at = now() - interval '2 hours'`)
	got, err = s.dataTransferPrice(ctx, "ec2", "eu-north-1")
	if err != nil || len(got.Provider) != 1 {
		t.Fatalf("refresh: %+v %v", got, err)
	}
	var rows int
	systemScan(t, s, `SELECT count(*) FROM price_cache WHERE os = 'TRANSFER' AND instance_type LIKE 'provider:%'`, nil, &rows)
	if rows != 1 {
		t.Errorf("%d inter-region rows after the refresh, want 1", rows)
	}
}

// An answer without an internet price, or with a malformed one, is
// refused: never a partial or zero price.
func TestDataTransferPriceRefusesIncomplete(t *testing.T) {
	bad := eun1Transfer()
	bad.Internet = PerGB{}
	p := &fakePriceProvider{transfer: map[string]DataTransferPrices{"eu-north-1": bad}}
	s := providerPriceServer(t, p)
	if _, err := s.dataTransferPrice(context.Background(), "ec2", "eu-north-1"); err == nil {
		t.Error("no internet price accepted")
	}
	bad = eun1Transfer()
	bad.Zone.Rx = "-1"
	p.transfer["eu-north-1"] = bad
	if _, err := s.dataTransferPrice(context.Background(), "ec2", "eu-north-1"); err == nil {
		t.Error("a negative price accepted")
	}
}
