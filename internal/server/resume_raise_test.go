package server

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/marcioapm/lux/internal/spec"
)

// raiseCase is a stopped Run in pool "shaped" (testShape) of size from,
// its saved state measured at peak, resumed asking for res.
type raiseCase struct {
	name       string
	from       string
	peak       int64
	res        resumeResources
	size       string // the size it gets
	raisedFrom string // "": not raised
}

// raiseCheck resumes id with req, validateOnly first, and checks the dry
// run changes nothing and answers the resize the resume applies, the Run
// gets size with raisedFrom (none if ""), and GET and the event say so.
func raiseCheck(t *testing.T, s *Server, id string, req resumeRequest, size, raisedFrom string, peak int64) {
	t.Helper()
	const raise = `SELECT coalesce(size_raised_from, '') || '/' || coalesce(size_raised_for, '') || '/' || coalesce(size_raised_to, '') FROM runs WHERE id = $1`
	before := storedResources(t, s, id)
	_, beforeSize := runSizing(t, s, id)
	beforeRaise := queryOne[string](t, s, raise, id)
	req.ValidateOnly = true
	dry, err := resumeReq(s, id, req)
	if err != nil {
		t.Fatalf("validateOnly: %v", err)
	}
	if got, state := storedResources(t, s, id), storedState(t, s, id); got != before || state != StateStopped {
		t.Fatalf("validateOnly changed the Run: %s %+v", state, got)
	}
	if _, sz := runSizing(t, s, id); sz != beforeSize || queryOne[string](t, s, raise, id) != beforeRaise {
		t.Fatalf("validateOnly stored size %q or a raise", sz)
	}
	req.ValidateOnly = false
	out, err := resumeReq(s, id, req)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	rz := out.Body.Resize
	if !reflect.DeepEqual(dry.Body.Resize, rz) {
		t.Errorf("validateOnly resize %+v, the resume's %+v", dry.Body.Resize, rz)
	}
	want := sized(testCPUs(t, size))
	if got := storedResources(t, s, id); got != want {
		t.Errorf("stored %+v, want %s's %+v", got, size, want)
	}
	if _, sz := runSizing(t, s, id); sz != size {
		t.Errorf("stored size %q, want %s", sz, size)
	}
	if rz == nil || rz.Size != size || rz.Applied != want || rz.RaisedFrom != raisedFrom {
		t.Fatalf("resize %+v, want %s raised from %q", rz, size, raisedFrom)
	}
	wantSizing := &SizeResolved{Size: size, CPUs: want.CPUs, Memory: want.Memory, Disk: want.Disk}
	sz := out.Body.Sizing
	if sz == nil || !reflect.DeepEqual(sz.Resolved, wantSizing) || sz.RaisedFrom != raisedFrom {
		t.Errorf("answer's sizing %+v, want %s raised from %q", sz, size, raisedFrom)
	}
	ev := resumeEvent(t, s, id)["resources"].(map[string]any)
	if raisedFrom == "" {
		if rz.RaisedFor != "" || rz.MeasuredBytes != nil || rz.NeededBytes != 0 || sz.RaisedFor != "" || ev["raisedFor"] != nil {
			t.Errorf("not raised, but resize %+v sizing %+v event %v", rz, sz, ev)
		}
		if got := queryOne[string](t, s, raise, id); got != "//" {
			t.Errorf("stored raise %q, want none", got)
		}
		return
	}
	if got, want := queryOne[string](t, s, raise, id), raisedFrom+"/saved state/"+size; got != want {
		t.Errorf("stored raise %q, want %q", got, want)
	}
	if rz.RaisedFor != "saved state" || rz.MeasuredBytes == nil || *rz.MeasuredBytes != peak || rz.NeededBytes != diskShrinkFloor(peak) {
		t.Errorf("resize %+v, want raised for saved state, measured %d, needed %d", rz, peak, diskShrinkFloor(peak))
	}
	if sz.RaisedFor != "saved state" {
		t.Errorf("sizing raisedFor %q", sz.RaisedFor)
	}
	if ev["raisedFor"] != "saved state" || ev["raisedFrom"] != raisedFrom || ev["size"] != size {
		t.Errorf("event resources %v", ev)
	}
}

// testCPUs is the cpus of testShape's size name.
func testCPUs(t *testing.T, name string) float64 {
	t.Helper()
	m, err := testShape.model()
	if err != nil {
		t.Fatal(err)
	}
	e, err := m.Parse(name)
	if err != nil {
		t.Fatal(err)
	}
	return e.CPUs
}

// A resume to a size whose disk cannot hold the Run's saved state with
// headroom gets the smallest larger size that can, never less than asked:
// one step, two steps, from raw cpus or memory, and a cpus request above
// the state's size wins over it. validateOnly answers the same.
func TestResumeShapedRaise(t *testing.T) {
	for _, c := range []raiseCase{
		// 30 GiB needs 37.5: m-1's 20 no, m-2's 40 yes.
		{"one step", "m-4-arm64", 30 * gib, resumeResources{Size: "m-1-arm64"}, "m-2-arm64", "m-1-arm64"},
		// 60 GiB needs 75: m-1's 20 and m-2's 40 no, m-4's 80 yes.
		{"two steps", "m-8-arm64", 60 * gib, resumeResources{Size: "m-1-arm64"}, "m-4-arm64", "m-1-arm64"},
		{"raw cpus", "m-8-arm64", 60 * gib, resumeResources{CPUs: f64(1.5)}, "m-4-arm64", "m-2-arm64"},
		// 100 GiB needs 125: m-4's 80 no, m-8's 160.
		{"raw memory", "m-8-arm64", 100 * gib, resumeResources{Memory: bytesPtr(12 * gib)}, "m-8-arm64", "m-4-arm64"},
		{"raw disk", "m-8-arm64", 30 * gib, resumeResources{Disk: spec.Bytes(30 * gib)}, "m-2-arm64", ""},
		// The state needs only m-2; the 4 cpus asked for stay.
		{"cpus above the state's size", "m-8-arm64", 30 * gib, resumeResources{CPUs: f64(4)}, "m-4-arm64", ""},
		// Exactly the floor: 32 GiB needs 40, m-2's disk.
		{"state at the floor", "m-4-arm64", 32 * gib, resumeResources{Size: "m-2-arm64"}, "m-2-arm64", ""},
		{"one byte over the floor", "m-4-arm64", 32*gib + 1, resumeResources{Size: "m-2-arm64"}, "m-4-arm64", "m-2-arm64"},
		// 32 GiB needs 40: raised past m-1 to m-2, whose disk is exactly that.
		{"raised to a size at the floor", "m-4-arm64", 32 * gib, resumeResources{Size: "m-1-arm64"}, "m-2-arm64", "m-1-arm64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			id := stoppedShaped(t, s, "shaped", spec.Resources{Size: c.from}, i64(c.peak))
			res := c.res
			raiseCheck(t, s, id, resumeRequest{Resources: &res}, c.size, c.raisedFrom, c.peak)
		})
	}
}

// raisedRun is a Run in pool "shaped" (testShape) of m-4-arm64 with 30 GiB
// of saved state, resumed asking for m-1-arm64 and raised to m-2-arm64.
func raisedRun(t *testing.T, s *Server) string {
	t.Helper()
	shapedPool(t, s, "shaped", &testShape)
	id := stoppedShaped(t, s, "shaped", spec.Resources{Size: "m-4-arm64"}, i64(30*gib))
	raiseCheck(t, s, id, resumeRequest{Resources: &resumeResources{Size: "m-1-arm64"}}, "m-2-arm64", "m-1-arm64", 30*gib)
	return id
}

// A raise is the Run's until a later resume sizes it: one that gets what
// it asks for clears it.
func TestResumeShapedRaiseCleared(t *testing.T) {
	s := testServer(t)
	id := raisedRun(t, s)
	key := apiKey(t, s, new("t1"), "read")
	if got := getSizing(t, s, key, id); got == nil || got.RaisedFrom != "m-1-arm64" || got.RaisedFor != "saved state" {
		t.Errorf("GET sizing %+v, want raised from m-1-arm64 for saved state", got)
	}
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	raiseCheck(t, s, id, resumeRequest{Resources: &resumeResources{Size: "m-8-arm64"}}, "m-8-arm64", "", 30*gib)
	if got := getSizing(t, s, key, id); got == nil || got.RaisedFrom != "" || got.RaisedFor != "" {
		t.Errorf("GET sizing %+v after a resize without a raise", got)
	}
}

// A luxd without size_raised_to resizes a raised Run by rewriting its
// resolved_size and resources alone: GET shows the new size without the
// raise, which described the old one.
func TestResumeShapedRaiseHiddenAfterForeignResize(t *testing.T) {
	s := testServer(t)
	id := raisedRun(t, s)
	key := apiKey(t, s, new("t1"), "read")
	execSQL(t, s, context.Background(), `UPDATE runs SET resolved_size = 'm-8-arm64',
		spec = jsonb_set(spec, '{resources}', spec->'resources' || $2::jsonb) WHERE id = $1`,
		id, map[string]any{"cpus": 8, "memory": testShape.Memory, "disk": testShape.Disk})
	got := getSizing(t, s, key, id)
	if got == nil || got.Resolved == nil || got.Resolved.Size != "m-8-arm64" {
		t.Fatalf("GET sizing %+v, want m-8-arm64", got)
	}
	if got.RaisedFrom != "" || got.RaisedFor != "" {
		t.Errorf("GET sizing raised from %q for %q, want no raise on a size it did not raise to", got.RaisedFrom, got.RaisedFor)
	}
}

// A raised Run moved to an unshaped pool loses its raise with its size.
func TestResumeRaisedOutOfShapedPool(t *testing.T) {
	s := testServer(t)
	shapedPool(t, s, "plain", nil)
	id := raisedRun(t, s)
	execSQL(t, s, context.Background(), `UPDATE runs SET state = 'stopped' WHERE id = $1`, id)
	if _, err := resumeReq(s, id, resumeRequest{Placement: &resumeTarget{PoolID: "plain"}}); err != nil {
		t.Fatal(err)
	}
	if n := queryOne[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND resolved_size IS NULL
			AND size_raised_from IS NULL AND size_raised_for IS NULL AND size_raised_to IS NULL`, id); n != 1 {
		t.Errorf("after the move: size or raise columns still set")
	}
}

// No size holds the saved state: refused with size_below_saved_state
// naming the largest size, by validateOnly and the resume alike, nothing
// changed. Without a final measurement a shrink is refused
// (size_below_saved_state).
func TestResumeShapedRaiseRefused(t *testing.T) {
	const tail = "; a shaped pool never keeps a larger disk than its size, so choose a size with more disk"
	for _, c := range []struct {
		name      string
		peak      *int64
		placement string
		res       resumeResources
		msg       string
	}{
		// 150 GiB needs 187.5: more than m-8's 160.
		{"no size fits", i64(150 * gib), "", resumeResources{Size: "m-4-arm64"},
			"pool shaped: no size from m-4-arm64 up holds the Run's saved state: its saved state used up to 150.0 GiB, " +
				"which needs at least 187.5 GiB of disk (that plus max(25%, 1 GiB)); the largest, m-8-arm64, has 160.0 GiB of disk"},
		{"unmeasured", nil, "", resumeResources{Size: "m-4-arm64"},
			"size m-4-arm64 has 80.0 GiB of disk, less than the Run has now: no disk use is recorded for the snapshot it resumes from" + tail},
		{"not final", i64(gib), "lost", resumeResources{CPUs: f64(1)},
			"size m-1-arm64 has 20.0 GiB of disk, less than the Run has now: the placement that took the snapshot it resumes from ended without reporting its final disk use" + tail},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "shaped", &testShape)
			id := stoppedShaped(t, s, "shaped", spec.Resources{Size: "m-8-arm64"}, c.peak)
			if c.placement != "" {
				execSQL(t, s, context.Background(), `UPDATE placements SET state = $2 WHERE run_id = $1`, id, c.placement)
			}
			res := c.res
			for _, dry := range []bool{true, false} {
				_, err := resumeReq(s, id, resumeRequest{Resources: &res, ValidateOnly: dry})
				he := refused(t, err, http.StatusUnprocessableEntity, "size_below_saved_state", fmt.Sprintf("validateOnly %v", dry))
				if he.Error() != c.msg {
					t.Errorf("validateOnly %v: %q, want %q", dry, he.Error(), c.msg)
				}
			}
			if got, state := storedResources(t, s, id), storedState(t, s, id); got != sized(8) || state != StateStopped {
				t.Errorf("after the refusals: %s %+v", state, got)
			}
			if _, size := runSizing(t, s, id); size != "m-8-arm64" {
				t.Errorf("after the refusals: size %q", size)
			}
		})
	}
}

// A move into a shaped pool resolves there and is raised there too: a size
// asked for in the destination whose disk cannot hold the state gets the
// destination's smallest size that can. A move asking for nothing rounds
// the Run's resources up, its disk included, so never shrinks.
func TestResumeIntoShapedPoolRaise(t *testing.T) {
	for _, c := range []struct {
		name       string
		res        *resumeResources
		size       string
		raisedFrom string
	}{
		{"a size too small for the state", &resumeResources{Size: "m-1-arm64"}, "m-4-arm64", "m-1-arm64"},
		{"raw cpus too small for the state", &resumeResources{CPUs: f64(0.5)}, "m-4-arm64", "m-05-arm64"},
		{"nothing asked: its disk rounds up", nil, "m-8-arm64", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(t)
			shapedPool(t, s, "plain", nil)
			shapedPool(t, s, "shaped", &testShape)
			// 50 GiB needs 62.5: m-2's 40 no, m-4's 80 yes.
			id := stoppedShaped(t, s, "plain", spec.Resources{CPUs: 1, Memory: spec.Bytes(4 * gib), Disk: spec.Bytes(100 * gib)}, i64(50*gib))
			raiseCheck(t, s, id, resumeRequest{Placement: &resumeTarget{PoolID: "shaped"}, Resources: c.res}, c.size, c.raisedFrom, 50*gib)
			if got := queryOne[string](t, s, `SELECT pool_id FROM runs WHERE id = $1`, id); got != "shaped" {
				t.Errorf("pool %s, want shaped", got)
			}
		})
	}
}
