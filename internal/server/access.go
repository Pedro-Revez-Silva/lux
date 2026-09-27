package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
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
// application, not expired. The user's name and picture come from Access's
// identity endpoint, cached by email.
type cfAccess struct {
	team     string // https://<team>.cloudflareaccess.com
	verifier *oidc.IDTokenVerifier
	client   *http.Client

	mu         sync.Mutex
	identities map[string]cachedIdentity
}

type cachedIdentity struct {
	identity
	until time.Time
}

// identity is what Access's identity endpoint says of a user.
type identity struct {
	Name string
	// Picture: the photo's URL from the identity provider's picture claim,
	// when Access passes it (the IdP's OIDC claims); https only.
	Picture string
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
		team:       team,
		verifier:   oidc.NewVerifier(team, keys, &oidc.Config{ClientID: aud}),
		client:     &http.Client{Timeout: 5 * time.Second},
		identities: map[string]cachedIdentity{},
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
func (a *cfAccess) user(ctx context.Context, token string) (email string, id identity, err error) {
	t, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return "", identity{}, errf(http.StatusUnauthorized, "unauthorized", "invalid Cloudflare Access token: %v", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := t.Claims(&claims); err != nil || !ValidAccessOperatorEmail(claims.Email) {
		return "", identity{}, errf(http.StatusUnauthorized, "unauthorized", "the Cloudflare Access token names no valid user")
	}
	return claims.Email, a.lookupIdentity(ctx, claims.Email, token), nil
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

// lookupIdentity is the user's name and picture from Access's identity
// endpoint (the token's claims carry only the email), cached for an hour;
// the name is the email if unknown. A failed lookup is cached for a minute
// only, so a blip does not hide the name for long.
func (a *cfAccess) lookupIdentity(ctx context.Context, email, token string) identity {
	a.mu.Lock()
	c, ok := a.identities[email]
	a.mu.Unlock()
	if ok && time.Now().Before(c.until) {
		return c.identity
	}
	id, ttl := identity{Name: email}, time.Minute
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.team+"/cdn-cgi/access/get-identity", nil)
	if err == nil {
		req.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: token})
		if resp, err := a.client.Do(req); err == nil {
			// Picture is any JSON: a provider that sends something other
			// than a string must not cost the name.
			var body struct {
				Name       string `json:"name"`
				OIDCFields struct {
					Picture any `json:"picture"`
				} `json:"oidc_fields"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil {
				if body.Name != "" {
					id.Name = body.Name
				}
				if pic, ok := body.OIDCFields.Picture.(string); ok {
					id.Picture = httpsURL(pic)
				}
				ttl = time.Hour
			}
			resp.Body.Close()
		}
	}
	a.mu.Lock()
	a.identities[email] = cachedIdentity{id, time.Now().Add(ttl)}
	a.mu.Unlock()
	return id
}

// httpsURL is s if it is an absolute https URL, else "".
func httpsURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return u.String()
}

// initCFTenant binds the configured name to its current ID before serving.
// An absent tenant leaves operator access available but no tenant access.
func (s *Server) initCFTenant(ctx context.Context) error {
	if s.cfAccess == nil || s.cfg.ConsoleAuth.CFDefaultTenant == "" {
		return nil
	}
	id, err := s.resolveTenant(ctx, s.cfg.ConsoleAuth.CFDefaultTenant)
	if he, ok := err.(*HTTPError); ok && he.Status == http.StatusNotFound {
		s.cfTenantID = ""
		s.log.Warn("Cloudflare Access default tenant is unavailable", "tenant", s.cfg.ConsoleAuth.CFDefaultTenant)
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve Cloudflare Access default tenant: %w", err)
	}
	s.cfTenantID = id
	return nil
}

func (s *Server) accessTenant(ctx context.Context) (string, error) {
	if s.cfTenantID == "" {
		return "", errf(http.StatusForbidden, "forbidden", "Cloudflare Access default tenant is unavailable")
	}
	var id string
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1`, s.cfTenantID).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errf(http.StatusForbidden, "forbidden", "Cloudflare Access default tenant is unavailable")
	}
	return id, err
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
	email, id, err := s.cfAccess.user(r.Context(), token)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Email: email, Name: id.Name, Picture: id.Picture}
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
		id, err := s.accessTenant(r.Context())
		if err != nil {
			return p, err
		}
		p.TenantID = id
	}
	if !p.Can(scope) {
		return p, errf(http.StatusForbidden, "forbidden", "scope %q", scope)
	}
	return p, nil
}
