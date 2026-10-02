package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

// secretsSpec has a secret of every kind: TOKEN (env), GIT_TOKEN (a git
// credential), REG (registry auth) and HDR (a service's header).
func secretsSpec() spec.RunSpec {
	return spec.RunSpec{
		Image: spec.Image{Ref: "alpine", RegistryAuth: []spec.RegistryAuth{{Registry: "ghcr.io", Secret: "REG"}}},
		Workload: spec.Workload{Adapter: "generic", Command: []string{"true"},
			Services: []spec.Service{{Name: "api", URL: "https://api.example.com/", Headers: []spec.MCPHeader{{Name: "Authorization", Secret: "HDR"}}}}},
		Volumes:   []spec.Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}},
		Git:       &spec.Git{Repositories: []spec.Repository{{Name: "app", URL: "https://git.example.com/app.git", Credential: "GIT_TOKEN"}}},
		Secrets:   []spec.Secret{{Name: "TOKEN", Value: "token-1"}, {Name: "GIT_TOKEN", Value: "git-1"}, {Name: "REG", Value: "reg-1"}, {Name: "HDR", Value: "hdr-1"}},
		Network:   spec.Network{Unrestricted: true},
		Placement: spec.Placement{Pool: "default"},
	}
}

// baseValues supplies every secret secretsSpec declares.
func baseValues() []spec.Secret {
	return []spec.Secret{{Name: "TOKEN", Value: "token-2"}, {Name: "GIT_TOKEN", Value: "git-2"}, {Name: "REG", Value: "reg-2"}, {Name: "HDR", Value: "hdr-2"}}
}

// stoppedWithSecrets submits secretsSpec as tenant t1 and stops it as
// stoppedRun does: a snapshot on host ha, uploaded.
func stoppedWithSecrets(t *testing.T, s *Server) string {
	t.Helper()
	return stoppedWith(t, s, secretsSpec())
}

// stoppedWith submits sp as tenant t1 and stops it as stoppedWithSecrets.
func stoppedWith(t *testing.T, s *Server, sp spec.RunSpec) string {
	t.Helper()
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t1', 't1') ON CONFLICT DO NOTHING`)
	namedPools(t, s, "default")
	out, err := s.submitRun(tenantCtx("t1"), &submitRunInput{Body: sp})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Body.ID
	execSQL(t, s, ctx, `INSERT INTO hosts (id, name, pool_id, state, capacity, last_heartbeat)
		VALUES ('ha', 'ha', 'default', 'ready', '{"cpus":4,"memory":8589934592}', now()) ON CONFLICT DO NOTHING`)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources)
		VALUES ('p-'||$1, 't1', $1, 'ha', 1, 'exited', '{}')`, id)
	execSQL(t, s, ctx, `INSERT INTO snapshots (id, tenant_id, run_id, placement_id, epoch, manifest, host_id, uploaded)
		VALUES ('snap-'||$1, 't1', $1, 'p-'||$1, 1, '{"volumes":[]}', 'ha', true)`, id)
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', current_epoch = 1, snapshot_id = 'snap-'||id, pool_id = 'default' WHERE id = $1`, id)
	return id
}

func resumeSecrets(ctx context.Context, s *Server, id string, req resumeRequest) (*resumeOutput, error) {
	return s.resumeRun(ctx, &resumeRunInput{RunPath: RunPath{ID: id}, Body: &req})
}

func with(secs []spec.Secret, more ...spec.Secret) []spec.Secret {
	return append(slices.Clone(secs), more...)
}

// storedSecrets is the Run's stored spec.secrets and runs.secrets.
func storedSecrets(t *testing.T, s *Server, id string) ([]spec.Secret, []spec.SecretRef) {
	t.Helper()
	var secs []spec.Secret
	var refs []spec.SecretRef
	systemScan(t, s, `SELECT coalesce(spec->'secrets', '[]'), secrets FROM runs WHERE id = $1`, []any{id}, &secs, &refs)
	return secs, refs
}

func secretNames(secs []spec.Secret) []string {
	var n []string
	for _, s := range secs {
		n = append(n, s.Name)
	}
	slices.Sort(n)
	return n
}

func refNames(refs []spec.SecretRef) []string {
	var n []string
	for _, r := range refs {
		n = append(n, r.Name)
	}
	return n
}

// A value for a name the Run lacks declares it: env by default, or file
// at its path, stored without the value, its fingerprint in runs.secrets,
// its value held for the placement. The next resume needs it.
func TestResumeDeclaresSecrets(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	out, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(),
		spec.Secret{Name: "EXTRA", Value: "extra-1"},
		spec.Secret{Name: "CONF", Value: "conf-1", As: "file", Path: "/home/agent/.conf"})})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.State != StateResuming {
		t.Fatalf("state %s", out.Body.State)
	}
	secs, refs := storedSecrets(t, s, id)
	byName := map[string]spec.Secret{}
	for _, sec := range secs {
		if sec.Value != "" {
			t.Errorf("stored a value for %s", sec.Name)
		}
		byName[sec.Name] = sec
	}
	if e := byName["EXTRA"]; e.As != "env" || e.RunnerOnly {
		t.Errorf("EXTRA stored as %+v, want env", e)
	}
	if c := byName["CONF"]; c.As != "file" || c.Path != "/home/agent/.conf" || c.RunnerOnly {
		t.Errorf("CONF stored as %+v, want file at its path", c)
	}
	if got, want := refNames(refs), []string{"CONF", "EXTRA", "GIT_TOKEN", "HDR", "REG", "TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runs.secrets %v, want %v", got, want)
	}
	for _, r := range refs {
		if r.Name == "EXTRA" && r.Fingerprint != spec.Fingerprint("EXTRA", "extra-1") {
			t.Errorf("EXTRA's fingerprint %s", r.Fingerprint)
		}
	}
	if vals, ok := s.secrets.get(id); !ok || vals["EXTRA"] != "extra-1" || vals["CONF"] != "conf-1" {
		t.Errorf("held values %v", vals)
	}
	if ev := resumeEvent(t, s, id); !reflect.DeepEqual(ev["addedSecrets"], []any{"EXTRA", "CONF"}) {
		t.Errorf("resume.requested addedSecrets %v", ev["addedSecrets"])
	}

	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	_, err = resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: baseValues()})
	he := refused(t, err, http.StatusUnprocessableEntity, "secrets_required", "resume without the declared secrets")
	if !reflect.DeepEqual(he.Details, []string{"CONF", "EXTRA"}) {
		t.Errorf("missing %v, want CONF and EXTRA", he.Details)
	}
}

// A declaration submit would refuse is a 422 invalid_spec naming it, and
// nothing changes: not the spec, not runs.secrets, not the state.
func TestResumeRefusesInvalidDeclarations(t *testing.T) {
	for _, c := range []struct {
		name    string
		secrets []spec.Secret
		problem string
	}{
		{"invalid name", []spec.Secret{{Name: "1BAD", Value: "v-12345"}}, `invalid name "1BAD"`},
		{"reserved prefix", []spec.Secret{{Name: "lux-x", Value: "v-12345", As: "file", Path: "/x"}}, "prefix is reserved"},
		{"not an env name", []spec.Secret{{Name: "a.b", Value: "v-12345"}}, "not a valid environment variable name"},
		{"file without a path", []spec.Secret{{Name: "F", Value: "v-12345", As: "file"}}, "absolute path"},
		{"file with a relative path", []spec.Secret{{Name: "F", Value: "v-12345", As: "file", Path: "rel"}}, "absolute path"},
		{"unknown as", []spec.Secret{{Name: "F", Value: "v-12345", As: "disk"}}, "as must be env, file or none"},
		{"twice", []spec.Secret{{Name: "NEW", Value: "v-12345"}, {Name: "NEW", Value: "v-67890"}}, `duplicate "NEW"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedWithSecrets(t, s)
			beforeSecs, beforeRefs := storedSecrets(t, s, id)
			_, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(), c.secrets...)})
			he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume")
			if !strings.Contains(strings.Join(he.Details, "\n"), c.problem) {
				t.Errorf("details %q, want %q", he.Details, c.problem)
			}
			secs, refs := storedSecrets(t, s, id)
			if !reflect.DeepEqual(secs, beforeSecs) || !reflect.DeepEqual(refs, beforeRefs) || storedState(t, s, id) != StateStopped {
				t.Errorf("a refused resume changed the Run: %+v %+v", secs, refs)
			}
		})
	}
}

// A credential of a repository the same resume adds stays addRepositories'
// (runner-only, as none), not an env secret.
func TestResumeAddedRepositoryCredentialIsNotDeclaredAsEnv(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	_, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{
		Secrets: with(baseValues(), spec.Secret{Name: "NEW_GIT", Value: "new-git-1"}),
		Git:     &resumeGit{Repositories: []spec.Repository{{Name: "two", URL: "https://git.example.com/two.git", Credential: "NEW_GIT"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secs, refs := storedSecrets(t, s, id)
	i := slices.IndexFunc(secs, func(sec spec.Secret) bool { return sec.Name == "NEW_GIT" })
	if i < 0 || secs[i].As != "none" || !secs[i].RunnerOnly {
		t.Fatalf("NEW_GIT stored as %+v, want runner-only", secs)
	}
	if n := refNames(refs); !slices.Contains(n, "NEW_GIT") {
		t.Fatalf("runs.secrets %v", n)
	}
	if ev := resumeEvent(t, s, id); ev["addedSecrets"] != nil {
		t.Errorf("addedSecrets %v: a credential is the repository's", ev["addedSecrets"])
	}
}

// A retry of a resume on a Run already resuming changes nothing: the first
// resume's secrets stand, a declaration or removal in the retry included,
// and it is answered 202 without a second resume.requested.
func TestResumeSecretsRetryWhileResuming(t *testing.T) {
	extra := spec.Secret{Name: "EXTRA", Value: "extra-1"}
	for _, c := range []struct {
		name  string
		retry resumeRequest
	}{
		{"identical", resumeRequest{Secrets: with(baseValues(), extra)}},
		{"another new secret", resumeRequest{Secrets: with(baseValues(), extra, spec.Secret{Name: "MORE", Value: "more-1"})}},
		{"a removal", resumeRequest{Secrets: baseValues(), RemoveSecrets: []string{"EXTRA"}}},
		{"an invalid removal", resumeRequest{Secrets: baseValues(), RemoveSecrets: []string{"GIT_TOKEN"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedWithSecrets(t, s)
			if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(), extra)}); err != nil {
				t.Fatal(err)
			}
			secs, refs := storedSecrets(t, s, id)
			out, err := resumeSecrets(tenantCtx("t1"), s, id, c.retry)
			if err != nil || out.Status != http.StatusAccepted {
				t.Fatalf("retry: %v", err)
			}
			secs2, refs2 := storedSecrets(t, s, id)
			if !reflect.DeepEqual(secs2, secs) || !reflect.DeepEqual(refs2, refs) {
				t.Errorf("the retry changed the secrets: %v %v", secretNames(secs2), refNames(refs2))
			}
			var events int
			systemScan(t, s, `SELECT count(*) FROM run_events WHERE run_id = $1 AND type = 'resume.requested'`, []any{id}, &events)
			if events != 1 {
				t.Errorf("%d resume.requested, want 1", events)
			}
			if vals, _ := s.secrets.get(id); vals["EXTRA"] != "extra-1" || vals["MORE"] != "" {
				t.Errorf("held values %v, want the first resume's", vals)
			}
		})
	}
}

// An operator's resume without values uses those this luxd still holds:
// it works with nothing new declared, and may remove secrets. Without held
// values it is refused, unless no secret would be left needing one.
// Declaring needs values, which the cache cannot give.
func TestOperatorResumeSecrets(t *testing.T) {
	op := context.WithValue(context.Background(), principalKey, Principal{Operator: true, TenantID: "t1", Scopes: []string{"admin", "operator"}})
	stopped := func(t *testing.T, s *Server, id string) {
		execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	}
	t.Run("from held values", func(t *testing.T) {
		s := testServer(t)
		id := stoppedWithSecrets(t, s)
		if _, err := resumeSecrets(op, s, id, resumeRequest{}); err != nil {
			t.Fatal(err)
		}
		if _, refs := storedSecrets(t, s, id); len(refs) != 4 {
			t.Errorf("runs.secrets %v", refNames(refs))
		}
	})
	t.Run("removing, from held values", func(t *testing.T) {
		s := testServer(t)
		id := stoppedWithSecrets(t, s)
		if _, err := resumeSecrets(op, s, id, resumeRequest{RemoveSecrets: []string{"TOKEN"}}); err != nil {
			t.Fatal(err)
		}
		secs, refs := storedSecrets(t, s, id)
		if want := []string{"GIT_TOKEN", "HDR", "REG"}; !reflect.DeepEqual(secretNames(secs), want) || !reflect.DeepEqual(refNames(refs), want) {
			t.Errorf("secrets %v / %v, want %v", secretNames(secs), refNames(refs), want)
		}
		if vals, _ := s.secrets.get(id); len(vals) != 3 || vals["TOKEN"] != "" {
			t.Errorf("held values %v", vals)
		}
	})
	t.Run("without held values", func(t *testing.T) {
		s := testServer(t)
		id := stoppedWithSecrets(t, s)
		s.secrets.drop(id)
		_, err := resumeSecrets(op, s, id, resumeRequest{RemoveSecrets: []string{"TOKEN"}})
		if he := refused(t, err, http.StatusUnprocessableEntity, "secrets_required", "operator resume"); !strings.Contains(he.Message, "no longer holds") {
			t.Errorf("message %q", he.Message)
		}
		if _, refs := storedSecrets(t, s, id); len(refs) != 4 {
			t.Errorf("a refused resume changed runs.secrets: %v", refNames(refs))
		}
	})
	t.Run("declaring needs every value", func(t *testing.T) {
		s := testServer(t)
		id := stoppedWithSecrets(t, s)
		_, err := resumeSecrets(op, s, id, resumeRequest{Secrets: []spec.Secret{{Name: "EXTRA", Value: "extra-1"}}})
		he := refused(t, err, http.StatusUnprocessableEntity, "secrets_required", "operator resume with only a new secret")
		if !reflect.DeepEqual(he.Details, []string{"GIT_TOKEN", "HDR", "REG", "TOKEN"}) {
			t.Errorf("missing %v", he.Details)
		}
		if _, refs := storedSecrets(t, s, id); len(refs) != 4 {
			t.Errorf("a refused resume declared: %v", refNames(refs))
		}
		stopped(t, s, id)
		if _, err := resumeSecrets(op, s, id, resumeRequest{Secrets: with(baseValues(), spec.Secret{Name: "EXTRA", Value: "extra-1"})}); err != nil {
			t.Fatal(err)
		}
		if _, refs := storedSecrets(t, s, id); !slices.Contains(refNames(refs), "EXTRA") {
			t.Errorf("runs.secrets %v", refNames(refs))
		}
	})
}

// removeSecrets through the HTTP API: decoded, and a refusal is the
// 422 invalid_spec body with the problem in its details.
func TestResumeSecretsHTTP(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	key := apiKey(t, s, new("t1"), "run", "read")
	vals := []map[string]string{}
	for _, sec := range baseValues() {
		vals = append(vals, map[string]string{"name": sec.Name, "value": sec.Value})
	}
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{"secrets": vals[:1], "removeSecrets": []string{"GIT_TOKEN"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `\"GIT_TOKEN\" is a git credential`) {
		t.Fatalf("remove a git credential: %d %s", w.Code, w.Body)
	}
	vals = append(vals, map[string]string{"name": "EXTRA", "value": "extra-1"})
	w = apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{"secrets": vals[1:], "removeSecrets": []string{"TOKEN"}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	g := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+id, nil)
	var run struct {
		Secrets []spec.SecretRef `json:"secrets"`
	}
	if err := json.Unmarshal(g.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if got := refNames(run.Secrets); !reflect.DeepEqual(got, []string{"EXTRA", "GIT_TOKEN", "HDR", "REG"}) {
		t.Errorf("GET secrets %v", got)
	}
}

// A resume that adds a repository with a new credential, declares a secret
// and removes one, but lacks a required value, is a 422 secrets_required
// naming only that value; the spec, secrets and state stay as they were and
// no resume.requested is recorded.
func TestResumeMissingValueRollsBackSecretChanges(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	key := apiKey(t, s, new("t1"), "run", "read")
	type runView struct {
		State   string           `json:"state"`
		Spec    json.RawMessage  `json:"spec"`
		Secrets []spec.SecretRef `json:"secrets"`
	}
	view := func() runView {
		t.Helper()
		var r runView
		if code := getJSON(t, s, key, "/v1/runs/"+id, &r); code != http.StatusOK {
			t.Fatalf("GET %d", code)
		}
		return r
	}
	before := view()

	vals := []map[string]string{
		{"name": "GIT_TOKEN", "value": "git-2"}, {"name": "HDR", "value": "hdr-2"},
		{"name": "NEW_GIT", "value": "new-git-1"}, {"name": "EXTRA", "value": "extra-1"},
	}
	w := apiCall(t, s, key, http.MethodPost, "/v1/runs/"+id+"/resume", map[string]any{
		"secrets":       vals,
		"removeSecrets": []string{"TOKEN"},
		"git":           map[string]any{"repositories": []map[string]any{{"name": "two", "url": "https://git.example.com/two.git", "credential": "NEW_GIT"}}},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("resume: %d %s, want 422", w.Code, w.Body)
	}
	var problem struct {
		Error struct {
			Code    string   `json:"code"`
			Details []string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Error.Code != "secrets_required" || !reflect.DeepEqual(problem.Error.Details, []string{"REG"}) {
		t.Fatalf("refusal %s, want secrets_required naming only REG", w.Body)
	}

	after := view()
	if after.State != StateStopped || before.State != StateStopped {
		t.Errorf("state %s -> %s, want stopped", before.State, after.State)
	}
	if string(after.Spec) != string(before.Spec) {
		t.Errorf("spec changed:\n%s\n%s", before.Spec, after.Spec)
	}
	if !reflect.DeepEqual(after.Secrets, before.Secrets) {
		t.Errorf("secrets %+v, want %+v", after.Secrets, before.Secrets)
	}
	var events struct {
		Events []Event `json:"events"`
	}
	if code := getJSON(t, s, key, "/v1/runs/"+id+"/events", &events); code != http.StatusOK {
		t.Fatalf("GET events %d", code)
	}
	if len(events.Events) == 0 {
		t.Fatal("GET events listed none, not even the submit's")
	}
	for _, ev := range events.Events {
		if ev.Type == "resume.requested" {
			t.Errorf("resume.requested recorded: %v", ev.Data)
		}
	}
}

// removeSecrets takes a secret out of the stored spec and runs.secrets:
// its value is not held for the placement, and the next resume does not
// need it.
func TestResumeRemovesSecrets(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	rest := slices.DeleteFunc(baseValues(), func(sec spec.Secret) bool { return sec.Name == "TOKEN" })
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: rest, RemoveSecrets: []string{"TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	secs, refs := storedSecrets(t, s, id)
	want := []string{"GIT_TOKEN", "HDR", "REG"}
	if got := secretNames(secs); !reflect.DeepEqual(got, want) {
		t.Errorf("spec.secrets %v, want %v", got, want)
	}
	if got := refNames(refs); !reflect.DeepEqual(got, want) {
		t.Errorf("runs.secrets %v, want %v", got, want)
	}
	if vals, _ := s.secrets.get(id); len(vals) != 3 || vals["TOKEN"] != "" {
		t.Errorf("held values %v", vals)
	}
	if ev := resumeEvent(t, s, id); !reflect.DeepEqual(ev["removedSecrets"], []any{"TOKEN"}) {
		t.Errorf("resume.requested removedSecrets %v", ev["removedSecrets"])
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: rest}); err != nil {
		t.Fatalf("resume without the removed secret: %v", err)
	}

	// A value supplied with a removal of another name (a rotation) or a
	// stale value for a removed name the client still sends are both fine:
	// the latter declares it again.
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(rest, spec.Secret{Name: "TOKEN", Value: "token-3"})}); err != nil {
		t.Fatal(err)
	}
	if _, refs := storedSecrets(t, s, id); !slices.Contains(refNames(refs), "TOKEN") {
		t.Errorf("TOKEN not declared again: %v", refNames(refs))
	}
}

// Every removal that cannot be is a 422 invalid_spec naming it, and the
// Run is as it was: its spec (repositories too), runs.secrets, its state.
func TestResumeRefusesRemovals(t *testing.T) {
	without := func(name string) []spec.Secret {
		return slices.DeleteFunc(baseValues(), func(sec spec.Secret) bool { return sec.Name == name })
	}
	addTwo := &resumeGit{Repositories: []spec.Repository{{Name: "two", URL: "https://git.example.com/two.git", Credential: "NEW_GIT"}}}
	for _, c := range []struct {
		name    string
		req     resumeRequest
		problem string
	}{
		{"unknown", resumeRequest{Secrets: baseValues(), RemoveSecrets: []string{"NOPE"}}, `"NOPE": the Run has no such secret`},
		{"git credential", resumeRequest{Secrets: without("GIT_TOKEN"), RemoveSecrets: []string{"GIT_TOKEN"}}, `"GIT_TOKEN" is a git credential`},
		{"registry credential", resumeRequest{Secrets: without("REG"), RemoveSecrets: []string{"REG"}}, `"REG" is a registry credential`},
		{"header secret", resumeRequest{Secrets: without("HDR"), RemoveSecrets: []string{"HDR"}}, `"HDR" values an MCP server's or service's header`},
		{"also supplied", resumeRequest{Secrets: baseValues(), RemoveSecrets: []string{"TOKEN"}}, `"TOKEN" is also in secrets`},
		{"also declared", resumeRequest{Secrets: with(baseValues(), spec.Secret{Name: "NEW", Value: "new-12345"}), RemoveSecrets: []string{"NEW"}}, `"NEW" is also in secrets`},
		{"an added repository's credential", resumeRequest{Secrets: baseValues(), Git: addTwo, RemoveSecrets: []string{"NEW_GIT"}},
			`"NEW_GIT" is the credential of a repository this resume adds`},
		{"twice", resumeRequest{Secrets: without("TOKEN"), RemoveSecrets: []string{"TOKEN", "TOKEN"}}, `removeSecrets: duplicate "TOKEN"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			id := stoppedWithSecrets(t, s)
			var before []byte
			systemScan(t, s, `SELECT spec::text FROM runs WHERE id = $1`, []any{id}, &before)
			_, beforeRefs := storedSecrets(t, s, id)
			_, err := resumeSecrets(tenantCtx("t1"), s, id, c.req)
			he := refused(t, err, http.StatusUnprocessableEntity, "invalid_spec", "resume")
			if !strings.Contains(strings.Join(he.Details, "\n"), c.problem) {
				t.Errorf("details %q, want %q", he.Details, c.problem)
			}
			var after []byte
			systemScan(t, s, `SELECT spec::text FROM runs WHERE id = $1`, []any{id}, &after)
			_, refs := storedSecrets(t, s, id)
			if string(after) != string(before) || !reflect.DeepEqual(refs, beforeRefs) || storedState(t, s, id) != StateStopped {
				t.Errorf("a refused resume changed the Run:\n%s\n%s\n%v", before, after, refs)
			}
		})
	}
}

// A resume declares an as: none secret, whose name need not be an env
// name; a later resume removes it and no longer needs it.
func TestResumeDeclaresAndRemovesNoneSecret(t *testing.T) {
	s := testServer(t)
	id := stoppedWithSecrets(t, s)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: with(baseValues(), spec.Secret{Name: "a.b", Value: "ab-1", As: "none"})}); err != nil {
		t.Fatal(err)
	}
	secs, refs := storedSecrets(t, s, id)
	i := slices.IndexFunc(secs, func(sec spec.Secret) bool { return sec.Name == "a.b" })
	if i < 0 || secs[i].As != "none" || secs[i].Value != "" {
		t.Fatalf("a.b stored as %+v", secs)
	}
	if !slices.Contains(refNames(refs), "a.b") {
		t.Fatalf("runs.secrets %v", refNames(refs))
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: baseValues(), RemoveSecrets: []string{"a.b"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"GIT_TOKEN", "HDR", "REG", "TOKEN"}
	if secs, refs := storedSecrets(t, s, id); !reflect.DeepEqual(secretNames(secs), want) || !reflect.DeepEqual(refNames(refs), want) {
		t.Fatalf("secrets %v / %v, want %v", secretNames(secs), refNames(refs), want)
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{Secrets: baseValues()}); err != nil {
		t.Fatalf("resume without a.b: %v", err)
	}
}

// Removing a Run's only secret leaves it with none: that resume needs no
// values, nor does the next.
func TestResumeRemovesLastSecret(t *testing.T) {
	s := testServer(t)
	sp := secretsSpec()
	sp.Image.RegistryAuth = nil
	sp.Workload.Services = nil
	sp.Git = nil
	sp.Secrets = []spec.Secret{{Name: "TOKEN", Value: "token-1"}}
	id := stoppedWith(t, s, sp)
	if _, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{RemoveSecrets: []string{"TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	if secs, refs := storedSecrets(t, s, id); len(secs) != 0 || len(refs) != 0 {
		t.Fatalf("secrets %v / %v, want none", secretNames(secs), refNames(refs))
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	out, err := resumeSecrets(tenantCtx("t1"), s, id, resumeRequest{})
	if err != nil {
		t.Fatalf("resume of a Run without secrets: %v", err)
	}
	if out.Body.State != StateResuming {
		t.Fatalf("state %s", out.Body.State)
	}
}
