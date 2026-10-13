package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// listedPool is one pool of GET /v1/pools as JSON, by name.
func listedPool(t *testing.T, s *Server, key, name string) map[string]any {
	t.Helper()
	w := apiCall(t, s, key, http.MethodGet, "/v1/pools", nil)
	var body struct {
		Pools []map[string]any `json:"pools"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("GET /v1/pools: %d %s", w.Code, w.Body)
	}
	for _, p := range body.Pools {
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("no pool %s in %s", name, w.Body)
	return nil
}

// GET /v1/pools says each pool's platforms and whether it offers nested
// containers, from the facts a resume into it is checked against: its
// hosts' arch and nested registration, an ec2 template's instance types in
// the catalog and its nestedContainers. Unknown: platforms absent,
// nestedContainers null.
func TestPoolsListPlatformsAndCapabilities(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_, _ = movedRun(t, s)
	addCatalog(t, s)
	execSQL(t, s, ctx, `UPDATE hosts SET arch = 'arm64', labels = '{"nested":"true"}' WHERE id = 'hb'`)
	poolID(t, s, "empty", "t1")
	ec2Pool(t, s, "graviton", `{"region":"us-east-1","instanceType":"m7g.4xlarge"}`)
	ec2Pool(t, s, "nest", `{"region":"us-east-1","nestedContainers":true}`)
	ec2Pool(t, s, "odd", `{"region":"us-east-1","instanceType":"x9.huge","fallbackInstanceTypes":["m7i.4xlarge"]}`)
	key := apiKey(t, s, new("t1"), "read")
	for _, c := range []struct {
		pool      string
		platforms any
		nested    any
	}{
		{"big", []any{"linux/arm64"}, true},       // its host: arm64, --nested
		{"default", []any{"linux/amd64"}, false},  // hosts ha (no arch: unknown) and hc (amd64), neither nested
		{"empty", nil, nil},                       // static, no host: nothing known
		{"graviton", []any{"linux/arm64"}, false}, // the catalog's m7g; template without nestedContainers
		{"nest", nil, true},                       // no instanceType: platform unknown; the template opts in
		{"odd", []any{"linux/amd64"}, false},      // the catalog knows one of its two types
	} {
		p := listedPool(t, s, key, c.pool)
		caps, _ := p["capabilities"].(map[string]any)
		if got := p["platforms"]; jsonText(got) != jsonText(c.platforms) {
			t.Errorf("%s: platforms %v, want %v", c.pool, got, c.platforms)
		}
		if got, present := caps["nestedContainers"]; !present || got != c.nested {
			t.Errorf("%s: capabilities %v, want nestedContainers %v", c.pool, caps, c.nested)
		}
	}
}

// A platform pool's facts, for a tenant, leave out another tenant's hosts
// registered into it: their arch and nesting are not the tenant's to use.
func TestPoolsListIgnoresAnotherTenantsHosts(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_, _ = movedRun(t, s)
	execSQL(t, s, ctx, `INSERT INTO tenants (id, name) VALUES ('t2', 't2')`)
	readyHost(t, s, "h1", "shared", "", false)
	readyHost(t, s, "h2", "shared", "", false)
	execSQL(t, s, ctx, `UPDATE hosts SET arch = 'amd64', registered_at = now() WHERE id = 'h1'`)
	execSQL(t, s, ctx, `UPDATE hosts SET tenant_id = 't2', arch = 'arm64', labels = '{"nested":"true"}', registered_at = now() WHERE id = 'h2'`)
	p := listedPool(t, s, apiKey(t, s, new("t1"), "read"), "shared")
	caps, _ := p["capabilities"].(map[string]any)
	if jsonText(p["platforms"]) != `["linux/amd64"]` || caps["nestedContainers"] != false {
		t.Errorf("t1 sees shared as %v %v, want [linux/amd64] and nestedContainers false", p["platforms"], caps)
	}
}

// An ec2 pool's host of an older template counts while it is live (it
// can take a Run), never as the host the pool launches next.
func TestPoolsListOlderTemplateHost(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	_, _ = movedRun(t, s)
	pool := ec2Pool(t, s, "burst", `{"region":"us-east-1"}`)
	ec2Host(t, s, "old", pool, "ready", time.Minute, `{"cpus":16}`, `{"nested":"true"}`)
	execSQL(t, s, ctx, `UPDATE hosts SET launch_template = '{"region":"us-east-1","version":0}' WHERE id = 'old'`)
	key := apiKey(t, s, new("t1"), "read")
	nested := func() any {
		caps, _ := listedPool(t, s, key, "burst")["capabilities"].(map[string]any)
		return caps["nestedContainers"]
	}
	if got := nested(); got != true {
		t.Errorf("with the older template's nested host ready: nestedContainers %v, want true", got)
	}
	execSQL(t, s, ctx, `UPDATE hosts SET state = 'terminated' WHERE id = 'old'`)
	if got := nested(); got != false {
		t.Errorf("once it has gone: nestedContainers %v, want false", got)
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
