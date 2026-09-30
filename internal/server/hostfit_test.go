package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

func fitFixture() (pendingRun, *candidateHost) {
	r := pendingRun{TenantID: "t1", PoolID: new("pool")}
	r.Spec.Resources = spec.Resources{CPUs: 2.5, Memory: 8192, Disk: 4096}
	r.Spec.Placement.Pool = "pool"
	h := &candidateHost{ID: "h", PoolID: "pool", Connected: true,
		Capacity: proto.Capacity{CPUs: 4, Memory: 16384, Disk: 8192, Runs: 2}}
	return r, h
}

func TestHostFitConstraints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pendingRun, *candidateHost)
		want   []fitBlocker
	}{
		{"fits", func(r *pendingRun, h *candidateHost) {}, nil},
		{"disconnected", func(r *pendingRun, h *candidateHost) { h.Connected = false }, []fitBlocker{{Reason: "host is not connected"}}},
		{"pool identity", func(r *pendingRun, h *candidateHost) { h.PoolID = "other"; h.Pool = "pool" }, []fitBlocker{{Reason: "host is not in its pool pool"}}},
		{"unbound pool", func(r *pendingRun, h *candidateHost) { r.PoolID = nil }, []fitBlocker{{Reason: "host is not in its pool pool"}}},
		{"retired", func(r *pendingRun, h *candidateHost) { h.Retired = true }, []fitBlocker{{Reason: "its pool was removed"}}},
		{"foreign tenant", func(r *pendingRun, h *candidateHost) { h.TenantID = new("t2"); h.Shared = true }, []fitBlocker{{Reason: "host belongs to another tenant"}}},
		{"own tenant", func(r *pendingRun, h *candidateHost) { h.TenantID = new("t1") }, nil},
		{"exclusive occupied", func(r *pendingRun, h *candidateHost) { h.Tenants = []string{"t1", "t2"} }, []fitBlocker{{Reason: "non-shared host is used by another tenant"}}},
		{"exclusive same tenant", func(r *pendingRun, h *candidateHost) { h.Tenants = []string{"t1"} }, nil},
		{"shared", func(r *pendingRun, h *candidateHost) { h.Shared = true; h.Tenants = []string{"t2"} }, nil},
		{"labels deterministic", func(r *pendingRun, h *candidateHost) {
			r.Spec.Placement.Requires = map[string]string{"z": "v", "a": "v"}
		}, []fitBlocker{{Reason: `requires label a=v (host has "")`}, {Reason: `requires label z=v (host has "")`}}},
		{"labels match", func(r *pendingRun, h *candidateHost) {
			r.Spec.Placement.Requires = map[string]string{"arch": "arm64"}
			h.Labels = map[string]string{"arch": "arm64"}
		}, nil},
		{"nested", func(r *pendingRun, h *candidateHost) { r.Spec.Sandbox.NestedContainers = true }, []fitBlocker{{Reason: "host does not support nested containers"}}},
		{"nested supported", func(r *pendingRun, h *candidateHost) {
			r.Spec.Sandbox.NestedContainers = true
			h.Labels = map[string]string{"nested": "true"}
		}, nil},
		{"chosen mismatch", func(r *pendingRun, h *candidateHost) { r.PlaceOn = "other" }, []fitBlocker{{Reason: "waiting for its chosen host other"}}},
		{"chosen overrides placement", func(r *pendingRun, h *candidateHost) {
			r.PlaceOn = h.ID
			r.PoolID = nil
			h.Retired = true
			r.Spec.Placement.Requires = map[string]string{"arch": "missing"}
		}, nil},
		{"chosen preserves isolation", func(r *pendingRun, h *candidateHost) { r.PlaceOn = h.ID; h.Tenants = []string{"t2"} }, []fitBlocker{{Reason: "non-shared host is used by another tenant"}}},
		{"chosen preserves nested", func(r *pendingRun, h *candidateHost) { r.PlaceOn = h.ID; r.Spec.Sandbox.NestedContainers = true }, []fitBlocker{{Reason: "host does not support nested containers"}}},
		{"unlimited", func(r *pendingRun, h *candidateHost) {
			h.Capacity = proto.Capacity{}
			h.UsedCPUs = 100
			h.UsedMem = 100000
			h.UsedDisk = 100000
			h.UsedRuns = 100
		}, nil},
		{"exact boundary", func(r *pendingRun, h *candidateHost) {
			h.UsedCPUs = 1.5
			h.UsedMem = 8192
			h.UsedDisk = 4096
			h.UsedRuns = 1
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, h := fitFixture()
			tc.change(&r, h)
			before, _ := json.Marshal(h)
			if got := hostFit(r, h); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("blockers = %+v, want %+v", got, tc.want)
			}
			after, _ := json.Marshal(h)
			if string(before) != string(after) {
				t.Fatal("hostFit mutated candidate")
			}
		})
	}
}

func TestHostFitResourceIdentityAndReservation(t *testing.T) {
	r, h := fitFixture()
	h.UsedCPUs, h.UsedMem, h.UsedDisk, h.UsedRuns = 2, 10000, 5000, 2
	want := []fitBlocker{
		{Resource: "cpus", Requested: 2.5, Used: 2, Capacity: 4, Available: 2},
		{Resource: "memory", Requested: 8192, Used: 10000, Capacity: 16384, Available: 6384},
		{Resource: "disk", Requested: 4096, Used: 5000, Capacity: 8192, Available: 3192},
		{Resource: "runs", Requested: 1, Used: 2, Capacity: 2, Available: 0},
	}
	if got := hostFit(r, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("blockers = %+v, want %+v", got, want)
	}
	for _, b := range want {
		encoded, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var decoded fitBlocker
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != b {
			t.Fatalf("JSON round trip = %+v, want %+v", decoded, b)
		}
	}
	wantReason := "waiting for host h: cpus requested 2.5, used 2, capacity 4, available 2; memory requested 8192 bytes, used 10000 bytes, capacity 16384 bytes, available 6384 bytes; disk requested 4096 bytes, used 5000 bytes, capacity 8192 bytes, available 3192 bytes; runs requested 1, used 2, capacity 2, available 0"
	if got := hostFitReason(h, want); got != wantReason {
		t.Fatalf("reason = %q, want %q", got, wantReason)
	}
	r, h = fitFixture()
	reserveHost(h, r)
	if h.UsedCPUs != 2.5 || h.UsedMem != 8192 || h.UsedDisk != 4096 || h.UsedRuns != 1 || !reflect.DeepEqual(h.Tenants, []string{"t1"}) {
		t.Fatalf("reservation = %+v", h)
	}
	if got := hostFit(r, h); !reflect.DeepEqual(got, []fitBlocker{{Resource: "cpus", Requested: 2.5, Used: 2.5, Capacity: 4, Available: 1.5}}) {
		t.Fatalf("second fit = %+v", got)
	}
	r.TenantID = "t2"
	if got := hostFit(r, h); len(got) != 2 || got[0].Reason != "non-shared host is used by another tenant" {
		t.Fatalf("second tenant fit = %+v", got)
	}
}

func TestHostFitMemoryIntegerBoundary(t *testing.T) {
	r, h := fitFixture()
	h.Capacity.Memory = 1 << 54
	h.UsedMem = h.Capacity.Memory - 8191
	got := hostFit(r, h)
	if len(got) != 1 || got[0].Resource != "memory" {
		t.Fatalf("one byte over capacity: %+v", got)
	}
	h.UsedMem--
	if got := hostFit(r, h); len(got) != 0 {
		t.Fatalf("exact capacity: %+v", got)
	}
}
