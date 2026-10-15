package cli

import (
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/proto"
)

func TestWriteNetClassesOrderAndUnknown(t *testing.T) {
	var b strings.Builder
	writeNetClasses(&b, map[string]proto.NetBytes{
		"unknown": {Rx: 1}, "provider:us-west-2": {Tx: 2048}, "local": {Rx: 1024, Tx: 1},
		"provider:eu-west-1": {Tx: 3}, "zone": {Rx: 5},
	})
	var keys []string
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		keys = append(keys, strings.Fields(l)[0])
	}
	want := []string{"local", "zone", "provider:eu-west-1", "provider:us-west-2", "unknown"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Errorf("order %v, want %v", keys, want)
	}
	if !strings.Contains(b.String(), "unknown") || !strings.Contains(b.String(), "unclassified") {
		t.Errorf("unknown not marked unclassified:\n%s", b.String())
	}
}
