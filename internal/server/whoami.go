package server

import (
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Whoami is what a key is: an operator's (every tenant) or a tenant's.
type Whoami struct {
	Operator bool   `json:"operator"`
	Tenant   string `json:"tenant" doc:"The key's tenant's name; empty for an operator key."`
	TenantID string `json:"tenantId"`
	KeyID    string `json:"keyId,omitempty"`
	// Email and Name: a person's, signed in through console auth (no key).
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	// Picture: the person's photo URL, from the identity provider.
	Picture string `json:"picture,omitempty"`
	// ConsoleAuth is luxd's console auth: key or cloudflare-access.
	ConsoleAuth string   `json:"consoleAuth"`
	Scopes      []string `json:"scopes"`
}

type whoamiOutput struct {
	Body Whoami
}

// whoami is GET /v1/whoami: the caller's key, so a client (the console)
// knows what to offer without probing.
func (s *Server) whoami(ctx context.Context, _ *struct{}) (*whoamiOutput, error) {
	p := principal(ctx)
	w := Whoami{Operator: p.Operator, KeyID: p.KeyID, Email: p.Email, Name: p.Name, Picture: p.Picture, Scopes: slices.Clone(p.Scopes),
		ConsoleAuth: cmp.Or(s.cfg.ConsoleAuth.Mode, "key")}
	if !p.Operator {
		w.TenantID = p.TenantID
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT name FROM tenants WHERE id = $1`, p.TenantID).Scan(&w.Tenant)
		})
		if err != nil {
			return nil, err
		}
	}
	return &whoamiOutput{w}, nil
}
