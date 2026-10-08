package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/ids"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// tinyPNG is a 1×1 PNG.
var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa75\x81\x84\x00\x00\x00\x00IEND\xaeB`\x82")

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func png(name string) map[string]any {
	return map[string]any{"name": name, "contentType": "image/png", "data": b64(tinyPNG)}
}

// inputFixture: tenant t1 with a run-scoped key, a running claude-code Run
// r1 and a running generic Run r2, each placed on h1 at epoch 1.
func inputFixture(t *testing.T) (*Server, string) {
	t.Helper()
	s := testServer(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, state) VALUES ('h1', 'h1', 'ready')`)
	execSQL(t, s, ctx, `INSERT INTO runs (id, tenant_id, spec, state, current_epoch) VALUES
		('r1', 't1', '{"workload": {"adapter": "claude-code"}}', 'running', 1),
		('r2', 't1', '{"workload": {"adapter": "generic"}}', 'running', 1)`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state) VALUES
		('p1', 't1', 'r1', 'h1', 1, 'running'), ('p2', 't1', 'r2', 'h1', 1, 'running')`)
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'k', $1, ARRAY['run', 'read'])`, ids.Hash(key))
	return s, key
}

func postJSON(t *testing.T, s *Server, key, path string, body []byte) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func errCode(out map[string]any) (string, string) {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	msg, _ := e["message"].(string)
	return code, msg
}

// Each refusal of the contract, by code and by what its message names.
func TestInputAttachmentsRefused(t *testing.T) {
	s, key := inputFixture(t)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F'}
	big := append(append([]byte{}, tinyPNG...), make([]byte, 5<<20)...)
	many := []any{}
	for range 11 {
		many = append(many, png("a.png"))
	}
	cases := []struct {
		name, run, code, want string
		atts                  []any
	}{
		{"bad base64", "r1", "invalid_attachment", "attachments[0]: data is not standard base64",
			[]any{map[string]any{"name": "a.png", "contentType": "image/png", "data": "!!not base64!!"}}},
		{"data: prefix", "r1", "invalid_attachment", "attachments[0]: data is not standard base64",
			[]any{map[string]any{"name": "a.png", "contentType": "image/png", "data": "data:image/png;base64," + b64(tinyPNG)}}},
		{"type mismatch", "r1", "invalid_attachment", "attachments[1]: contentType image/png does not match its bytes (image/jpeg)",
			[]any{png("ok.png"), map[string]any{"name": "b.png", "contentType": "image/png", "data": b64(jpeg)}}},
		{"unknown type", "r1", "invalid_attachment", `attachments[0]: contentType "image/svg+xml"`,
			[]any{map[string]any{"name": "a.svg", "contentType": "image/svg+xml", "data": b64([]byte("<svg/>"))}}},
		{"too big", "r1", "invalid_attachment", "attachments[0]: too big",
			[]any{map[string]any{"name": "big.png", "contentType": "image/png", "data": b64(big)}}},
		{"too many", "r1", "invalid_attachment", "attachments: 11 attachments, at most 10", many},
		{"slash", "r1", "invalid_attachment", `attachments[0]: name must not contain / or \`, []any{png("../etc/passwd")}},
		{"backslash", "r1", "invalid_attachment", `attachments[0]: name must not contain / or \`, []any{png(`a\b.png`)}},
		{"control", "r1", "invalid_attachment", "attachments[0]: name must not contain NUL or control characters", []any{png("a\x00.png")}},
		{"empty name", "r1", "invalid_attachment", "attachments[0]: name is required", []any{png("")}},
		{"long name", "r1", "invalid_attachment", "attachments[0]: name is 256 bytes, at most 255", []any{png(strings.Repeat("n", 256))}},
		{"generic", "r2", "attachments_unsupported", "the Run's adapter is generic", []any{png("a.png")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"text": "look", "attachments": c.atts})
			status, out := postJSON(t, s, key, "/v1/runs/"+c.run+"/input", body)
			code, msg := errCode(out)
			if status != http.StatusBadRequest || code != c.code || !strings.Contains(msg, c.want) {
				t.Fatalf("got %d %s %q, want 400 %s containing %q", status, code, msg, c.code, c.want)
			}
		})
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_messages`); n != 0 {
		t.Fatalf("%d host messages queued by refused inputs", n)
	}
}

// The body limit still applies: images that fit each limit but not 8 MiB
// together are refused before validation.
func TestInputAttachmentsBodyLimit(t *testing.T) {
	s, key := inputFixture(t)
	img := append(append([]byte{}, tinyPNG...), make([]byte, 4<<20)...)
	body, _ := json.Marshal(map[string]any{"attachments": []any{
		map[string]any{"name": "a.png", "contentType": "image/png", "data": b64(img)},
		map[string]any{"name": "b.png", "contentType": "image/png", "data": b64(img)},
	}})
	status, out := postJSON(t, s, key, "/v1/runs/r1/input", body)
	code, msg := errCode(out)
	if status != http.StatusBadRequest || code != "bad_request" || !strings.Contains(msg, "request body too large") {
		t.Fatalf("got %d %v, want luxd's body limit", status, out)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM host_messages`); n != 0 {
		t.Fatalf("%d host messages queued", n)
	}
}

// Accepted: text may be empty; the runner gets the images, the input
// event their metadata only.
func TestInputAttachmentsAccepted(t *testing.T) {
	s, key := inputFixture(t)
	body, _ := json.Marshal(map[string]any{"requestId": "req-img", "interrupt": true, "attachments": []any{png("shot.png")}})
	status, out := postJSON(t, s, key, "/v1/runs/r1/input", body)
	if status != http.StatusAccepted {
		t.Fatalf("got %d %v", status, out)
	}
	var typ string
	var payload []byte
	systemScan(t, s, `SELECT type, payload FROM host_messages WHERE run_id = 'r1'`, nil, &typ, &payload)
	var in proto.Input
	if err := json.Unmarshal(payload, &in); err != nil {
		t.Fatal(err)
	}
	if typ != proto.MsgInput || !in.Interrupt || len(in.Attachments) != 1 || in.Attachments[0].Data != b64(tinyPNG) || in.Attachments[0].Name != "shot.png" {
		t.Fatalf("queued %s %+v", typ, in)
	}
	ev := queryOne[string](t, s, `SELECT data::text FROM run_events WHERE run_id = 'r1' AND type = 'input'`)
	var d struct {
		Attachments []spec.AttachmentMeta `json:"attachments"`
	}
	_ = json.Unmarshal([]byte(ev), &d)
	sum := sha256.Sum256(tinyPNG)
	want := spec.AttachmentMeta{Name: "shot.png", ContentType: "image/png", Size: len(tinyPNG), SHA256: hex.EncodeToString(sum[:])}
	if strings.Contains(ev, b64(tinyPNG)) || len(d.Attachments) != 1 || d.Attachments[0] != want {
		t.Fatalf("input event %s", ev)
	}
}

// An image of exactly 5 MiB decoded is accepted (its body is under 8 MiB).
func TestInputAttachmentsFiveMiBAccepted(t *testing.T) {
	s, key := inputFixture(t)
	img := make([]byte, 5<<20)
	copy(img, tinyPNG)
	body, _ := json.Marshal(map[string]any{"requestId": "req-big", "attachments": []any{
		map[string]any{"name": "big.png", "contentType": "image/png", "data": b64(img)}}})
	if status, out := postJSON(t, s, key, "/v1/runs/r1/input", body); status != http.StatusAccepted {
		t.Fatalf("got %d %v", status, out)
	}
	if n := queryOne[int](t, s, `SELECT (payload->'attachments'->0->>'data' = $1)::int FROM host_messages WHERE run_id = 'r1'`, b64(img)); n != 1 {
		t.Fatal("queued input without the image")
	}
}

// promptServer: tenant t1 with a default static pool.
func promptServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	mustPut(t, s, "t1", Pool{Name: "default", Provider: "static", IsDefault: mark(true)})
	return s
}

func submitWithAttachments(s *Server, adapter string, atts []spec.Attachment) (*submitRunOutput, error) {
	w := spec.Workload{Adapter: adapter, Command: []string{"agent"}, Prompt: "look", Attachments: atts}
	sp := spec.RunSpec{Image: spec.Image{Ref: "alpine"}, Workload: w}
	if adapter == "claude-code" {
		sp.Volumes = []spec.Volume{{Name: "home", Path: "/home/agent", Kind: "state"}}
	}
	return s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
}

// workload.attachments is checked at submit with /input's rules and codes;
// an accepted Run's views show their names and types, not their bytes.
func TestSubmitAttachments(t *testing.T) {
	s := promptServer(t)
	ok := spec.Attachment{Name: "a.png", ContentType: "image/png", Data: b64(tinyPNG)}
	for _, c := range []struct {
		adapter string
		atts    []spec.Attachment
		code    string
	}{
		{"claude-code", []spec.Attachment{{Name: "a.gif", ContentType: "image/gif", Data: b64(tinyPNG)}}, "invalid_attachment"},
		{"claude-code", []spec.Attachment{{Name: "a/b.png", ContentType: "image/png", Data: b64(tinyPNG)}}, "invalid_attachment"},
		{"generic", []spec.Attachment{ok}, "attachments_unsupported"},
		{"", []spec.Attachment{ok}, "attachments_unsupported"},
	} {
		_, err := submitWithAttachments(s, c.adapter, c.atts)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Code != c.code {
			t.Fatalf("%s %+v: got %v, want 400 %s", c.adapter, c.atts[0].Name, err, c.code)
		}
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d Runs created by refused submits", n)
	}
	out, err := submitWithAttachments(s, "claude-code", []spec.Attachment{ok})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Body.Spec.Workload.Attachments; len(got) != 1 || got[0].Data != "" || got[0].Name != "a.png" {
		t.Fatalf("returned spec attachments %+v", got)
	}
}

// A prompt's image bytes are kept beside the spec, not in it: Run views
// and the stored spec carry names and types; the first placement's Assign
// carries the bytes outside its spec; the ack drops them from the queued
// message; the Run keeps them past its first start.
func TestPromptAttachmentsBesideSpec(t *testing.T) {
	s := promptServer(t)
	ctx := context.Background()
	key := ids.Secret("luxk")
	execSQL(t, s, ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ('k1', 't1', 'k', $1, ARRAY['run', 'read'])`, ids.Hash(key))
	data := b64(tinyPNG)
	out, err := submitWithAttachments(s, "claude-code", []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	wantView := []spec.Attachment{{Name: "a.png", ContentType: "image/png"}}

	if spec := queryOne[string](t, s, `SELECT spec::text FROM runs WHERE id = $1`, id); strings.Contains(spec, data) {
		t.Fatalf("stored spec holds the bytes: %s", spec)
	}
	if got := queryOne[string](t, s, `SELECT prompt_attachments->0->>'data' FROM runs WHERE id = $1`, id); got != data {
		t.Fatalf("prompt_attachments data %q", got)
	}
	var one Run
	if code := getJSON(t, s, key, "/v1/runs/"+id, &one); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	var list struct {
		Runs []Run `json:"runs"`
	}
	if code := getJSON(t, s, key, "/v1/runs", &list); code != http.StatusOK || len(list.Runs) != 1 {
		t.Fatalf("list: %d %+v", code, list)
	}
	for at, got := range map[string][]spec.Attachment{"get": one.Spec.Workload.Attachments, "list": list.Runs[0].Spec.Workload.Attachments} {
		if len(got) != 1 || got[0] != wantView[0] {
			t.Fatalf("%s: attachments %+v, want %+v", at, got, wantView)
		}
	}

	readyHost(t, s, "h1", "default", "t1", false)
	schedule(t, s)
	var msgID int64
	var payload []byte
	systemScan(t, s, `SELECT id, payload FROM host_messages WHERE run_id = $1 AND type = $2`, []any{id, proto.MsgAssign}, &msgID, &payload)
	var a proto.Assign
	if err := json.Unmarshal(payload, &a); err != nil {
		t.Fatal(err)
	}
	if a.Resume != nil || len(a.PromptAttachments) != 1 || a.PromptAttachments[0].Data != data || a.PromptAttachments[0].Name != "a.png" {
		t.Fatalf("assign prompt attachments %+v", a.PromptAttachments)
	}
	if got := a.Spec.Workload.Attachments; len(got) != 1 || got[0] != wantView[0] {
		t.Fatalf("assign spec attachments %+v", got)
	}

	if err := s.ackMessage(ctx, "h1", msgID); err != nil {
		t.Fatal(err)
	}
	systemScan(t, s, `SELECT payload FROM host_messages WHERE id = $1`, []any{msgID}, &payload)
	a = proto.Assign{}
	if err := json.Unmarshal(payload, &a); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), data) || a.RunID != id || a.Spec.Workload.Adapter != "claude-code" {
		t.Fatalf("acked assign payload %s", payload)
	}

	applyRunStatus(t, s, id, a.Epoch, proto.Status{State: "running"})
	// Running alone is not a resume point: with no session and no snapshot
	// the next placement starts afresh and is sent them again.
	if got := queryOne[string](t, s, `SELECT coalesce(prompt_attachments->0->>'data', '') FROM runs WHERE id = $1`, id); got != data {
		t.Fatal("prompt_attachments dropped when the first placement started")
	}
}

// promptRun submits a claude-code Run with one image, places it and reports
// its placement running; it returns the Run's id and the image's base64.
func promptRun(t *testing.T) (*Server, string, string) {
	t.Helper()
	s := promptServer(t)
	s.cfg.LeaseDuration = time.Minute
	data := b64(tinyPNG)
	out, err := submitWithAttachments(s, "claude-code", []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	readyHost(t, s, "h1", "default", "t1", false)
	schedule(t, s)
	applyRunStatus(t, s, out.Body.ID, 1, proto.Status{State: "running"})
	return s, out.Body.ID, data
}

func applyRunStatus(t *testing.T, s *Server, id string, epoch int, st proto.Status) {
	t.Helper()
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return s.applyStatus(context.Background(), tx, "t1", id, epoch, st)
	}); err != nil {
		t.Fatal(err)
	}
}

// failResumeAssign exits epoch 1 with code 1, resumes the Run, schedules
// it and returns the epoch-2 Assign.
func failResumeAssign(t *testing.T, s *Server, id string) proto.Assign {
	t.Helper()
	code := 1
	applyRunStatus(t, s, id, 1, proto.Status{State: "exited", ExitCode: &code})
	if st := queryOne[string](t, s, `SELECT state FROM runs WHERE id = $1`, id); st != StateFailed {
		t.Fatalf("state %s after exit code 1, want failed", st)
	}
	if _, err := s.resumeRun(tenantCtx("t1"), &resumeRunInput{RunPath: RunPath{ID: id}}); err != nil {
		t.Fatal(err)
	}
	schedule(t, s)
	var payload []byte
	systemScan(t, s, `SELECT payload FROM host_messages WHERE run_id = $1 AND type = $2 AND epoch = 2`, []any{id, proto.MsgAssign}, &payload)
	var a proto.Assign
	if err := json.Unmarshal(payload, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// A Run that started but failed before it had a session or a snapshot is
// resumed as a first placement: the prompt goes again, with its images.
func TestPromptAttachmentsResentAfterSessionlessFailure(t *testing.T) {
	s, id, data := promptRun(t)
	a := failResumeAssign(t, s, id)
	if a.Resume != nil || len(a.PromptAttachments) != 1 || a.PromptAttachments[0].Data != data {
		t.Fatalf("epoch-2 assign resume %+v prompt attachments %+v", a.Resume, a.PromptAttachments)
	}
}

// Once a Run has a session, its next placement resumes it: no images are
// sent, and the Run drops them.
func TestPromptAttachmentsDroppedOnResume(t *testing.T) {
	s, id, _ := promptRun(t)
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return s.applyAdapterEvent(context.Background(), tx, "t1", id, 1, proto.AdapterEvent{SessionID: "sess-1"})
	}); err != nil {
		t.Fatal(err)
	}
	a := failResumeAssign(t, s, id)
	if a.Resume == nil || a.Resume.SessionID != "sess-1" || len(a.PromptAttachments) != 0 {
		t.Fatalf("epoch-2 assign resume %+v prompt attachments %+v", a.Resume, a.PromptAttachments)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND prompt_attachments IS NULL`, id); n != 1 {
		t.Fatal("prompt_attachments kept after a resuming placement was assigned")
	}
}

// A Run terminated before it was ever placed keeps no image bytes.
func TestPromptAttachmentsDroppedOnTerminate(t *testing.T) {
	s := promptServer(t)
	out, err := submitWithAttachments(s, "claude-code", []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: b64(tinyPNG)}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	if _, err := s.terminateRun(tenantCtx("t1"), &RunPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	if st := queryOne[string](t, s, `SELECT state FROM runs WHERE id = $1`, id); st != StateTerminated {
		t.Fatalf("state %s, want terminated", st)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND prompt_attachments IS NULL`, id); n != 1 {
		t.Fatal("prompt_attachments kept by a terminated Run")
	}
}

// A Run that succeeds keeps its prompt's image bytes: a resume of it with
// no session or snapshot is a first placement, which sends them again.
func TestPromptAttachmentsKeptOnSuccess(t *testing.T) {
	s := promptServer(t)
	out, err := submitWithAttachments(s, "claude-code", []spec.Attachment{{Name: "a.png", ContentType: "image/png", Data: b64(tinyPNG)}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	if err := s.db.Tx(context.Background(), store.System(), func(tx pgx.Tx) error {
		return setRunState(context.Background(), tx, "t1", id, StateSucceeded, "", 0)
	}); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND prompt_attachments IS NOT NULL`, id); n != 1 {
		t.Fatal("prompt_attachments dropped by a succeeded Run")
	}
}

// An acked input keeps its text and request id, not its images' bytes.
func TestInputAckDropsAttachments(t *testing.T) {
	s, key := inputFixture(t)
	body, _ := json.Marshal(map[string]any{"requestId": "req-img", "text": "see", "attachments": []any{png("shot.png")}})
	if status, out := postJSON(t, s, key, "/v1/runs/r1/input", body); status != http.StatusAccepted {
		t.Fatalf("got %d %v", status, out)
	}
	var id int64
	systemScan(t, s, `SELECT id FROM host_messages WHERE run_id = 'r1'`, nil, &id)
	if err := s.ackMessage(context.Background(), "h1", id); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	systemScan(t, s, `SELECT payload FROM host_messages WHERE id = $1`, []any{id}, &payload)
	var in proto.Input
	if err := json.Unmarshal(payload, &in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), b64(tinyPNG)) || len(in.Attachments) != 0 || in.Text != "see" || in.RequestID != "req-img" {
		t.Fatalf("acked input payload %s", payload)
	}
}
