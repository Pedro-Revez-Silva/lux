package spec

import (
	"strings"
	"testing"
)

func TestCheckRunName(t *testing.T) {
	for _, ok := range []string{"", "fix flaky test", " padded ", strings.Repeat("a", MaxRunName), strings.Repeat("é", MaxRunName/2), "emoji 🙂", "web#412 — review"} {
		if err := CheckRunName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, c := range []struct{ name, want string }{
		{strings.Repeat("a", MaxRunName+1), "201 bytes, at most 200"},
		{strings.Repeat("é", MaxRunName/2) + "a", "201 bytes, at most 200"},
		{"bad \xff utf-8", "not valid UTF-8"},
		{"line\nbreak", "control character U+000A"},
		{"nul\x00", "control character U+0000"},
		{"tab\there", "control character U+0009"},
		{"del\x7f", "control character U+007F"},
		{"c1 \u0085", "control character U+0085"},
	} {
		err := CheckRunName(c.name)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.name, err, c.want)
		}
	}
}
