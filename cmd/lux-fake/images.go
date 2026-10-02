package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/marcioapm/lux/internal/spec"
)

// contentBlock is one block of a message's content, in any of the shapes
// the protocols give images:
//
//	text                         {"type":"text","text"}
//	Claude Code image            {"type":"image","source":{"type":"base64","media_type","data"}}
//	ACP image                    {"type":"image","mimeType","data"}
//	Codex localImage / image     {"type":"localImage","path"} / {"type":"image","url":"data:…"}
//	OpenCode file part           {"type":"file","mime","filename","url":"data:…"}
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
	Source   struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
	Path     string `json:"path"`
	URL      string `json:"url"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
}

// images describes each image in the content as the agent got it, for its
// reply: "image <shape> <type> <bytes> sha256=<hex>[ <where>]". An image
// it cannot read, one whose declared type is not its bytes', and a block
// of a type no protocol gives (real agents refuse those) are described
// with "image <shape> error: …", which the e2e tests never expect.
func (t textBlocks) images() []string {
	var out []string
	for _, b := range t {
		var shape, typ, where string
		var data []byte
		var err error
		switch {
		case b.Type == "" || b.Type == "text":
			continue
		case b.Type == "image" && b.Source.Type == "base64":
			shape, typ = "claude", b.Source.MediaType
			data, err = base64.StdEncoding.DecodeString(b.Source.Data)
		case b.Type == "image" && b.MimeType != "":
			shape, typ = "acp", b.MimeType
			data, err = base64.StdEncoding.DecodeString(b.Data)
		case b.Type == "image" && b.URL != "":
			shape = "codex-url"
			typ, data, err = dataURL(b.URL)
		case b.Type == "localImage":
			shape, where = "codex-local", b.Path
			data, err = os.ReadFile(b.Path)
			typ = "file"
		case b.Type == "file":
			shape, where = "opencode-file", b.Filename
			var urlType string
			urlType, data, err = dataURL(b.URL)
			typ = b.Mime
			if err == nil && urlType != b.Mime {
				err = fmt.Errorf("mime %s, data URL %s", b.Mime, urlType)
			}
		default:
			out = append(out, fmt.Sprintf("image unknown-block error: type %q", b.Type))
			continue
		}
		if err == nil && typ != "file" {
			if got := spec.SniffImage(data); got != typ {
				err = fmt.Errorf("declared %s, bytes are %q", typ, got)
			}
		}
		if err != nil {
			out = append(out, fmt.Sprintf("image %s error: %v", shape, err))
			continue
		}
		sum := sha256.Sum256(data)
		line := fmt.Sprintf("image %s %s %d sha256=%s", shape, typ, len(data), hex.EncodeToString(sum[:]))
		if where != "" {
			line += " " + where
		}
		out = append(out, line)
	}
	return out
}

// dataURL decodes data:<type>;base64,<data>.
func dataURL(u string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	head, data, ok2 := strings.Cut(rest, ",")
	typ, b64 := strings.CutSuffix(head, ";base64")
	if !ok || !ok2 || !b64 {
		return "", nil, fmt.Errorf("not a base64 data URL")
	}
	b, err := base64.StdEncoding.DecodeString(data)
	return typ, b, err
}
