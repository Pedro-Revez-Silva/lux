package hostboot

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvLinesOmitsEmptyFields(t *testing.T) {
	got := Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x"}.Lines()
	want := "LUX_URL=http://10.0.1.10:7070\nLUX_HOST_TOKEN=luxh_x\n"
	if got != want {
		t.Errorf("Lines() = %q, want %q", got, want)
	}
}

func TestValidUserData(t *testing.T) {
	for v, want := range map[string]bool{"": true, "ignition": true, "script": true, "env": true, "cloud-init": false, "IGNITION": false} {
		if got := ValidUserData(v); got != want {
			t.Errorf("ValidUserData(%q) = %v, want %v", v, got, want)
		}
	}
}

// Ignition renders valid JSON, spec 3.4.0, writing the env file with 0600
// and installing the unit enabled; zincati is masked so a runner host
// never auto-updates and reboots mid-Run.
func TestIgnitionShape(t *testing.T) {
	b, err := Ignition(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h", EC2IMDS: "http://169.254.169.254"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if v := cfg["ignition"].(map[string]any)["version"]; v != ignitionSpec {
		t.Errorf("version %v, want %v", v, ignitionSpec)
	}
	files := cfg["storage"].(map[string]any)["files"].([]any)
	var envFile map[string]any
	for _, f := range files {
		fm := f.(map[string]any)
		if fm["path"] == "/etc/lux/runner.env" {
			envFile = fm
		}
	}
	if envFile == nil {
		t.Fatal("no /etc/lux/runner.env file entry")
	}
	if envFile["mode"].(float64) != 384 { // 0600 decimal
		t.Errorf("runner.env mode %v, want 384 (0600)", envFile["mode"])
	}
	units := cfg["systemd"].(map[string]any)["units"].([]any)
	var runnerUnit, zincati map[string]any
	for _, u := range units {
		um := u.(map[string]any)
		switch um["name"] {
		case UnitName:
			runnerUnit = um
		case "zincati.service":
			zincati = um
		}
	}
	if runnerUnit == nil || runnerUnit["enabled"] != true {
		t.Errorf("lux-runner.service not enabled: %+v", runnerUnit)
	}
	if zincati == nil || zincati["mask"] != true {
		t.Errorf("zincati.service not masked: %+v", zincati)
	}
	if len(b) > 16<<10 {
		t.Errorf("rendered Ignition config is %d bytes, over the EC2 user-data limit", len(b))
	}
}

// Both Ignition and the script format append the subuid/subgid range only
// through FetchBinariesScript's own idempotent grep-then-append, not
// separately: Ignition installs no /etc/subuid or /etc/subgid file entry
// of its own (its `append` has no "only if absent", which would duplicate
// the entry on a second boot if it ever ran twice), and the fetch script
// embedded in the Ignition config is the identical constant the script
// format also embeds. One append behaviour, shared, rather than two that
// could drift.
func TestIgnitionAndScriptShareOneSubuidAppend(t *testing.T) {
	b, err := Ignition(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	files := cfg["storage"].(map[string]any)["files"].([]any)
	var fetchScript map[string]any
	for _, f := range files {
		fm := f.(map[string]any)
		switch fm["path"] {
		case "/etc/subuid", "/etc/subgid":
			t.Errorf("Ignition still writes %v directly; subuid/subgid must come from FetchBinariesScript alone", fm["path"])
		case FetchBinariesPath:
			fetchScript = fm
		}
	}
	if fetchScript == nil {
		t.Fatal("no fetch-binaries.sh file entry")
	}
	source := fetchScript["contents"].(map[string]any)["source"].(string)
	_, b64, _ := strings.Cut(source, ",")
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != FetchBinariesScript {
		t.Error("the fetch script Ignition embeds diverged from FetchBinariesScript")
	}
}

// The subuid/subgid append inside FetchBinariesScript is idempotent: run
// twice (as a reboot's ExecStartPre would), it leaves exactly one entry.
// Extracted from the constant itself (not reimplemented) and redirected
// at temp files instead of /etc/subuid and /etc/subgid, so it runs
// without root and still catches a regression in the real logic.
func TestFetchScriptsSubuidAppendIsIdempotent(t *testing.T) {
	var lines []string
	for _, l := range strings.Split(FetchBinariesScript, "\n") {
		if strings.Contains(l, "containers:") && strings.Contains(l, "grep -q") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("FetchBinariesScript: found %d subuid/subgid append lines, want 2", len(lines))
	}
	dir := t.TempDir()
	subuid := filepath.Join(dir, "subuid")
	subgid := filepath.Join(dir, "subgid")
	script := strings.NewReplacer("/etc/subuid", subuid, "/etc/subgid", subgid).Replace(strings.Join(lines, "\n"))

	run := func() {
		t.Helper()
		if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("running the subuid append: %v\n%s", err, out)
		}
	}
	run()
	run()

	got, err := os.ReadFile(subuid)
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(entries) != 1 || entries[0] != SubuidRange {
		t.Errorf("subuid after two runs: %q, want exactly one entry %q", got, SubuidRange)
	}
}

// A value with a shell metacharacter is quoted, not interpolated raw: a
// host token or URL is never trusted to be shell-safe as given. Runs the
// env-export prefix Script produces under bash and prints the token
// back, rather than pattern-matching the quoted form, so a differently
// spelled but still-safe quoting would also pass, and an unsafe one
// would fail by actually breaking out.
func TestScriptQuotesValues(t *testing.T) {
	token := "a'; rm -rf /; echo '"
	env := Env{URL: "http://x", HostToken: token}
	got := Script(env)
	prefix, _, ok := strings.Cut(got, strings.TrimPrefix(scriptBody, "#!/bin/bash\n"))
	if !ok {
		t.Fatal("Script's body diverged from scriptBody; cannot isolate the exported prefix")
	}
	out, err := exec.Command("bash", "-c", prefix+`printf '%s' "$LUX_HOST_TOKEN"`).Output()
	if err != nil {
		t.Fatalf("running the exported env under bash: %v", err)
	}
	if string(out) != token {
		t.Errorf("LUX_HOST_TOKEN round-tripped through bash as %q, want %q", out, token)
	}
}

// Script exports the env ahead of the shared, unfilled script body: the
// two must differ only by that prefix.
func TestScriptWrapsBootstrap(t *testing.T) {
	env := Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h"}
	got := Script(env)
	if !strings.HasPrefix(got, "#!/bin/bash\nexport LUX_URL='http://10.0.1.10:7070'\nexport LUX_HOST_TOKEN='luxh_x'\nexport LUX_HOST_NAME='h'\n") {
		t.Errorf("Script did not export the env first:\n%s", got[:200])
	}
	if !strings.HasSuffix(got, strings.TrimPrefix(scriptBody, "#!/bin/bash\n")) {
		t.Error("Script's body diverged from the shared scriptBody")
	}
}

// bootstrap.sh's env checks run before anything else (guard, then
// set -euo pipefail, then the ": ${VAR:?msg}" checks), so running it
// with LUX_URL unset is safe: it exits non-zero with the message before
// touching packages or files.
func TestBootstrapWithNoLuxURLExitsNonZeroWithTheMessage(t *testing.T) {
	cmd := exec.Command("bash", "-c", Bootstrap())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LUX_HOST_TOKEN=tok"} // LUX_URL deliberately unset
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("bootstrap.sh with no LUX_URL exited 0; want non-zero. Output:\n%s", out)
	}
	if !strings.Contains(string(out), "LUX_URL is required") {
		t.Errorf("bootstrap.sh with no LUX_URL: output %q, want it to mention LUX_URL is required", out)
	}
}

// LUX_NESTED is written only for a nested-capable pool, and the runner
// accepts exactly "true" (on), "" and "false" (off): a typo must not boot a
// host that silently lacks what its pool promises.
func TestNestedEnv(t *testing.T) {
	if got := (Env{URL: "u", HostToken: "t"}).Lines(); strings.Contains(got, "LUX_NESTED") {
		t.Errorf("default env carries LUX_NESTED: %q", got)
	}
	if got := (Env{URL: "u", HostToken: "t", Nested: true}).Lines(); !strings.Contains(got, "LUX_NESTED=true\n") {
		t.Errorf("nested env lacks LUX_NESTED=true: %q", got)
	}
	if s := Script(Env{URL: "u", HostToken: "t", Nested: true}); !strings.Contains(s, "export LUX_NESTED='true'\n") {
		t.Errorf("script does not export LUX_NESTED: %s", s[:200])
	}
	for v, want := range map[string]bool{"": false, "false": false, "true": true} {
		if got, err := NestedFromEnv(v); err != nil || got != want {
			t.Errorf("NestedFromEnv(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"1", "yes", "TRUE", "True", " true"} {
		if _, err := NestedFromEnv(v); err == nil {
			t.Errorf("NestedFromEnv(%q) accepted", v)
		}
	}
}
