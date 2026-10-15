package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, k := range []string{"LUX_URL", "LUX_API_KEY", "LUX_TENANT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if c := LoadConfig(); c != (Config{}) {
		t.Fatalf("no file, no env: %+v", c)
	}
	// Where LoadConfig reads it on this platform (darwin ignores
	// XDG_CONFIG_HOME), now that HOME is the temporary one.
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfgDir, "lux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("url = \"https://file\"\napi_key = \"kfile\"\ntenant = \"tfile\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := LoadConfig(); c != (Config{URL: "https://file", APIKey: "kfile", Tenant: "tfile"}) {
		t.Fatalf("file: %+v", c)
	}
	// The environment wins; an empty LUX_URL or LUX_API_KEY does not, an
	// empty LUX_TENANT does (no tenant).
	t.Setenv("LUX_URL", "https://env")
	t.Setenv("LUX_API_KEY", "")
	t.Setenv("LUX_TENANT", "")
	if c := LoadConfig(); c != (Config{URL: "https://env", APIKey: "kfile"}) {
		t.Fatalf("env: %+v", c)
	}
	// A non-empty LUX_API_KEY beats the file's key.
	t.Setenv("LUX_API_KEY", "kenv")
	if c := LoadConfig(); c.APIKey != "kenv" {
		t.Fatalf("env key: %+v", c)
	}
}
