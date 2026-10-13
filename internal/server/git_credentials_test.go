package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// credsFixture: wakeFixture with r1's spec pushing app (credential
// GIT_TOKEN) and lib (no credential), h1's runner taking credentials and
// polling this luxd, and s's log, at debug, in the returned buffer.
func credsFixture(t *testing.T) (*Server, context.Context, string, *syncBuffer) {
	t.Helper()
	s, ctx, key, _ := wakeFixture(t)
	logs := &syncBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	execSQL(t, s, ctx, `UPDATE runs SET spec = '{"secrets": [{"name": "GIT_TOKEN"}], "git": {"push": {"branch": "lux/x"}, "repositories": [
		{"name": "app", "url": "https://x/app.git", "path": "/w/app", "credential": "GIT_TOKEN"},
		{"name": "lib", "url": "https://x/lib.git", "path": "/w/lib"}]}}'`)
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY['diff', 'git-credentials']`)
	s.hub.polled("h1")
	return s, ctx, key, logs
}

// deliver returns the push and sync messages pendingMessages hands h1 now
// (as a reconnect replays them), decoded.
func deliver(t *testing.T, s *Server, ctx context.Context) (pushes []proto.Push, syncs []proto.Sync, ids []int64) {
	t.Helper()
	execSQL(t, s, ctx, `UPDATE host_messages SET delivered_at = NULL WHERE host_id = 'h1'`)
	frames, err := s.pendingMessages(ctx, "h1", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		switch f.Type {
		case proto.MsgPush:
			var m proto.Push
			if err := json.Unmarshal(f.Data, &m); err != nil {
				t.Fatal(err)
			}
			pushes = append(pushes, m)
		case proto.MsgSync:
			var m proto.Sync
			if err := json.Unmarshal(f.Data, &m); err != nil {
				t.Fatal(err)
			}
			syncs = append(syncs, m)
		default:
			continue
		}
		ids = append(ids, f.ID)
	}
	return pushes, syncs, ids
}

// stored fails if value is in any row of any table, or in the log.
func notStored(t *testing.T, s *Server, ctx context.Context, logs *syncBuffer, value string) {
	t.Helper()
	if strings.Contains(logs.String(), value) {
		t.Fatalf("%q logged:\n%s", value, logs.String())
	}
	err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
		if err != nil {
			return err
		}
		tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, tbl := range tables {
			var n int
			name := pgx.Identifier{tbl}.Sanitize()
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+name+` x WHERE strpos(x::text, $1) > 0`, value).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				t.Errorf("%q stored in %d row(s) of %s", value, n, tbl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Credentials on a push or a sync reach the runner in the message
// delivered to it, and nowhere else: not in host_messages, events, any
// other row, nor the log. Once acked, luxd forgets them.
func TestPushAndSyncCarryCredentials(t *testing.T) {
	s, ctx, key, logs := credsFixture(t)
	const pushTok, syncTok = "ghs_push_value_1", "ghs_sync_value_2"
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{"requestId": "p1", "credentials": map[string]string{"app": pushTok}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	w = apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", map[string]any{"requestId": "s1",
		"sync": []map[string]string{{"repo": "app", "ref": "main"}}, "credentials": map[string]string{"app": syncTok}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("sync: %d %s", w.Code, w.Body)
	}
	pushes, syncs, msgIDs := deliver(t, s, ctx)
	if len(pushes) != 1 || pushes[0].RequestID != "p1" || pushes[0].Credentials["app"] != pushTok || len(pushes[0].Credentials) != 1 {
		t.Fatalf("delivered push: %+v", pushes)
	}
	if len(syncs) != 1 || syncs[0].RequestID != "s1" || syncs[0].Credentials["app"] != syncTok || len(syncs[0].Credentials) != 1 {
		t.Fatalf("delivered sync: %+v", syncs)
	}
	// Delivered again (a reconnect) before the ack: still with them.
	if again, _, _ := deliver(t, s, ctx); len(again) != 1 || again[0].Credentials["app"] != pushTok {
		t.Fatalf("redelivered push: %+v", again)
	}
	var names []string
	systemScan(t, s, `SELECT data->'credentials' FROM run_events WHERE run_id = $1 AND type = 'push.requested'`, []any{r1}, &names)
	if !slices.Equal(names, []string{"app"}) {
		t.Fatalf("push.requested names %v", names)
	}
	names = nil
	systemScan(t, s, `SELECT data->'credentials' FROM run_events WHERE run_id = $1 AND type = 'sync.requested'`, []any{r1}, &names)
	if !slices.Equal(names, []string{"app"}) {
		t.Fatalf("sync.requested names %v", names)
	}
	for _, tok := range []string{pushTok, syncTok} {
		notStored(t, s, ctx, logs, tok)
	}
	for _, id := range msgIDs {
		if err := s.ackMessage(ctx, "h1", id); err != nil {
			t.Fatal(err)
		}
		if _, held := s.secrets.getMsg(id); held {
			t.Fatalf("message %d's credentials held after its ack", id)
		}
	}
	for _, tok := range []string{pushTok, syncTok} {
		notStored(t, s, ctx, logs, tok)
	}
	// Without credentials: nothing attached, nothing named.
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{"requestId": "p2"}); w.Code != http.StatusAccepted {
		t.Fatalf("plain push: %d %s", w.Code, w.Body)
	}
	if pushes, _, _ := deliver(t, s, ctx); len(pushes) != 1 || pushes[0].Credentials != nil || pushes[0].CredentialRepos != nil {
		t.Fatalf("plain push delivered: %+v", pushes)
	}
}

// A name that is not one of the Run's repositories, one whose repository
// has no credential, and an empty value are 422, nothing queued; the
// value is in no error.
func TestCredentialsValidation(t *testing.T) {
	s, _, key, _ := credsFixture(t)
	for _, path := range []string{"push", "sync"} {
		for _, c := range []struct {
			creds map[string]string
			want  string
		}{
			{map[string]string{"nope": "secret-v"}, `the run has no repository \"nope\"`},
			{map[string]string{"lib": "secret-v"}, `repository \"lib\" has no credential`},
			{map[string]string{"app": ""}, "credentials[app]: empty"},
		} {
			body := map[string]any{"credentials": c.creds}
			if path == "sync" {
				body["sync"] = []map[string]string{{"repo": "app", "ref": "main"}}
			}
			before := countMessages(t, s)
			w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/"+path, body)
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid_request") ||
				!strings.Contains(w.Body.String(), c.want) || strings.Contains(w.Body.String(), "secret-v") {
				t.Fatalf("%s %v: %d %s", path, c.creds, w.Code, w.Body)
			}
			if after := countMessages(t, s); after != before {
				t.Fatalf("%s %v: a message was queued", path, c.creds)
			}
		}
	}
}

func countMessages(t *testing.T, s *Server) (n int) {
	t.Helper()
	systemScan(t, s, `SELECT count(*) FROM host_messages WHERE type IN ('push', 'sync')`, nil, &n)
	return n
}

// A host whose runner predates credentials: a push or sync with them is
// 409 git_credentials_unsupported, nothing queued; without them it goes.
func TestCredentialsOnAnOldRunner(t *testing.T) {
	s, ctx, key, _ := credsFixture(t)
	execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY['diff', 'sync-modes']`)
	sync := []map[string]string{{"repo": "app", "ref": "main"}}
	for _, c := range []struct {
		path, code string
		body       map[string]any
	}{
		{"push", "git_credentials_unsupported", map[string]any{"credentials": map[string]string{"app": "v"}}},
		{"sync", "git_credentials_unsupported", map[string]any{"sync": sync, "credentials": map[string]string{"app": "v"}}},
	} {
		before := countMessages(t, s)
		w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/"+c.path, c.body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), c.code) || !strings.Contains(w.Body.String(), "upgrade the host") {
			t.Fatalf("%s on an old runner: %d %s", c.path, w.Code, w.Body)
		}
		if countMessages(t, s) != before {
			t.Fatalf("%s: a refused request was queued", c.path)
		}
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{}); w.Code != http.StatusAccepted {
		t.Fatalf("push without credentials: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", map[string]any{"sync": sync}); w.Code != http.StatusAccepted {
		t.Fatalf("sync without credentials: %d %s", w.Code, w.Body)
	}
}

// A host whose runner has no connection to this luxd (it talks to another
// one): a push or sync with credentials is 503 host_unreachable, with no
// message queued, no event written and nothing held; without them it goes.
func TestCredentialsForAHostElsewhere(t *testing.T) {
	s, _, key, _ := credsFixture(t)
	s.hub.mu.Lock()
	delete(s.hub.polls, "h1")
	s.hub.mu.Unlock()
	sync := []map[string]string{{"repo": "app", "ref": "main"}}
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"push", map[string]any{"credentials": map[string]string{"app": "secret-v"}}},
		{"sync", map[string]any{"sync": sync, "credentials": map[string]string{"app": "secret-v"}}},
	} {
		w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/"+c.path, c.body)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusServiceUnavailable || body.Error.Code != "host_unreachable" {
			t.Fatalf("%s to a host elsewhere: %d %s", c.path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret-v") {
			t.Fatalf("%s to a host elsewhere leaked credentials: %s", c.path, w.Body)
		}
	}
	var queued, events, held int
	systemScan(t, s, `SELECT (SELECT count(*) FROM host_messages WHERE type IN ('push', 'sync')),
		(SELECT count(*) FROM run_events WHERE run_id = $1 AND type IN ('push.requested', 'sync.requested', 'git.push', 'git.sync', 'sync.done'))`,
		[]any{r1}, &queued, &events)
	s.secrets.mu.Lock()
	held = len(s.secrets.msgs)
	s.secrets.mu.Unlock()
	if queued != 0 || events != 0 || held != 0 {
		t.Fatalf("refused requests left %d message(s), %d event(s), %d held", queued, events, held)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{}); w.Code != http.StatusAccepted {
		t.Fatalf("push without credentials: %d %s", w.Code, w.Body)
	}
	if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", map[string]any{"sync": sync}); w.Code != http.StatusAccepted {
		t.Fatalf("sync without credentials: %d %s", w.Code, w.Body)
	}
	if n := countMessages(t, s); n != 2 {
		t.Fatalf("%d messages queued without credentials, want 2", n)
	}
}

// A push whose runner never acks it (its host is lost): luxd forgets its
// credentials with the Run's secrets, and when the Run is terminated
// through the API.
func TestHeldCredentialsGoWithTheRun(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(t *testing.T, s *Server, ctx context.Context, key string)
	}{
		{"secrets dropped", func(t *testing.T, s *Server, _ context.Context, _ string) { s.secrets.drop(r1) }},
		{"terminated", func(t *testing.T, s *Server, ctx context.Context, key string) {
			execSQL(t, s, ctx, `UPDATE runs SET state = 'lost' WHERE id = $1`, r1)
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/terminate", nil); w.Code != http.StatusAccepted {
				t.Fatalf("terminate: %d %s", w.Code, w.Body)
			}
			var state string
			systemScan(t, s, `SELECT state FROM runs WHERE id = $1`, []any{r1}, &state)
			if state != StateTerminated {
				t.Fatalf("run %s after terminate", state)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := credsFixture(t)
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{"requestId": "p1",
				"credentials": map[string]string{"app": "v1"}}); w.Code != http.StatusAccepted {
				t.Fatalf("push: %d %s", w.Code, w.Body)
			}
			pushes, _, msgIDs := deliver(t, s, ctx)
			if len(pushes) != 1 || pushes[0].Credentials["app"] != "v1" {
				t.Fatalf("delivered: %+v", pushes)
			}
			if _, held := s.secrets.getMsg(msgIDs[0]); !held {
				t.Fatal("credentials not held before the Run ends")
			}
			c.end(t, s, ctx, key)
			if _, held := s.secrets.getMsg(msgIDs[0]); held {
				t.Fatalf("message %d's credentials held after the Run's end", msgIDs[0])
			}
		})
	}
}

// A push or sync whose credentials this luxd no longer holds (it
// restarted), or whose host re-registered without the capability, is never
// delivered: it ends failed (git.push; git.sync and sync.done), saying why.
func TestCredentialedMessageRefusedAtDelivery(t *testing.T) {
	for _, c := range []struct {
		name, why string
		lose      func(s *Server, ctx context.Context)
	}{
		{"luxd restarted", credentialsGone, func(s *Server, _ context.Context) { s.secrets = newSecretCache() }},
		{"runner downgraded", credentialsUnsupported, func(s *Server, ctx context.Context) {
			execSQL(t, s, ctx, `UPDATE hosts SET capabilities = ARRAY['diff']`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ctx, key, _ := credsFixture(t)
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/push", map[string]any{"requestId": "p1",
				"credentials": map[string]string{"app": "v1"}}); w.Code != http.StatusAccepted {
				t.Fatalf("push: %d %s", w.Code, w.Body)
			}
			if w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+r1+"/sync", map[string]any{"requestId": "s1",
				"sync": []map[string]string{{"repo": "app", "ref": "main"}}, "credentials": map[string]string{"app": "v2"}}); w.Code != http.StatusAccepted {
				t.Fatalf("sync: %d %s", w.Code, w.Body)
			}
			c.lose(s, ctx)
			var queued []int64
			systemScan(t, s, `SELECT array_agg(id ORDER BY id) FROM host_messages WHERE type IN ('push', 'sync')`, nil, &queued)
			for range 2 {
				if pushes, syncs, _ := deliver(t, s, ctx); len(pushes)+len(syncs) != 0 {
					t.Fatalf("delivered: %+v %+v", pushes, syncs)
				}
			}
			if len(queued) != 2 {
				t.Fatalf("queued %v", queued)
			}
			for _, id := range queued {
				if _, held := s.secrets.getMsg(id); held {
					t.Fatalf("message %d's credentials held after its refusal", id)
				}
			}
			var results []proto.PushResult
			systemScan(t, s, `SELECT data->'results' FROM run_events WHERE run_id = $1 AND type = 'git.push' AND data->>'requestId' = 'p1'`, []any{r1}, &results)
			if len(results) != 2 || results[0].Repo != "app" || results[0].Status != "failed" || results[0].Error != c.why ||
				results[1].Repo != "lib" || results[1].Status != "failed" {
				t.Fatalf("git.push: %+v", results)
			}
			var syncErr, changed string
			systemScan(t, s, `SELECT (SELECT data->>'error' FROM run_events WHERE run_id = $1 AND type = 'git.sync' AND data->>'requestId' = 's1'),
				(SELECT data->>'changed' FROM run_events WHERE run_id = $1 AND type = 'sync.done' AND data->>'requestId' = 's1')`, []any{r1}, &syncErr, &changed)
			if syncErr != c.why || changed != "false" {
				t.Fatalf("git.sync error %q, sync.done changed %q", syncErr, changed)
			}
		})
	}
}
