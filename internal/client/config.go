package client

import (
	"cmp"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is where the lux CLI finds luxd and its credentials.
type Config struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
	// Tenant, for an operator key (Client.Tenant).
	Tenant string `toml:"tenant"`
}

// LoadConfig reads the CLI's configuration: $XDG_CONFIG_HOME (or the
// platform's user config dir)/lux/config.toml, overridden by LUX_URL and
// LUX_API_KEY when they are non-empty, and by LUX_TENANT whenever it is
// set (empty too: no tenant). A missing or unreadable file is no
// configuration. Command-line flags, a caller's own, go on top.
func LoadConfig() Config {
	c := readConfigFile()
	c.URL = cmp.Or(os.Getenv("LUX_URL"), c.URL)
	c.APIKey = cmp.Or(os.Getenv("LUX_API_KEY"), c.APIKey)
	if env, ok := os.LookupEnv("LUX_TENANT"); ok {
		c.Tenant = env
	}
	return c
}

func readConfigFile() Config {
	var c Config
	dir, err := os.UserConfigDir()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(filepath.Join(dir, "lux", "config.toml"))
	if err != nil {
		return c
	}
	_ = toml.Unmarshal(b, &c)
	return c
}
