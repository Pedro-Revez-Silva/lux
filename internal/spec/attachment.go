package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Attachment is an image given to an agent with its input: with a steer
// (POST /v1/runs/{id}/input) or with the workload's first prompt
// (workload.attachments).
type Attachment struct {
	Name        string `json:"name" yaml:"name" doc:"Shown to people; 1-255 bytes of UTF-8 without /, \\, NUL or control characters. Never used as a path as given."`
	ContentType string `json:"contentType" yaml:"contentType" doc:"image/png, image/jpeg, image/webp or image/gif; the bytes' magic number must match."`
	Data        string `json:"data,omitempty" yaml:"data,omitempty" doc:"Required: the image, standard base64 (no data: prefix), at most 5 MiB decoded. Run views leave it out."`
	// Path is where the shim wrote the image in the container; the shim
	// sets it, nobody else.
	Path string `json:"-" yaml:"-"`
}

// AttachmentMeta is what records and events say of an attachment: never
// its bytes.
type AttachmentMeta struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
}

// Attachment limits (docs/runspec.md).
const (
	MaxAttachments     = 10
	MaxAttachmentBytes = 5 << 20
	maxAttachmentName  = 255
)

// imageTypes are the content types lux takes, in the order SniffImage
// tries them, each with a check of the decoded bytes' magic number.
var imageTypes = []struct {
	typ   string
	magic func([]byte) bool
}{
	{"image/png", func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }},
	{"image/jpeg", func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) }},
	{"image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	{"image/webp", func(b []byte) bool {
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	}},
}

// ImageTypes is the content types lux takes, for messages: "image/png,
// image/jpeg, image/gif or image/webp".
func ImageTypes() string {
	names := make([]string, len(imageTypes))
	for i, t := range imageTypes {
		names[i] = t.typ
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func imageMagic(typ string) func([]byte) bool {
	for _, t := range imageTypes {
		if t.typ == typ {
			return t.magic
		}
	}
	return nil
}

// SniffImage is the content type of an image lux takes, from its magic
// number; "" if it is none of them.
func SniffImage(b []byte) string {
	for _, t := range imageTypes {
		if t.magic(b) {
			return t.typ
		}
	}
	return ""
}

// CheckAttachments checks a list of attachments, decoding each once: the
// metadata of each when all are good, else what is wrong (at names the
// list in the messages, e.g. "attachments").
func CheckAttachments(at string, list []Attachment) ([]AttachmentMeta, []string) {
	var errs []string
	if len(list) > MaxAttachments {
		errs = append(errs, fmt.Sprintf("%s: %d attachments, at most %d", at, len(list), MaxAttachments))
	}
	meta := make([]AttachmentMeta, len(list))
	for i, a := range list {
		m, err := checkAttachment(a)
		if err != "" {
			errs = append(errs, fmt.Sprintf("%s[%d]: %s", at, i, err))
		}
		meta[i] = m
	}
	if len(errs) > 0 || len(list) == 0 {
		return nil, errs
	}
	return meta, nil
}

func checkAttachment(a Attachment) (AttachmentMeta, string) {
	if msg := checkAttachmentName(a.Name); msg != "" {
		return AttachmentMeta{}, msg
	}
	magic := imageMagic(a.ContentType)
	if magic == nil {
		return AttachmentMeta{}, fmt.Sprintf("contentType %q: need %s", a.ContentType, ImageTypes())
	}
	// Refused before decoding: base64 is 4 characters per 3 bytes.
	if a.Data == "" {
		return AttachmentMeta{}, "data is required"
	}
	if base64.StdEncoding.DecodedLen(len(a.Data)) > MaxAttachmentBytes+2 {
		return AttachmentMeta{}, fmt.Sprintf("too big: more than %d bytes decoded", MaxAttachmentBytes)
	}
	b, err := base64.StdEncoding.Strict().DecodeString(a.Data)
	if err != nil {
		return AttachmentMeta{}, "data is not standard base64: " + err.Error()
	}
	if len(b) > MaxAttachmentBytes {
		return AttachmentMeta{}, fmt.Sprintf("too big: %d bytes decoded, at most %d", len(b), MaxAttachmentBytes)
	}
	if !magic(b) {
		got := SniffImage(b)
		if got == "" {
			got = "not an image lux takes"
		}
		return AttachmentMeta{}, fmt.Sprintf("contentType %s does not match its bytes (%s)", a.ContentType, got)
	}
	return MetaOf(a, b), ""
}

func checkAttachmentName(name string) string {
	switch {
	case name == "":
		return "name is required"
	case len(name) > maxAttachmentName:
		return fmt.Sprintf("name is %d bytes, at most %d", len(name), maxAttachmentName)
	case !utf8.ValidString(name):
		return "name is not valid UTF-8"
	case strings.ContainsAny(name, `/\`):
		return `name must not contain / or \`
	case strings.ContainsFunc(name, unicode.IsControl):
		return "name must not contain NUL or control characters"
	}
	return ""
}

// Decoded is the attachment's bytes; nil if its data is not base64 (a
// checked attachment always is).
func (a Attachment) Decoded() []byte {
	b, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return nil
	}
	return b
}

// Meta is what records say of the attachment.
func (a Attachment) Meta() AttachmentMeta { return MetaOf(a, a.Decoded()) }

// MetaOf is a's metadata from its decoded bytes b.
func MetaOf(a Attachment, b []byte) AttachmentMeta {
	sum := sha256.Sum256(b)
	return AttachmentMeta{Name: a.Name, ContentType: a.ContentType, Size: len(b), SHA256: hex.EncodeToString(sum[:])}
}

// AttachmentsMeta is Meta of each; nil for none.
func AttachmentsMeta(list []Attachment) []AttachmentMeta {
	if len(list) == 0 {
		return nil
	}
	out := make([]AttachmentMeta, len(list))
	for i, a := range list {
		out[i] = a.Meta()
	}
	return out
}

// RuntimeInputsDir is $LUX_INPUTS for a Run with no state volume: on the
// runtime volume, which a stop on the same host keeps and a move does not.
const RuntimeInputsDir = "/.lux/run/inputs"

// InputsDir is where the shim writes input images ($LUX_INPUTS) for a
// workload whose user's home is home, and the volume mount it is on (root).
// It is .lux-inputs at the root of a state volume, so a snapshot carries
// it through stop, resume and migration: the one holding the adapter's
// session, else the one holding home, else the first; a volume whose root
// is inside a git checkout is skipped for the next. $LUX_ARTIFACTS is on
// the runtime volume, never a state volume. With no state volume left it
// is RuntimeInputsDir.
func (s *RunSpec) InputsDir(home string) (dir, root string) {
	var cands []string
	for _, p := range Adapters[s.Workload.Adapter].StatePaths {
		cands = append(cands, path.Clean(strings.ReplaceAll(p, "$HOME", home)))
	}
	cands = append(cands, path.Clean(home))
	for _, v := range s.Volumes {
		if v.Kind == "state" {
			cands = append(cands, v.Path)
		}
	}
	inCheckout := func(p string) bool {
		if s.Git == nil {
			return false
		}
		for _, r := range s.Git.Repositories {
			if Under(p, r.Path) {
				return true
			}
		}
		return false
	}
	for _, c := range cands {
		v := s.stateVolumeFor(c)
		if v == nil {
			continue
		}
		if d := path.Join(v.Path, ".lux-inputs"); !inCheckout(d) {
			return d, v.Path
		}
	}
	return RuntimeInputsDir, path.Dir(RuntimeInputsDir)
}
