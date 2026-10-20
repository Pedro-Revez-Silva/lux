package hostboot

import (
	"encoding/base64"
	"encoding/json"
)

// ignitionSpec: the newest spec every supported Fedora CoreOS stable
// release reads.
const ignitionSpec = "3.4.0"

type ignitionConfig struct {
	Ignition ignitionMeta    `json:"ignition"`
	Storage  ignitionStorage `json:"storage,omitempty"`
	Systemd  ignitionSystemd `json:"systemd,omitempty"`
}

type ignitionMeta struct {
	Version string `json:"version"`
}

type ignitionStorage struct {
	Files []ignitionFile `json:"files,omitempty"`
}

type ignitionFile struct {
	Path string `json:"path"`
	// Mode: Ignition wants decimal (0644 -> 420), not octal-as-written.
	Mode     int                  `json:"mode,omitempty"`
	Contents ignitionFileContents `json:"contents"`
}

type ignitionFileContents struct {
	Source string `json:"source"`
}

type ignitionSystemd struct {
	Units []ignitionUnit `json:"units,omitempty"`
}

type ignitionUnit struct {
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled,omitempty"`
	Mask     bool   `json:"mask,omitempty"`
	Contents string `json:"contents,omitempty"`
}

func dataURL(content string) string {
	return "data:;base64," + base64.StdEncoding.EncodeToString([]byte(content))
}

// Ignition renders the config Fedora CoreOS reads from EC2 user data: the
// env file, the fetch script, the writeback limits and the enabled unit.
// zincati.service is masked so a runner host never auto-updates and
// reboots mid-Run.
func Ignition(env Env) ([]byte, error) {
	cfg := ignitionConfig{
		Ignition: ignitionMeta{Version: ignitionSpec},
		Storage: ignitionStorage{
			Files: []ignitionFile{
				{Path: "/etc/lux/runner.env", Mode: 0o600, Contents: ignitionFileContents{Source: dataURL(env.Lines())}},
				{Path: FetchBinariesPath, Mode: 0o755, Contents: ignitionFileContents{Source: dataURL(FetchBinariesScript)}},
				{Path: WritebackSysctlPath, Mode: 0o644, Contents: ignitionFileContents{Source: dataURL(WritebackSysctl)}},
			},
		},
		Systemd: ignitionSystemd{
			Units: []ignitionUnit{
				{Name: UnitName, Enabled: true, Contents: Unit},
				{Name: "zincati.service", Mask: true},
			},
		},
	}
	return json.Marshal(cfg)
}
