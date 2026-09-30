package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A retired key is accepted, ignored and warned about, in the file and
// in the environment; any other unknown key still stops luxd.
func TestRetiredKeys(t *testing.T) {
	saved, savedStderr := retiredKeys, stderr
	t.Cleanup(func() { retiredKeys, stderr = saved, savedStderr })
	retiredKeys = []retiredKey{{toml: "s3.legacy_acl", env: "LUX_S3_LEGACY_ACL"}, {toml: "old_mode"}}
	var warnings bytes.Buffer
	stderr = &warnings

	path := filepath.Join(t.TempDir(), "luxd.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old_mode = \"fast\"\nlisten = \"0.0.0.0:7070\"\n[s3]\nbucket = \"b\"\nlegacy_acl = \"private-secret\"\n")
	t.Setenv("LUX_S3_LEGACY_ACL", "public-secret")
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:7070" || c.S3.Bucket != "b" {
		t.Errorf("the rest of the file was not read: listen %q bucket %q", c.Listen, c.S3.Bucket)
	}
	want := "luxd: warning: retired: old_mode; remove it\n" +
		"luxd: warning: retired: s3.legacy_acl; remove it\n" +
		"luxd: warning: retired: LUX_S3_LEGACY_ACL; remove it\n"
	if warnings.String() != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", warnings.String(), want)
	}

	// Alongside a key that is not retired, only that one is refused.
	write("old_mode = \"fast\"\nlisen = \"x\"\n")
	if _, err := loadConfig(path); err == nil || !strings.HasSuffix(err.Error(), ": unknown keys: lisen") {
		t.Errorf("got %v, want only lisen refused", err)
	}

	// Off the list, the same key is unknown again.
	retiredKeys = nil
	write("old_mode = \"fast\"\n")
	if _, err := loadConfig(path); err == nil || !strings.HasSuffix(err.Error(), ": unknown keys: old_mode") {
		t.Errorf("got %v, want old_mode refused", err)
	}
}
