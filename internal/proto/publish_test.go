package proto

import (
	"strings"
	"testing"
)

func TestValidArtifactName(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"notes.md", true},
		{"design/notes.md", true},
		{"a/b/c/d.txt", true},
		{"ünïcode.txt", true},
		{"..hidden", true},
		{"a..b", true},
		{"", false},
		{"/etc/passwd", false},
		{"..", false},
		{"../x", false},
		{"a/../b", false},
		{"a/..", false},
		{".", false},
		{"./a", false},
		{"a/./b", false},
		{"a//b", false},
		{"a/", false},
		{"a\x00b", false},
		{"a\nb", false},
		{"a\x7fb", false},
		{"bad\xffutf8", false},
		{strings.Repeat("x", 255), true},
		{strings.Repeat("x", 256), false},
		{"d/" + strings.Repeat("x", 256), false},
		{strings.Repeat(strings.Repeat("x", 255)+"/", 3) + strings.Repeat("y", 254) + "/z", true},
		{strings.Repeat(strings.Repeat("x", 255)+"/", 3) + strings.Repeat("y", 254) + "/zz", false},
	} {
		if err := ValidArtifactName(c.name); (err == nil) != c.ok {
			t.Errorf("ValidArtifactName(%q) = %v, want ok %v", c.name, err, c.ok)
		}
	}
}

func TestValidContentType(t *testing.T) {
	for _, c := range []struct {
		ct string
		ok bool
	}{
		{"", true},
		{"text/markdown", true},
		{"text/plain; charset=utf-8", true},
		{"not a type", false},
		{"text/" + strings.Repeat("x", 300), false},
		{"text/plain; a=\"x\x00y\"", false},
		{"text/plain; a=\"x\x01y\"", false},
		{"text/plain; a=\"x\x7fy\"", false},
		{"text/plain; a=\"x\xffy\"", false},
	} {
		if err := ValidContentType(c.ct); (err == nil) != c.ok {
			t.Errorf("ValidContentType(%q) = %v, want ok %v", c.ct, err, c.ok)
		}
	}
}

func TestValidDescription(t *testing.T) {
	for _, c := range []struct {
		d  string
		ok bool
	}{
		{"", true},
		{"the design\n\tsecond line", true},
		{"ünïcode", true},
		{"a\x00b", false},
		{"a\x1bb", false},
		{"a\rb", false},
		{"a\x7fb", false},
		{"bad\xffutf8", false},
		{strings.Repeat("x", MaxDescription), true},
		{strings.Repeat("x", MaxDescription+1), false},
	} {
		if err := ValidDescription(c.d); (err == nil) != c.ok {
			t.Errorf("ValidDescription(%q) = %v, want ok %v", c.d, err, c.ok)
		}
	}
}

func TestValidArtifactID(t *testing.T) {
	for _, c := range []struct {
		id string
		ok bool
	}{
		{"art_abcdefghijklmn27", true},
		{"art_/../xxxxxxxxxxx", false},
		{"art_abcdefghijklmn2", false},
		{"art_ABCDEFGHIJKLMN27", false},
		{"blob_abcdefghijklmn27", false},
	} {
		if got := ValidArtifactID(c.id); got != c.ok {
			t.Errorf("ValidArtifactID(%q) = %v, want %v", c.id, got, c.ok)
		}
	}
}
