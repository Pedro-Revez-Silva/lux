package server

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Static prices (docs/costs.md, section 2): a host that registered itself
// has no provider to price it, so it carries a flat hourly price of its
// own, set through the API or copied from its pool's default when it first
// registers. syncStaticRate turns that price and the host's advertised
// capacity into host_rates periods.

// currencyCode: ISO 4217, as prices are stored and shown (never converted).
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// validPrice checks an hourly price and its currency, both given or both
// empty: a non-negative decimal numeric(24, 9) holds exactly.
func validPrice(price, currency string) error {
	switch {
	case price == "" && currency == "":
		return nil
	case price == "" || currency == "":
		return errf(http.StatusUnprocessableEntity, "invalid_price", "hourlyPrice and currency go together")
	case !costAmount.MatchString(price) || price[0] == '-':
		return errf(http.StatusUnprocessableEntity, "invalid_price", "hourlyPrice %q: want a non-negative decimal with up to 9 fractional digits", price)
	case !currencyCode.MatchString(currency):
		return errf(http.StatusUnprocessableEntity, "invalid_price", "currency %q: want an ISO 4217 code such as USD", currency)
	}
	return nil
}

// syncStaticRate keeps a host's 'static' rate periods in step with its
// price and advertised capacity, in the caller's (system) transaction,
// which holds the host row locked (FOR UPDATE): if either changed, the open
// period is closed and, while the host has a price, a new one opens at the
// same instant. No price: no open period. Closed periods are never
// touched. Called on every hello and on every price change; registering
// (the host's first hello, which set its registered_at): its first period
// opens when it registered, so its billed window has no gap before it.
func syncStaticRate(ctx context.Context, tx pgx.Tx, hostID string, registering bool) error {
	// The instant is taken after the lock, not at the transaction's start
	// (now()): a transaction that waited for the row must not close a
	// period before the one that held it opened it.
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return err
	}
	b := &pgx.Batch{}
	b.Queue(`UPDATE host_rates r SET valid_to = $2
		FROM hosts h
		WHERE h.id = $1 AND r.host_id = h.id AND r.valid_to IS NULL AND r.source = 'static'
		  AND (h.hourly_price IS DISTINCT FROM r.per_hour OR h.price_currency IS DISTINCT FROM r.currency
		       OR coalesce((h.capacity->>'cpus')::float8, 0) <> r.cap_cpus
		       OR coalesce((h.capacity->>'memory')::int8, 0) <> r.cap_memory)`, hostID, at)
	b.Queue(`INSERT INTO host_rates (host_id, valid_from, per_hour, currency, cap_cpus, cap_memory, source)
		SELECT h.id, CASE WHEN $3 THEN h.registered_at ELSE $2 END, h.hourly_price, h.price_currency,
			coalesce((h.capacity->>'cpus')::float8, 0), coalesce((h.capacity->>'memory')::int8, 0), 'static'
		FROM hosts h
		WHERE h.id = $1 AND h.hourly_price IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM host_rates r WHERE r.host_id = h.id AND r.valid_to IS NULL)`, hostID, at, registering)
	return tx.SendBatch(ctx, b).Close()
}

// HostPrice is a static host's flat hourly price.
type HostPrice struct {
	HourlyPrice string `json:"hourlyPrice" doc:"Per hour, a decimal string with up to 9 fractional digits." example:"0.40"`
	Currency    string `json:"currency" doc:"ISO 4217." example:"USD"`
}

type setHostPriceInput struct {
	HostPath
	TenantQuery
	Body HostPrice
}

type hostPriceOutput struct {
	Body struct {
		Host string `json:"host" doc:"The host's id."`
		HostPrice
	} `nameHint:"HostPriceSet"`
}

type clearHostPriceInput struct {
	HostPath
	TenantQuery
}

// setHostPrice sets a static host's hourly price: its open rate period
// closes now and a new one opens at the new price.
func (s *Server) setHostPrice(ctx context.Context, in *setHostPriceInput) (*hostPriceOutput, error) {
	if in.Body.HourlyPrice == "" || in.Body.Currency == "" {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_price", "hourlyPrice and currency are required (DELETE clears the price)")
	}
	if err := validPrice(in.Body.HourlyPrice, in.Body.Currency); err != nil {
		return nil, err
	}
	id, err := s.priceHost(ctx, in.ID, &in.Body)
	if err != nil {
		return nil, err
	}
	out := &hostPriceOutput{}
	out.Body.Host, out.Body.HostPrice = id, in.Body
	return out, nil
}

// clearHostPrice removes a static host's price: its open period closes now
// and no new one opens, so its Runs get no compute line from then on.
func (s *Server) clearHostPrice(ctx context.Context, in *clearHostPriceInput) (*struct{}, error) {
	if _, err := s.priceHost(ctx, in.ID, nil); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

// priceHost sets (nil: clears) the price of a host the principal may
// change, as it may change the host's pool: a tenant its own hosts, an
// operator any host. A host the principal cannot see is not found; a
// platform host a tenant sees is not its to price.
func (s *Server) priceHost(ctx context.Context, ref string, price *HostPrice) (string, error) {
	p := principal(ctx)
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		if id, err = s.resolveHost(ctx, tx, p, ref, false); err != nil {
			return err
		}
		var mine, provisioned bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(tenant_id = $2, false), provision_requested_at IS NOT NULL
			FROM hosts WHERE id = $1 FOR UPDATE`, id, p.TenantID).Scan(&mine, &provisioned); err != nil {
			return err
		}
		switch {
		case !mine && !p.Operator:
			return errf(http.StatusForbidden, "forbidden", "only operators price platform hosts")
		case provisioned:
			return errf(http.StatusUnprocessableEntity, "invalid_price", "host %s was launched by its pool's provider, which prices it", ref)
		}
		var amount, currency *string
		if price != nil {
			amount, currency = &price.HourlyPrice, &price.Currency
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET hourly_price = $2::text::numeric, price_currency = $3 WHERE id = $1`,
			id, amount, currency); err != nil {
			return err
		}
		return syncStaticRate(ctx, tx, id, false)
	})
	return id, err
}

// ValidPoolPrice checks a pool's default price (luxd admin create-pool
// and putPool): only a static pool has one (an ec2 pool's hosts are priced
// by the provider).
func ValidPoolPrice(provider, price, currency string) error {
	if price == "" && currency == "" {
		return nil
	}
	if provider != "static" {
		return errf(http.StatusUnprocessableEntity, "invalid_pool", "hourlyPrice is for static pools: %s pools are priced by the provider", provider)
	}
	return validPrice(price, currency)
}
