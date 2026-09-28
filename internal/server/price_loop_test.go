package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blockingPriceProvider struct {
	started chan time.Time
}

func (p *blockingPriceProvider) OnDemand(ctx context.Context, _, _ string) (HourlyRate, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return HourlyRate{}, errors.New("price request has no deadline")
	}
	p.started <- deadline
	<-ctx.Done()
	return HourlyRate{}, ctx.Err()
}

func (p *blockingPriceProvider) SpotHistory(ctx context.Context, _, _ string, _, _ time.Time) ([]SpotRate, error) {
	return nil, errors.New("unexpected spot request")
}

func TestPriceRefreshDoesNotBlockLaunchOrCostDrain(t *testing.T) {
	s := testServer(t)
	p := &blockingPriceProvider{started: make(chan time.Time, 1)}
	s.cfg.Costs = CostsConfig{
		Enabled: true, ComputeEC2: true, PricesRefresh: time.Hour,
		Every: time.Hour, DrainEvery: 10 * time.Millisecond, Batch: 10,
		Prices: map[string]PriceProvider{"ec2": p},
	}
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, name, provider) VALUES ('pool1', 'burst', 'ec2')`)
	launched := make(chan error, 1)
	go func() {
		launched <- s.launch(ctx, &fakeLaunchProvider{launched: Launched{
			ProviderID: "i-test", InstanceType: "m7i.large", Market: MarketOnDemand,
		}}, poolRow{ID: "pool1", Name: "burst", Provider: "ec2", Template: []byte(`{"region":"us-east-1"}`)})
	}()
	select {
	case err := <-launched:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch waited for pricing")
	}

	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.priceLoop(lctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case deadline := <-p.started:
		if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
			t.Errorf("provider deadline is not bounded to 30 seconds: %v", remaining)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not discover the launched host")
	}

	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state) VALUES ('r1', 't1', '{}', 'stopped')`)
	execSQL(t, s, ctx, `INSERT INTO cost_pending (run_id, due_at, reason) VALUES ('r1', now(), 'state:stopped')`)
	drained := make(chan struct{})
	go func() { s.costLoop(lctx); close(drained) }()
	defer func() { cancel(); <-drained }()
	deadline := time.Now().Add(5 * time.Second)
	for pending(t, s, "r1") != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := pending(t, s, "r1"); got != "" {
		t.Errorf("cost drain blocked by pricing: %q", got)
	}
}
