package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/store"
)

// The memory limit a runner reports for a placement's container is kept
// with the placement and returned by GET /v1/runs/{id}: the first nonzero
// report wins, and a report without one neither sets nor clears it.
func TestPlacementMemoryLimit(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	key := serversFixture(t, s, ctx)
	placements := func() []Placement {
		t.Helper()
		w := apiCall(t, s, key, http.MethodGet, "/v1/runs/"+r1, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get run: %d %s", w.Code, w.Body)
		}
		var run struct {
			Placements []Placement `json:"placements"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		return run.Placements
	}
	if pl := placements(); len(pl) != 1 || pl[0].MemoryLimit != nil {
		t.Fatalf("before any report: %+v", pl)
	}
	report := func(st proto.Status) {
		t.Helper()
		if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
			return s.applyStatus(ctx, tx, "t1", r1, 1, st)
		}); err != nil {
			t.Fatal(err)
		}
	}
	report(proto.Status{State: "running"})
	if pl := placements(); len(pl) != 1 || pl[0].MemoryLimit != nil {
		t.Fatalf("a report without a limit must leave it absent: %+v", pl)
	}
	report(proto.Status{State: "running", MemoryLimit: 15 << 30})
	report(proto.Status{State: "running", MemoryLimit: 16 << 30})
	report(proto.Status{State: "running"})
	if pl := placements(); len(pl) != 1 || pl[0].MemoryLimit == nil || *pl[0].MemoryLimit != 15<<30 {
		t.Fatalf("after the reports: %+v, want the first nonzero, 15 GiB", pl)
	}
}
