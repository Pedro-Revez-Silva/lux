package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/internal/blob"
)

// TestMain lets tests run luxd itself: the test binary re-executed with
// luxdExecEnv set runs main instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv(luxdExecEnv) == "1" {
		os.Args = append([]string{"luxd"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const luxdExecEnv = "LUXD_TEST_EXEC"

type luxdResult struct {
	code           int
	stdout, stderr string
}

// runLuxd runs luxd with args and only the given environment (plus an
// AWS configuration pointed at nothing, so the host's own is never read).
func runLuxd(t *testing.T, env []string, args ...string) luxdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	home := t.TempDir()
	cmd.Env = append([]string{
		luxdExecEnv + "=1",
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"AWS_CONFIG_FILE=" + filepath.Join(home, "aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(home, "aws-credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
	}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("luxd %q did not finish before its deadline: %v", args, ctx.Err())
	}
	var exit *exec.ExitError
	code := 0
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run luxd %q: %v", args, err)
	}
	return luxdResult{code, stdout.String(), stderr.String()}
}

// fakePostgres is a TCP listener standing where PostgreSQL would be: it
// counts connections and closes each at once.
type fakePostgres struct {
	ln    net.Listener
	conns atomic.Int32
	done  chan struct{}
}

func newFakePostgres(t *testing.T) *fakePostgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePostgres{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-f.done })
	return f
}

func (f *fakePostgres) url() string {
	return fmt.Sprintf("postgres://lux_app:db-secret-%s@%s/lux?sslmode=disable&connect_timeout=2", "x7Q", f.ln.Addr())
}

// connections is the count once every accepted connection is counted.
func (f *fakePostgres) connections() int32 {
	f.ln.Close()
	<-f.done
	return f.conns.Load()
}

func writeConfig(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "luxd.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// validConfig is a configuration serve accepts, with its S3 endpoint at
// s3URL and no static keys.
func validConfig(dbURL, s3URL string) string {
	return fmt.Sprintf("listen = \"127.0.0.1:0\"\n[database]\nurl = %q\n[s3]\nbucket = \"lux-test\"\nendpoint = %q\n", dbURL, s3URL)
}

// staticKeys follows validConfig's [s3] table: with them the SDK needs no
// credential source to sign a request.
const staticKeys = "access_key = \"AKIATEST\"\nsecret_key = \"s3-secret-value\"\n"

// badConfigs are configurations serve refuses before connecting, each
// with the exact message luxd prints. validate and serve must agree on
// every one of them.
func badConfigs(dbURL, s3URL string) []struct {
	name, file string
	env        []string
	want       string
} {
	valid := validConfig(dbURL, s3URL)
	previewEnv := []string{"LUX_PREVIEW_DOMAIN=lux.example.com", "LUX_PUBLIC_URL=https://lux.example.com"}
	return []struct {
		name, file string
		env        []string
		want       string
	}{
		{"database.url missing", "[s3]\nbucket = \"lux-test\"\n", nil,
			"database.url is required (database.url in the config file, or LUX_DATABASE_URL)"},
		{"s3.bucket missing", fmt.Sprintf("[database]\nurl = %q\n", dbURL), nil,
			"s3.bucket is required (s3.bucket in the config file, or LUX_S3_BUCKET)"},
		{"database.url malformed", "[database]\nurl = \"postgres://lux:pw-secret@[::1\"\n[s3]\nbucket = \"b\"\n", nil,
			"database.url (LUX_DATABASE_URL) is not a valid PostgreSQL connection string"},
		{"listen malformed", valid, []string{"LUX_LISTEN=7070"},
			`listen (LUX_LISTEN) "7070": want host:port`},
		{"listen port too large", valid, []string{"LUX_LISTEN=127.0.0.1:99999"},
			`listen (LUX_LISTEN) "127.0.0.1:99999": want a port number 0-65535, not a service name`},
		{"listen port name", valid, []string{"LUX_LISTEN=127.0.0.1:not-a-port"},
			`listen (LUX_LISTEN) "127.0.0.1:not-a-port": want a port number 0-65535, not a service name`},
		{"listen port 65536", valid, []string{"LUX_LISTEN=:65536"},
			`listen (LUX_LISTEN) ":65536": want a port number 0-65535, not a service name`},
		{"listen port negative", valid, []string{"LUX_LISTEN=:-1"},
			`listen (LUX_LISTEN) ":-1": want a port number 0-65535, not a service name`},
		{"preview.listen port too large", valid, append(previewEnv, "LUX_PREVIEW_LISTEN=127.0.0.1:99999"),
			`configuration: preview.listen (LUX_PREVIEW_LISTEN) "127.0.0.1:99999": want a port number 0-65535, not a service name`},
		{"preview.listen port name", valid, append(previewEnv, "LUX_PREVIEW_LISTEN=127.0.0.1:not-a-port"),
			`configuration: preview.listen (LUX_PREVIEW_LISTEN) "127.0.0.1:not-a-port": want a port number 0-65535, not a service name`},
		{"preview.listen port 65536", valid, append(previewEnv, "LUX_PREVIEW_LISTEN=:65536"),
			`configuration: preview.listen (LUX_PREVIEW_LISTEN) ":65536": want a port number 0-65535, not a service name`},
		{"preview.listen port negative", valid, append(previewEnv, "LUX_PREVIEW_LISTEN=:-1"),
			`configuration: preview.listen (LUX_PREVIEW_LISTEN) ":-1": want a port number 0-65535, not a service name`},
		{"unknown key", "lisen = \"x\"\n" + valid, nil,
			"%PATH%: unknown keys: lisen"},
		{"bad duration", valid + "[costs]\nevery = \"soon\"\n", nil,
			`%PATH%: line 8 column 9: costs.every: toml: time: invalid duration "soon"`},
		{"bad env value", valid, []string{"LUX_LEASE=soon"},
			`LUX_LEASE: "soon": time: invalid duration "soon"`},
		{"console auth", valid + "[console]\nauth = \"magic\"\n", nil,
			`configuration: console.auth (LUX_CONSOLE_AUTH) "magic": want key or cloudflare-access`},
		{"cloudflare access incomplete", valid, []string{"LUX_CONSOLE_AUTH=cloudflare-access", "LUX_CF_ACCESS_TEAM=acme", "LUX_CF_ACCESS_AUD=aud", "LUX_CF_ACCESS_DEFAULT_TENANT=t"},
			"configuration: console.cloudflare_access.operators (LUX_CF_ACCESS_OPERATORS) needs at least one email"},
		{"preview without public_url", valid, []string{"LUX_PREVIEW_DOMAIN=lux.example.com"},
			"configuration: preview.auth ticket needs public_url (LUX_PUBLIC_URL): previews send people there to sign in"},
		{"cost plugin", valid + "[[costs.plugin]]\nname = \"compute\"\nurl = \"https://ledger.example\"\n", nil,
			"configuration: costs.plugin[0].name must be nonempty, unique and not compute"},
		{"defaults", valid, []string{"LUX_DEFAULT_PIDS=-1"},
			"configuration: defaults.pids (LUX_DEFAULT_PIDS) must be a positive number"},
		{"s3.endpoint relative", valid, []string{"LUX_S3_ENDPOINT=minio:9000"},
			"s3.endpoint (LUX_S3_ENDPOINT): want an absolute http or https URL"},
		{"s3.endpoint scheme", valid, []string{"LUX_S3_ENDPOINT=ftp://secret-host/"},
			"s3.endpoint (LUX_S3_ENDPOINT): want an absolute http or https URL"},
		{"s3.public_endpoint no host", valid, []string{"LUX_S3_PUBLIC_ENDPOINT=https://"},
			"s3.public_endpoint (LUX_S3_PUBLIC_ENDPOINT): want an absolute http or https URL"},
		{"s3.region empty", valid + "region = \"\"\n", nil,
			"s3.region (LUX_S3_REGION) is empty: set a region or remove the key (default us-east-1)"},
		{"s3.access_key alone", valid, []string{"LUX_S3_ACCESS_KEY=AKIA"},
			"s3.access_key and s3.secret_key (LUX_S3_ACCESS_KEY, LUX_S3_SECRET_KEY): set both or neither"},
		{"s3.secret_key alone", valid, []string{"LUX_S3_SECRET_KEY=s3-secret-value"},
			"s3.access_key and s3.secret_key (LUX_S3_ACCESS_KEY, LUX_S3_SECRET_KEY): set both or neither"},
	}
}

// An empty host, an empty port, ports 0 and 65535 and a bracketed IPv6 host
// are valid listen addresses, and so is an empty listen setting.
func TestValidateAcceptsListenPorts(t *testing.T) {
	pg := newFakePostgres(t)
	path := writeConfig(t, 0o600, validConfig(pg.url(), "http://127.0.0.1:1"))
	for _, addr := range []string{":0", ":8080", ":65535", "127.0.0.1:", "[::1]:8080"} {
		env := []string{"LUX_LISTEN=" + addr}
		preview := []string{"LUX_PREVIEW_DOMAIN=lux.example.com", "LUX_PUBLIC_URL=https://lux.example.com", "LUX_PREVIEW_LISTEN=" + addr}
		for _, env := range [][]string{env, preview} {
			if r := runLuxd(t, env, "--config", path, "validate"); r.code != 0 {
				t.Fatalf("%q: %+v", env, r)
			}
		}
	}
	// An empty variable is ignored, so an empty listen can only come from the file.
	body := strings.Replace(validConfig(pg.url(), "http://127.0.0.1:1"), `listen = "127.0.0.1:0"`, `listen = ""`, 1)
	emptyListen := writeConfig(t, 0o600, body)
	if r := runLuxd(t, nil, "--config", emptyListen, "validate"); r.code != 0 || r.stdout != "ok: "+emptyListen+"\n" || r.stderr != "" {
		t.Fatalf("empty listen: %+v", r)
	}
}

// countingServer is an HTTP endpoint that counts the requests it gets.
type countingServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	s := &countingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.Close)
	return s
}

// validate builds no AWS client: with defaults mode auto the SDK asks IMDS
// for the instance's region even when the keys are static.
func TestValidateDoesNotAskIMDS(t *testing.T) {
	pg := newFakePostgres(t)
	imds := newCountingServer(t)
	s3 := newCountingServer(t)
	path := writeConfig(t, 0o600, validConfig(pg.url(), s3.URL)+staticKeys)
	env := []string{
		"AWS_EC2_METADATA_DISABLED=false",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT=" + imds.URL,
		"AWS_DEFAULTS_MODE=auto",
	}
	for _, cmd := range []string{"validate", "check-config"} {
		if r := runLuxd(t, env, "--config", path, cmd); r.code != 0 {
			t.Fatalf("%s: %+v", cmd, r)
		}
	}
	if n := imds.requests.Load(); n != 0 {
		t.Fatalf("validate made %d requests to IMDS", n)
	}
	if n := s3.requests.Load(); n != 0 {
		t.Fatalf("validate made %d requests to S3", n)
	}

	// The control: building the S3 client from the same plan and
	// environment does reach the fixture.
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	home := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "aws-credentials"))
	_, _, plan, err := loadServe(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blob.New(context.Background(), plan.s3); err != nil {
		t.Fatal(err)
	}
	if imds.requests.Load() == 0 {
		t.Fatal("blob.New never reached the IMDS fixture: the zero-request assertion proves nothing")
	}
}

// A container credentials URL naming a host that does not resolve makes
// the SDK look it up with no deadline; validate must not wait on it.
func TestValidateIgnoresContainerCredentials(t *testing.T) {
	pg := newFakePostgres(t)
	path := writeConfig(t, 0o600, validConfig(pg.url(), "http://127.0.0.1:1"))
	r := runLuxd(t, []string{"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://unresolvable.invalid/creds"}, "--config", path, "validate")
	if r.code != 0 || r.stdout != "ok: "+path+"\n" || r.stderr != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestValidate(t *testing.T) {
	pg := newFakePostgres(t)
	s3 := newCountingServer(t)
	// The control: a request to the fixture is counted.
	resp, err := http.Get(s3.URL + "/lux-test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := s3.requests.Load(); n != 1 {
		t.Fatalf("the S3 fixture counted %d requests for 1", n)
	}
	valid := writeConfig(t, 0o600, validConfig(pg.url(), s3.URL)+staticKeys)

	for _, cmd := range []string{"validate", "check-config"} {
		t.Run(cmd, func(t *testing.T) {
			r := runLuxd(t, nil, "--config", valid, cmd)
			if r.code != 0 || r.stdout != "ok: "+valid+"\n" || r.stderr != "" {
				t.Fatalf("valid file: %+v", r)
			}
			// --config after the command, as serve takes it.
			if r := runLuxd(t, nil, cmd, "--config", valid); r.code != 0 || r.stdout != "ok: "+valid+"\n" {
				t.Fatalf("--config after: %+v", r)
			}
			// The environment alone, when no file exists at the default path.
			if _, err := os.Stat(defaultConfigPath); errors.Is(err, os.ErrNotExist) {
				r := runLuxd(t, []string{"LUX_DATABASE_URL=" + pg.url(), "LUX_S3_BUCKET=b", "LUX_S3_ENDPOINT=" + s3.URL}, cmd)
				if r.code != 0 || r.stdout != "ok: no file\n" {
					t.Fatalf("no file: %+v", r)
				}
			}

			for _, bad := range badConfigs(pg.url(), s3.URL) {
				t.Run(bad.name, func(t *testing.T) {
					path := writeConfig(t, 0o600, bad.file)
					r := runLuxd(t, bad.env, "--config", path, cmd)
					want := strings.ReplaceAll(bad.want, "%PATH%", path)
					if r.code != 1 || r.stderr != "luxd: "+want+"\n" || r.stdout != "" {
						t.Fatalf("got %+v\nwant exit 1 and %q", r, want)
					}
					// Every secret value in badConfigs is spelled -secret or secret-.
					if strings.Contains(r.stderr, "-secret") || strings.Contains(r.stderr, "secret-") {
						t.Fatalf("a secret was printed: %q", r.stderr)
					}
				})
			}

			t.Run("world-readable", func(t *testing.T) {
				path := writeConfig(t, 0o644, validConfig(pg.url(), s3.URL)+"access_key = \"AKIA\"\nsecret_key = \"s3-secret-value\"\n")
				r := runLuxd(t, nil, "--config", path, cmd)
				if r.code != 0 || r.stdout != "ok: "+path+"\n" || !strings.Contains(r.stderr, "warning: others can read "+path) {
					t.Fatalf("got %+v", r)
				}
				if strings.Contains(r.stdout+r.stderr, "secret-") || strings.Contains(r.stdout+r.stderr, "AKIA") {
					t.Fatalf("a secret was printed: %+v", r)
				}
			})

			t.Run("extra argument", func(t *testing.T) {
				r := runLuxd(t, nil, "--config", valid, cmd, "now")
				if r.code != 2 || !strings.Contains(r.stderr, "usage: luxd") || r.stdout != "" {
					t.Fatalf("got %+v", r)
				}
			})
		})
	}
	if n := pg.connections(); n != 0 {
		t.Fatalf("validate connected to the database %d times", n)
	}
	if n := s3.requests.Load() - 1; n != 0 {
		t.Fatalf("validate made %d requests to S3", n)
	}
}

// serve refuses exactly the configurations validate refuses, with the
// same message, before it connects; a configuration validate accepts
// gets as far as the database.
func TestServeRefusesWhatValidateRefuses(t *testing.T) {
	pg := newFakePostgres(t)
	s3 := newCountingServer(t)
	for _, bad := range badConfigs(pg.url(), s3.URL) {
		t.Run(bad.name, func(t *testing.T) {
			path := writeConfig(t, 0o600, bad.file)
			v := runLuxd(t, bad.env, "--config", path, "validate")
			s := runLuxd(t, bad.env, "--config", path, "serve")
			if v.code != 1 || s.code != 1 || v.stderr != s.stderr {
				t.Fatalf("validate %+v\nserve %+v", v, s)
			}
			for _, e := range bad.env {
				k, val, _ := strings.Cut(e, "=")
				t.Setenv(k, val)
			}
			_, _, _, err := loadServe(path)
			if err == nil || "luxd: "+err.Error()+"\n" != v.stderr {
				t.Fatalf("loadServe: %v, validate printed %q", err, v.stderr)
			}
		})
	}
	if n := pg.connections(); n != 0 {
		t.Fatalf("serve connected to the database %d times with a refused configuration", n)
	}
	if n := s3.requests.Load(); n != 0 {
		t.Fatalf("serve made %d requests to S3 with a refused configuration", n)
	}

	// The control: the listener does see serve when the configuration passes.
	pg = newFakePostgres(t)
	path := writeConfig(t, 0o600, validConfig(pg.url(), s3.URL)+staticKeys)
	if r := runLuxd(t, nil, "--config", path, "validate"); r.code != 0 {
		t.Fatalf("validate: %+v", r)
	}
	s := runLuxd(t, nil, "--config", path, "serve")
	if s.code != 1 || !strings.Contains(s.stderr, "connect to database") {
		t.Fatalf("serve: %+v", s)
	}
	if n := pg.connections(); n == 0 {
		t.Fatal("serve never reached the fake database: the no-connection assertions prove nothing")
	}
}
