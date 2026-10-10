package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// volumesProvider answers Volumes from vols (or fails with err), and
// records each call's ids.
type volumesProvider struct {
	fakeLaunchProvider
	vols  map[string][]HostVolume
	err   error
	calls [][]string
}

func (p *volumesProvider) Volumes(_ context.Context, _ json.RawMessage, ids []string) (map[string][]HostVolume, error) {
	p.calls = append(p.calls, slices.Clone(ids))
	if p.err != nil {
		return nil, p.err
	}
	out := map[string][]HostVolume{}
	for _, id := range ids {
		if v, ok := p.vols[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// One Volumes call per region for every live provider host whose volumes are
// unknown; the answer is stored and shown on GET /v1/hosts; a failed call
// leaves them unknown (NULL), never empty; a host whose volumes are known is
// not asked again.
func TestRecordVolumes(t *testing.T) {
	s, keys := costFixture(t)
	ctx := context.Background()
	execSQL(t, s, ctx, `INSERT INTO pools (id, tenant_id, name, provider, template) VALUES ('pec2', 't1', 'burst', 'ec2', '{"region":"eu-north-1"}')`)
	execSQL(t, s, ctx, `INSERT INTO hosts (id, tenant_id, pool_id, name, state, provider_id, provision_requested_at, launch_template) VALUES
		('h1', 't1', 'pec2', 'h1', 'ready', 'i-1', now(), '{"region":"eu-north-1"}'),
		('h2', 't1', 'pec2', 'h2', 'ready', 'i-2', now(), '{"region":"eu-north-1"}'),
		('h3', 't1', 'pec2', 'h3', 'ready', 'i-3', now(), '{"region":"us-east-1"}'),
		('gone', 't1', 'pec2', 'gone', 'terminated', 'i-4', now(), '{"region":"eu-north-1"}')`)
	gp3 := []HostVolume{{Type: "gp3", SizeGiB: 100, IOPS: 3000, ThroughputMiBps: 125}}
	p := &volumesProvider{err: errors.New("UnauthorizedOperation")}
	s.cfg.Providers = map[string]Provider{"ec2": p}

	s.recordVolumes(ctx)
	var unknown int
	systemScan(t, s, `SELECT count(*) FROM hosts WHERE volumes IS NULL`, nil, &unknown)
	if unknown != 4 || len(p.calls) != 2 {
		t.Fatalf("after a failure: %d unknown, calls %v", unknown, p.calls)
	}

	p.err, p.calls = nil, nil
	p.vols = map[string][]HostVolume{"i-1": gp3, "i-3": {}}
	s.recordVolumes(ctx)
	if fmt.Sprint(p.calls) != "[[i-1 i-2] [i-3]]" {
		t.Errorf("calls %v, want one per region", p.calls)
	}
	var got map[string]*string
	got = map[string]*string{}
	for _, id := range []string{"h1", "h2", "h3", "gone"} {
		var v *string
		systemScan(t, s, `SELECT volumes::text FROM hosts WHERE id = $1`, []any{id}, &v)
		got[id] = v
	}
	if got["h1"] == nil || *got["h1"] != `[{"iops": 3000, "type": "gp3", "sizeGiB": 100, "throughputMiBps": 125}]` ||
		got["h2"] != nil || got["h3"] == nil || *got["h3"] != "[]" || got["gone"] != nil {
		t.Errorf("stored %v", nullable(got))
	}

	var h Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h1", &h); code != http.StatusOK || h.Volumes == nil || fmt.Sprint(*h.Volumes) != "[{gp3 100 3000 125 false}]" {
		t.Errorf("GET host: %d %v", code, h.Volumes)
	}
	var h2 Host
	if code := getJSON(t, s, keys["t1"], "/v1/hosts/h2", &h2); code != http.StatusOK || h2.Volumes != nil {
		t.Errorf("GET unknown host volumes: %d %v", code, h2.Volumes)
	}

	p.calls = nil
	s.recordVolumes(ctx)
	if fmt.Sprint(p.calls) != "[[i-2]]" {
		t.Errorf("known hosts asked again: %v", p.calls)
	}
}

func nullable(m map[string]*string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v != nil {
			out[k] = *v
		} else {
			out[k] = "NULL"
		}
	}
	return out
}
