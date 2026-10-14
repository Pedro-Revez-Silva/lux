package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// placement is set by POST /v1/pools, read back by GET, recorded in the
// pool's config_changed event, and cleared by a set without it; a value
// other than spread or pack is 422 invalid_pool and stores nothing.
func TestPoolPlacementAPI(t *testing.T) {
	s := testServer(t)
	execSQL(t, s, context.Background(), `INSERT INTO tenants (id, name) VALUES ('t1', 't1')`)
	key := apiKey(t, s, new("t1"), "admin", "read")
	post := func(placement any) (int, string, string) {
		body := map[string]any{"name": "p", "provider": "static"}
		if placement != nil {
			body["placement"] = placement
		}
		w := apiCall(t, s, key, http.MethodPost, "/v1/pools", body)
		if w.Code == http.StatusOK {
			return w.Code, "", ""
		}
		code, msg := apiError(t, w.Body.Bytes())
		return w.Code, code, msg
	}
	listed := func() string {
		t.Helper()
		var list struct {
			Pools []Pool `json:"pools"`
		}
		w := apiCall(t, s, key, http.MethodGet, "/v1/pools", nil)
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Pools) != 1 {
			t.Fatalf("pools %s: %v", w.Body, err)
		}
		return list.Pools[0].Placement
	}
	stored := func() string {
		return queryOne[string](t, s, `SELECT coalesce(placement, '<null>') FROM pools WHERE name = 'p'`)
	}
	for _, c := range []struct {
		in         any
		status     int
		code, text string
	}{
		{"random", http.StatusUnprocessableEntity, "invalid_pool", `placement "random": want spread or pack`},
		{"Pack", http.StatusUnprocessableEntity, "invalid_pool", `placement "Pack": want spread or pack`},
		{1, http.StatusBadRequest, "bad_request", ""},
	} {
		if status, code, msg := post(c.in); status != c.status || code != c.code || c.text != "" && msg != c.text {
			t.Errorf("placement %v: %d %s %q, want %d %s %q", c.in, status, code, msg, c.status, c.code, c.text)
		}
		if n := queryOne[int](t, s, `SELECT count(*) FROM pools`); n != 0 {
			t.Fatalf("placement %v stored a pool", c.in)
		}
	}
	for _, c := range []struct {
		in     any
		stored string
		listed string
	}{
		{"pack", "pack", "pack"},
		{"spread", "spread", "spread"},
		{nil, "<null>", ""},
	} {
		if code, errCode, msg := post(c.in); code != http.StatusOK {
			t.Fatalf("placement %v: %d %s %s", c.in, code, errCode, msg)
		}
		if got := stored(); got != c.stored {
			t.Errorf("placement %v: stored %s, want %s", c.in, got, c.stored)
		}
		if got := listed(); got != c.listed {
			t.Errorf("placement %v: listed %q, want %q", c.in, got, c.listed)
		}
	}
	changes := queryOne[[]string](t, s, `SELECT array_agg(coalesce(data->'changes'->>'placement', '-') ORDER BY id) FROM pool_events
		WHERE type = $1`, evConfigChanged)
	want := []string{`{"new": "pack", "old": null}`, `{"new": "spread", "old": "pack"}`, `{"new": null, "old": "spread"}`}
	if fmt.Sprint(changes) != fmt.Sprint(want) {
		t.Errorf("config_changed placement changes %v, want %v", changes, want)
	}
}
