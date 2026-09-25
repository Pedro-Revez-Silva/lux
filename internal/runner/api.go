package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// api is the runner's plain-HTTP client for luxd: polling, and blob
// uploads and downloads. Authenticated with the host token.
type api struct {
	base  string
	token string
	host  string
	http  *http.Client
}

func newAPI(base, token, host string) *api {
	return &api{
		base:  strings.TrimRight(base, "/"),
		token: token,
		host:  host,
		http: &http.Client{
			Timeout: 0, // uploads can be long; callers use contexts
			// Redirects are followed by hand so the token never goes to
			// S3 with a presigned URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (a *api) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+a.token)
	return r, nil
}

func (a *api) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return a.doJSON(ctx, http.MethodPost, path, bytes.NewReader(b), out)
}

func (a *api) getJSON(ctx context.Context, path string, out any) error {
	return a.doJSON(ctx, http.MethodGet, path, nil, out)
}

func (a *api) doJSON(ctx context.Context, method, path string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := a.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// upload PUTs a blob; luxd streams it into S3.
func (a *api) upload(ctx context.Context, blobID string, body io.Reader, size int64) error {
	r, err := a.req(ctx, http.MethodPut, "/runner/v1/blobs/"+blobID+"?host="+a.host, body)
	if err != nil {
		return err
	}
	r.ContentLength = size
	r.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{resp.StatusCode, fmt.Sprintf("upload %s: %s: %s", blobID, resp.Status, bytes.TrimSpace(msg))}
	}
	return nil
}

// download returns a blob's body: luxd redirects to a presigned URL, which
// is fetched without the host token.
func (a *api) download(ctx context.Context, blobID string) (io.ReadCloser, error) {
	r, err := a.req(ctx, http.MethodGet, "/runner/v1/blobs/"+blobID+"?host="+a.host, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusTemporaryRedirect {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		r2, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		resp, err = http.DefaultClient.Do(r2)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpError{resp.StatusCode, fmt.Sprintf("download %s: %s: %s", blobID, resp.Status, bytes.TrimSpace(msg))}
	}
	return resp.Body, nil
}

type httpError struct {
	Status int
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }
