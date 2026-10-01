package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// docs/openapi.yaml is generated from the code; this keeps it current.
func TestOpenAPIIsCurrent(t *testing.T) {
	got, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("docs/openapi.yaml is stale: run `go run ./cmd/luxd openapi > docs/openapi.yaml`")
	}
}

// Responses stay what they were before huma: no $schema, no Link header,
// lux's error envelope.
func TestResponsesAsBefore(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	h := s.Handler()
	for _, c := range []struct{ path, want string }{
		{"/health", `{"status":"ok"}` + "\n"},
		{"/v1/runs", `{"error":{"code":"unauthorized","message":"missing API key"}}` + "\n"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path, nil))
		if w.Body.String() != c.want || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Link") != "" {
			t.Errorf("%s: %d %v %q", c.path, w.Code, w.Header(), w.Body)
		}
	}
}

func TestHumaErrorsInLuxShape(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{huma.NewError(http.StatusUnprocessableEntity, "validation failed", &huma.ErrorDetail{Location: "query.limit", Message: "invalid integer"}),
			`{"error":{"code":"invalid_request","message":"validation failed: query.limit: invalid integer"}}`},
		{huma.NewError(http.StatusUnprocessableEntity, "validation failed", &huma.ErrorDetail{Location: "body", Message: "unexpected EOF"}),
			`{"error":{"code":"bad_request","message":"invalid JSON body: unexpected EOF"}}`},
		{huma.NewError(http.StatusBadRequest, "request body is required"),
			`{"error":{"code":"bad_request","message":"invalid JSON body: EOF"}}`},
		{huma.NewError(http.StatusInternalServerError, "cannot read request body", errors.New("unexpected EOF")),
			`{"error":{"code":"bad_request","message":"invalid JSON body: unexpected EOF"}}`},
		{huma.NewError(http.StatusUnprocessableEntity, "validation failed", errors.New("a"), errors.New("b")),
			`{"error":{"code":"invalid_request","details":["a","b"],"message":"validation failed"}}`},
	} {
		b, _ := json.Marshal(c.err)
		if string(b) != c.want {
			t.Errorf("got %s, want %s", b, c.want)
		}
	}
}

// Every operation on a tenant's object carries its owner, so an operator's
// request on it runs in that tenant's scope.
func TestOwnedOperationsHaveTheirOwner(t *testing.T) {
	api := (&Server{}).newAPI(http.NewServeMux())
	n := 0
	for path, item := range api.OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Delete} {
			if op == nil {
				continue
			}
			_, owned := op.Metadata[ownerKey]
			want := strings.HasPrefix(path, "/v1/runs/{") || strings.HasPrefix(path, "/v1/artifacts/{") || strings.HasPrefix(path, "/v1/servers/{")
			if owned != want {
				t.Errorf("%s %s: owned %v, want %v", op.Method, path, owned, want)
			}
			if owned {
				n++
			}
		}
	}
	if n < 15 {
		t.Errorf("only %d owned operations", n)
	}
}
