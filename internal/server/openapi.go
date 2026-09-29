package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
	"github.com/marcioapm/lux/internal/version"
)

// The tenant API is declared with huma, so docs/openapi.yaml is generated
// from these types and handlers (luxd openapi). The runner's routes are not
// part of it: they stay plain handlers (runnerRoutes).

func init() {
	// huma's own errors in lux's error shape, and for a body that does not
	// decode, the same error as before huma.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		switch {
		case status == http.StatusRequestEntityTooLarge:
			return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: http: request body too large")
		case msg == "request body is required":
			return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: EOF")
		case msg == "cannot read request body":
			// The client's body broke off (not a server fault): a 400, as
			// the decoder reported it before huma.
			reason := "unexpected EOF"
			if len(errs) > 0 && errs[0] != nil {
				reason = errs[0].Error()
			}
			return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: %s", reason)
		}
		var details []string
		decoding := msg == "validation failed" && len(errs) > 0
		for _, err := range errs {
			if err == nil {
				continue
			}
			var d *huma.ErrorDetail
			if ed, ok := err.(huma.ErrorDetailer); ok {
				d = ed.ErrorDetail()
			} else {
				d = &huma.ErrorDetail{Message: err.Error()}
			}
			decoding = decoding && d.Location == "body"
			// The location and message, never the value: it can be the
			// whole request body.
			if d.Location != "" && d.Location != "body" {
				details = append(details, d.Location+": "+d.Message)
			} else {
				details = append(details, d.Message)
			}
		}
		if decoding {
			return errf(http.StatusBadRequest, "bad_request", "invalid JSON body: %s", strings.Join(details, "; "))
		}
		he := &HTTPError{Status: status, Code: errorCode(status), Message: msg}
		// One problem goes in the message; several are listed, as
		// invalid_spec's are.
		switch {
		case len(details) == 1:
			he.Message += ": " + details[0]
		case len(details) > 1:
			he.Details = details
		}
		return he
	}
	huma.NewErrorWithContext = func(_ huma.Context, status int, msg string, errs ...error) huma.StatusError {
		return huma.NewError(status, msg, errs...)
	}
}

func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnprocessableEntity:
		return "invalid_request"
	case http.StatusInternalServerError:
		return "internal"
	}
	return strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_"))
}

// jsonFormat encodes exactly as writeJSON does (HTML-escaped, one line), so
// responses are the bytes they were before huma. A failed write is the
// client's problem, as it was there: huma would panic on it.
var jsonFormat = huma.Format{
	Marshal: func(w io.Writer, v any) error {
		_ = json.NewEncoder(w).Encode(v)
		return nil
	},
	Unmarshal: decodeStrict,
}

// decodeStrict decodes a request body as readJSON does: unknown fields
// refused, anything after the first value ignored.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Schema names, where two packages use the same type name.
var schemaNames = map[reflect.Type]string{
	reflect.TypeFor[Placement](): "RunPlacement",
	reflect.TypeFor[errorBody](): "Error",
}

// The schemas of spec's types with their own JSON, given here so spec (in
// the runner and the shim too) does not depend on huma. Each accepts what
// its UnmarshalJSON does.
type (
	durationSchema struct{}
	bytesSchema    struct{}
)

func (durationSchema) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Description: `A Go duration ("4h", "90s"), or a number of seconds. Responses carry the string.`,
		OneOf: []*huma.Schema{
			{Type: huma.TypeString, Examples: []any{"4h"}},
			{Type: huma.TypeNumber},
		},
	}
}

func (bytesSchema) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Description: `A number of bytes, or a size: "8Gi", "512Mi", "1G". Responses carry the number.`,
		OneOf: []*huma.Schema{
			{Type: huma.TypeInteger, Format: "int64"},
			{Type: huma.TypeString, Examples: []any{"8Gi"}},
		},
	}
}

func schemaNamer(t reflect.Type, hint string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n, ok := schemaNames[t]; ok {
		return n
	}
	return huma.DefaultSchemaNamer(t, hint)
}

// newAPI declares the tenant API on mux.
func (s *Server) newAPI(mux *http.ServeMux) huma.API {
	registry := huma.NewMapRegistry("#/components/schemas/", schemaNamer)
	registry.RegisterTypeAlias(reflect.TypeFor[spec.Duration](), reflect.TypeFor[durationSchema]())
	registry.RegisterTypeAlias(reflect.TypeFor[spec.Bytes](), reflect.TypeFor[bytesSchema]())
	cfg := huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info: &huma.Info{
				Title:   "lux",
				Version: version.Version,
				Description: "The lux tenant API. Every /v1 call authenticates with an API key " +
					"(`Authorization: Bearer <key>`); a key has scopes `read`, `run` and `admin`, " +
					"each implying the ones before it.\n\n" +
					"An operator key (`luxd admin create-operator-key`, scope `operator`) belongs to no tenant: " +
					"it sees and acts on every tenant through the same operations. A Run's operations act in the Run's own tenant; " +
					"lists span every tenant unless `?tenant=` (an id or a name) narrows them; creating needs `?tenant=`.\n\n" +
					"Errors are JSON: `{\"error\": {\"code\": \"not_found\", \"message\": \"...\", " +
					"\"details\": [\"...\"]}}`, with a stable `code`.",
			},
			Components: &huma.Components{
				Schemas: registry,
				SecuritySchemes: map[string]*huma.SecurityScheme{
					"apiKey": {Type: "http", Scheme: "bearer", Description: "A tenant API key (luxd admin create-key), or an operator key (luxd admin create-operator-key)."},
				},
			},
			Tags: []*huma.Tag{
				{Name: "runs", Description: "Submit Runs and follow them through their lifecycle."},
				{Name: "interactive", Description: "Steer a live Run, run commands in it, attach to it, reach its ports."},
				{Name: "servers", Description: "A Run's servers: named ports, with commands lux runs, and their preview URLs."},
				{Name: "hosts", Description: "The hosts that run your workloads."},
				{Name: "pools", Description: "Pools of hosts, and how they are provisioned."},
				{Name: "history", Description: "The system, hosts and Runs now and over time."},
				{Name: "operators", Description: "For operator keys: every tenant."},
			},
		},
		OpenAPIPath:   "/openapi",
		Formats:       map[string]huma.Format{"application/json": jsonFormat},
		DefaultFormat: "application/json",
	}
	// Fields are optional unless a handler says otherwise, as when bodies
	// were decoded by hand; unknown fields are refused.
	cfg.FieldsOptionalByDefault = true
	api := humago.New(mux, cfg)
	api.UseMiddleware(asBefore)
	s.routes(api)
	return api
}

// OpenAPI is the tenant API's spec, as YAML. It needs no database.
func OpenAPI() ([]byte, error) {
	return (&Server{}).newAPI(http.NewServeMux()).OpenAPI().YAML()
}

// asBefore keeps requests meaning what they did before huma: a body is
// JSON whatever its Content-Type, and a bare query key (?follow) is empty,
// as net/url reads it, where huma would read it as "true".
func asBefore(ctx huma.Context, next func(huma.Context)) {
	r, _ := humago.Unwrap(ctx)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			r.Header.Set("Content-Type", "application/json")
		}
	}
	if q := r.URL.RawQuery; q != "" {
		parts := strings.Split(q, "&")
		for i, kv := range parts {
			if kv != "" && !strings.Contains(kv, "=") {
				parts[i] = kv + "="
			}
		}
		r.URL.RawQuery = strings.Join(parts, "&")
	}
	next(huma.WithValue(ctx, requestKey, r))
}

// requireKey authenticates the API key (authKey) as huma middleware: the
// Principal goes in the request's context.
func (s *Server) requireKey(scope string) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		var p Principal
		var err error
		if tp, ok := ctx.Context().Value(principalKey).(Principal); ok {
			// Authenticated already, by a stream ticket (streamAuth).
			p = tp
			if !p.Can(scope) {
				err = errf(http.StatusForbidden, "forbidden", "scope %q", scope)
			}
		} else if key := bearerToken(ctx.Header("Authorization")); key != "" || s.cfAccess == nil {
			p, err = s.authKey(ctx.Context(), key, scope)
		} else {
			r, _ := humago.Unwrap(ctx)
			p, err = s.consoleUser(r, scope)
		}
		if err == nil {
			p, err = s.operatorScope(ctx, p)
		}
		if err != nil {
			r, w := humago.Unwrap(ctx)
			s.writeError(w, r, err)
			return
		}
		next(huma.WithValue(ctx, principalKey, p))
	}
}

// owner says how an operation finds the tenant owning the object its path
// names: a query selecting tenant_id by the path parameter param. Every
// operation under an owned path gets its owner when registered, read by
// operatorScope; there is no path an owned object is reached by without.
type owner struct {
	param, query string
}

var owners = []struct {
	prefix string
	owner
}{
	{"/v1/runs/{id}", owner{"id", `SELECT tenant_id FROM runs WHERE id = $1`}},
	{"/v1/artifacts/{aid}", owner{"aid", `SELECT tenant_id FROM artifacts WHERE id = $1`}},
}

const ownerKey = "lux-owner"

// operatorScope puts an operator's request in one tenant's scope where it
// has one: the tenant owning the object the operation acts on
// (so every query of the handler runs as that tenant's), or else ?tenant=.
// Without either, the principal has no tenant: reads span every tenant,
// and handlers that create a tenant's objects refuse it (forTenant).
func (s *Server) operatorScope(ctx huma.Context, p Principal) (Principal, error) {
	if !p.Operator {
		return p, nil
	}
	o, ok := ctx.Operation().Metadata[ownerKey].(owner)
	if !ok {
		return s.narrow(ctx.Context(), p, ctx.Query("tenant"))
	}
	err := s.db.Tx(ctx.Context(), store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx.Context(), o.query, ctx.Param(o.param)).Scan(&p.TenantID)
	})
	return p, err
}

// forTenant refuses an operator that did not name a tenant: what h
// creates belongs to one.
func forTenant[I, O any](h func(context.Context, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		if principal(ctx).TenantID == "" {
			return nil, errf(http.StatusBadRequest, "tenant_required", "an operator key must name a tenant: ?tenant=<id or name>")
		}
		return h(ctx, in)
	}
}

// register declares one operation. A scope makes it need an API key with
// that scope. Handler errors become lux errors as they always have.
func register[I, O any](s *Server, api huma.API, op huma.Operation, scope string, h func(context.Context, *I) (*O, error)) {
	for _, o := range owners {
		if strings.HasPrefix(op.Path, o.prefix) {
			if op.Metadata == nil {
				op.Metadata = map[string]any{}
			}
			op.Metadata[ownerKey] = o.owner
		}
	}
	if scope != "" {
		op.Security = []map[string][]string{{"apiKey": {}}}
		op.Middlewares = append(op.Middlewares, s.requireKey(scope))
		op.Errors = append([]int{http.StatusUnauthorized, http.StatusForbidden}, op.Errors...)
		op.Description = strings.TrimSpace(op.Description + fmt.Sprintf("\n\nNeeds the `%s` scope.", scope))
	}
	// Bodies are decoded as they always were (decodeStrict), not validated
	// against their schema first: the schema documents them, and errors
	// stay what clients know. huma refuses a body of exactly MaxBodyBytes.
	op.SkipValidateBody = true
	op.MaxBodyBytes = maxBody + 1
	op.BodyReadTimeout = -1 // no deadline of its own, as before
	huma.Register(api, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := h(ctx, in)
		if err != nil {
			r := ctx.Value(requestKey).(*http.Request)
			if he := s.httpError(err, r.Method, r.URL.Path); he != nil {
				return nil, he
			}
			return nil, nil // the client has gone
		}
		return out, nil
	})
}

// streamed runs a handler that writes its own response (SSE, a download, a
// WebSocket) on the raw request, after huma has parsed its input.
func streamed[I any](s *Server, h func(http.ResponseWriter, *http.Request, *I) error) func(context.Context, *I) (*huma.StreamResponse, error) {
	return func(_ context.Context, in *I) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			if err := h(w, r, in); err != nil {
				s.writeError(w, r, err)
			}
		}}, nil
	}
}

// schemaRef registers a type's schema and refers to it.
func schemaRef[T any](api huma.API) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
}

// sseEvents documents a text/event-stream: one schema per event type.
func sseEvents(events ...*huma.Schema) *huma.Schema {
	return &huma.Schema{
		Description: "Server-sent events: `event: <type>` and `data: <JSON>` lines, each event ended by a blank line.",
		Type:        huma.TypeArray,
		Items:       &huma.Schema{OneOf: events},
	}
}

func sseEvent(name, doc string, data *huma.Schema) *huma.Schema {
	return &huma.Schema{
		Title:       name,
		Description: doc,
		Type:        huma.TypeObject,
		Properties: map[string]*huma.Schema{
			"event": {Type: huma.TypeString, Const: name},
			"data":  data,
		},
		Required: []string{"event", "data"},
	}
}
