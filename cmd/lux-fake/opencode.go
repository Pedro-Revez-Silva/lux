package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// opencodeServer is the HTTP server `opencode acp --port <p>` runs in the
// same process as its ACP, as far as lux uses it (verified against
// opencode 1.18.31, docs/agent-protocols.md §3):
//
//	GET  /event                         the event bus, as server-sent events
//	POST /session/{id}/prompt_async     {"messageID", "parts"}: store a user
//	                                    message under messageID and join it
//	                                    to the running loop (204)
//	GET  /session/{id}/message          the stored messages, oldest first
//	GET  /session/status                {sessionID: {"type":"busy"}} while a
//	                                    loop runs, {} otherwise
//
// The bus reports each stored user message (message.updated, role user),
// each model step as an assistant message.updated whose parentID is the
// user message it answers, and a loop's end (session.idle).
type opencodeServer struct {
	a    *agent
	mu   sync.Mutex
	subs []chan []byte
	n    int
	// msgs: the session's stored messages; busy: a loop runs.
	msgs []map[string]string
	busy bool
	// run starts a loop for a prompt when none is running.
	run func(p prompt)
}

func (o *opencodeServer) listen(args []string) {
	i := slices.Index(args, "--port")
	if i < 0 || i+1 >= len(args) {
		return
	}
	host := "127.0.0.1"
	if j := slices.Index(args, "--hostname"); j >= 0 && j+1 < len(args) {
		host = args[j+1]
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, args[i+1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "opencode server:", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /event", o.events)
	mux.HandleFunc("POST /session/{id}/prompt_async", o.promptAsync)
	mux.HandleFunc("GET /session/{id}/message", o.messages)
	mux.HandleFunc("GET /session/status", o.sessionStatus)
	go http.Serve(l, mux)
}

func (o *opencodeServer) events(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 256)
	o.mu.Lock()
	o.subs = append(o.subs, ch)
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.subs = slices.DeleteFunc(o.subs, func(c chan []byte) bool { return c == ch })
		o.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: %s\n\n", `{"type":"server.connected","properties":{}}`)
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
	}
}

func (o *opencodeServer) publish(typ string, props map[string]any) {
	b, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("evt_%d", time.Now().UnixNano()), "type": typ, "properties": props})
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, c := range o.subs {
		select {
		case c <- b:
		default:
		}
	}
}

// messageID is a new id for a message OpenCode stores itself.
func (o *opencodeServer) messageID() string {
	o.mu.Lock()
	o.n++
	n := o.n
	o.mu.Unlock()
	return fmt.Sprintf("msg_%012x%014d", time.Now().UnixMilli()*0x1000&(1<<48-1), n)
}

func (o *opencodeServer) messages(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	out := []map[string]any{}
	for _, m := range o.msgs {
		out = append(out, map[string]any{"info": m, "parts": []any{}})
	}
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (o *opencodeServer) sessionStatus(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	st := map[string]any{}
	if o.busy {
		st[o.a.session] = map[string]string{"type": "busy"}
	}
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func (o *opencodeServer) store(m map[string]string) {
	o.mu.Lock()
	o.msgs = append(o.msgs, m)
	o.mu.Unlock()
}

// stored reports a user message saved to the session.
func (o *opencodeServer) stored(p prompt) {
	o.store(map[string]string{"id": p.id, "role": "user", "sessionID": o.a.session})
	o.publish("message.updated", map[string]any{"sessionID": o.a.session, "info": map[string]any{
		"id": p.id, "role": "user", "sessionID": o.a.session, "time": map[string]any{"created": time.Now().UnixMilli()}}})
}

// step reports a model step answering the user message parent.
func (o *opencodeServer) step(parent string) {
	id := o.messageID()
	o.store(map[string]string{"id": id, "role": "assistant", "parentID": parent, "sessionID": o.a.session})
	o.publish("message.updated", map[string]any{"sessionID": o.a.session, "info": map[string]any{
		"id": id, "role": "assistant", "parentID": parent, "sessionID": o.a.session,
		"time": map[string]any{"created": time.Now().UnixMilli()}}})
}

func (o *opencodeServer) status(busy bool) {
	o.mu.Lock()
	o.busy = busy
	o.mu.Unlock()
	if busy {
		o.publish("session.status", map[string]any{"sessionID": o.a.session, "status": map[string]string{"type": "busy"}})
		return
	}
	o.publish("session.status", map[string]any{"sessionID": o.a.session, "status": map[string]string{"type": "idle"}})
	o.publish("session.idle", map[string]any{"sessionID": o.a.session})
}

func (o *opencodeServer) promptAsync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID string     `json:"messageID"`
		Parts     textBlocks `json:"parts"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || r.PathValue("id") != o.a.session {
		http.Error(w, `{"name":"BadRequest"}`, http.StatusBadRequest)
		return
	}
	if body.MessageID != "" && !strings.HasPrefix(body.MessageID, "msg_") {
		http.Error(w, `{"name":"BadRequest","data":{"message":"messageID must start with msg_"}}`, http.StatusBadRequest)
		return
	}
	p := prompt{text: body.Parts.String(), id: body.MessageID}
	if p.id == "" {
		p.id = o.messageID()
	}
	o.stored(p)
	if !o.a.addSteer(p) {
		go o.run(p)
	}
	w.WriteHeader(http.StatusNoContent)
}
