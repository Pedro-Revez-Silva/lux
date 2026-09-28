package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Stream tickets: what a browser uses where it cannot send an
// Authorization header (a WebSocket, a redirect to a preview). A ticket is
// minted with the caller's own credentials, bound to one Run, one kind
// (exec: the exec, attach and ports streams; preview: the preview
// listener's sign-in) and the minting principal, and is good once, for
// ticketTTL. Only its hash is stored, so any luxd can redeem it.

const ticketTTL = 60 * time.Second

// Ticket kinds.
const (
	TicketExec    = "exec"
	TicketPreview = "preview"
)

type ticketRequest struct {
	Kind string `json:"kind" enum:"exec,preview" doc:"exec: for ?ticket= on the Run's exec, attach and ports streams. preview: for its preview URLs' sign-in (/.lux/auth)."`
}

type mintTicketInput struct {
	RunPath
	Body ticketRequest
}

// Ticket is a minted stream ticket.
type Ticket struct {
	Ticket    string    `json:"ticket" doc:"Single use; send it as ?ticket= (never logged by luxd)."`
	Kind      string    `json:"kind"`
	RunID     string    `json:"runId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type ticketOutput struct {
	Status int
	Body   Ticket
}

func (s *Server) mintTicket(ctx context.Context, in *mintTicketInput) (*ticketOutput, error) {
	p := principal(ctx)
	kind := in.Body.Kind
	if kind != TicketExec && kind != TicketPreview {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_request", "kind: want exec or preview")
	}
	if kind == TicketPreview && !s.previewTickets() {
		return nil, errf(http.StatusConflict, "previews_off", "this luxd signs no one in to previews with tickets (preview.domain is not set, or previews use Cloudflare Access)")
	}
	// A preview is a read; a stream acts on the Run.
	if kind == TicketExec && !p.Can("run") {
		return nil, errf(http.StatusForbidden, "forbidden", "an exec ticket needs the %q scope", "run")
	}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error { return requireRun(ctx, tx, in.ID) })
	if err != nil {
		return nil, err
	}
	raw := ids.Secret("tkt")
	t := Ticket{Ticket: raw, Kind: kind, RunID: in.ID, ExpiresAt: time.Now().Add(ticketTTL).UTC().Truncate(time.Millisecond)}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO stream_tickets (token_hash, tenant_id, run_id, kind, principal, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, ids.Hash(raw), p.TenantID, in.ID, kind, ticketPrincipal(p), t.ExpiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &ticketOutput{http.StatusCreated, t}, nil
}

// storedPrincipal is a Principal as a ticket keeps it.
type storedPrincipal struct {
	TenantID string   `json:"tenantId"`
	KeyID    string   `json:"keyId,omitempty"`
	Scopes   []string `json:"scopes"`
	Operator bool     `json:"operator,omitempty"`
	Email    string   `json:"email,omitempty"`
	Name     string   `json:"name,omitempty"`
	Picture  string   `json:"picture,omitempty"`
}

func ticketPrincipal(p Principal) storedPrincipal {
	return storedPrincipal{p.TenantID, p.KeyID, p.Scopes, p.Operator, p.Email, p.Name, p.Picture}
}

func (sp storedPrincipal) principal() Principal {
	return Principal{TenantID: sp.TenantID, KeyID: sp.KeyID, Scopes: sp.Scopes, Operator: sp.Operator, Email: sp.Email, Name: sp.Name, Picture: sp.Picture}
}

var errBadTicket = errf(http.StatusUnauthorized, "unauthorized", "invalid, used or expired ticket")

// redeemTicket uses a ticket for runID and kind: once, before it expires,
// and only while the key that minted it (if a key did) is not revoked.
func (s *Server) redeemTicket(ctx context.Context, raw, runID, kind string) (Principal, error) {
	var sp storedPrincipal
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE stream_tickets SET used_at = now()
			WHERE token_hash = $1 AND run_id = $2 AND kind = $3 AND used_at IS NULL AND expires_at > now()
			RETURNING principal`, ids.Hash(raw), runID, kind).Scan(&sp)
		if err != nil || sp.KeyID == "" {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1 AND revoked_at IS NULL)`, sp.KeyID).Scan(&live); err != nil {
			return err
		}
		if !live {
			return pgx.ErrNoRows
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, errBadTicket
	}
	return sp.principal(), err
}

// ticketReaper deletes tickets well past their expiry.
func (s *Server) ticketReaper(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM stream_tickets WHERE expires_at < now() - interval '1 hour'`)
			return err
		})
		if err != nil && ctx.Err() == nil {
			s.log.Warn("tickets: reap", "err", err)
		}
		wait(ctx, nil, 10*time.Minute)
	}
}

// ---- stream routes ---------------------------------------------------------

// streamAuth runs before requireKey on the exec, attach and ports routes:
// it refuses a WebSocket upgrade from a page of another origin, and
// redeems ?ticket= for a request with no Authorization header.
func (s *Server) streamAuth(ctx huma.Context, next func(huma.Context)) {
	r, w := humago.Unwrap(ctx)
	if r.Header.Get("Upgrade") != "" && !s.originAllowed(r) {
		s.writeError(w, r, errf(http.StatusForbidden, "bad_origin", "streams cannot be opened from %s", r.Header.Get("Origin")))
		return
	}
	raw := r.URL.Query().Get("ticket")
	if raw == "" || r.Header.Get("Authorization") != "" {
		next(ctx)
		return
	}
	p, err := s.redeemTicket(ctx.Context(), raw, ctx.Param("id"), TicketExec)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !p.Can("run") {
		s.writeError(w, r, errf(http.StatusForbidden, "forbidden", "scope %q", "run"))
		return
	}
	next(huma.WithValue(ctx, principalKey, p))
}

// originAllowed: a request without an Origin (not from a browser page)
// passes; one with must come from luxd's public URL, from the request's
// own host, or from a configured origin (console.allowed_origins).
func (s *Server) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	origin := strings.ToLower(u.Scheme + "://" + u.Host)
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if pu, err := url.Parse(s.cfg.PublicURL); err == nil && pu.Host != "" && origin == strings.ToLower(pu.Scheme+"://"+pu.Host) {
		return true
	}
	return slices.ContainsFunc(s.cfg.AllowedOrigins, func(a string) bool { return strings.EqualFold(strings.TrimRight(a, "/"), origin) })
}
