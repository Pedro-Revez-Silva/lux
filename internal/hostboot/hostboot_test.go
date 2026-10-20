package hostboot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
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
	got = Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", Memory: "68719476736"}.Lines()
	want = "LUX_URL=http://10.0.1.10:7070\nLUX_HOST_TOKEN=luxh_x\nLUX_RUNNER_MEMORY=68719476736\n"
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

// parseSysctl reads a sysctl.d file the way systemd-sysctl does: key = value
// lines, blank lines and #/; comments ignored.
func parseSysctl(t *testing.T, raw string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("not a sysctl line: %q", line)
		}
		got[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return got
}

// installedFile returns the mode and decoded contents of the file Ignition
// config b installs at path. Its own struct, not ignitionConfig, so a wrong
// json tag in the production type is caught rather than agreed with.
func installedFile(t *testing.T, b []byte, path string) (mode int, content string) {
	t.Helper()
	var cfg struct {
		Storage struct {
			Files []struct {
				Path     string `json:"path"`
				Mode     int    `json:"mode"`
				Contents struct {
					Source string `json:"source"`
				} `json:"contents"`
			} `json:"files"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, f := range cfg.Storage.Files {
		if f.Path != path {
			continue
		}
		b64, ok := strings.CutPrefix(f.Contents.Source, "data:;base64,")
		if !ok {
			t.Fatalf("%s source is not a base64 data URL: %.40s", path, f.Contents.Source)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		return f.Mode, string(raw)
	}
	t.Fatalf("no %s in the Ignition config", path)
	return 0, ""
}

var wantWriteback = map[string]string{"vm.dirty_background_bytes": "67108864", "vm.dirty_bytes": "268435456"}

// An Ignition host gets the writeback limits as a sysctl.d file, which
// systemd-sysctl applies at boot, before lux-runner starts.
func TestIgnitionCapsWriteback(t *testing.T) {
	b, err := Ignition(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h"})
	if err != nil {
		t.Fatal(err)
	}
	mode, raw := installedFile(t, b, "/etc/sysctl.d/90-lux-writeback.conf")
	if mode != 0o644 {
		t.Errorf("mode %o, want 644", mode)
	}
	if got := parseSysctl(t, raw); !maps.Equal(got, wantWriteback) {
		t.Errorf("sysctls %v, want %v", got, wantWriteback)
	}
}

// A script-format host (an EC2 pool on a stock AMI) writes the same file
// and loads it at once: cloud-init runs after systemd-sysctl has already
// applied /etc/sysctl.d.
func TestScriptCapsWriteback(t *testing.T) {
	root := bootScript(t, Script(Env{URL: "http://luxd", HostToken: "luxh_x", HostName: "h"}))
	raw, err := os.ReadFile(root + "/etc/sysctl.d/90-lux-writeback.conf")
	if err != nil {
		t.Fatal(err)
	}
	if got := parseSysctl(t, string(raw)); !maps.Equal(got, wantWriteback) {
		t.Errorf("sysctls %v, want %v", got, wantWriteback)
	}
	loaded, err := os.ReadFile(root + "/sysctl.calls")
	if err != nil || !strings.Contains(string(loaded), "-p "+root+"/etc/sysctl.d/90-lux-writeback.conf") {
		t.Errorf("the script did not load the file with sysctl -p; sysctl calls: %q", loaded)
	}
}

// A host without sysctl, or one where loading the file fails, still
// boots its runner: Script runs under set -e, and the cap is not worth
// failing a host over. The file is still written for the next boot.
func TestScriptBootsWithoutWorkingSysctl(t *testing.T) {
	for name, sysctl := range map[string]string{"missing": "", "failing": "exit 1"} {
		t.Run(name, func(t *testing.T) {
			root := bootScriptWith(t, Script(Env{URL: "http://luxd", HostToken: "luxh_x", HostName: "h"}), sysctl)
			if _, err := os.Stat(root + "/etc/sysctl.d/90-lux-writeback.conf"); err != nil {
				t.Errorf("the file was not written: %v", err)
			}
			if _, err := os.Stat(root + "/etc/lux/runner.env"); err != nil {
				t.Errorf("the runner was not set up: %v", err)
			}
		})
	}
}

// The bootstrap script sets up static hosts, which may run other things:
// run, it must neither write a sysctl.d file nor call sysctl.
func TestBootstrapLeavesWritebackAlone(t *testing.T) {
	root := bootScript(t, Bootstrap(), "LUX_URL=http://luxd", "LUX_HOST_TOKEN=luxh_x", "LUX_HOST_NAME=h")
	if _, err := os.Stat(root + "/etc/sysctl.d"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("bootstrap.sh created /etc/sysctl.d (stat err %v)", err)
	}
	if calls, err := os.ReadFile(root + "/sysctl.calls"); err == nil {
		t.Errorf("bootstrap.sh ran sysctl: %q", calls)
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

// Script exports the env ahead of the shared, unfilled script body and
// adds the writeback limits after it: the two differ only by those.
func TestScriptWrapsBootstrap(t *testing.T) {
	env := Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_x", HostName: "h"}
	got := Script(env)
	if !strings.HasPrefix(got, "#!/bin/bash\nexport LUX_URL='http://10.0.1.10:7070'\nexport LUX_HOST_TOKEN='luxh_x'\nexport LUX_HOST_NAME='h'\n") {
		t.Errorf("Script did not export the env first:\n%s", got[:200])
	}
	if !strings.HasSuffix(got, strings.TrimPrefix(scriptBody, "#!/bin/bash\n")+writebackScript) {
		t.Error("Script is not the exports, the shared scriptBody, then the writeback limits")
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

// A shaped pool's host gets its nominal cpus, memory and disk and the
// pool's disk headroom in runner.env, in every format; an unshaped pool's
// renderings are byte for byte what they were (the expected bytes are
// written out, not derived from the code under test).
func TestShapedEnv(t *testing.T) {
	plain := Env{URL: "http://luxd", HostToken: "luxh_x", HostName: "h", EC2IMDS: "http://169.254.169.254", Memory: "68719476736"}
	const plainLines = "LUX_URL=http://luxd\nLUX_HOST_TOKEN=luxh_x\nLUX_HOST_NAME=h\nLUX_EC2_IMDS=http://169.254.169.254\nLUX_RUNNER_MEMORY=68719476736\n"
	if got := plain.Lines(); got != plainLines {
		t.Errorf("unshaped Lines() = %q, want %q", got, plainLines)
	}
	const plainScriptHead = "#!/bin/bash\nexport LUX_URL='http://luxd'\nexport LUX_HOST_TOKEN='luxh_x'\nexport LUX_HOST_NAME='h'\n" +
		"export LUX_EC2_IMDS='http://169.254.169.254'\nexport LUX_RUNNER_MEMORY='68719476736'\n[ -n \"${BASH_VERSION:-}\" ]"
	if got := Script(plain); !strings.HasPrefix(got, plainScriptHead) {
		t.Errorf("unshaped Script() starts %q, want %q", got[:len(plainScriptHead)], plainScriptHead)
	}

	shaped := plain
	shaped.CPUs, shaped.Memory, shaped.Disk, shaped.DiskHeadroom = "8", "34359738368", "171798691840", "10737418240"
	const shapedLines = "LUX_URL=http://luxd\nLUX_HOST_TOKEN=luxh_x\nLUX_HOST_NAME=h\nLUX_EC2_IMDS=http://169.254.169.254\n" +
		"LUX_RUNNER_CPUS=8\nLUX_RUNNER_MEMORY=34359738368\nLUX_RUNNER_DISK=171798691840\nLUX_RUNNER_DISK_HEADROOM=10737418240\n"
	if got := shaped.Lines(); got != shapedLines {
		t.Errorf("shaped Lines() = %q, want %q", got, shapedLines)
	}
	// What the script persists for systemd is what the runner gets.
	want := map[string]string{"LUX_URL": "http://luxd", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_EC2_IMDS": "http://169.254.169.254",
		"LUX_RUNNER_CPUS": "8", "LUX_RUNNER_MEMORY": "34359738368", "LUX_RUNNER_DISK": "171798691840", "LUX_RUNNER_DISK_HEADROOM": "10737418240"}
	if got := bootRunnerEnv(t, Script(shaped)); !maps.Equal(got, want) {
		t.Errorf("shaped script: runner.env %v, want %v", got, want)
	}
	// Headroom 0 is a value: written, so the runner scales.
	zero := shaped
	zero.DiskHeadroom = "0"
	if got := bootRunnerEnv(t, Script(zero)); got["LUX_RUNNER_DISK_HEADROOM"] != "0" {
		t.Errorf("headroom 0: runner.env %v", got)
	}
	b, err := Ignition(shaped)
	if err != nil {
		t.Fatal(err)
	}

	if _, got := installedFile(t, b, "/etc/lux/runner.env"); got != shapedLines {
		t.Errorf("ignition runner.env %q, want %q", got, shapedLines)
	}
}

// bootRunnerEnv runs script under bash with its fixed paths moved into a
// temp root and podman, nft, git and systemctl stubbed, then returns the
// /etc/lux/runner.env it wrote, parsed. environ is added to a bare env.
func bootRunnerEnv(t *testing.T, script string, environ ...string) map[string]string {
	t.Helper()
	root := bootScript(t, script, environ...)
	data, err := os.ReadFile(root + "/etc/lux/runner.env")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("runner.env line %q is not KEY=value", line)
		}
		got[k] = v
	}
	return got
}

// bootScript runs a rendered script against a temp root (its /etc, the
// install dir and cgroup paths rewritten there; podman, nft, git,
// systemctl and sysctl stubbed, sysctl recording its arguments in
// sysctl.calls) and returns that root.
func bootScript(t *testing.T, script string, environ ...string) string {
	t.Helper()
	return bootScriptWith(t, script, "<record>", environ...)
}

// bootScriptWith is bootScript with sysctl's stub body chosen: "<record>"
// records its arguments, "" leaves sysctl off PATH altogether.
func bootScriptWith(t *testing.T, script, sysctl string, environ ...string) string {
	t.Helper()
	root := t.TempDir()
	stubs := filepath.Join(root, "stubs")
	for _, dir := range []string{stubs, root + "/etc/systemd/system", root + InstallDir, root + "/sys/fs/cgroup"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(root+"/sys/fs/cgroup/cgroup.controllers", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stubBodies := map[string]string{"podman": "echo 5.2.0", "nft": "exit 0", "git": "exit 0", "systemctl": "exit 0"}
	switch sysctl {
	case "<record>":
		stubBodies["sysctl"] = `echo "$*" >> "` + root + `/sysctl.calls"`
	case "":
		// No stub: the script's PATH is stubs, /usr/bin and /bin, so a host
		// sysctl there would still be found. Fail the test rather than pass
		// it for the wrong reason.
		for _, d := range []string{"/usr/bin", "/bin"} {
			if _, err := os.Stat(d + "/sysctl"); err == nil {
				t.Skipf("%s/sysctl exists on this machine; cannot simulate a host without it", d)
			}
		}
	default:
		stubBodies["sysctl"] = sysctl
	}
	for name, body := range stubBodies {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixed := []string{"/etc/", InstallDir, "/sys/fs/cgroup/"}
	var pairs []string
	for _, p := range fixed {
		pairs = append(pairs, p, root+p)
	}
	script = strings.NewReplacer(pairs...).Replace(script)
	for _, p := range fixed {
		if strings.Count(script, p) != strings.Count(script, root+p) {
			t.Fatalf("the script still names %s outside the temp root", p)
		}
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append([]string{"PATH=" + stubs + ":/usr/bin:/bin", "HOME=" + root}, environ...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the script: %v\n%s", err, out)
	}
	return root
}

// systemd starts the runner from /etc/lux/runner.env, not from the
// script's environment: what the script writes there is what the runner
// gets. LUX_NESTED=true survives next to the memory; an ordinary template
// and anything but exactly "true" leave it out.
func TestScriptPersistsNestedAndMemoryToRunnerEnv(t *testing.T) {
	base := map[string]string{"LUX_URL": "http://luxd", "LUX_HOST_TOKEN": "luxh_x", "LUX_HOST_NAME": "h", "LUX_RUNNER_MEMORY": "68719476736"}
	with := func(k, v string) map[string]string {
		m := maps.Clone(base)
		m[k] = v
		return m
	}
	env := Env{URL: "http://luxd", HostToken: "luxh_x", HostName: "h", Memory: "68719476736"}
	nested := env
	nested.Nested = true
	if got, want := bootRunnerEnv(t, Script(nested)), with("LUX_NESTED", "true"); !maps.Equal(got, want) {
		t.Errorf("nested template: runner.env %v, want %v", got, want)
	}
	if got := bootRunnerEnv(t, Script(env)); !maps.Equal(got, base) {
		t.Errorf("ordinary template: runner.env %v, want %v", got, base)
	}
	for _, v := range []string{"true", "false", "TRUE"} {
		want := base
		if v == "true" {
			want = with("LUX_NESTED", "true")
		}
		if got := bootRunnerEnv(t, Bootstrap(), "LUX_URL=http://luxd", "LUX_HOST_TOKEN=luxh_x", "LUX_HOST_NAME=h", "LUX_RUNNER_MEMORY=68719476736", "LUX_NESTED="+v); !maps.Equal(got, want) {
			t.Errorf("bootstrap.sh with LUX_NESTED=%s: runner.env %v, want %v", v, got, want)
		}
	}
}
