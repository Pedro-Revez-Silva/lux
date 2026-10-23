package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// assignManagedThenDowngrade makes r1 stopped with spec sp, assigns its
// resume (epoch 2) to h1 while h1 has managed images, then re-registers
// h1 with caps (none: an older runner) and no live placements.
func assignManagedThenDowngrade(t *testing.T, s *Server, ctx context.Context, hello *proto.Hello, register func([]string), sp spec.RunSpec, caps []string) {
	t.Helper()
	if err := sp.Normalize(spec.BuiltinDefaults); err != nil {
		t.Fatal(err)
	}
	execSQL(t, s, ctx, `UPDATE runs SET state = 'stopped', spec = $1 WHERE id = $2`, sp, r1)
	execSQL(t, s, ctx, `UPDATE placements SET state = 'exited'`)
	execSQL(t, s, ctx, `UPDATE host_messages SET acked_at = now()`)
	register([]string{proto.CapManagedImages, proto.CapNonPreemptible})
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		return s.assign(ctx, tx, pendingRun{ID: r1, TenantID: "t1", Epoch: 1, Spec: sp}, &candidateHost{ID: "h1"})
	}); err != nil {
		t.Fatal(err)
	}
	hello.Live = nil
	register(caps)
}

func managedDeliveryServer(t *testing.T) (*Server, context.Context, *proto.Hello, func([]string)) {
	t.Helper()
	s, ctx, _, hello, register := syncModesFixture(t)
	s.cfg.Images = ImagesConfig{Repository: "registry.test/managed", CredentialEnv: "LUX_TEST_MANAGED_CREDENTIAL"}
	t.Setenv("LUX_TEST_MANAGED_CREDENTIAL", "AWS:synthetic")
	return s, ctx, hello, register
}

// assigns are the assignments both delivery paths (the connection's, and
// the poll's) would send h1.
func assigns(t *testing.T, s *Server, ctx context.Context) []proto.Assign {
	t.Helper()
	var out []proto.Assign
	for _, undelivered := range []bool{true, false} {
		frames, err := s.pendingMessages(ctx, "h1", undelivered)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			if f.Type == proto.MsgAssign {
				var a proto.Assign
				if err := json.Unmarshal(f.Data, &a); err != nil {
					t.Fatal(err)
				}
				out = append(out, a)
			}
		}
	}
	return out
}

var publishing = spec.RunSpec{Image: spec.Image{Build: &spec.Build{Containerfile: "FROM scratch\nCOPY saved.txt /saved\n", Publish: true,
	ContextFiles: map[string]string{"saved.txt": "required context"}}}, Workload: spec.Workload{Command: []string{"true"}}}

// An assignment that needs managed images, replayed to a host whose
// runner re-registered without them, is not delivered (by connection or
// poll), nor its credential: its unstarted placement is given up and the
// Run queued again, once, so that a runner with them takes it.
func TestManagedAssignmentRefusedAfterDowngrade(t *testing.T) {
	for name, sp := range map[string]spec.RunSpec{
		"publish with a context": publishing,
		"published image as ref": {Image: spec.Image{Ref: "registry.test/managed/t1@sha256:" + strings.Repeat("a", 64)}, Workload: spec.Workload{Command: []string{"true"}}},
		"shared base as FROM":    {Image: spec.Image{Build: &spec.Build{Containerfile: "FROM registry.test/base@sha256:" + strings.Repeat("b", 64) + "\n"}}, Workload: spec.Workload{Command: []string{"true"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s, ctx, hello, register := managedDeliveryServer(t)
			s.cfg.Images.Shared = []string{"registry.test/base"}
			assignManagedThenDowngrade(t, s, ctx, hello, register, sp, nil)
			for range 2 {
				if got := assigns(t, s, ctx); len(got) != 0 {
					t.Fatalf("delivered to the older runner: %+v", got[0].ManagedImages)
				}
			}
			var state, placement, exitReason string
			var lost int
			systemScan(t, s, `SELECT r.state, p.state, coalesce(p.exit_reason, ''),
					(SELECT count(*) FROM run_events e WHERE e.run_id = r.id AND e.type = 'state' AND e.data->>'state' = 'lost')
				FROM runs r JOIN placements p ON p.run_id = r.id AND p.epoch = 2 WHERE r.id = $1`, []any{r1}, &state, &placement, &exitReason, &lost)
			if state != StateResuming || placement != "lost" || exitReason != managedImagesGone || lost != 1 {
				t.Fatalf("run %s, placement %s (%q), lost %d times", state, placement, exitReason, lost)
			}
		})
	}
}

// A runner with managed images still gets it, credential and all; a Run
// needing nothing new is delivered to an older runner as before, and so
// is a managed-looking one on a luxd without [images].
func TestManagedAssignmentDeliveredWhereSupported(t *testing.T) {
	s, ctx, hello, register := managedDeliveryServer(t)
	assignManagedThenDowngrade(t, s, ctx, hello, register, publishing, []string{proto.CapManagedImages})
	got := assigns(t, s, ctx)
	if len(got) == 0 || got[0].ManagedImages == nil || got[0].ManagedImages.Credential != "AWS:synthetic" || !got[0].Spec.Image.Build.Publish {
		t.Fatalf("capable runner: %+v", got)
	}

	s, ctx, hello, register = managedDeliveryServer(t)
	plain := spec.RunSpec{Image: spec.Image{Ref: "alpine"}, Workload: spec.Workload{Command: []string{"true"}}}
	assignManagedThenDowngrade(t, s, ctx, hello, register, plain, nil)
	if got := assigns(t, s, ctx); len(got) == 0 || got[0].ManagedImages != nil {
		t.Fatalf("a plain Run on an older runner: %+v", got)
	}

	s, ctx, hello, register = managedDeliveryServer(t)
	ref := spec.RunSpec{Image: spec.Image{Ref: "registry.test/managed/t1@sha256:" + strings.Repeat("a", 64)}, Workload: spec.Workload{Command: []string{"true"}}}
	assignManagedThenDowngrade(t, s, ctx, hello, register, ref, nil)
	s.cfg.Images = ImagesConfig{}
	if got := assigns(t, s, ctx); len(got) == 0 || got[0].ManagedImages != nil {
		t.Fatalf("no managed registry: %+v", got)
	}
}

// An older epoch's undeliverable assignment is only acked: it never ends
// the Run's current placement.
func TestStaleManagedAssignmentLeavesTheCurrentPlacement(t *testing.T) {
	s, ctx, hello, register := managedDeliveryServer(t)
	assignManagedThenDowngrade(t, s, ctx, hello, register, publishing, nil)
	execSQL(t, s, ctx, `INSERT INTO placements (id, tenant_id, run_id, host_id, epoch, state, resources, lease_expires_at)
		SELECT 'plc3', tenant_id, run_id, host_id, 3, 'assigned', resources, lease_expires_at FROM placements WHERE run_id = $1 AND epoch = 2`, r1)
	execSQL(t, s, ctx, `UPDATE runs SET current_epoch = 3 WHERE id = $1`, r1)
	if got := assigns(t, s, ctx); len(got) != 0 {
		t.Fatalf("the stale assignment was delivered: %+v", got)
	}
	var state, p2, p3 string
	var acked bool
	systemScan(t, s, `SELECT r.state, p2.state, p3.state,
			(SELECT acked_at IS NOT NULL FROM host_messages WHERE run_id = r.id AND epoch = 2 AND type = 'assign')
		FROM runs r JOIN placements p2 ON p2.run_id = r.id AND p2.epoch = 2 JOIN placements p3 ON p3.run_id = r.id AND p3.epoch = 3
		WHERE r.id = $1`, []any{r1}, &state, &p2, &p3, &acked)
	if p3 != "assigned" || p2 != "assigned" || !acked || state == StateResuming {
		t.Fatalf("run %s, epoch 2 %s, epoch 3 %s, acked %v", state, p2, p3, acked)
	}
}
