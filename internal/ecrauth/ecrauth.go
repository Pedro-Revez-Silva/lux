// Package ecrauth gets luxd an Amazon ECR authorization token for its
// managed image registry ([images] auth = "ecr"), with the AWS SDK's
// default credential chain (on a control host, its instance role).
//
// An ECR token is a registry password valid until the expiresAt ECR
// returns with it (12 hours from issue, per AWS), whatever the lifetime of
// the AWS credentials that asked for it; the SDK refreshes those by itself.
// The token is handed to runners for whole placements, so the Provider
// gives out only a token with at least MinValidity left, fetching a new
// one when the one it holds has less, and none at all when even a fresh
// token has less.
package ecrauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/smithy-go"
)

// Bounds of a Provider's calls.
const (
	// CallTimeout bounds one GetAuthorizationToken (credential resolution
	// included).
	CallTimeout = 10 * time.Second
	// FailureBackoff: after a failed refresh, no call is made for this
	// long; askers get the failure.
	FailureBackoff = 30 * time.Second
	// MinMinValidity and MaxMinValidity bound MinValidity: an ECR token
	// lives 12 hours.
	MinMinValidity = 30 * time.Minute
	MaxMinValidity = 11 * time.Hour
)

var hostRe = regexp.MustCompile(`^([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)

// ParseHost reads a private ECR registry host: its account and region.
func ParseHost(host string) (account, region string, ok bool) {
	m := hostRe.FindStringSubmatch(host)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// API is the ECR call the Provider makes.
type API interface {
	GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, opts ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// Provider holds one registry's token. Safe for concurrent use: one
// refresh at a time, others wait for it.
type Provider struct {
	host, account, region string
	minValidity           time.Duration
	// newAPI makes the client on first need: nothing calls AWS before.
	newAPI func(ctx context.Context) (API, error)
	now    func() time.Time

	mu       sync.Mutex
	api      API
	token    string
	expires  time.Time
	failedAt time.Time
	failure  error
}

// New is a Provider for registry host (account.dkr.ecr.region.amazonaws.com)
// giving out tokens with at least minValidity left. It calls no AWS API.
func New(host string, minValidity time.Duration) (*Provider, error) {
	account, region, ok := ParseHost(host)
	if !ok {
		return nil, fmt.Errorf("ecr: %q is not a private ECR registry host (<account>.dkr.ecr.<region>.amazonaws.com)", host)
	}
	if minValidity < MinMinValidity || minValidity > MaxMinValidity {
		return nil, fmt.Errorf("ecr: minimum validity %s outside %s to %s", minValidity, MinMinValidity, MaxMinValidity)
	}
	return &Provider{host: host, account: account, region: region, minValidity: minValidity, now: time.Now,
		newAPI: func(ctx context.Context) (API, error) {
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				return nil, err
			}
			return ecr.NewFromConfig(cfg), nil
		}}, nil
}

// WithAPI replaces the ECR client (tests) and the clock (nil: unchanged).
func (p *Provider) WithAPI(api API, now func() time.Time) *Provider {
	p.newAPI = func(context.Context) (API, error) { return api, nil }
	if now != nil {
		p.now = now
	}
	return p
}

// Credential is a registry credential, "AWS:<password>", with at least
// the minimum validity left, and when it expires. Its error never holds a
// token or a raw response: only what went wrong, and an AWS error code.
func (p *Provider) Credential(ctx context.Context) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.token != "" && p.expires.Sub(now) >= p.minValidity {
		return p.token, p.expires, nil
	}
	if p.failure != nil && now.Sub(p.failedAt) < FailureBackoff {
		return "", time.Time{}, p.failure
	}
	token, expires, err := p.fetch(ctx)
	if err != nil {
		p.failure, p.failedAt = err, now
		return "", time.Time{}, err
	}
	p.failure = nil
	p.token, p.expires = token, expires
	if left := expires.Sub(p.now()); left < p.minValidity {
		// Kept (a later ask may find it no better, without calling AWS
		// before the backoff), but not given out.
		p.failure, p.failedAt = fmt.Errorf("ecr: a new token has %s left, under the minimum validity %s", left.Round(time.Second), p.minValidity), now
		return "", time.Time{}, p.failure
	}
	return token, expires, nil
}

func (p *Provider) fetch(ctx context.Context) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if p.api == nil {
		api, err := p.newAPI(ctx)
		if err != nil {
			return "", time.Time{}, errors.New("ecr: no AWS configuration: " + code(err))
		}
		p.api = api
	}
	out, err := p.api.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{RegistryIds: []string{p.account}})
	if err != nil {
		return "", time.Time{}, errors.New("ecr: GetAuthorizationToken failed: " + code(err))
	}
	for _, d := range out.AuthorizationData {
		if u, err := url.Parse(aws.ToString(d.ProxyEndpoint)); err != nil || u.Host != p.host {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(aws.ToString(d.AuthorizationToken))
		user, pass, ok := strings.Cut(string(raw), ":")
		if err != nil || !ok || user != "AWS" || pass == "" {
			return "", time.Time{}, errors.New("ecr: the authorization token is not AWS:<password>")
		}
		if d.ExpiresAt == nil {
			return "", time.Time{}, errors.New("ecr: the authorization token has no expiry")
		}
		return "AWS:" + pass, *d.ExpiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("ecr: no authorization token for %s", p.host)
}

// code is what an AWS error says, without its message or anything it
// echoes: the API error code, a timeout, or that credentials are missing.
func code(err error) string {
	var api smithy.APIError
	switch {
	case errors.As(err, &api):
		return api.ErrorCode()
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case strings.Contains(err.Error(), "failed to refresh cached credentials") || strings.Contains(err.Error(), "no EC2 IMDS role found"):
		return "no AWS credentials"
	default:
		return "request failed"
	}
}
