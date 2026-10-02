package cli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lux steer --image: each file is an attachment typed by its magic bytes
// (whatever its extension says), named by its base name; the message may
// be left out.
func TestSteerImages(t *testing.T) {
	dir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\nrest")
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2}
	for name, b := range map[string][]byte{"shot.png": png, "photo.png": jpeg, "notes.txt": []byte("hello")} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var body map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requestId":"in_1"}`))
	})
	if _, err := runCLI(t, h, "steer", "run_1", "look", "--image", filepath.Join(dir, "shot.png"), "--image", filepath.Join(dir, "photo.png")); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body["attachments"])
	want := `[{"contentType":"image/png","data":"` + base64.StdEncoding.EncodeToString(png) + `","name":"shot.png"},` +
		`{"contentType":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString(jpeg) + `","name":"photo.png"}]`
	if string(b) != want || body["text"] != "look" {
		t.Fatalf("body %v\nattachments %s\nwant %s", body, b, want)
	}
	if _, err := runCLI(t, h, "steer", "run_1", "--image", filepath.Join(dir, "shot.png")); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["text"]; ok {
		t.Fatalf("image only: %v", body)
	}
	body = nil
	_, err := runCLI(t, h, "steer", "run_1", "x", "--image", filepath.Join(dir, "notes.txt"))
	if err == nil || !strings.Contains(err.Error(), "not an image lux takes (image/png, image/jpeg, image/gif or image/webp)") || body != nil {
		t.Fatalf("a text file: %v, sent %v", err, body)
	}
	if _, err := runCLI(t, h, "steer", "run_1"); err == nil {
		t.Fatal("no message and no image accepted")
	}
}
