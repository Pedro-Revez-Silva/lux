// Package client is a Go client for the lux API, used by the CLI.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

type Client struct {
	Base string
	Key  string
	// Tenant, for an operator key: every request is narrowed to this tenant
	// (an id or a name). Tenant keys ignore it.
	Tenant string
	HTTP   *http.Client
}

func New(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{}}
}

// url is the absolute URL of path (which may carry a query).
func (c *Client) url(path string) string {
	if c.Tenant == "" {
		return c.Base + path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return c.Base + path + sep + "tenant=" + url.QueryEscape(c.Tenant)
}

// APIError is an error response from luxd.
type APIError struct {
	Status     int
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Details    []string `json:"details"`
	RetryAfter int      `json:"retryAfter"`
	Reason     string   `json:"reason"`
	SnapshotID string   `json:"snapshotId"`
}

func (e *APIError) Error() string {
	s := e.Message
	if len(e.Details) > 1 {
		s += "\n  - " + strings.Join(e.Details, "\n  - ")
	}
	return s
}

func (c *Client) Do(ctx context.Context, method, path string, in, out any, hdr ...string) error {
	_, err := c.DoHeader(ctx, method, path, in, out, hdr...)
	return err
}

// DoHeader is Do, also returning the response's headers.
func (c *Client) DoHeader(ctx context.Context, method, path string, in, out any, hdr ...string) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.Header, decodeError(resp)
	}
	if out == nil {
		return resp.Header, nil
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error APIError `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		e.Error.Status = resp.StatusCode
		return &e.Error
	}
	return &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(b))}
}

// SSEEvent is one server-sent event.
type SSEEvent struct {
	Event string
	Data  json.RawMessage
}

// Stream opens an SSE endpoint and calls fn for each event until the
// stream ends, fn returns an error, or ctx is cancelled.
func (c *Client) Stream(ctx context.Context, path string, q url.Values, fn func(SSEEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path+"?"+q.Encode()), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var ev SSEEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Event != "" || ev.Data != nil {
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = SSEEvent{}
		case strings.HasPrefix(line, "event: "):
			ev.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = append(ev.Data, strings.TrimPrefix(line, "data: ")...)
		}
	}
	return sc.Err()
}

// Dial opens an interactive stream (exec, attach, a port): a WebSocket
// carrying JSON messages (see the server's stream.go). An API error
// before the upgrade (not running, no such port) is returned as such.
func (c *Client) Dial(ctx context.Context, path string) (*websocket.Conn, error) {
	u := "ws" + strings.TrimPrefix(c.url(path), "http")
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.Key}},
		HTTPClient: c.HTTP,
	})
	if err != nil {
		if resp != nil && resp.StatusCode/100 != 1 {
			if resp.Body == nil {
				resp.Body = io.NopCloser(strings.NewReader(""))
			}
			return nil, decodeError(resp)
		}
		return nil, err
	}
	ws.SetReadLimit(4 << 20)
	return ws, nil
}
