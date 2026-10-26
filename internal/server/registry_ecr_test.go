package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

const testPullRole = "arn:aws:iam::123456789012:role/lux-registry-pull"

// fakeECR answers AssumeRole with session credentials for the owner read
// from the session tag, and GetAuthorizationToken with AWS:<owner>-pw-<n>.
type fakeECR struct {
	mu         sync.Mutex
	now        func() time.Time
	assumed    []sts.AssumeRoleInput
	tokens     int
	sessionTTL time.Duration // 0: the requested duration
	tokenTTL   time.Duration // 0: 12h
	fail       error         // AssumeRole fails
	noCreds    bool          // AssumeRole answers no credentials
	noExpiry   bool          // AssumeRole's credentials carry no expiry
	// token replaces GetAuthorizationToken's answer.
	token func() (*ecrtypes.AuthorizationData, error)
	// contexts gets the context of each call, AssumeRole's then
	// GetAuthorizationToken's.
	contexts []context.Context
}

func (f *fakeECR) AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*ststypes.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assumed = append(f.assumed, *in)
	f.contexts = append(f.contexts, ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.fail != nil {
		return nil, f.fail
	}
	if f.noCreds {
		return nil, nil
	}
	owner := ""
	for _, tag := range in.Tags {
		if aws.ToString(tag.Key) == "lux-owner" {
			owner = aws.ToString(tag.Value)
		}
	}
	ttl := f.sessionTTL
	if ttl == 0 {
		ttl = time.Duration(aws.ToInt32(in.DurationSeconds)) * time.Second
	}
	exp := f.now().Add(ttl)
	c := &ststypes.Credentials{AccessKeyId: aws.String("ASIA" + owner), SecretAccessKey: aws.String("sk"), SessionToken: aws.String(owner), Expiration: &exp}
	if f.noExpiry {
		c.Expiration = nil
	}
	return c, nil
}

func (f *fakeECR) AuthorizationToken(ctx context.Context, c *ststypes.Credentials) (*ecrtypes.AuthorizationData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = append(f.contexts, ctx)
	f.tokens++
	if f.token != nil {
		return f.token()
	}
	ttl := cmp.Or(f.tokenTTL, 12*time.Hour)
	exp := f.now().Add(ttl)
	tok := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "AWS:%s-pw-%d", aws.ToString(c.SessionToken), f.tokens))
	return &ecrtypes.AuthorizationData{AuthorizationToken: &tok, ExpiresAt: &exp}, nil
}

func testECR(now func() time.Time) (*ECRSource, *fakeECR) {
	f := &fakeECR{now: now}
	return &ECRSource{roleARN: testPullRole, api: f}, f
}

var ecrNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// One AssumeRole per mint: on the pull role, named and tagged for the
// owner, for an hour; the ECR token decoded to AWS and its password.
func TestECRMintAssumesTheRoleForTheOwner(t *testing.T) {
	src, fake := testECR(func() time.Time { return ecrNow })
	for _, owner := range []string{"acme", "team.b"} {
		tok, err := src.Mint(context.Background(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if tok.User != "AWS" || !strings.HasPrefix(tok.Password, owner+"-pw-") || !tok.Expires.Equal(ecrNow.Add(time.Hour)) {
			t.Fatalf("%s: %+v", owner, tok)
		}
	}
	if len(fake.assumed) != 2 || fake.tokens != 2 {
		t.Fatalf("%d AssumeRole, %d GetAuthorizationToken calls, want 2 each", len(fake.assumed), fake.tokens)
	}
	for i, owner := range []string{"acme", "team.b"} {
		c := fake.assumed[i]
		if aws.ToString(c.RoleArn) != testPullRole ||
			aws.ToString(c.RoleSessionName) != "luxd-registry-"+owner || aws.ToInt32(c.DurationSeconds) != 3600 ||
			len(c.Tags) != 1 || aws.ToString(c.Tags[0].Key) != "lux-owner" || aws.ToString(c.Tags[0].Value) != owner {
			t.Errorf("AssumeRole for %s: role %s session %s duration %d tags %+v", owner, aws.ToString(c.RoleArn),
				aws.ToString(c.RoleSessionName), aws.ToInt32(c.DurationSeconds), c.Tags)
		}
	}
}

// The token's expiry is the earlier of the session's and ECR's.
func TestECRExpiryIsTheEarlier(t *testing.T) {
	for name, tc := range map[string]struct{ session, token, want time.Duration }{
		"ECR first":     {time.Hour, 45 * time.Minute, 45 * time.Minute},
		"session first": {50 * time.Minute, 12 * time.Hour, 50 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			src, fake := testECR(func() time.Time { return ecrNow })
			fake.sessionTTL, fake.tokenTTL = tc.session, tc.token
			tok, err := src.Mint(context.Background(), "acme")
			if err != nil {
				t.Fatal(err)
			}
			if !tok.Expires.Equal(ecrNow.Add(tc.want)) {
				t.Fatalf("expires %v, want %v", tok.Expires.Sub(ecrNow), tc.want)
			}
		})
	}
	// Session credentials without an expiry: ECR's.
	src, fake := testECR(func() time.Time { return ecrNow })
	fake.tokenTTL, fake.noExpiry = 2*time.Hour, true
	if tok, err := src.Mint(context.Background(), "acme"); err != nil || !tok.Expires.Equal(ecrNow.Add(2*time.Hour)) {
		t.Fatalf("%+v %v, want ECR's expiry", tok, err)
	}
}

// requireECRFails mints acme with fake configured by setup, and requires
// no token and an error naming the step, never sentinel.
func requireECRFails(t *testing.T, setup func(*fakeECR), step, sentinel string) *fakeECR {
	t.Helper()
	src, fake := testECR(func() time.Time { return ecrNow })
	setup(fake)
	tok, err := src.Mint(context.Background(), "acme")
	if tok != (RegistryToken{}) {
		t.Fatalf("a token on failure: %+v", tok)
	}
	if err == nil || !strings.Contains(err.Error(), step) || (sentinel != "" && strings.Contains(err.Error(), sentinel)) {
		t.Fatalf("error %v: want %q, never %q", err, step, sentinel)
	}
	return fake
}

func TestECRSTSFailure(t *testing.T) {
	fake := requireECRFails(t, func(f *fakeECR) {
		f.fail = errors.New("AccessDenied: not authorized to perform sts:TagSession")
	}, "sts:AssumeRole: AccessDenied", "")
	if fake.tokens != 0 {
		t.Fatal("ECR called after AssumeRole failed")
	}
}

// AssumeRole answering without credentials (nor an error) is a failure,
// and ECR is not called.
func TestECRSTSNoCredentials(t *testing.T) {
	if fake := requireECRFails(t, func(f *fakeECR) { f.noCreds = true }, "sts:AssumeRole: no credentials", ""); fake.tokens != 0 {
		t.Fatal("ECR called without credentials")
	}
}

func TestECRFailure(t *testing.T) {
	requireECRFails(t, func(f *fakeECR) {
		f.token = func() (*ecrtypes.AuthorizationData, error) {
			return nil, errors.New("AccessDeniedException: ecr:GetAuthorizationToken")
		}
	}, "ecr:GetAuthorizationToken: AccessDeniedException", "")
}

// A response without a token or its expiry is no token.
func TestECREmptyToken(t *testing.T) {
	future := ecrNow.Add(time.Hour)
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:SENTINEL-empty"))
	for name, d := range map[string]*ecrtypes.AuthorizationData{
		"nil":        nil,
		"no token":   {ExpiresAt: &future},
		"no expiry":  {AuthorizationToken: &tok},
		"empty data": {},
	} {
		t.Run(name, func(t *testing.T) {
			requireECRFails(t, func(f *fakeECR) {
				f.token = func() (*ecrtypes.AuthorizationData, error) { return d, nil }
			}, "ecr:GetAuthorizationToken: no token", "SENTINEL")
		})
	}
}

// A token that does not decode to AWS:<password> is refused without
// quoting it.
func TestECRMalformedToken(t *testing.T) {
	for name, raw := range map[string]string{
		"not base64":  "SENTINEL!!not*base64",
		"no colon":    base64.StdEncoding.EncodeToString([]byte("SENTINEL-no-colon")),
		"not AWS":     base64.StdEncoding.EncodeToString([]byte("root:SENTINEL-pw")),
		"no password": base64.StdEncoding.EncodeToString([]byte("AWS:")),
	} {
		step := "token is not AWS:password"
		if name == "not base64" {
			step = "token is not base64"
		}
		t.Run(name, func(t *testing.T) {
			exp := ecrNow.Add(time.Hour)
			requireECRFails(t, func(f *fakeECR) {
				f.token = func() (*ecrtypes.AuthorizationData, error) {
					return &ecrtypes.AuthorizationData{AuthorizationToken: &raw, ExpiresAt: &exp}, nil
				}
			}, step, "SENTINEL")
		})
	}
}

// Mint hands its context to both AWS calls: a cancelled one reaches them
// and the mint fails with it.
func TestECRMintPassesItsContext(t *testing.T) {
	src, fake := testECR(func() time.Time { return ecrNow })
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "mint")
	if _, err := src.Mint(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if len(fake.contexts) != 2 || fake.contexts[0].Value(key{}) != "mint" || fake.contexts[1].Value(key{}) != "mint" {
		t.Fatalf("the AWS calls did not get Mint's context: %v", fake.contexts)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := src.Mint(cancelled, "acme"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mint: %v, want context.Canceled", err)
	}
}

// The SDK client fails on a cancelled context before any request leaves.
// Both services' endpoints are a local server that fails the test on any
// request; static credentials, empty shared config and no inherited
// endpoint, profile or role setting keep the SDK off every real endpoint.
func TestECRSDKHonoursCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the SDK sent %s %s after its context was cancelled", r.Method, r.URL)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer srv.Close()
	empty := func(name string) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDTEST", "AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_ENDPOINT_URL": srv.URL, "AWS_ENDPOINT_URL_STS": srv.URL, "AWS_ENDPOINT_URL_ECR": srv.URL,
		"AWS_CONFIG_FILE": empty("config"), "AWS_SHARED_CREDENTIALS_FILE": empty("credentials"),
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	for _, k := range []string{"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_SESSION_TOKEN",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_USE_FIPS_ENDPOINT", "AWS_USE_DUALSTACK_ENDPOINT"} {
		t.Setenv(k, "") // restored when the test ends
		os.Unsetenv(k)
	}
	src := NewECRSource(ECRConfig{Region: "eu-west-1", PullRoleARN: testPullRole})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := src.Mint(ctx, "acme"); !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "sts:AssumeRole: ") {
		t.Fatalf("AssumeRole on a cancelled context: %v", err)
	}
	exp := time.Now().Add(time.Hour)
	creds := &ststypes.Credentials{AccessKeyId: aws.String("ASIA"), SecretAccessKey: aws.String("sk"), SessionToken: aws.String("st"), Expiration: &exp}
	if _, err := src.api.AuthorizationToken(ctx, creds); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetAuthorizationToken on a cancelled context: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancelled calls took %s", d)
	}
}

// Behind the minter, an ECR token reaches the runner as the auth.json
// entry <host>/<owner>, user AWS; an ECR failure's backoff never quotes
// a token.
func TestECRBehindTheMinter(t *testing.T) {
	clock := &testClock{t: ecrNow}
	src, fake := testECR(clock.now)
	m := newRegistryMinter(RegistryConfig{Host: testRegistryHost, Grants: []RegistryGrant{{Owner: "acme", Tenants: []string{"*"}}}, Source: src}, clock.now)
	useManualTimers(m)
	done := make(minted, 1)
	m.ready = done.signal
	closeMinter(t, m)
	creds, errs := settle(t, m, done, 1, "t1")
	if errs != nil || len(creds) != 1 || creds[0].Registry != testRegistryHost+"/acme" || creds[0].User != "AWS" || creds[0].Password != "acme-pw-1" {
		t.Fatalf("%+v %v", creds, errs)
	}

	raw := base64.StdEncoding.EncodeToString([]byte("root:SENTINEL-pw"))
	exp := ecrNow.Add(12 * time.Hour)
	fake.mu.Lock()
	fake.token = func() (*ecrtypes.AuthorizationData, error) {
		return &ecrtypes.AuthorizationData{AuthorizationToken: &raw, ExpiresAt: &exp}, nil
	}
	fake.mu.Unlock()
	clock.advance(31 * time.Minute) // below the reuse floor: a fresh mint
	_, errs = settle(t, m, done, 2, "t1")
	if errs["acme"] == nil || strings.Contains(errs["acme"].Error(), "SENTINEL") {
		t.Fatalf("failed mint: %v", errs)
	}
	_, errs = settle(t, m, done, 3, "t1")
	if !errors.Is(errs["acme"], errRegistryBackoff) || strings.Contains(errs["acme"].Error(), "SENTINEL") {
		t.Fatalf("backoff: %v", errs)
	}
}
