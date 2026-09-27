package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ConsoleAuth configures key or Cloudflare Access authentication.
type ConsoleAuth struct {
	Mode            string
	CFTeam, CFAud   string
	CFOperators     []string
	CFDefaultTenant string
}

// cfAccess verifies Cloudflare Access tokens: signed by the team's keys
// (fetched and cached by go-oidc), issued by the team, for this
// application, not expired. The user's name comes from Access's identity
// endpoint, cached by email.
type cfAccess struct {
	team     string // https://<team>.cloudflareaccess.com
	verifier *oidc.IDTokenVerifier
	client   *http.Client

	mu    sync.Mutex
	names map[string]cachedName
}

type cachedName struct {
	name string
	at   time.Time
}

func newCFAccess(team, aud string) *cfAccess {
	if !strings.Contains(team, ".") {
		team += ".cloudflareaccess.com"
	}
	if !strings.Contains(team, "://") {
		team = "https://" + team
	}
	team = strings.TrimRight(team, "/")
	keys := oidc.NewRemoteKeySet(context.Background(), team+"/cdn-cgi/access/certs")
	return &cfAccess{
		team:     team,
		verifier: oidc.NewVerifier(team, keys, &oidc.Config{ClientID: aud}),
		client:   &http.Client{Timeout: 5 * time.Second},
		names:    map[string]cachedName{},
	}
}

// accessToken is the request's Access token, if any: the header Access
// adds in front of luxd, or the browser's cookie. A cookie is sent with
// any request the browser makes, a cross-site one included, so it
// authenticates only reads, or requests that show they come from this
// origin (fetch sets Sec-Fetch-Site; a form post from elsewhere cannot
// fake it).
func accessToken(r *http.Request) string {
	if t := r.Header.Get("Cf-Access-Jwt-Assertion"); t != "" {
		return t
	}
	c, err := r.Cookie("CF_Authorization")
	if err != nil {
		return ""
	}
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !safe && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		return ""
	}
	return c.Value
}

// user verifies a token and says who it is. A service token (no email) is
// refused: the console is for people.
func (a *cfAccess) user(ctx context.Context, token string) (email, name string, err error) {
	t, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return "", "", errf(http.StatusUnauthorized, "unauthorized", "invalid Cloudflare Access token: %v", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := t.Claims(&claims); err != nil || !ValidAccessOperatorEmail(claims.Email) {
		return "", "", errf(http.StatusUnauthorized, "unauthorized", "the Cloudflare Access token names no valid user")
	}
	return claims.Email, a.name(ctx, claims.Email, token), nil
}

// ValidAccessOperatorEmail accepts a single plain ASCII mailbox.
func ValidAccessOperatorEmail(email string) bool {
	if email == "" || email != strings.TrimSpace(email) || !isASCII(email) {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email && strings.Contains(email, "@")
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// name is the user's name from Access's identity endpoint (the token's
// claims carry only the email), cached for an hour; the email if unknown.
func (a *cfAccess) name(ctx context.Context, email, token string) string {
	a.mu.Lock()
	c, ok := a.names[email]
	a.mu.Unlock()
	if ok && time.Since(c.at) < time.Hour {
		return c.name
	}
	name := email
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.team+"/cdn-cgi/access/get-identity", nil)
	if err == nil {
		req.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: token})
		if resp, err := a.client.Do(req); err == nil {
			var id struct {
				Name string `json:"name"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&id) == nil && id.Name != "" {
				name = id.Name
			}
			resp.Body.Close()
		}
	}
	a.mu.Lock()
	a.names[email] = cachedName{name, time.Now()}
	a.mu.Unlock()
	return name
}

// consoleUser authenticates an Access user as an allowlisted operator or as
// the configured default tenant. Only the verified JWT email selects a role.
func (s *Server) consoleUser(r *http.Request, scope string) (Principal, error) {
	if s.cfAccess == nil {
		return Principal{}, errf(http.StatusUnauthorized, "unauthorized", "missing API key")
	}
	token := accessToken(r)
	if token == "" {
		return Principal{}, errf(http.StatusUnauthorized, "unauthorized", "missing API key or Cloudflare Access token")
	}
	email, name, err := s.cfAccess.user(r.Context(), token)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Email: email, Name: name}
	ref := s.cfg.ConsoleAuth.CFDefaultTenant
	if ref == "" || len(s.cfg.ConsoleAuth.CFOperators) == 0 {
		return p, errf(http.StatusForbidden, "forbidden", "Cloudflare Access authorization is not configured")
	}
	for _, allowed := range s.cfg.ConsoleAuth.CFOperators {
		if strings.EqualFold(email, allowed) {
			p.Operator, p.Scopes = true, []string{"operator"}
			break
		}
	}
	if !p.Operator {
		p.Scopes = []string{"admin"}
		if !p.Can(scope) {
			return p, errf(http.StatusForbidden, "forbidden", "scope %q", scope)
		}
		id, err := s.resolveTenant(r.Context(), ref)
		if err != nil {
			if httpErr, ok := err.(*HTTPError); ok && httpErr.Status == http.StatusNotFound {
				return p, errf(http.StatusForbidden, "forbidden", "Cloudflare Access default tenant is unavailable")
			}
			return p, err
		}
		p.TenantID = id
	}
	if !p.Can(scope) {
		return p, errf(http.StatusForbidden, "forbidden", "scope %q", scope)
	}
	return p, nil
}
