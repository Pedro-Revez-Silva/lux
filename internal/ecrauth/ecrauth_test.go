package ecrauth

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/smithy-go"
)

const host = "123456789012.dkr.ecr.eu-north-1.amazonaws.com"

type fakeECR struct {
	calls    atomic.Int32
	life     time.Duration // token lifetime from now()
	endpoint string
	token    string
	err      error
	delay    time.Duration
	now      func() time.Time
	asked    []string
	mu       sync.Mutex
}

func (f *fakeECR) GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, _ ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.asked = append(f.asked, in.RegistryIds...)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	tok := f.token
	if tok == "" {
		tok = base64.StdEncoding.EncodeToString([]byte("AWS:pw-" + time.Now().String()))
	}
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []types.AuthorizationData{{
		AuthorizationToken: aws.String(tok),
		ExpiresAt:          aws.Time(f.now().Add(f.life)),
		ProxyEndpoint:      aws.String("https://" + f.endpoint),
	}}}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func provider(t *testing.T, f *fakeECR, min time.Duration) (*Provider, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	f.now = c.now
	if f.endpoint == "" {
		f.endpoint = host
	}
	p, err := New(host, min)
	if err != nil {
		t.Fatal(err)
	}
	return p.WithAPI(f, c.now), c
}

func TestParseHost(t *testing.T) {
	for h, ok := range map[string]bool{
		host: true, "123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn": true,
		"12345678901.dkr.ecr.eu-north-1.amazonaws.com": false, "123456789012.dkr.ecr.eu-north-1.amazonaws.com.evil.io": false,
		"ghcr.io": false, "public.ecr.aws": false, "123456789012.dkr.ecr.EU-north-1.amazonaws.com": false,
	} {
		if _, _, got := ParseHost(h); got != ok {
			t.Errorf("%s: %v", h, got)
		}
	}
	if _, err := New("ghcr.io", 4*time.Hour); err == nil {
		t.Fatal("ghcr.io accepted")
	}
	for _, d := range []time.Duration{time.Minute, 12 * time.Hour} {
		if _, err := New(host, d); err == nil {
			t.Fatalf("min validity %s accepted", d)
		}
	}
}

// Nothing calls AWS, or loads its configuration, until a credential is
// asked for.
func TestNoCallBeforeNeed(t *testing.T) {
	p, err := New(host, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if p.api != nil {
		t.Fatal("client made at New")
	}
	made := 0
	f := &fakeECR{life: 12 * time.Hour, endpoint: host, now: time.Now}
	p.newAPI = func(context.Context) (API, error) { made++; return f, nil }
	p.Credential(context.Background())
	p.Credential(context.Background())
	if made != 1 || f.calls.Load() != 1 {
		t.Fatalf("client made %d times, %d calls", made, f.calls.Load())
	}
}

// A token is reused while it has the minimum validity left, for the
// account the host names; renewed once it has less, never handed out
// with less: a 3h build and its publication fit in what it is given.
func TestTokenValidityAndRefresh(t *testing.T) {
	f := &fakeECR{life: 12 * time.Hour}
	p, c := provider(t, f, 4*time.Hour)
	tok, exp, err := p.Credential(context.Background())
	if err != nil || !strings.HasPrefix(tok, "AWS:pw-") || exp.Sub(c.t) != 12*time.Hour {
		t.Fatalf("%q %s %v", tok, exp, err)
	}
	if f.asked[0] != "123456789012" {
		t.Fatalf("asked for %v", f.asked)
	}
	issued := c.t
	for _, after := range []time.Duration{time.Hour, 7 * time.Hour, 8 * time.Hour} {
		c.t = issued.Add(after)
		got, gotExp, err := p.Credential(context.Background())
		if err != nil || got != tok || gotExp != exp {
			t.Fatalf("at +%s: a new token (%v)", after, err)
		}
		if gotExp.Sub(c.t) < 4*time.Hour {
			t.Fatalf("handed out with %s left", gotExp.Sub(c.t))
		}
	}
	c.t = exp.Add(-4*time.Hour + time.Second) // under 4h left
	got, gotExp, err := p.Credential(context.Background())
	if err != nil || got == tok || gotExp.Sub(c.t) != 12*time.Hour || f.calls.Load() != 2 {
		t.Fatalf("not renewed: %v calls=%d", err, f.calls.Load())
	}
}

// A fresh token with less than the minimum is not handed out, and the
// next ask within the backoff does not call ECR again.
func TestShortTokenIsRefused(t *testing.T) {
	f := &fakeECR{life: time.Hour}
	p, c := provider(t, f, 4*time.Hour)
	if tok, _, err := p.Credential(context.Background()); err == nil || tok != "" || !strings.Contains(err.Error(), "under the minimum validity") {
		t.Fatalf("%q %v", tok, err)
	}
	if _, _, err := p.Credential(context.Background()); err == nil || f.calls.Load() != 1 {
		t.Fatalf("calls %d %v", f.calls.Load(), err)
	}
	c.t = c.t.Add(FailureBackoff)
	f.life = 12 * time.Hour
	if tok, _, err := p.Credential(context.Background()); err != nil || tok == "" || f.calls.Load() != 2 {
		t.Fatalf("after backoff: %v calls %d", err, f.calls.Load())
	}
}

// Failures say what failed by code only (no message, nothing echoed),
// back off, and wrong answers (another registry, a malformed token) are
// refused.
func TestFailuresAreBoundedAndRedacted(t *testing.T) {
	secret := "AKIASECRET/very-secret-key"
	f := &fakeECR{life: 12 * time.Hour, err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "user " + secret + " is not authorized"}}
	p, c := provider(t, f, 4*time.Hour)
	_, _, err := p.Credential(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("%v", err)
	}
	for range 5 {
		p.Credential(context.Background())
	}
	if f.calls.Load() != 1 {
		t.Fatalf("hammered: %d calls", f.calls.Load())
	}
	c.t = c.t.Add(FailureBackoff)
	f.err = nil
	f.endpoint = "999999999999.dkr.ecr.eu-north-1.amazonaws.com"
	if _, _, err := p.Credential(context.Background()); err == nil || !strings.Contains(err.Error(), "no authorization token for "+host) {
		t.Fatalf("another registry's token: %v", err)
	}
	c.t = c.t.Add(FailureBackoff)
	f.endpoint, f.token = host, base64.StdEncoding.EncodeToString([]byte("notaws:x"))
	if _, _, err := p.Credential(context.Background()); err == nil || !strings.Contains(err.Error(), "not AWS:<password>") {
		t.Fatalf("malformed token: %v", err)
	}
	c.t = c.t.Add(FailureBackoff)
	f.token = ""
	if tok, _, err := p.Credential(context.Background()); err != nil || tok == "" {
		t.Fatalf("recovered: %v", err)
	}
}

// A call is bounded: a hung ECR answers with a timeout, not a hung luxd.
func TestCallIsBounded(t *testing.T) {
	f := &fakeECR{life: 12 * time.Hour, delay: time.Hour}
	p, _ := provider(t, f, 4*time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := p.Credential(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

// Concurrent askers share one refresh.
func TestConcurrentAskersShareOneCall(t *testing.T) {
	f := &fakeECR{life: 12 * time.Hour, delay: 50 * time.Millisecond}
	p, _ := provider(t, f, 4*time.Hour)
	var wg sync.WaitGroup
	toks := make([]string, 20)
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toks[i], _, _ = p.Credential(context.Background())
		}()
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("%d calls", f.calls.Load())
	}
	for _, tok := range toks {
		if tok == "" || tok != toks[0] {
			t.Fatalf("tokens %v", toks)
		}
	}
}
