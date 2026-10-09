package proto

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/marcioapm/lux/internal/ids"
)

// Publishing an artifact: `lux-shim publish FILE --name NAME` runs in the
// container as the workload user, opens FILE itself and sends its bytes
// over ShimPublishSocket: one PublishRequest JSON line, then exactly Size
// bytes, then a half-close. The shim (root) never opens a path the client
// names. It stages the bytes in ShimConfig.ArtifactsDir, writes an
// EvArtifact record (StagedArtifact) and only then answers with one
// PublishReply line.

const (
	// ShimPublishSocket is the workload user's (mode 0600); ShimSocket
	// stays root's.
	ShimPublishSocket = "/.lux/run/publish.sock"
	// MaxArtifactBytes caps one artifact file.
	MaxArtifactBytes = 1 << 30
	// PublishedPrefix is the path a published artifact is listed under.
	PublishedPrefix = "/.lux/artifacts/"
	// MaxDescription and MaxArtifactName bound a publish's text fields.
	MaxDescription  = 4096
	MaxArtifactName = 1024
)

type PublishRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size"`
}

type PublishReply struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Error  string `json:"error,omitempty"`
}

// StagedArtifact is EvArtifact's data. File is the staged copy's path
// relative to the runtime volume (ShimRunDir).
type StagedArtifact struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	File        string `json:"file"`
}

// DetectContentType is a file's content type by its name's extension, else
// sniffed from head, its first 512 bytes.
func DetectContentType(name string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

// ValidArtifactName checks a published artifact's name: relative,
// path.Clean-stable, no "." or ".." segment, no control characters, valid
// UTF-8, each segment at most 255 bytes. Sub-directories are allowed.
func ValidArtifactName(name string) error {
	switch {
	case name == "":
		return errors.New("empty name")
	case len(name) > MaxArtifactName:
		return fmt.Errorf("name longer than %d bytes", MaxArtifactName)
	case !utf8.ValidString(name):
		return errors.New("name is not valid UTF-8")
	case strings.HasPrefix(name, "/"):
		return errors.New("name must be relative, not start with /")
	}
	if hasControl(name, "") {
		return errors.New("name has a control character")
	}
	for _, seg := range strings.Split(name, "/") {
		switch {
		case seg == "..":
			return errors.New("name has a .. segment")
		case seg == ".":
			return errors.New("name has a . segment")
		case len(seg) > 255:
			return errors.New("a name segment is longer than 255 bytes")
		}
	}
	if path.Clean(name) != name {
		return errors.New("name is not a clean path (empty segment or trailing /)")
	}
	return nil
}

// ValidPublishFields checks a publish's name, content type and
// description, in that order; the shim, the runner and luxd all apply it.
func ValidPublishFields(name, contentType, description string) error {
	if err := ValidArtifactName(name); err != nil {
		return err
	}
	if err := ValidContentType(contentType); err != nil {
		return err
	}
	return ValidDescription(description)
}

// ValidArtifactID: what the shim mints (ids.New(ids.Artifact)). A root
// workload can forge records, and the id names files on the host.
func ValidArtifactID(id string) bool {
	rest, ok := strings.CutPrefix(id, ids.Artifact+"_")
	if !ok || len(rest) != 16 {
		return false
	}
	for _, c := range rest {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// ValidContentType accepts "" (detected) or a parseable media type of
// printable UTF-8: mime.ParseMediaType lets a NUL through in a quoted
// parameter, which Postgres text cannot store.
func ValidContentType(ct string) error {
	if ct == "" {
		return nil
	}
	if len(ct) > 255 {
		return errors.New("content type longer than 255 bytes")
	}
	if !utf8.ValidString(ct) {
		return errors.New("content type is not valid UTF-8")
	}
	if hasControl(ct, "") {
		return errors.New("content type has a control character")
	}
	if _, _, err := mime.ParseMediaType(ct); err != nil {
		return fmt.Errorf("content type: %w", err)
	}
	return nil
}

// ValidDescription: at most MaxDescription bytes of UTF-8 text, no control
// characters but tab and newline.
func ValidDescription(d string) error {
	if len(d) > MaxDescription || !utf8.ValidString(d) {
		return fmt.Errorf("description: at most %d bytes of UTF-8 text", MaxDescription)
	}
	if hasControl(d, "\t\n") {
		return errors.New("description has a control character")
	}
	return nil
}

// hasControl: s has a byte below 0x20 or 0x7f that is not in allowed.
func hasControl(s, allowed string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 || c == 0x7f) && strings.IndexByte(allowed, c) < 0 {
			return true
		}
	}
	return false
}
