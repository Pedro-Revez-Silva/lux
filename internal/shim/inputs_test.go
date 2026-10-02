package shim

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/internal/adapter"
	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

var testPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-not-a-real-image-but-png-magic")

func pngAttachment(name string) spec.Attachment {
	return spec.Attachment{Name: name, ContentType: "image/png", Data: base64.StdEncoding.EncodeToString(testPNG)}
}

func inputsShim(t *testing.T) (*Shim, string, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.jsonl")
	out, err := OpenOutput(path, NewRedactor(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	s := &Shim{out: out, red: NewRedactor(nil), user: &userInfo{uid: os.Getuid(), gid: os.Getgid(), home: home},
		cfg: proto.ShimConfig{InputsDir: filepath.Join(home, ".lux-inputs"), InputsRoot: home}}
	return s, home, path
}

// Each image is written under $LUX_INPUTS/<request id>/<n>-<safe name>,
// private to the workload user; names never act as paths.
func TestWriteInputs(t *testing.T) {
	s, home, _ := inputsShim(t)
	if err := s.prepareInputs(); err != nil {
		t.Fatal(err)
	}
	got, meta, err := s.writeInputs("in_1", []spec.Attachment{pngAttachment("shot.png"), pngAttachment("..")})
	if err != nil {
		t.Fatal(err)
	}
	if want := spec.AttachmentsMeta([]spec.Attachment{pngAttachment("shot.png"), pngAttachment("..")}); !slices.Equal(meta, want) {
		t.Fatalf("meta %+v, want %+v", meta, want)
	}
	dir := filepath.Join(home, ".lux-inputs", "in_1")
	want := []string{filepath.Join(dir, "1-shot.png"), filepath.Join(dir, "2-..-")}
	if got[0].Path != want[0] || !strings.HasPrefix(got[1].Path, want[1]) || filepath.Dir(got[1].Path) != dir {
		t.Fatalf("paths %q %q", got[0].Path, got[1].Path)
	}
	for _, a := range got {
		b, err := os.ReadFile(a.Path)
		if err != nil || !bytes.Equal(b, testPNG) {
			t.Fatalf("%s: %v %q", a.Path, err, b)
		}
		if fi, _ := os.Stat(a.Path); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", a.Path, fi.Mode())
		}
	}
	for _, d := range []string{filepath.Join(home, ".lux-inputs"), dir} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v", d, fi.Mode())
		}
	}
}

// A link the workload planted where an image goes is replaced, not
// written through; one on the way out of the volume stops the write.
func TestWriteInputsLinks(t *testing.T) {
	s, home, _ := inputsShim(t)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".lux-inputs", "r1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "1-a.png")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.writeInputs("r1", []spec.Attachment{pngAttachment("a.png")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("wrote through the link: %q", b)
	}
	// $LUX_INPUTS/r2 a link to a directory outside the volume.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(home, ".lux-inputs", "r2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.writeInputs("r2", []spec.Attachment{pngAttachment("victim")}); err == nil {
		t.Fatal("wrote through a link out of the volume")
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("victim changed: %q", b)
	}
}

// Records carry each image's metadata (lux.input, and lux.input.failed
// after acceptance), even when the adapter reports the input without its
// payload, and never its bytes.
func TestInputRecordsAttachments(t *testing.T) {
	s, _, path := inputsShim(t)
	k := &sink{s: s}
	in := proto.Input{RequestID: "img", Text: "look", Attachments: []spec.Attachment{pngAttachment("shot.png")}}
	s.rememberAttachments(in.RequestID, spec.AttachmentsMeta(in.Attachments))
	k.InputAccepted(proto.Input{RequestID: "img"}, adapter.Delivery{Lands: adapter.LandsNextStep, Receipt: true})
	k.InputFailed(proto.Input{RequestID: "img"}, errors.New("the Run stopped before the agent read it"))
	failed := proto.Input{RequestID: "bad", Attachments: []spec.Attachment{pngAttachment("x.png")}}
	s.rememberAttachments(failed.RequestID, spec.AttachmentsMeta(failed.Attachments))
	k.InputFailed(failed, errors.New("the agent does not take images"))
	plain := proto.Input{RequestID: "plain", Text: "hi"}
	k.InputAccepted(plain, adapter.Delivery{Lands: adapter.LandsNextStep})
	s.out.Close()

	meta := spec.AttachmentsMeta(in.Attachments)
	type rec struct {
		Type string
		Data map[string]json.RawMessage
	}
	var recs []rec
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(in.Attachments[0].Data)) {
		t.Fatalf("an image's bytes are in the records")
	}
	for _, r := range readRecords(t, path) {
		var ev struct {
			Type string                     `json:"type"`
			Data map[string]json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(r.Event, &ev)
		recs = append(recs, rec{ev.Type, ev.Data})
	}
	if len(recs) != 4 {
		t.Fatalf("%d records", len(recs))
	}
	wantMeta, _ := json.Marshal(meta)
	for i := range 3 {
		if string(recs[i].Data["attachments"]) != string(wantMeta) && i != 2 {
			t.Errorf("record %d (%s) attachments %s, want %s", i, recs[i].Type, recs[i].Data["attachments"], wantMeta)
		}
	}
	if m, _ := json.Marshal(spec.AttachmentsMeta(failed.Attachments)); string(recs[2].Data["attachments"]) != string(m) {
		t.Errorf("failed before accepted: %s", recs[2].Data["attachments"])
	}
	if _, ok := recs[3].Data["attachments"]; ok {
		t.Errorf("an input without images has attachments: %v", recs[3])
	}
	if len(s.inputMeta) != 0 {
		t.Errorf("metadata kept past the last record: %v", s.inputMeta)
	}
}

func TestSafeComponent(t *testing.T) {
	for in, want := range map[string]string{
		"shot.png": "shot.png", "in_01J": "in_01J", "a b.png": "a_b-*.png", "é.png": "_-*.png",
		"..": "..-*", ".": ".-*", "a b": "a_b-*", ".bash rc": ".bash_rc-*",
	} {
		got := safeComponent(in)
		if pre, post, ok := strings.Cut(want, "*"); ok {
			if !strings.HasPrefix(got, pre) || !strings.HasSuffix(got, post) || len(got) != len(pre)+8+len(post) {
				t.Errorf("%q: %q, want %s<8 hex>%s", in, got, pre, post)
			}
		} else if got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if safeComponent("a b") == safeComponent("a_b") {
		t.Error("two names, one file")
	}
}
