package spec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

func TestCheckAttachmentsTypes(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	ok := map[string][]byte{
		"image/png":  []byte("\x89PNG\r\n\x1a\nrest"),
		"image/jpeg": {0xff, 0xd8, 0xff, 0xdb},
		"image/gif":  []byte("GIF89a..."),
		"image/webp": []byte("RIFF\x10\x00\x00\x00WEBPVP8 "),
	}
	for typ, b := range ok {
		meta, errs := CheckAttachments("a", []Attachment{{Name: "x", ContentType: typ, Data: enc(b)}})
		if errs != nil {
			t.Errorf("%s refused: %v", typ, errs)
		}
		sum := sha256.Sum256(b)
		if want := (AttachmentMeta{Name: "x", ContentType: typ, Size: len(b), SHA256: hex.EncodeToString(sum[:])}); len(meta) != 1 || meta[0] != want {
			t.Errorf("%s meta %+v, want %+v", typ, meta, want)
		}
		if got := SniffImage(b); got != typ {
			t.Errorf("SniffImage %s = %q", typ, got)
		}
	}
	meta, errs := CheckAttachments("a", []Attachment{{Name: "x", ContentType: "image/webp", Data: enc([]byte("RIFF\x10\x00\x00\x00WAVE"))}})
	if len(errs) != 1 || meta != nil || !strings.Contains(errs[0], "a[0]: contentType image/webp does not match its bytes (not an image lux takes)") {
		t.Fatalf("got %v %v", meta, errs)
	}
	_, errs = CheckAttachments("a", []Attachment{{Name: "x", ContentType: "image/svg+xml", Data: enc([]byte("<svg/>"))}})
	if len(errs) != 1 || !strings.HasSuffix(errs[0], `contentType "image/svg+xml": need image/png, image/jpeg, image/gif or image/webp`) {
		t.Fatalf("got %v", errs)
	}
	// Unpadded base64 is not standard base64 (13 bytes: padded with ==).
	_, errs = CheckAttachments("a", []Attachment{{Name: "x", ContentType: "image/png", Data: strings.TrimRight(enc([]byte("\x89PNG\r\n\x1a\nrest!")), "=")}})
	if len(errs) != 1 || !strings.Contains(errs[0], "not standard base64") {
		t.Fatalf("got %v", errs)
	}
}

// Every limit at its boundary: the largest accepted, the smallest refused.
func TestCheckAttachmentsLimits(t *testing.T) {
	png := func(n int) Attachment {
		b := make([]byte, n)
		copy(b, "\x89PNG\r\n\x1a\n")
		return Attachment{Name: "a.png", ContentType: "image/png", Data: base64.StdEncoding.EncodeToString(b)}
	}
	named := func(n int) Attachment { a := png(16); a.Name = strings.Repeat("n", n); return a }
	for _, c := range []struct {
		name string
		list []Attachment
		want string // "" accepted, else a refusal containing it
	}{
		{"5 MiB exactly", []Attachment{png(MaxAttachmentBytes)}, ""},
		{"5 MiB + 1", []Attachment{png(MaxAttachmentBytes + 1)}, "a[0]: too big: 5242881 bytes decoded, at most 5242880"},
		{"5 MiB + 3: refused before decoding", []Attachment{png(MaxAttachmentBytes + 3)}, "a[0]: too big: more than 5242880 bytes decoded"},
		{"10", slices.Repeat([]Attachment{png(16)}, 10), ""},
		{"11", slices.Repeat([]Attachment{png(16)}, 11), "a: 11 attachments, at most 10"},
		{"255-byte name", []Attachment{named(255)}, ""},
		{"256-byte name", []Attachment{named(256)}, "a[0]: name is 256 bytes, at most 255"},
	} {
		meta, errs := CheckAttachments("a", c.list)
		switch {
		case c.want == "" && (errs != nil || len(meta) != len(c.list)):
			t.Errorf("%s: refused %v", c.name, errs)
		case c.want != "" && (len(errs) != 1 || errs[0] != c.want || meta != nil):
			t.Errorf("%s: got %v, want %q", c.name, errs, c.want)
		}
	}
}

func TestAttachmentMeta(t *testing.T) {
	a := Attachment{Name: "n.gif", ContentType: "image/gif", Data: base64.StdEncoding.EncodeToString([]byte("GIF89a"))}
	m := a.Meta()
	sum := sha256.Sum256([]byte("GIF89a"))
	want := AttachmentMeta{Name: "n.gif", ContentType: "image/gif", Size: 6, SHA256: hex.EncodeToString(sum[:])}
	if m != want {
		t.Fatalf("%+v", m)
	}
}

func TestInputsDir(t *testing.T) {
	home := Volume{Name: "home", Path: "/home/agent", Kind: "state"}
	ws := Volume{Name: "workspace", Path: "/workspace", Kind: "state"}
	cache := Volume{Name: "cache", Path: "/cache", Kind: "ephemeral"}
	codexVol := Volume{Name: "codex", Path: "/home/agent/.codex", Kind: "state"}
	repoAtHome := &Git{Repositories: []Repository{{Name: "dots", Path: "/home/agent"}}}
	for _, c := range []struct {
		name, adapter string
		vols          []Volume
		git           *Git
		dir, root     string
	}{
		{"agent home volume", "claude-code", []Volume{ws, home}, nil, "/home/agent/.lux-inputs", "/home/agent"},
		{"session volume of its own", "codex", []Volume{ws, home, codexVol}, nil, "/home/agent/.codex/.lux-inputs", "/home/agent/.codex"},
		{"acp: the home's", "acp", []Volume{ws, home}, nil, "/home/agent/.lux-inputs", "/home/agent"},
		{"acp: the first state volume", "acp", []Volume{cache, ws}, nil, "/workspace/.lux-inputs", "/workspace"},
		{"checkout over the home", "claude-code", []Volume{home, ws}, repoAtHome, "/workspace/.lux-inputs", "/workspace"},
		{"checkout over every state volume", "claude-code", []Volume{home}, repoAtHome, RuntimeInputsDir, "/.lux/run"},
		{"no state volume", "acp", []Volume{cache}, nil, RuntimeInputsDir, "/.lux/run"},
	} {
		s := RunSpec{Workload: Workload{Adapter: c.adapter}, Volumes: c.vols, Git: c.git}
		if dir, root := s.InputsDir("/home/agent"); dir != c.dir || root != c.root {
			t.Errorf("%s: %s on %s, want %s on %s", c.name, dir, root, c.dir, c.root)
		}
	}
}
