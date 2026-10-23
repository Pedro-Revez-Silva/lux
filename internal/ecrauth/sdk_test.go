package ecrauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

// The real SDK client, against a local stand-in for ECR's JSON protocol:
// the request is a signed GetAuthorizationToken for the configured
// account, and its answer (or error) is read as Provider expects. No AWS.
func TestSDKAgainstAFakeECR(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var targets, auths []string
	deny := false
	expires := time.Now().Add(12 * time.Hour).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		mu.Lock()
		bodies, targets, auths = append(bodies, body), append(targets, r.Header.Get("X-Amz-Target")), append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		if deny {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"__type":"AccessDeniedException","message":"User: arn:aws:sts::123456789012:assumed-role/luxd/AKIASECRETSESSION is not authorized"}`)
			return
		}
		tok := base64.StdEncoding.EncodeToString([]byte("AWS:the-registry-password"))
		fmt.Fprintf(w, `{"authorizationData":[{"authorizationToken":%q,"expiresAt":%d,"proxyEndpoint":"https://%s"}]}`, tok, expires.Unix(), host)
	}))
	defer srv.Close()
	client := ecr.New(ecr.Options{Region: "eu-north-1", BaseEndpoint: aws.String(srv.URL),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secretkey", "")})
	p, err := New(host, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p.WithAPI(client, nil)
	tok, exp, err := p.Credential(context.Background())
	if err != nil || tok != "AWS:the-registry-password" || !exp.Equal(expires) {
		t.Fatalf("%q %s %v", tok, exp, err)
	}
	if targets[0] != "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken" ||
		!strings.HasPrefix(auths[0], "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auths[0], "/eu-north-1/ecr/") {
		t.Fatalf("request: %s %s", targets[0], auths[0])
	}
	if ids, _ := bodies[0]["registryIds"].([]any); len(ids) != 1 || ids[0] != "123456789012" {
		t.Fatalf("registryIds %v", bodies[0])
	}
	// Cached: no second request.
	p.Credential(context.Background())
	if len(targets) != 1 {
		t.Fatalf("%d requests", len(targets))
	}
	// A denial, from a fresh Provider: the code, nothing of the message.
	deny = true
	q, _ := New(host, 4*time.Hour)
	q.WithAPI(client, nil)
	if _, _, err := q.Credential(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDeniedException") ||
		strings.Contains(err.Error(), "AKIASECRETSESSION") || strings.Contains(err.Error(), "arn:aws") {
		t.Fatalf("denial: %v", err)
	}
}
