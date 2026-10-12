package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/proto"
)

// The image platforms come with the imageReady mark and are served on the
// placement; a later report without them (an older runner, or one that
// could not tell) keeps them; one outside the bounds stores nothing.
func TestImagePlatformsOnPlacement(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}})
	if got := getRun(t, s, key).Placements[0].ImagePlatforms; got != nil {
		t.Fatalf("before a runner sent any: %v, want absent", got)
	}

	want := []string{"linux/amd64", "linux/arm64/v8"}
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}, ImagePlatforms: want})
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0), "volumesRestored": ms(t0)}})
	if got := getRun(t, s, key).Placements[0].ImagePlatforms; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("platforms %v, want %v", got, want)
	}

	tooMany := make([]string, maxImagePlatforms+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("linux/arch%d", i)
	}
	for _, bad := range [][]string{tooMany, {"linux/" + strings.Repeat("x", maxImagePlatformLen)}, {""}} {
		reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)}, ImagePlatforms: bad})
		if got := getRun(t, s, key).Placements[0].ImagePlatforms; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("after an out-of-bounds list: %v, want %v kept", got, want)
		}
	}
}

// A list with an entry Postgres cannot store as text (a NUL) is dropped
// whole, and the status it came with still applies.
func TestImagePlatformsWithNULDropped(t *testing.T) {
	s, ctx, key := stageFixture(t)
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	reportStatus(t, s, ctx, proto.Status{State: "starting", Times: map[string]int64{"imageReady": ms(t0)},
		ImagePlatforms: []string{"linux/amd64", "linux/arm64/v8\x00"}})
	run := getRun(t, s, key)
	if got := run.Placements[0].ImagePlatforms; got != nil {
		t.Fatalf("platforms %q, want absent", got)
	}
	if run.Placements[0].ImageReadyAt == nil {
		t.Fatalf("imageReady not applied: %+v", run.Placements[0])
	}
}

// JSON decoding already replaces invalid UTF-8, so this one is checked on
// the filter itself.
func TestPlatformsInvalidUTF8Dropped(t *testing.T) {
	if got := platforms([]string{"linux/amd64", "linux/\xff"}); got != nil {
		t.Fatalf("%q, want nil", got)
	}
}
