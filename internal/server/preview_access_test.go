package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeAccess is a Cloudflare Access team: its certs endpoint serves one RSA
// key, and sign issues tokens for an email with it.
type fakeAccess struct {
	URL  string
	sign func(email string) string
}

func newFakeAccess(t *testing.T, aud string) fakeAccess {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cdn-cgi/access/certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	sign := func(email string) string {
		head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		now := time.Now()
		body, _ := json.Marshal(map[string]any{"iss": srv.URL, "aud": []string{aud}, "email": email,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "sub": email})
		unsigned := b64(head) + "." + b64(body)
		sum := sha256.Sum256([]byte(unsigned))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + b64(sig)
	}
	return fakeAccess{URL: srv.URL, sign: sign}
}

// Previews behind Cloudflare Access: a person of another tenant gets no
// page and wakes nothing; one of the server's tenant gets the waking page,
// and one wake.
func TestAccessPreviewTenantIsolation(t *testing.T) {
	s, ctx, key, _ := wakeFixture(t)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped'`)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	sv := createSrv(t, s, key, map[string]any{"name": "web", "port": 3000, "command": []string{"serve"}, "wake": "request",
		"hostname": "web.pr1.lux.example.com", "runId": r1})
	access := newFakeAccess(t, "preview-aud")
	s.cfg.ConsoleAuth = ConsoleAuth{Mode: "cloudflare-access", CFTeam: access.URL, CFOperators: []string{"op@example.com"}}
	s.cfg.Preview.Auth, s.cfg.Preview.CFAud = "cloudflare-access", "preview-aud"
	s.preview = newPreviews(s)
	if err := s.preview.init(ctx); err != nil {
		t.Fatal(err)
	}
	get := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "https://web.pr1.lux.example.com/", nil)
		req.Header.Set("Accept", "text/html")
		req.Header.Set("Cf-Access-Jwt-Assertion", access.sign(email))
		w := httptest.NewRecorder()
		s.preview.ServeHTTP(w, req)
		return w
	}
	wakes := func() int { return len(serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")) }

	// Access people are the default tenant's: t2 here.
	s.cfg.ConsoleAuth.CFDefaultTenant, s.cfTenantID = "t2", "t2"
	for range 3 {
		if w := get("eve@example.com"); w.Code != http.StatusUnauthorized || w.Body.String() == "" {
			t.Fatalf("other tenant: %d %s", w.Code, w.Body)
		}
	}
	if n := wakes(); n != 0 {
		t.Fatalf("another tenant's request woke it: %d", n)
	}
	// The server's tenant: the waking page, one wake, by that person.
	s.cfg.ConsoleAuth.CFDefaultTenant, s.cfTenantID = "t1", "t1"
	for range 3 {
		if w := get("ada@example.com"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("same tenant: %d %s", w.Code, w.Body)
		}
	}
	ev := serverEventsOf(t, s, ctx, sv.ID, "server.wake_requested")
	if len(ev) != 1 || ev[0]["by"] != "ada@example.com" {
		t.Fatalf("wakes: %+v", ev)
	}
}
