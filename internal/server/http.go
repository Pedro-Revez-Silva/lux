package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/store"
)

// Principal is who a request is from: a tenant's key, or an operator's.
// An operator key belongs to no tenant; it sees and acts on every tenant.
type Principal struct {
	TenantID string
	KeyID    string
	Scopes   []string
	Operator bool
	// Email, Name and Picture (a photo's URL, if any): a person's, when
	// authenticated as one (console auth) rather than by an API key.
	Email, Name, Picture string
}

// Actor names who a request is from in events: the person, or the key.
func (p Principal) Actor() string {
	if p.Email != "" {
		return p.Email
	}
	return p.KeyID
}

func (p Principal) Can(scope string) bool {
	for _, s := range p.Scopes {
		// operator implies admin implies run implies read.
		if s == scope || s == "operator" || (s == "admin" && scope != "operator") || (s == "run" && scope == "read") {
			return true
		}
	}
	return false
}

type ctxKey int

const (
	principalKey ctxKey = iota
	hostKey
	requestKey
)

// principal is who the request is from, put in its context by requireKey.
func principal(ctx context.Context) Principal { return ctx.Value(principalKey).(Principal) }

// HTTPError is an error with a status and a stable code. On the wire:
// {"error":{"code":..., "details":[...], "message":...}}.
type HTTPError struct {
	Status  int
	Code    string
	Message string
	Details []string
	// RetryAfter (seconds), Reason and SnapshotID qualify diff_pending and
	// diff_unavailable.
	RetryAfter int
	Reason     string
	SnapshotID string
}

func (e *HTTPError) Error() string { return e.Message }

// GetStatus makes it a huma.StatusError.
func (e *HTTPError) GetStatus() int { return e.Status }

// errorBody is the error envelope; fields in the order the map-based
// encoding this replaced produced them.
type errorBody struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code       string   `json:"code" doc:"Stable error code, e.g. not_found, invalid_spec, quota_exceeded."`
	Details    []string `json:"details,omitempty" doc:"Every problem, when there are several (invalid_spec, secrets_required, validation)."`
	Message    string   `json:"message"`
	RetryAfter int      `json:"retryAfter,omitempty" doc:"diff_pending: seconds to wait before asking again."`
	Reason     string   `json:"reason,omitempty" doc:"diff_unavailable: why the snapshot has no diff."`
	SnapshotID string   `json:"snapshotId,omitempty" doc:"diff_pending, diff_unavailable: the snapshot concerned."`
}

func (e *HTTPError) MarshalJSON() ([]byte, error) {
	return json.Marshal(errorBody{errorInfo{Code: e.Code, Details: e.Details, Message: e.Message,
		RetryAfter: e.RetryAfter, Reason: e.Reason, SnapshotID: e.SnapshotID}})
}

func (e *HTTPError) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[errorBody](), true, "Error")
}

func errf(status int, code, f string, a ...any) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: fmt.Sprintf(f, a...)}
}

var errNotFound = errf(http.StatusNotFound, "not_found", "not found")

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if he := s.httpError(err, r.Method, r.URL.Path); he != nil {
		if he.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(he.RetryAfter))
		}
		writeJSON(w, he.Status, he)
	}
}

// httpError maps a handler's error to what the client sees: nil when the
// client has gone. Anything unexpected is logged and hidden.
func (s *Server) httpError(err error, method, path string) *HTTPError {
	var he *HTTPError
	switch {
	case errors.As(err, &he):
		return he
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case errors.Is(err, context.Canceled):
		return nil
	}
	s.log.Error("request failed", "method", method, "path", path, "err", err)
	return errf(http.StatusInternalServerError, "internal", "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: %v", err)
	}
	return nil
}

// maxBody bounds a JSON request body.
const maxBody = 8 << 20

// authKey authenticates an API key (a tenant's, or an operator's: no
// tenant) and requires a scope.
func (s *Server) authKey(ctx context.Context, key, scope string) (Principal, error) {
	var p Principal
	if key == "" {
		return p, errf(http.StatusUnauthorized, "unauthorized", "missing API key")
	}
	var tenant *string
	var stale bool
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT tenant_id, id, scopes, coalesce(last_used_at < now() - interval '1 minute', true)
			FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, ids.Hash(key)).
			Scan(&tenant, &p.KeyID, &p.Scopes, &stale)
		p.Operator = tenant == nil
		if tenant != nil {
			p.TenantID = *tenant
		}
		if err != nil || !stale {
			return err
		}
		// last_used_at is coarse on purpose: writing it on every request
		// would put a row lock and a WAL write on every call.
		_, err = tx.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, p.KeyID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errf(http.StatusUnauthorized, "unauthorized", "invalid API key")
	}
	if err != nil {
		return p, err
	}
	if !p.Can(scope) {
		return p, errf(http.StatusForbidden, "forbidden", "this key lacks the %q scope", scope)
	}
	return p, nil
}

// narrow applies an operator's ?tenant= (an id or, failing that, a
// name): its request becomes one of that tenant. Tenant keys ignore it.
func (s *Server) narrow(ctx context.Context, p Principal, ref string) (Principal, error) {
	if !p.Operator || ref == "" {
		return p, nil
	}
	id, err := s.resolveTenant(ctx, ref)
	p.TenantID = id
	return p, err
}

func (s *Server) resolveTenant(ctx context.Context, ref string) (string, error) {
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1 OR name = $1 ORDER BY id = $1 DESC LIMIT 1`, ref).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusNotFound, "not_found", "no tenant %s", ref)
	}
	return id, err
}

// scope is the RLS scope of a read: the principal's tenant, or every
// tenant for an operator that did not narrow it.
func (p Principal) scope() store.Scope {
	if p.TenantID == "" {
		return store.System()
	}
	return store.Tenant(p.TenantID)
}

func bearer(r *http.Request) string { return bearerToken(r.Header.Get("Authorization")) }

func bearerToken(h string) string {
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/health" {
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
