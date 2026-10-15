package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

const testSnippet = `<meta name="t" content="1">`

// A server's inject is carried by every path that defines a server: the
// spec at submit, a Run's servers (POST, PUT), and the tenant's servers
// (POST, PATCH); and it is validated on each.
func TestServerInjectCarried(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	capability := apiCall(t, s, key, http.MethodGet, "/v1/whoami", nil)
	var who Whoami
	if err := json.Unmarshal(capability.Body.Bytes(), &who); err != nil || capability.Code != http.StatusOK || !who.ServerInject {
		t.Fatalf("capability: %d %s (%v)", capability.Code, capability.Body, err)
	}

	w := apiCall(t, s, key, http.MethodPost, "/v1/runs", map[string]any{
		"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"sleep", "infinity"},
			"servers": []map[string]any{{"name": "web", "port": 8080, "inject": testSnippet}}}})
	if w.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	var run Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	var got []RunServer
	err := s.db.Tx(ctx, store.Tenant("t1"), func(tx pgx.Tx) error {
		var err error
		got, err = s.listServersTx(ctx, tx, run.ID)
		return err
	})
	if err != nil || len(got) != 1 || got[0].Inject != testSnippet {
		t.Fatalf("spec server: %+v %v", got, err)
	}
	bad := apiCall(t, s, key, http.MethodPost, "/v1/runs", map[string]any{
		"image": map[string]any{"ref": "alpine"}, "workload": map[string]any{"command": []string{"sleep", "infinity"},
			"servers": []map[string]any{{"name": "web", "port": 8080, "inject": strings.Repeat("a", 8193)}}}})
	if bad.Code != http.StatusUnprocessableEntity || !strings.Contains(bad.Body.String(), "inject") {
		t.Fatalf("oversized spec inject: %d %s", bad.Code, bad.Body)
	}

	// A Run's servers: POST, then PUT replaces it.
	w = apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/servers", map[string]any{"name": "api", "port": 3000, "inject": testSnippet})
	var rs RunServer
	_ = json.Unmarshal(w.Body.Bytes(), &rs)
	if w.Code != http.StatusCreated || rs.Inject != testSnippet {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w = apiCall(t, s, key, http.MethodPut, "/v1/runs/"+r1+"/servers/api", map[string]any{"port": 3000, "inject": "<script src=/x.js></script>"})
	_ = json.Unmarshal(w.Body.Bytes(), &rs)
	if w.Code != http.StatusOK || rs.Inject != "<script src=/x.js></script>" {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPut, "/v1/runs/"+r1+"/servers/api", map[string]any{"port": 3000, "inject": "a\x00"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("put NUL: %d %s", w.Code, w.Body)
	}
	// PUT replaces the whole server: leaving inject out clears it.
	w = apiCall(t, s, key, http.MethodPut, "/v1/runs/"+r1+"/servers/api", map[string]any{"port": 3000})
	rs = RunServer{}
	_ = json.Unmarshal(w.Body.Bytes(), &rs)
	if w.Code != http.StatusOK || rs.Inject != "" {
		t.Fatalf("put without inject: %d %s", w.Code, w.Body)
	}

	// The tenant's servers: POST, PATCH sets and clears, a PATCH without
	// it keeps it.
	sv := createSrv(t, s, key, map[string]any{"name": "own", "port": 3000, "inject": testSnippet})
	if sv.Inject != testSnippet {
		t.Fatalf("create: %+v", sv)
	}
	patch := func(body map[string]any) TenantServer {
		w := apiCall(t, s, key, http.MethodPatch, "/v1/servers/"+sv.ID, body)
		if w.Code != http.StatusOK {
			t.Fatalf("patch %v: %d %s", body, w.Code, w.Body)
		}
		var out TenantServer
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if out := patch(map[string]any{"port": 3001}); out.Inject != testSnippet {
		t.Fatalf("patch without inject: %q", out.Inject)
	}
	if out := patch(map[string]any{"inject": "<link rel=x>"}); out.Inject != "<link rel=x>" {
		t.Fatalf("patch inject: %q", out.Inject)
	}
	if out := patch(map[string]any{"inject": ""}); out.Inject != "" {
		t.Fatalf("patch clear: %q", out.Inject)
	}
	if w := apiCall(t, s, key, http.MethodPatch, "/v1/servers/"+sv.ID, map[string]any{"inject": strings.Repeat("a", 8193)}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch oversized: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/servers", map[string]any{"name": "bad", "port": 1, "inject": "a\x00"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create NUL: %d %s", w.Code, w.Body)
	}
}
