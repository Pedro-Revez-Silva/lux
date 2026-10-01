package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// rpcConn is a JSON-RPC 2.0 client over newline-delimited JSON on a
// process's stdio: the framing both ACP agents and Codex's app-server use.
type rpcConn struct {
	lw      lineWriter
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	// closed: readLoop has returned; no response can come.
	closed bool
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage
	Err    *rpcError
}

func (c *rpcConn) attach(w io.Writer) {
	c.mu.Lock()
	if c.pending == nil {
		c.pending = map[int64]chan rpcResponse{}
	}
	c.mu.Unlock()
	c.lw.set(w)
}

// call sends a request and waits for its response.
func (c *rpcConn) call(method string, params any) (json.RawMessage, error) {
	wait, err := c.start(method, params)
	if err != nil {
		return nil, err
	}
	return wait()
}

// start sends a request and returns once it is written; wait blocks for
// its response.
func (c *rpcConn) start(method string, params any) (wait func() (json.RawMessage, error), err error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: process exited", method)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.lw.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return func() (json.RawMessage, error) {
		r := <-ch
		if r.Err != nil {
			return nil, fmt.Errorf("%s: %s (%d)", method, r.Err.Message, r.Err.Code)
		}
		return r.Result, nil
	}, nil
}

func (c *rpcConn) notify(method string, params any) error {
	return c.lw.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcConn) reply(id json.RawMessage, result any) error {
	return c.lw.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *rpcConn) replyError(id json.RawMessage, code int, msg string) error {
	return c.lw.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// readLoop dispatches everything the process writes until its stdout
// closes: responses to their callers, requests and notifications to the
// handlers, and anything that is not JSON-RPC to other (as plain output).
// Pending calls fail when it returns.
func (c *rpcConn) readLoop(r io.Reader, onRequest, onNotify func(rpcMsg), other func([]byte)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m rpcMsg
		// Codex's app-server omits "jsonrpc"; an id or a method is enough.
		if err := json.Unmarshal(line, &m); err != nil || (len(m.ID) == 0 && m.Method == "") {
			other(append(append([]byte{}, line...), '\n'))
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			onRequest(m)
		case m.Method != "":
			onNotify(m)
		case len(m.ID) > 0:
			var id int64
			_ = json.Unmarshal(m.ID, &id)
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcResponse{Result: m.Result, Err: m.Error}
			}
		}
	}
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- rpcResponse{Err: &rpcError{Message: "process exited"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}
